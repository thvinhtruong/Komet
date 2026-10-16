package provision

import (
	"flag"
	"fmt"
	"github.com/komet-cli/komet/internal/proc"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RunDaemon is the body of the hidden `komet __daemon` command. It owns
// one llama-server process for its whole life and exits when the server is
// idle for keep_alive_sec, when the server dies, or when told to stop.
func (m *Manager) RunDaemon(args []string) error {
	fs := flag.NewFlagSet("__daemon", flag.ContinueOnError)
	modelPath := fs.String("model", "", "")
	name := fs.String("name", "", "")
	ramMB := fs.Int("ram-mb", 0, "")
	port := fs.Int("port", 0, "")
	if err := fs.Parse(args); err != nil {
		return err
	}
	lc := m.cfg.Local
	bin, err := m.ServerBin()
	if err != nil {
		return err
	}
	serverArgs := []string{
		"-m", *modelPath,
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(*port),
		"-t", strconv.Itoa(lc.Threads), // CPU threads for generation
		"-tb", strconv.Itoa(lc.Threads), // and for prompt processing
		"-c", strconv.Itoa(lc.Ctx), // context bounds KV-cache memory
		"-np", "1", // one slot: one KV cache, no surprise growth
		"-ngl", strconv.Itoa(lc.GPULayers),
	}
	limits := fmt.Sprintf("threads=%d ctx=%d nice=%d", lc.Threads, lc.Ctx, lc.Nice)

	name0, argv := bin, serverArgs
	if lc.CgroupLimits && runtime.GOOS == "linux" {
		if sr, err := exec.LookPath("systemd-run"); err == nil {
			// Hard caps enforced by the kernel: the model cannot take more
			// than its budget even if our estimate is wrong.
			memMax := *ramMB + *ramMB/4
			argv = append([]string{"--user", "--scope", "--quiet",
				"-p", fmt.Sprintf("MemoryMax=%dM", memMax),
				"-p", fmt.Sprintf("CPUQuota=%d%%", lc.Threads*100),
				"--", bin}, serverArgs...)
			name0 = sr
			limits += fmt.Sprintf(" cgroup(MemoryMax=%dM CPUQuota=%d%%)", memMax, lc.Threads*100)
		} else {
			log.Printf("cgroup_limits requested but systemd-run not found; continuing without")
		}
	}

	log.Printf("starting %s %s", name0, strings.Join(argv, " "))
	child := exec.Command(name0, argv...)
	child.Stdout, child.Stderr = os.Stdout, os.Stderr
	if err := child.Start(); err != nil {
		return fmt.Errorf("start llama-server: %w", err)
	}
	// Lower priority so the user's foreground apps win any CPU contention.
	if err := proc.SetNice(child.Process.Pid, lc.Nice); err != nil {
		log.Printf("set priority: %v", err)
	}

	st := &State{
		DaemonPID: os.Getpid(), ServerPID: child.Process.Pid, Port: *port,
		Model: *name, ModelPath: *modelPath, Started: time.Now(), Limits: limits,
	}
	if err := m.writeState(st); err != nil {
		_ = child.Process.Kill()
		return err
	}
	m.touch()

	exited := make(chan error, 1)
	go func() { exited <- child.Wait() }()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)

	keepAlive := time.Duration(lc.KeepAliveSec) * time.Second
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	readyLogged := false
	cleanup := func(reason string) {
		log.Printf("stopping llama-server: %s", reason)
		proc.Terminate(child.Process.Pid)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = child.Process.Kill()
			<-exited
		}
		if cur, err := m.readState(); err == nil && cur.DaemonPID == os.Getpid() {
			_ = os.Remove(m.statePath())
		}
	}
	for {
		select {
		case err := <-exited:
			log.Printf("llama-server exited: %v", err)
			if cur, e := m.readState(); e == nil && cur.DaemonPID == os.Getpid() {
				_ = os.Remove(m.statePath())
			}
			return err
		case s := <-sigs:
			cleanup("signal " + s.String())
			return nil
		case <-tick.C:
			if !readyLogged && healthy(*port) {
				readyLogged = true
				st.Ready = time.Now()
				_ = m.writeState(st)
				log.Printf("ready in %s", st.Ready.Sub(st.Started).Round(time.Millisecond))
			}
			fi, err := os.Stat(m.activityPath())
			if err != nil || time.Since(fi.ModTime()) > keepAlive {
				cleanup(fmt.Sprintf("idle for more than %s", keepAlive))
				return nil
			}
		}
	}
}
