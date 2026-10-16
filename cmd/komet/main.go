// Command komet is a local-first AI terminal assistant that runs a small
// model on your machine inside a resource envelope, checks its answer with
// cheap deterministic gates, and escalates to a cloud model only when the
// local draft is not good enough.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"time"

	"github.com/komet-cli/komet/internal/config"
	"github.com/komet-cli/komet/internal/hw"
	"github.com/komet-cli/komet/internal/ledger"
	"github.com/komet-cli/komet/internal/provision"
	"github.com/komet-cli/komet/internal/remote"
	"github.com/komet-cli/komet/internal/router"
	"github.com/komet-cli/komet/internal/ui"
)

var version = "0.2.0-prototype"

const usage = `komet - local-first AI for your terminal

Usage:
  komet "<what you want to do>"   turn a request into a shell command
  komet ask "<question>"          ask anything; pipe in context: cat log | komet ask "why?"
  komet init                      write ~/.komet/config.json
  komet doctor                    show hardware, models and which tiers are usable
  komet status                    show the local model server
  komet stop                      unload the local model now
  komet stats                     routing, cost and savings from the ledger

Remote targets (your VMs, RunPod pods, EC2 instances):
  komet targets                   list targets, their state, hardware and spend
  komet up <target>               wake, provision and connect (stays up while used)
  komet down <target> [--keep]    stop the session and the machine (--keep: leave it running)

Flags:
  -r, --run           run the suggested command after confirmation
  --local             local tier only (never touches the network)
  --cloud             cloud tier only
  -t, --target NAME   use this target only (wakes it if needed)
  -v, --verbose       show every workflow leg
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, ui.Red("komet: ")+err.Error())
		os.Exit(1)
	}
}

type flags struct {
	run, verbose bool
	force        string
	rest         []string
}

func parseFlags(args []string) (flags, error) {
	var f flags
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "-r", "--run":
			f.run = true
		case "-v", "--verbose":
			f.verbose = true
		case "--local", "--cloud", "-t", "--target":
			if f.force != "" {
				return f, errors.New("use only one of --local, --cloud and --target")
			}
			f.force = strings.TrimPrefix(a, "--")
			if a == "-t" || a == "--target" {
				if i+1 >= len(args) {
					return f, errors.New("--target needs a target name")
				}
				i++
				f.force = args[i]
			}
		case "--":
			f.rest = append(f.rest, args[i+1:]...)
			return f, nil
		default:
			if strings.HasPrefix(a, "-") && len(a) > 1 {
				return f, fmt.Errorf("unknown flag %s", a)
			}
			f.rest = append(f.rest, a)
		}
	}
	return f, nil
}

func run(args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Print(usage)
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	mgr := provision.New(cfg)
	rm := remote.New(cfg)

	switch args[0] {
	case "__daemon":
		return mgr.RunDaemon(args[1:])
	case "__session":
		return rm.RunSession(args[1:])
	case "targets":
		return cmdTargets(cfg, rm)
	case "up":
		return cmdUp(cfg, rm, args[1:])
	case "down":
		return cmdDown(cfg, rm, args[1:])
	case "version", "--version":
		fmt.Println("komet", version)
		return nil
	case "init":
		return cmdInit(cfg)
	case "doctor":
		return cmdDoctor(cfg, mgr)
	case "status":
		return cmdStatus(cfg, mgr)
	case "stop":
		if mgr.Stop() {
			fmt.Println("local model unloaded")
		} else {
			fmt.Println("no local model running")
		}
		return nil
	case "stats":
		entries, err := ledger.Read(cfg.LogPath())
		if err != nil {
			return err
		}
		fmt.Print(ledger.Summary(entries))
		printRemoteSpend(cfg, rm)
		return nil
	case "ask":
		return cmdQuery(cfg, mgr, rm, router.ModeAsk, args[1:])
	default:
		return cmdQuery(cfg, mgr, rm, router.ModeCommand, args)
	}
}

func readStdin() (string, error) {
	if ui.StdinTTY {
		return "", nil
	}
	fi, err := os.Stdin.Stat()
	if err != nil || (fi.Mode()&os.ModeNamedPipe == 0 && fi.Mode().IsRegular() == false) {
		return "", nil
	}
	b, err := io.ReadAll(io.LimitReader(os.Stdin, 256<<10))
	return string(b), err
}

func cmdQuery(cfg *config.Config, mgr *provision.Manager, rm *remote.Manager, mode router.Mode, args []string) error {
	f, err := parseFlags(args)
	if err != nil {
		return err
	}
	prompt := strings.TrimSpace(strings.Join(f.rest, " "))
	if prompt == "" {
		return errors.New("empty request; try: komet \"find files larger than 100MB\"")
	}
	stdin, err := readStdin()
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	sp := ui.NewSpinner()
	r := &router.Router{
		Cfg:      cfg,
		Local:    mgr,
		Remote:   rm,
		CloudKey: cfg.CloudKey(),
		Probe:    hw.Probe,
		Progress: sp.Set,
	}
	res, err := r.Run(ctx, router.Request{Mode: mode, Prompt: prompt, Context: stdin, Force: f.force})
	sp.Stop()
	if res != nil && len(res.Entry.Legs) > 0 {
		if lerr := ledger.Append(cfg.LogPath(), res.Entry); lerr != nil && f.verbose {
			fmt.Fprintln(os.Stderr, ui.Dim("ledger: "+lerr.Error()))
		}
	}
	if err != nil {
		return err
	}

	if mode == router.ModeCommand {
		if res.Command.Command == "" {
			report(res, f.verbose)
			return errors.New("no usable command produced")
		}
		fmt.Println(ui.Bold(res.Command.Command))
		if res.Command.Explanation != "" {
			fmt.Fprintln(os.Stderr, ui.Dim("  ↳ "+res.Command.Explanation))
		}
	} else {
		fmt.Println(res.Text)
	}
	report(res, f.verbose)

	if f.run && mode == router.ModeCommand {
		return confirmAndRun(res)
	}
	return nil
}

func report(res *router.Result, verbose bool) {
	e := res.Entry
	status := ui.Green("gates ✓")
	if !res.Gate.Passed {
		status = ui.Yellow("gates ✗ " + strings.Join(res.Gate.Failures, "; "))
	}
	model := res.Model
	if model == "" && len(e.Legs) > 0 {
		model = e.Legs[len(e.Legs)-1].Model
	}
	fmt.Fprintf(os.Stderr, "%s %s\n", ui.Dim(fmt.Sprintf("  %s · %s · %.1fs · $%.4f ·",
		e.Route, model, float64(e.TotalMS)/1000, e.CostUSD)), status)
	if len(e.Redacted) > 0 {
		fmt.Fprintln(os.Stderr, ui.Yellow("  ⚠ masked secrets before the cloud call: "+strings.Join(e.Redacted, ", ")))
	}
	for _, w := range res.Gate.Warnings {
		fmt.Fprintln(os.Stderr, ui.Yellow("  ⚠ "+w))
	}
	for _, d := range res.Gate.Danger {
		fmt.Fprintln(os.Stderr, ui.Red("  ⚠ destructive: "+d))
	}
	if verbose {
		if e.Reason != "" {
			fmt.Fprintln(os.Stderr, ui.Dim("  why: "+e.Reason))
		}
		for i, l := range e.Legs {
			line := fmt.Sprintf("  leg %d: %-5s %-28s %5dms in=%d out=%d $%.5f passed=%v",
				i+1, l.Tier, l.Model, l.LatencyMS, l.InTokens, l.OutTokens, l.CostUSD, l.Passed)
			if l.Error != "" {
				line += " error=" + l.Error
			}
			fmt.Fprintln(os.Stderr, ui.Dim(line))
			for _, f := range l.Failures {
				fmt.Fprintln(os.Stderr, ui.Dim("         ✗ "+f))
			}
		}
	}
}

func confirmAndRun(res *router.Result) error {
	tty, err := os.Open("/dev/tty")
	if err != nil {
		return errors.New("--run needs an interactive terminal to confirm")
	}
	defer tty.Close()
	in := bufio.NewReader(tty)
	if len(res.Gate.Danger) > 0 || !res.Gate.Passed {
		warn := "This command is destructive."
		if !res.Gate.Passed {
			warn = "This command did not pass the quality gates."
		}
		fmt.Fprint(os.Stderr, ui.Red(warn+" Type 'yes' to run it: "))
		if ans, _ := in.ReadString('\n'); strings.TrimSpace(ans) != "yes" {
			fmt.Fprintln(os.Stderr, "cancelled")
			return nil
		}
	} else {
		fmt.Fprint(os.Stderr, "Run it? [y/N] ")
		if ans, _ := in.ReadString('\n'); !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "y") {
			fmt.Fprintln(os.Stderr, "cancelled")
			return nil
		}
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	c := exec.Command(shell, "-c", res.Command.Command)
	c.Stdin, c.Stdout, c.Stderr = tty, os.Stdout, os.Stderr
	return c.Run()
}

func cmdInit(cfg *config.Config) error {
	if _, err := os.Stat(cfg.Path()); err == nil {
		fmt.Println("config already exists:", cfg.Path())
		return nil
	}
	if err := cfg.Save(); err != nil {
		return err
	}
	_ = os.MkdirAll(cfg.Home()+"/models", 0o700)
	fmt.Println("wrote", cfg.Path())
	fmt.Println(`
