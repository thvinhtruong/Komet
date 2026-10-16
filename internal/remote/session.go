package remote

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/komet-cli/komet/internal/config"
	"github.com/komet-cli/komet/internal/proc"
	"github.com/komet-cli/komet/internal/router"
)

// RemotePort is where the model server listens on the target's loopback.
const RemotePort = 18091

// Phases of a session, in order. `komet up` shows them as they happen.
const (
	PhaseWaking     = "waking machine"
	PhaseSSH        = "waiting for ssh"
	PhaseProbing    = "probing hardware"
	PhaseInstalling = "installing engine (first time only)"
	PhaseStarting   = "starting model server"
	PhaseTunnel     = "opening tunnel"
	PhaseReady      = "ready"
)

// Boot records how long each phase took, so the next start can be
// predicted and slow phases are visible.
type Boot struct {
	At        time.Time `json:"at"`
	WakeS     float64   `json:"wake_s"`     // provider start until running with an address
	SSHS      float64   `json:"ssh_s"`      // until ssh login works
	InstallS  float64   `json:"install_s"`  // engine install (first time only)
	ServerS   float64   `json:"server_s"`   // download + load until healthy
	DownloadS float64   `json:"download_s"` // model download part of ServerS
	Download  bool      `json:"downloaded,omitempty"`
	TotalS    float64   `json:"total_s"`
	Reused    bool      `json:"reused"` // server was still running
	WasAwake  bool      `json:"was_awake"`
}

// Profile is what Komet remembers about a target between sessions.
type Profile struct {
	Target   string          `json:"target"`
	Kind     string          `json:"kind"`
	Hardware string          `json:"hardware,omitempty"`
	Probe    json.RawMessage `json:"probe,omitempty"`
	GPUs     []GPU           `json:"gpus,omitempty"`
	VRAMMB   int             `json:"vram_mb"`
	Engine   string          `json:"engine"`
	Model    string          `json:"model,omitempty"`
	Updated  time.Time       `json:"updated"`
	Boots    []Boot          `json:"boots,omitempty"` // most recent last, capped
}

type GPU struct {
	Name   string `json:"name"`
	VRAMMB int    `json:"vram_mb"`
	Driver string `json:"driver"`
}

type probeResult struct {
	GPUs          []GPU  `json:"gpus"`
	EnginePresent bool   `json:"engine_present"`
	EngineBin     string `json:"engine_bin"`
	DiskFreeMB    int    `json:"disk_free_mb"`
	RAMMB         int    `json:"ram_mb"`
	Cuda          string `json:"cuda"`
}

type startResult struct {
	Reused       bool    `json:"reused"`
	Downloaded   bool    `json:"downloaded"`
	DownloadS    float64 `json:"download_s"`
	EngineReadyS float64 `json:"engine_ready_s"`
}

// State is the live session, written by the supervisor.
type State struct {
	Target      string    `json:"target"`
	PID         int       `json:"pid"`
	Phase       string    `json:"phase"`
	Error       string    `json:"error,omitempty"`
	Host        string    `json:"host,omitempty"`
	SSHPort     int       `json:"ssh_port,omitempty"`
	LocalPort   int       `json:"local_port,omitempty"`
	Engine      string    `json:"engine,omitempty"`
	Model       string    `json:"model,omitempty"`        // display name
	ServedModel string    `json:"served_model,omitempty"` // name the API expects
	Hardware    string    `json:"hardware,omitempty"`
	CostPerHour float64   `json:"cost_per_hour"`
	Started     time.Time `json:"started"`
	Ready       time.Time `json:"ready,omitempty"`
	Boot        Boot      `json:"boot"`
}

