package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/komet-cli/komet/internal/config"
	"github.com/komet-cli/komet/internal/remote"
	"github.com/komet-cli/komet/internal/ui"
)

func secs(s float64) string {
	if s <= 0 {
		return "–"
	}
	return (time.Duration(s * float64(time.Second))).Round(100 * time.Millisecond).String()
}

func bootLine(b remote.Boot) string {
	parts := []string{"wake " + secs(b.WakeS), "ssh " + secs(b.SSHS)}
	if b.InstallS > 0 {
		parts = append(parts, "install "+secs(b.InstallS))
	}
	if b.Download {
		parts = append(parts, "model download "+secs(max(b.DownloadS, 0.1)))
	}
	srv := "server " + secs(b.ServerS)
	if b.Reused {
		srv = "server reused"
	}
	parts = append(parts, srv)
	if b.WasAwake {
		parts[0] = "wake – (already running)"
	}
	return strings.Join(parts, ", ")
}

func cmdUp(cfg *config.Config, rm *remote.Manager, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: komet up <target>")
	}
	name := args[0]
	t := cfg.Target(name)
	if t == nil {
		return fmt.Errorf("unknown target %q; see `komet targets`", name)
	}
	if prof := rm.Profile(name); prof != nil && len(prof.Boots) > 0 {
		last := prof.Boots[len(prof.Boots)-1]
		fmt.Fprintln(os.Stderr, ui.Dim(fmt.Sprintf("last start took %s (%s)", secs(last.TotalS), bootLine(last))))
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	sp := ui.NewSpinner()
	st, err := rm.Up(ctx, name, func(p string) {
		if !ui.StderrTTY {
			fmt.Fprintf(os.Stderr, "  %s\n", p)
		}
		sp.Set(p)
	})
	sp.Stop()
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "interrupted; the session keeps starting in the background. `komet down "+name+"` to cancel.")
		}
		return err
	}
	fmt.Printf("%s ready in %s\n", ui.Bold(name), secs(st.Boot.TotalS))
	fmt.Printf("  %s\n", bootLine(st.Boot))
	fmt.Printf("  %s · %s (%s) · $%.2f/h\n", st.Hardware, st.Model, st.Engine, st.CostPerHour)
	if t.OnIdle == "stop" && t.Kind != "ssh" {
		fmt.Printf("  stops the machine after %gm idle or %.0fh total; `komet down %s` to stop now\n", t.AutostopIdle, t.MaxUptimeHours, name)
	} else {
		fmt.Printf("  closes after %gm idle; `komet down %s` to stop now\n", t.AutostopIdle, name)
	}
	if st.CostPerHour == 0 && t.Kind == "aws" {
		fmt.Fprintln(os.Stderr, ui.Yellow("  ⚠ set cost_per_hour for this target so spend and budget are tracked"))
	}
	return nil
}

func cmdDown(cfg *config.Config, rm *remote.Manager, args []string) error {
	keep := false
	var name string
	for _, a := range args {
		switch a {
		case "--keep":
			keep = true
		default:
			name = a
		}
	}
	if name == "" {
		return errors.New("usage: komet down <target> [--keep]")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	msg, err := rm.Down(ctx, name, keep)
	if err != nil {
		return err
	}
	fmt.Println(name+":", msg)
	return nil
}

func cmdTargets(cfg *config.Config, rm *remote.Manager) error {
	if len(cfg.Targets) == 0 {
		fmt.Println("No targets configured. Add one to \"targets\" in", cfg.Path())
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, t := range cfg.Targets {
		session := "no session"
		if st, ok := rm.State(t.Name); ok {
			session = st.Phase
			if st.Phase == remote.PhaseReady {
				session = fmt.Sprintf("ready for %s · %s", time.Since(st.Ready).Round(time.Second), st.Model)
			}
		}
		machine := "?"
		if p, err := remote.NewProvider(&t); err == nil {
			if in, err := p.Info(ctx); err == nil {
				machine = in.State
				if in.CostPerHour > 0 {
					machine += fmt.Sprintf(" · $%.2f/h", in.CostPerHour)
				}
			} else {
				machine = ui.Yellow(err.Error())
			}
		}
		fmt.Printf("%s %s (%s, trust %s)\n", ui.Bold(t.Name), ui.Dim(t.Kind), t.Engine, t.Trust)
		fmt.Printf("  machine:  %s\n", machine)
		fmt.Printf("  session:  %s\n", session)
		if prof := rm.Profile(t.Name); prof != nil {
			hwDesc := prof.Hardware
			if prof.VRAMMB > 0 {
				hwDesc += fmt.Sprintf(" · %d MB VRAM", prof.VRAMMB)
			}
			fmt.Printf("  profile:  %s · model %s · %d starts recorded\n", hwDesc, prof.Model, len(prof.Boots))
			if n := len(prof.Boots); n > 0 {
				b := prof.Boots[n-1]
				fmt.Printf("  last up:  %s (%s)\n", secs(b.TotalS), bootLine(b))
			}
		} else {
			fmt.Printf("  profile:  not started yet\n")
		}
	}
	printRemoteSpend(cfg, rm)
	return nil
}

func printRemoteSpend(cfg *config.Config, rm *remote.Manager) {
	if len(cfg.Targets) == 0 {
		return
	}
	spent, recs := rm.SpentToday()
	var total, hours float64
	for _, r := range recs {
		total += r.USD
		hours += r.Hours
	}
	line := fmt.Sprintf("Remote spend:    $%.3f today", spent)
	if cfg.Budget.MaxUSDPerDay > 0 {
		line += fmt.Sprintf(" of $%.2f budget", cfg.Budget.MaxUSDPerDay)
	}
	line += fmt.Sprintf(" · $%.3f over %d sessions (%.2fh)", total, len(recs), hours)
	fmt.Println(line)
}