Next steps:
  1. Install llama.cpp so llama-server is on PATH (or set local.endpoint to Ollama/LM Studio).
  2. Put a GGUF model where local.models[].path points (~/.komet/models/).
  3. export ` + cfg.Cloud.APIKeyEnv + `=...   (any OpenAI-compatible provider; edit cloud.base_url/model)
  4. komet doctor`)
	return nil
}

func cmdDoctor(cfg *config.Config, mgr *provision.Manager) error {
	s := hw.Probe()
	ok, bad := ui.Green("✓"), ui.Red("✗")
	fmt.Println("Hardware:", s)
	fmt.Println("Config:  ", cfg.Path())
	fmt.Printf("Envelope: threads=%d ctx=%d nice=%d headroom=%dMB max_load=%.2f keep_alive=%ds cgroup=%v\n",
		cfg.Local.Threads, cfg.Local.Ctx, cfg.Local.Nice, cfg.Local.HeadroomMB, cfg.Local.MaxLoad,
		cfg.Local.KeepAliveSec, cfg.Local.CgroupLimits)
	fmt.Println()

	if mgr.External() {
		fmt.Println(ok, "local tier: external endpoint", cfg.Local.Endpoint)
	} else if bin, err := mgr.ServerBin(); err == nil {
		fmt.Println(ok, "llama-server:", bin)
	} else {
		fmt.Println(bad, "llama-server: not found")
	}
	for _, m := range cfg.Local.Models {
		mark := ok
		note := fmt.Sprintf("needs %d MB", m.RAMMB)
		if _, err := os.Stat(m.Path); err != nil {
			mark, note = bad, "file missing: "+m.Path
		}
		fmt.Printf("%s model %s (%s)\n", mark, m.Name, note)
	}
	if mod, why := mgr.Select(s); mod != nil {
		fmt.Println(ok, "local tier usable now with", mod.Name)
	} else {
		fmt.Println(bad, "local tier unavailable:", why)
	}
	fmt.Println()
	if cfg.Cloud.Enabled && cfg.CloudKey() != "" {
		fmt.Printf("%s cloud tier: %s @ %s\n", ok, cfg.Cloud.Model, cfg.Cloud.BaseURL)
	} else if !cfg.Cloud.Enabled {
		fmt.Println(bad, "cloud tier disabled")
	} else {
		fmt.Printf("%s cloud tier: set $%s to enable %s\n", bad, cfg.Cloud.APIKeyEnv, cfg.Cloud.Model)
	}
	fmt.Printf("  privacy=%s min_confidence=%.2f max_cloud_usd=%.3f\n", cfg.Policy.Privacy, cfg.Policy.MinConfidence, cfg.Policy.MaxCloudUSD)
	fmt.Println()
	for _, tool := range []string{"bash", "shellcheck"} {
		if p, err := exec.LookPath(tool); err == nil {
			fmt.Println(ok, "gate tool", tool+":", p)
		} else {
			fmt.Println(ui.Yellow("–"), "gate tool", tool+": not installed (gate skipped)")
		}
	}
	return nil
}

func cmdStatus(cfg *config.Config, mgr *provision.Manager) error {
	if mgr.External() {
		fmt.Println("local tier uses external endpoint", cfg.Local.Endpoint)
		return nil
	}
	st, running, healthy := mgr.Status()
	if !running {
		fmt.Println("no local model loaded (memory fully released)")
		return nil
	}
	state := "loading"
	if healthy {
		state = "ready"
	}
	fmt.Printf("model:    %s (%s)\n", st.Model, state)
	fmt.Printf("pid:      supervisor %d, llama-server %d\n", st.DaemonPID, st.ServerPID)
	fmt.Printf("endpoint: http://127.0.0.1:%d/v1\n", st.Port)
	fmt.Printf("limits:   %s\n", st.Limits)
	fmt.Printf("up:       %s", time.Since(st.Started).Round(time.Second))
	if !st.Ready.IsZero() {
		fmt.Printf(" (load took %s)", st.Ready.Sub(st.Started).Round(time.Millisecond))
	}
	fmt.Printf("\nunloads after %ds idle\n", cfg.Local.KeepAliveSec)
	return nil
}