// SessionRecord is appended to sessions.jsonl when a session ends.
type SessionRecord struct {
	Target      string    `json:"target"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Hours       float64   `json:"hours"`
	CostPerHour float64   `json:"cost_per_hour"`
	USD         float64   `json:"usd"`
	Reason      string    `json:"reason"`
}

type Manager struct {
	cfg *config.Config
}

func New(cfg *config.Config) *Manager { return &Manager{cfg: cfg} }

func (m *Manager) targetDir() string { return filepath.Join(m.cfg.Home(), "targets") }
func (m *Manager) runDir() string    { return filepath.Join(m.cfg.RunDir(), "targets") }
func (m *Manager) path(dir, name, ext string) string {
	return filepath.Join(dir, name+ext)
}
func (m *Manager) statePath(n string) string    { return m.path(m.runDir(), n, ".json") }
func (m *Manager) activityPath(n string) string { return m.path(m.runDir(), n, ".activity") }
func (m *Manager) downPath(n string) string     { return m.path(m.runDir(), n, ".down") }
func (m *Manager) lockPath(n string) string     { return m.path(m.runDir(), n, ".lock") }
func (m *Manager) LogPath(n string) string      { return m.path(m.runDir(), n, ".log") }
func (m *Manager) profilePath(n string) string  { return m.path(m.targetDir(), n, ".profile.json") }
func (m *Manager) knownHosts(n string) string   { return m.path(m.targetDir(), n, ".known_hosts") }
func (m *Manager) keyPath(n string) string      { return m.path(m.targetDir(), n, ".key") }
func (m *Manager) sessionsPath() string         { return filepath.Join(m.cfg.Home(), "sessions.jsonl") }

func (m *Manager) target(name string) (*config.Target, error) {
	t := m.cfg.Target(name)
	if t == nil {
		return nil, fmt.Errorf("unknown target %q", name)
	}
	return t, nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Profile returns what Komet remembers about a target (nil if never started).
func (m *Manager) Profile(name string) *Profile {
	var p Profile
	if readJSON(m.profilePath(name), &p) != nil {
		return nil
	}
	return &p
}

// State returns the live session state, if a supervisor is alive.
func (m *Manager) State(name string) (*State, bool) {
	var st State
	if readJSON(m.statePath(name), &st) != nil || !proc.Alive(st.PID) {
		return nil, false
	}
	return &st, true
}

func healthy(port int, timeout time.Duration) bool {
	c := http.Client{Timeout: timeout}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Running reports whether the target is ready to serve right now.
func (m *Manager) Running(name string) bool {
	st, ok := m.State(name)
	return ok && st.Phase == PhaseReady && healthy(st.LocalPort, 500*time.Millisecond)
}

func (m *Manager) touch(name string) {
	now := time.Now()
	if err := os.Chtimes(m.activityPath(name), now, now); err != nil {
		_ = os.WriteFile(m.activityPath(name), nil, 0o600)
	}
}

func (m *Manager) apiKey(name string) (string, error) {
	if b, err := os.ReadFile(m.keyPath(name)); err == nil && len(b) > 0 {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	k := "komet-" + hex.EncodeToString(buf)
	if err := os.MkdirAll(m.targetDir(), 0o700); err != nil {
		return "", err
	}
	return k, os.WriteFile(m.keyPath(name), []byte(k), 0o600)
}

// Ensure returns an endpoint for the target, waking it if needed. While the
// returned Release has not been called, the session counts as active.
func (m *Manager) Ensure(ctx context.Context, name string, progress func(string)) (*router.Endpoint, error) {
	if !m.Running(name) {
		if _, err := m.Up(ctx, name, progress); err != nil {
			return nil, err
		}
	}
	st, ok := m.State(name)
	if !ok {
		return nil, fmt.Errorf("target %s went away", name)
	}
	key, err := m.apiKey(name)
	if err != nil {
		return nil, err
	}
	m.touch(name)
	stop := make(chan struct{})
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-stop:
				m.touch(name)
				return
			case <-t.C:
				m.touch(name)
			}
		}
	}()
	return &router.Endpoint{
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d/v1", st.LocalPort),
		APIKey:  key,
		Model:   st.ServedModel,
		Release: func() { close(stop) },
	}, nil
}

// Up wakes the target and blocks until it is ready, reporting phases.
func (m *Manager) Up(ctx context.Context, name string, progress func(string)) (*State, error) {
	if _, err := m.target(name); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(m.runDir(), 0o700); err != nil {
		return nil, err
	}
	unlock, err := proc.Lock(m.lockPath(name))
	if err != nil {
		return nil, err
	}
	st, ok := m.State(name)
	if ok && st.Error != "" {
		// A failed session is still shutting down; let it finish first.
		for i := 0; i < 300 && proc.Alive(st.PID); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		ok = false
	}
	if !ok {
		if err := m.checkBudget(); err != nil {
			unlock()
			return nil, err
		}
		_ = os.Remove(m.statePath(name))
		_ = os.Remove(m.downPath(name))
		m.touch(name)
		if err := m.spawn(name); err != nil {
			unlock()
			return nil, err
		}
	}
	unlock()

	last := ""
	for {
		st, ok = m.State(name)
		var cur State
		_ = readJSON(m.statePath(name), &cur)
		if cur.Error != "" {
			return nil, fmt.Errorf("%s: %s (log: %s)", name, cur.Error, m.LogPath(name))
		}
		if ok && st.Phase == PhaseReady {
			return st, nil
		}
		if !ok && cur.PID != 0 && !proc.Alive(cur.PID) {
			return nil, fmt.Errorf("%s: session supervisor exited (log: %s)", name, m.LogPath(name))
		}
		if ok && st.Phase != last && progress != nil {
			last = st.Phase
			progress(fmt.Sprintf("%s · %s", name, st.Phase))
		}
		m.touch(name)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (m *Manager) spawn(name string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(m.LogPath(name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(self, "__session", "--target", name)
	cmd.Stdout, cmd.Stderr = logf, logf
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start session supervisor: %w", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return writeJSON(m.statePath(name), &State{Target: name, PID: pid, Phase: PhaseWaking, Started: time.Now()})
}

// Down ends the session. keepMachine only closes the tunnel and leaves the
// machine (and its model server) running.
func (m *Manager) Down(ctx context.Context, name string, keepMachine bool) (string, error) {
	t, err := m.target(name)
	if err != nil {
		return "", err
	}
	mode := "stop"
	if keepMachine {
		mode = "keep"
	}
	if st, ok := m.State(name); ok {
		_ = os.WriteFile(m.downPath(name), []byte(mode), 0o600)
		proc.Terminate(st.PID)
		for i := 0; i < 600 && proc.Alive(st.PID); i++ { // stopping can take a while
			time.Sleep(100 * time.Millisecond)
		}
		return "session closed", nil
	}
	// No supervisor: the machine may still be up from a crashed session.
	if keepMachine {
		return "no session running", nil
	}
	p, err := NewProvider(t)
	if err != nil {
		return "", err
	}
	in, err := p.Info(ctx)
	if err != nil && in.State == StateUnknown {
		return "", err
	}
	if in.State != StateRunning && in.State != StatePending {
		return "machine already " + in.State, nil
	}
	if in.Host != "" {
		c := &sshConn{t: t, host: in.Host, port: in.Port, knownHosts: m.knownHosts(name)}
		_ = c.Agent(ctx, nil, nil, "stop", t.WorkDir)
	}
	if t.Kind == "ssh" {
		return "model server stopped", nil
	}
	if err := p.Stop(ctx); err != nil {
		return "", err
	}
	return "machine stop requested", nil
}

// SpentToday sums finished sessions today plus the running ones so far.
func (m *Manager) SpentToday() (float64, []SessionRecord) {
	recs := m.Sessions()
	y, mo, d := time.Now().Date()
	day := time.Date(y, mo, d, 0, 0, 0, 0, time.Local)
	var usd float64
	for _, r := range recs {
		if r.End.After(day) {
			usd += r.USD
		}
	}
	for _, t := range m.cfg.Targets {
		if st, ok := m.State(t.Name); ok {
			from := st.Started
			if from.Before(day) {
				from = day
			}
			usd += time.Since(from).Hours() * st.CostPerHour
		}
	}
	return usd, recs
}

func (m *Manager) Sessions() []SessionRecord {
	f, err := os.Open(m.sessionsPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []SessionRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r SessionRecord
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].End.Before(out[j].End) })
	return out
}

func (m *Manager) checkBudget() error {
	max := m.cfg.Budget.MaxUSDPerDay
	if max <= 0 {
		return nil
	}
	if spent, _ := m.SpentToday(); spent >= max {
		return fmt.Errorf("daily budget reached ($%.2f of $%.2f); raise budget.max_usd_per_day to continue", spent, max)
	}
	return nil
}

// ------------------------------------------------------------- supervisor

// supervisor is the detached `komet __session` process that owns one
// session: it wakes the machine, provisions it, holds the tunnel, and
// tears everything down when idle, over budget, or told to.
type supervisor struct {
	m     *Manager
	t     *config.Target
	p     Provider
	st    State
	conn  *sshConn
	start time.Time
	// tunnelDone receives when the ssh tunnel process exits.
	tunnelDone chan struct{}
}

func (s *supervisor) save() {
	if err := writeJSON(s.m.statePath(s.t.Name), &s.st); err != nil {
		log.Printf("write state: %v", err)
	}
}

func (s *supervisor) phase(p string) {
	log.Printf("phase: %s", p)
	s.st.Phase = p
	s.save()
}

func (s *supervisor) fail(err error) error {
	log.Printf("error: %v", err)
	s.st.Error = err.Error()
	s.save()
	return err
}

func since(t time.Time) float64 { return time.Since(t).Round(100 * time.Millisecond).Seconds() }

// RunSession is the body of the hidden `komet __session` command.
func (m *Manager) RunSession(args []string) error {
	fs := flag.NewFlagSet("__session", flag.ContinueOnError)
	name := fs.String("target", "", "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	t, err := m.target(*name)
	if err != nil {
		return err
	}
	p, err := NewProvider(t)
	if err != nil {
		return err
	}
	log.SetPrefix("[" + t.Name + "] ")
	s := &supervisor{m: m, t: t, p: p, start: time.Now(), tunnelDone: make(chan struct{}, 4)}
	s.st = State{Target: t.Name, PID: os.Getpid(), Started: s.start, Engine: t.Engine}
	s.st.Boot.At = s.start

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	tunnel, err := s.bringUp(ctx)
	if err != nil {
		if ctx.Err() == nil {
			_ = s.fail(err)
		}
		s.shutdown(context.Background(), "startup failed: "+err.Error(), !s.st.Boot.WasAwake)
		return err
	}
	return s.serve(ctx, tunnel)
}

func (s *supervisor) bringUp(ctx context.Context) (*exec.Cmd, error) {
	t, m := s.t, s.m

	// 1. Wake the machine.
	s.phase(PhaseWaking)
	t0 := time.Now()
	in, err := s.p.Info(ctx)
	if err != nil && in.State != StateStopped {
		return nil, err
	}
	switch in.State {
	case StateGone:
		return nil, fmt.Errorf("machine no longer exists")
	case StateRunning:
		s.st.Boot.WasAwake = true
	default:
		if in.State == StateStopping {
			if err := s.waitState(ctx, StateStopped, 5*time.Minute); err != nil {
				return nil, err
			}
		}
		if t.Kind != "ssh" {
			// A fresh boot may come with a new container or a recycled IP,
			// hence a new host key: start a new trust-on-first-use pin.
			_ = os.Remove(m.knownHosts(t.Name))
		}
		log.Printf("starting machine")
		if err := s.p.Start(ctx); err != nil {
			return nil, err
		}
	}
	in, err = s.waitRunning(ctx, 10*time.Minute)
	if err != nil {
		return nil, err
	}
	s.st.Boot.WakeS = since(t0)
	s.st.Host, s.st.SSHPort, s.st.Hardware = in.Host, in.Port, in.Hardware
	s.st.CostPerHour = in.CostPerHour
	if t.CostPerHour > 0 {
		s.st.CostPerHour = t.CostPerHour
	}
	s.conn = &sshConn{t: t, host: in.Host, port: in.Port, knownHosts: m.knownHosts(t.Name)}

	// 2. SSH.
	s.phase(PhaseSSH)
	t0 = time.Now()
	var lastErr error
	for deadline := time.Now().Add(5 * time.Minute); ; {
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		lastErr = s.conn.Ping(c)
		cancel()
		if lastErr == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("ssh %s@%s:%d: %v", t.User, in.Host, in.Port, lastErr)
		}
		time.Sleep(2 * time.Second)
	}
	s.st.Boot.SSHS = since(t0)

	// 3. Probe and remember the hardware.
	s.phase(PhaseProbing)
	var raw json.RawMessage
	if err := s.conn.Agent(ctx, nil, &raw, "probe", t.WorkDir, t.Engine, t.ServerBin); err != nil {
		return nil, err
	}
	var pr probeResult
	_ = json.Unmarshal(raw, &pr)
	prof := m.Profile(t.Name)
	if prof == nil {
		prof = &Profile{Target: t.Name}
	}
	prof.Kind, prof.Engine, prof.Probe, prof.GPUs = t.Kind, t.Engine, raw, pr.GPUs
	prof.VRAMMB = 0
	for _, g := range pr.GPUs {
		prof.VRAMMB += g.VRAMMB
	}
	if in.Hardware != "" {
		prof.Hardware = in.Hardware
	} else if len(pr.GPUs) > 0 {
		prof.Hardware = fmt.Sprintf("%dx %s", len(pr.GPUs), pr.GPUs[0].Name)
	}
	s.st.Hardware = prof.Hardware

	mod, note := chooseModel(t, prof.VRAMMB)
	if mod == nil {
		return nil, errors.New(note)
	}
	if note != "" {
		log.Printf("%s", note)
	}
	s.st.Model = mod.Name
	s.st.ServedModel = mod.Name
	if t.Engine == "vllm" {
		s.st.ServedModel = mod.Path // vLLM serves under the repo id / path
	}
	prof.Model = mod.Name

	// 4. Engine (only the first time: it lives on the persistent volume).
	if !pr.EnginePresent {
		s.phase(PhaseInstalling)
		t0 = time.Now()
		c, cancel := context.WithTimeout(ctx, 45*time.Minute)
		err := s.conn.Agent(c, nil, nil, "install", t.WorkDir, t.Engine, t.InstallCmd)
		cancel()
		if err != nil {
			return nil, err
		}
		s.st.Boot.InstallS = since(t0)
	}

	// 5. Model server + watchdog.
	s.phase(PhaseStarting)
	t0 = time.Now()
	key, err := m.apiKey(t.Name)
	if err != nil {
		return nil, err
	}
	secrets := map[string]string{"KOMET_API_KEY": key}
	stopMode := s.p.StopMode()
	if stopMode == "runpod" {
		if k := os.Getenv(t.SelfStopKeyEnv); t.SelfStopKeyEnv != "" && k != "" {
			secrets["KOMET_STOP_KEY"] = k
		} else {
			log.Printf("no self_stop_key_env: the pod stops only when this computer stops it")
		}
	}
	// The remote watchdog is the safety net for when this computer vanishes:
	// it waits a grace period on top of the local idle timer.
	localIdle := int(t.AutostopIdle * 60)
	idle := localIdle + min(300, max(60, localIdle))
	maxUp := int(t.MaxUptimeHours * 3600)
	if t.OnIdle == "keep" {
		stopMode, idle = "none", maxUp
	}
	var sr startResult
	c, cancel := context.WithTimeout(ctx, 60*time.Minute)
	err = s.conn.Agent(c, secrets, &sr, "start", t.WorkDir, t.Engine, t.ServerBin, mod.Path, mod.URL,
		fmt.Sprint(RemotePort), fmt.Sprint(t.Ctx), fmt.Sprint(t.GPULayers), fmt.Sprint(t.Parallel),
		fmt.Sprint(idle), fmt.Sprint(maxUp), stopMode, t.PodID, t.APIBase)
	cancel()
	if err != nil {
		return nil, err
	}
	s.st.Boot.ServerS = since(t0)
	s.st.Boot.DownloadS, s.st.Boot.Download = sr.DownloadS, sr.Downloaded
	s.st.Boot.Reused = sr.Reused

	// 6. Tunnel.
	s.phase(PhaseTunnel)
	tunnel, port, err := s.openTunnel(ctx)
	if err != nil {
		return nil, err
	}
	s.st.LocalPort = port
	s.st.Boot.TotalS = since(s.start)
	s.st.Ready = time.Now()

	prof.Boots = append(prof.Boots, s.st.Boot)
	if len(prof.Boots) > 20 {
		prof.Boots = prof.Boots[len(prof.Boots)-20:]
	}
	prof.Updated = time.Now()
	if err := writeJSON(m.profilePath(t.Name), prof); err != nil {
		log.Printf("save profile: %v", err)
	}
	s.phase(PhaseReady)
	log.Printf("ready in %.1fs: %s on %s, $%.2f/h", s.st.Boot.TotalS, mod.Name, s.st.Hardware, s.st.CostPerHour)
	return tunnel, nil
}

func (s *supervisor) waitRunning(ctx context.Context, timeout time.Duration) (Info, error) {
	deadline := time.Now().Add(timeout)
	for {
		in, err := s.p.Info(ctx)
		if err == nil && in.State == StateRunning && in.Host != "" {
			return in, nil
		}
		if in.State == StateGone {
			return in, fmt.Errorf("machine no longer exists")
		}
		if time.Now().After(deadline) {
			if err != nil {
				return in, err
			}
			return in, fmt.Errorf("machine not running after %s (state %s)", timeout, in.State)
		}
		select {
		case <-ctx.Done():
			return in, ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

func (s *supervisor) waitState(ctx context.Context, want string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if in, _ := s.p.Info(ctx); in.State == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return fmt.Errorf("machine did not reach state %s", want)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func (s *supervisor) openTunnel(ctx context.Context) (*exec.Cmd, int, error) {
	port := s.st.LocalPort
	if port == 0 {
		var err error
		if port, err = freePort(); err != nil {
			return nil, 0, err
		}
	}
	cmd := s.conn.Tunnel(port, RemotePort)
	cmd.Stderr = log.Writer()
	if err := cmd.Start(); err != nil {
		return nil, 0, err
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	deadline := time.Now().Add(30 * time.Second)
	for !healthy(port, time.Second) {
		select {
		case <-exited:
			return nil, 0, fmt.Errorf("ssh tunnel exited")
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return nil, 0, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			return nil, 0, fmt.Errorf("model server not reachable through the tunnel")
		}
	}
	// Re-wrap so serve() can watch for exit.
	go func() { <-exited; s.tunnelDone <- struct{}{} }()
	return cmd, port, nil
}

func (s *supervisor) serve(ctx context.Context, tunnel *exec.Cmd) error {
	t := s.t
	idle := time.Duration(t.AutostopIdle * float64(time.Minute))
	maxUp := time.Duration(t.MaxUptimeHours * float64(time.Hour))
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	budgetTick := time.NewTicker(30 * time.Second)
	defer budgetTick.Stop()
	reconnects := 0
	for {
		select {
		case <-ctx.Done():
			mode, _ := os.ReadFile(s.m.downPath(t.Name))
			_ = tunnel.Process.Kill()
			stopMachine := strings.TrimSpace(string(mode)) != "keep"
			s.shutdown(context.Background(), "komet down", stopMachine)
			return nil
		case <-s.tunnelDone:
			// Network blip or the machine went away (e.g. the watchdog
			// stopped it). Try to reconnect a few times.
			reconnects++
			if reconnects > 3 {
				s.shutdown(context.Background(), "tunnel lost", false)
				return errors.New("tunnel lost")
			}
			log.Printf("tunnel exited; reconnecting (%d/3)", reconnects)
			time.Sleep(time.Duration(reconnects) * 2 * time.Second)
			var err error
			if tunnel, _, err = s.openTunnel(ctx); err != nil {
				log.Printf("reconnect failed: %v", err)
				s.shutdown(context.Background(), "tunnel lost", false)
				return err
			}
		case <-budgetTick.C:
			if err := s.m.checkBudget(); err != nil {
				_ = tunnel.Process.Kill()
				s.shutdown(context.Background(), "budget: "+err.Error(), true)
				return nil
			}
		case <-tick.C:
			reason := ""
			if fi, err := os.Stat(s.m.activityPath(t.Name)); err != nil || time.Since(fi.ModTime()) > idle {
				reason = fmt.Sprintf("idle for %s", idle)
			}
			if time.Since(s.start) > maxUp {
				reason = fmt.Sprintf("max uptime %s reached", maxUp)
			}
			if reason != "" {
				_ = tunnel.Process.Kill()
				s.shutdown(context.Background(), reason, t.OnIdle == "stop")
				return nil
			}
		}
	}
}

// shutdown stops the model server and, if stopMachine, the machine itself,
// then records the session's cost.
func (s *supervisor) shutdown(ctx context.Context, reason string, stopMachine bool) {
	t := s.t
	log.Printf("closing session: %s (stop machine: %v)", reason, stopMachine)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if stopMachine && s.conn != nil {
		if err := s.conn.Agent(ctx, nil, nil, "stop", t.WorkDir); err != nil {
			log.Printf("stop server: %v", err)
		}
	}
	if stopMachine && t.Kind != "ssh" {
		if err := s.p.Stop(ctx); err != nil {
			log.Printf("STOP FAILED, machine may still be billing: %v", err)
		} else {
			log.Printf("machine stop requested")
		}
	}
	end := time.Now()
	hours := end.Sub(s.start).Hours()
	rec := SessionRecord{Target: t.Name, Start: s.start, End: end, Hours: hours,
		CostPerHour: s.st.CostPerHour, USD: hours * s.st.CostPerHour, Reason: reason}
	if !stopMachine && t.Kind != "ssh" {
		rec.Reason += " (machine left running)"
	}
	if f, err := os.OpenFile(s.m.sessionsPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		b, _ := json.Marshal(rec)
		_, _ = f.Write(append(b, '\n'))
		f.Close()
	}
	if s.st.Error == "" { // keep a failed state so `komet up` can report it
		_ = os.Remove(s.m.statePath(t.Name))
	}
	_ = os.Remove(s.m.downPath(t.Name))
}

// chooseModel picks the largest configured model that fits in VRAM.
func chooseModel(t *config.Target, vramMB int) (*config.RemoteModel, string) {
	if vramMB == 0 {
		if t.Engine == "vllm" {
			return nil, "no NVIDIA GPU detected; the vllm engine needs one"
		}
		m := &t.Models[len(t.Models)-1]
		return m, "no NVIDIA GPU detected; running the smallest model on CPU"
	}
	usable := vramMB * 95 / 100
	var need []string
	for i := range t.Models {
		if t.Models[i].VRAMMB <= usable || t.Models[i].VRAMMB == 0 {
			return &t.Models[i], ""
		}
		need = append(need, fmt.Sprintf("%s needs %d MB", t.Models[i].Name, t.Models[i].VRAMMB))
	}
	return nil, fmt.Sprintf("no configured model fits in %d MB of VRAM (%s)", vramMB, strings.Join(need, ", "))
}
