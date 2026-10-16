// Package provision brings a local model up inside a resource envelope and
// takes it down again when it is no longer needed.
//
// Lifecycle: the first request spawns a small detached supervisor
// (`komet __daemon`), which starts llama-server with bounded threads,
// context and priority (and, optionally, a cgroup memory/CPU cap). Clients
// touch an activity file while they use it; once nobody has touched it for
// keep_alive_sec, the supervisor stops the model and exits, returning all
// memory to the OS. Repeated requests therefore pay the load cost once,
// and an idle machine pays nothing.
package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/komet-cli/komet/internal/config"
	"github.com/komet-cli/komet/internal/hw"
	"github.com/komet-cli/komet/internal/proc"
)

type State struct {
	DaemonPID int       `json:"daemon_pid"`
	ServerPID int       `json:"server_pid"`
	Port      int       `json:"port"`
	Model     string    `json:"model"`
	ModelPath string    `json:"model_path"`
	Started   time.Time `json:"started"`
	Ready     time.Time `json:"ready,omitempty"`
	Limits    string    `json:"limits"`
}

type Manager struct {
	cfg *config.Config
}

func New(cfg *config.Config) *Manager { return &Manager{cfg: cfg} }

func (m *Manager) statePath() string    { return filepath.Join(m.cfg.RunDir(), "server.json") }
func (m *Manager) activityPath() string { return filepath.Join(m.cfg.RunDir(), "activity") }
func (m *Manager) lockPath() string     { return filepath.Join(m.cfg.RunDir(), "lock") }
func (m *Manager) LogPath() string      { return filepath.Join(m.cfg.RunDir(), "server.log") }

// External reports whether the user pointed Komet at their own server.
func (m *Manager) External() bool { return m.cfg.Local.Endpoint != "" }

// ServerBin resolves the llama-server binary.
func (m *Manager) ServerBin() (string, error) {
	if b := m.cfg.Local.ServerBin; b != "" {
		if _, err := os.Stat(b); err != nil {
			return "", fmt.Errorf("local.server_bin %s: %w", b, err)
		}
		return b, nil
	}
	return exec.LookPath("llama-server")
}

// Select picks the largest configured model that fits right now, or explains
// why none does. This is the "hardware check" step of the router.
func (m *Manager) Select(s hw.Snapshot) (*config.LocalModel, string) {
	lc := m.cfg.Local
	if !lc.Enabled {
		return nil, "local tier disabled"
	}
	if m.External() {
		name := lc.EndpointModel
		if name == "" {
			name = "external"
		}
		return &config.LocalModel{Name: name}, ""
	}
	if _, err := m.ServerBin(); err != nil {
		return nil, "llama-server not found (install llama.cpp or set local.server_bin / local.endpoint)"
	}
	if lc.MaxLoad > 0 {
		if l := s.LoadPerCore(); l >= 0 && l > lc.MaxLoad {
			return nil, fmt.Sprintf("machine busy (load %.2f per core > %.2f)", l, lc.MaxLoad)
		}
	}
	var reasons []string
	for i := range lc.Models {
		mod := &lc.Models[i]
		if _, err := os.Stat(mod.Path); err != nil {
			reasons = append(reasons, mod.Name+": model file missing")
			continue
		}
		// A model that is already loaded costs nothing extra.
		if st, ok := m.running(); ok && st.ModelPath == mod.Path {
			return mod, ""
		}
		if s.AvailableMB >= 0 && s.AvailableMB-lc.HeadroomMB < mod.RAMMB {
			reasons = append(reasons, fmt.Sprintf("%s: needs %d MB, only %d MB free after %d MB headroom",
				mod.Name, mod.RAMMB, max(0, s.AvailableMB-lc.HeadroomMB), lc.HeadroomMB))
			continue
		}
		return mod, ""
	}
	if len(reasons) == 0 {
		return nil, "no local models configured"
	}
	return nil, strings.Join(reasons, "; ")
}

func (m *Manager) readState() (*State, error) {
	b, err := os.ReadFile(m.statePath())
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (m *Manager) writeState(st *State) error {
	b, _ := json.MarshalIndent(st, "", "  ")
	tmp := m.statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.statePath())
}

// running returns the live supervisor state, if any.
func (m *Manager) running() (*State, bool) {
	st, err := m.readState()
	if err != nil || !proc.Alive(st.DaemonPID) {
		return nil, false
	}
	return st, true
}

// Status describes the managed server for `komet status`.
func (m *Manager) Status() (*State, bool, bool) {
	st, ok := m.running()
	if !ok {
		return nil, false, false
	}
	return st, true, healthy(st.Port)
}

func (m *Manager) touch() {
	now := time.Now()
	if err := os.Chtimes(m.activityPath(), now, now); err != nil {
		_ = os.WriteFile(m.activityPath(), nil, 0o600)
	}
}

// Ensure makes the given model available and returns its OpenAI base URL.
// The returned release func must be called when the request finishes; until
// then the model is kept alive even if generation outlasts keep_alive_sec.
func (m *Manager) Ensure(ctx context.Context, mod *config.LocalModel, progress func(string)) (string, func(), error) {
	if m.External() {
		return strings.TrimRight(m.cfg.Local.Endpoint, "/"), func() {}, nil
	}
	if err := os.MkdirAll(m.cfg.RunDir(), 0o700); err != nil {
		return "", nil, err
	}
	unlock, err := proc.Lock(m.lockPath())
	if err != nil {
		return "", nil, err
	}
	defer unlock()

	m.touch()
	st, ok := m.running()
	if ok && st.ModelPath != mod.Path {
		progress("switching local model to " + mod.Name)
		m.stopLocked(st)
		ok = false
	}
	if !ok {
		progress("loading " + mod.Name)
		if st, err = m.spawn(mod); err != nil {
			return "", nil, err
		}
	}
	if err := waitHealthy(ctx, st, 180*time.Second); err != nil {
		return "", nil, fmt.Errorf("%w (see %s)", err, m.LogPath())
	}
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				m.touch()
				return
			case <-t.C:
				m.touch()
			}
		}
	}()
	return fmt.Sprintf("http://127.0.0.1:%d/v1", st.Port), func() { close(stop) }, nil
}

func (m *Manager) spawn(mod *config.LocalModel) (*State, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(m.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
	_ = os.Remove(m.statePath())
	cmd := exec.Command(self, "__daemon", "--model", mod.Path, "--name", mod.Name,
		"--ram-mb", strconv.Itoa(mod.RAMMB), "--port", strconv.Itoa(port))
	cmd.Stdout, cmd.Stderr = logf, logf
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start supervisor: %w", err)
	}
	_ = cmd.Process.Release()
	// The supervisor writes the state file once llama-server has started.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := m.readState(); err == nil && st.Port == port {
			return st, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil, fmt.Errorf("local model supervisor did not start (see %s)", m.LogPath())
}

// Stop unloads the local model now.
func (m *Manager) Stop() bool {
	unlock, err := proc.Lock(m.lockPath())
	if err == nil {
		defer unlock()
	}
	st, ok := m.running()
	if !ok {
		return false
	}
	m.stopLocked(st)
	return true
}

func (m *Manager) stopLocked(st *State) {
	proc.Terminate(st.DaemonPID)
	for i := 0; i < 100 && proc.Alive(st.DaemonPID); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	_ = os.Remove(m.statePath())
}

func waitHealthy(ctx context.Context, st *State, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if healthy(st.Port) {
			return nil
		}
		if !proc.Alive(st.DaemonPID) {
			return errors.New("local model server exited during startup")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("local model server did not become healthy in time")
}

func healthy(port int) bool {
	c := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
