// Package router is Komet's orchestration core: an N-tier cascade.
//
//	guard → build the tier chain → for each tier:
//	    draft → quality gates → accept, or escalate to the next tier
//	    with the failed draft and the exact gate failures
//
// Tiers are ordered by trust and cost:
//
//	local   (this machine, "device" trust)  bounded resources, free
//	targets (your VMs / GPU pods, "private") billed per hour, used when up
//	cloud   (public API, "public")          billed per token, last resort
//
// What each tier may see is decided by its trust level and policy.privacy:
// secrets are masked only when a request crosses into a public tier.
package router

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/komet-cli/komet/internal/config"
	"github.com/komet-cli/komet/internal/gate"
	"github.com/komet-cli/komet/internal/guard"
	"github.com/komet-cli/komet/internal/hw"
	"github.com/komet-cli/komet/internal/ledger"
	"github.com/komet-cli/komet/internal/llm"
)

type Mode string

const (
	ModeCommand Mode = "cmd" // natural language → one shell command
	ModeAsk     Mode = "ask" // free-form question, optionally with piped context
)

type Request struct {
	Mode    Mode
	Prompt  string
	Context string // piped stdin, e.g. a log or a file
	// Force pins the request to one tier: "local", "cloud" or a target name.
	Force string
}

type Result struct {
	Mode    Mode
	Command gate.Command
	Text    string
	Gate    gate.Result
	Tier    string // tier that produced the final answer
	Model   string
	Entry   ledger.Entry
}

// LocalTier is implemented by provision.Manager.
type LocalTier interface {
	Select(hw.Snapshot) (*config.LocalModel, string)
	Ensure(ctx context.Context, mod *config.LocalModel, progress func(string)) (string, func(), error)
}

// Endpoint is a ready-to-use model server on a remote target.
type Endpoint struct {
	BaseURL string
	APIKey  string
	Model   string
	Release func()
}

// RemoteTiers is implemented by remote.Manager.
type RemoteTiers interface {
	// Running reports whether the target is up and reachable right now.
	Running(name string) bool
	// Ensure wakes the target if needed and returns its endpoint.
	Ensure(ctx context.Context, name string, progress func(string)) (*Endpoint, error)
}

type Router struct {
	Cfg      *config.Config
	Local    LocalTier
	Remote   RemoteTiers // may be nil
	CloudKey string
	Probe    func() hw.Snapshot
	Progress func(string)
}

var ErrNoRoute = errors.New("no tier can serve this request")

const (
	trustDevice  = "device"
	trustPrivate = "private"
	trustPublic  = "public"
)

// tier is one rung of the cascade.
type tier struct {
	name  string
	kind  string // local | remote | cloud
	trust string
	open  func(ctx context.Context) (*llm.Client, func(), error)
	// constrained: the server supports grammar-constrained JSON, logprobs
	// and prompt caching (llama-server, vLLM).
	constrained bool
	priced      bool // billed per token at cloud prices
}

func (r *Router) progress(s string) {
	if r.Progress != nil {
		r.Progress(s)
	}
}

func (r *Router) cost(in, out int) float64 {
	return (float64(in)*r.Cfg.Cloud.PriceIn + float64(out)*r.Cfg.Cloud.PriceOut) / 1e6
}

var hardHints = []string{"architecture", "architect", "design a", "prove", "trade-off", "tradeoff",
	"refactor", "optimiz", "optimis", "algorithm", "in depth", "step by step", "compare"}

func (r *Router) isHard(req Request) (bool, string) {
	n := len(req.Prompt) + len(req.Context)
	if r.Cfg.Policy.HardPromptChars > 0 && n > r.Cfg.Policy.HardPromptChars {
		return true, fmt.Sprintf("input is %d chars (> %d)", n, r.Cfg.Policy.HardPromptChars)
	}
	if req.Mode == ModeAsk {
		p := strings.ToLower(req.Prompt)
		for _, h := range hardHints {
			if strings.Contains(p, h) {
				return true, fmt.Sprintf("looks like deep reasoning (%q)", h)
			}
		}
	}
	return false, ""
}

// allowed applies the privacy policy to a tier, given whether the request
// contains secrets.
func (r *Router) allowed(trust string, secrets bool) (bool, string) {
	if !secrets || trust == trustDevice {
		return true, ""
	}
	switch r.Cfg.Policy.Privacy {
	case "local_only":
		return false, "input contains secrets and privacy=local_only"
	case "private_only":
		if trust == trustPublic {
			return false, "input contains secrets and privacy=private_only"
		}
	}
	return true, ""
}

// chain builds the ordered list of tiers for this request, plus the reasons
// any tier was left out (for the ledger and -v output).
func (r *Router) chain(ctx context.Context, req Request, secrets bool) ([]tier, []string) {
	cfg := r.Cfg
	var tiers []tier
	var skipped []string
	skip := func(name, why string) { skipped = append(skipped, name+": "+why) }
	want := func(name string) bool { return req.Force == "" || req.Force == name }

	// Local.
	if want("local") {
		mod, why := r.Local.Select(r.Probe())
		if mod == nil {
			skip("local", why)
		} else {
			m := *mod
			tiers = append(tiers, tier{name: "local", kind: "local", trust: trustDevice, constrained: true,
				open: func(ctx context.Context) (*llm.Client, func(), error) {
					url, release, err := r.Local.Ensure(ctx, &m, r.progress)
					if err != nil {
						return nil, nil, err
					}
					model := m.Name
					if cfg.Local.EndpointModel != "" {
						model = cfg.Local.EndpointModel
					}
					r.progress("local · " + m.Name)
					return llm.New(url, "", model, 300*time.Second), release, nil
				}})
		}
	}

	// Remote targets: used automatically only when already running; a
	// forced target is woken up.
	for i := range cfg.Targets {
		t := &cfg.Targets[i]
		if !want(t.Name) {
			continue
		}
		if r.Remote == nil {
			skip(t.Name, "remote targets unavailable")
			continue
		}
		forced := req.Force == t.Name
		if !forced {
			if !t.AutoUse() {
				continue
			}
			if !r.Remote.Running(t.Name) {
				skip(t.Name, "not running (start it with `komet up "+t.Name+"`)")
				continue
			}
		}
		name := t.Name
		tiers = append(tiers, tier{name: name, kind: "remote", trust: t.Trust, constrained: true,
			open: func(ctx context.Context) (*llm.Client, func(), error) {
				ep, err := r.Remote.Ensure(ctx, name, r.progress)
				if err != nil {
					return nil, nil, err
				}
				r.progress(name + " · " + ep.Model)
				return llm.New(ep.BaseURL, ep.APIKey, ep.Model, 300*time.Second), ep.Release, nil
			}})
	}

	// Cloud.
	if want("cloud") {
		switch {
		case !cfg.Cloud.Enabled:
			skip("cloud", "disabled")
		case cfg.Cloud.BaseURL == "" || cfg.Cloud.Model == "":
			skip("cloud", "not configured")
		case r.CloudKey == "":
			skip("cloud", fmt.Sprintf("no API key in $%s", cfg.Cloud.APIKeyEnv))
		default:
			tiers = append(tiers, tier{name: "cloud", kind: "cloud", trust: trustPublic, priced: true,
				open: func(ctx context.Context) (*llm.Client, func(), error) {
					r.progress("cloud · " + cfg.Cloud.Model)
					return llm.New(cfg.Cloud.BaseURL, r.CloudKey, cfg.Cloud.Model, 120*time.Second), func() {}, nil
				}})
		}
	}

	// Privacy.
	var kept []tier
	for _, t := range tiers {
		if ok, why := r.allowed(t.trust, secrets); !ok {
			skip(t.name, why)
			continue
		}
		kept = append(kept, t)
	}
	tiers = kept

	// Difficulty: the device tier is not worth trying for deep requests
	// when something stronger is available.
	if req.Force == "" && len(tiers) > 1 && tiers[0].kind == "local" {
		if hard, why := r.isHard(req); hard {
			skip("local", why)
			tiers = tiers[1:]
		}
	}
	return tiers, skipped
}

func (r *Router) Run(ctx context.Context, req Request) (*Result, error) {
	start := time.Now()
	cfg := r.Cfg
	entry := ledger.Entry{Time: start, Mode: string(req.Mode)}

	if req.Force != "" && req.Force != "local" && req.Force != "cloud" && cfg.Target(req.Force) == nil {
		return nil, fmt.Errorf("unknown tier %q (use local, cloud or a target name)", req.Force)
	}

	redPrompt, f1 := guard.Redact(req.Prompt)
	redCtx, f2 := guard.Redact(req.Context)
	findings := append(f1, f2...)
	entry.Redacted = guard.Kinds(findings)
	secrets := len(findings) > 0

	tiers, skipped := r.chain(ctx, req, secrets)
	if len(tiers) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoRoute, strings.Join(skipped, "; "))
	}

	sys := systemPrompt(req.Mode)
	var best *Result // last result that produced a usable answer
	var draft string
	var failures []string
	var route []string
	redactedSent := false

	for i, t := range tiers {
		last := i == len(tiers)-1
		mask := t.trust == trustPublic && cfg.Policy.Privacy != "allow"
		prompt, context := req.Prompt, req.Context
		if mask {
			prompt, context = redPrompt, redCtx
		}
		msgs := []llm.Message{{Role: "system", Content: sys}, {Role: "user", Content: userMessage(prompt, context)}}
		if draft != "" && len(failures) > 0 {
			d := draft
			if mask {
				d, _ = guard.Redact(draft)
			}
			msgs = append(msgs,
				llm.Message{Role: "assistant", Content: d},
				llm.Message{Role: "user", Content: "That answer failed automatic checks:\n- " +
					strings.Join(failures, "\n- ") + "\nReturn a corrected answer in the same format."})
		}
		if t.priced && cfg.Policy.MaxCloudUSD > 0 {
			estIn := 0
			for _, m := range msgs {
				estIn += len(m.Content)/4 + 4
			}
			if est := r.cost(estIn, maxTokens(req.Mode)); est > cfg.Policy.MaxCloudUSD {
				err := fmt.Errorf("cloud call would cost up to $%.4f, above policy.max_cloud_usd $%.4f", est, cfg.Policy.MaxCloudUSD)
				if best != nil {
					best.Gate.Warnings = append(best.Gate.Warnings, err.Error())
					break
				}
				return nil, err
			}
		}

		route = append(route, t.name)
		leg := ledger.Leg{Tier: t.name, Kind: t.kind}
		out, err := r.callTier(ctx, t, req.Mode, msgs)
		if err != nil {
			leg.Error = err.Error()
			entry.Legs = append(entry.Legs, leg)
			failures = []string{t.name + " tier error: " + err.Error()}
			if last {
				break
			}
			continue
		}
		if mask && secrets {
			redactedSent = true
		}
		leg.Model = out.Model
		leg.LatencyMS = out.Latency.Milliseconds()
		leg.InTokens, leg.OutTokens = out.PromptTokens, out.OutputTokens
		if t.priced {
			leg.CostUSD = r.cost(out.PromptTokens, out.OutputTokens)
		}
		res := r.check(req.Mode, out, !last)
		res.Tier, res.Model = t.name, out.Model
		leg.Passed, leg.Failures = res.Gate.Passed, res.Gate.Failures
		entry.Legs = append(entry.Legs, leg)
		entry.CostUSD += leg.CostUSD
		entry.CloudOnlyUSD += r.cost(out.PromptTokens, out.OutputTokens)

		if usable(res) {
			best = res
		}
		if res.Gate.Passed {
			break
		}
		draft, failures = out.Text, res.Gate.Failures
	}

	entry.Route = strings.Join(route, "→")
	if len(skipped) > 0 {
		entry.Reason = "skipped " + strings.Join(skipped, "; ")
	}
	if len(entry.Legs) > 1 {
		var esc []string
		for _, l := range entry.Legs[:len(entry.Legs)-1] {
			if l.Error != "" {
				esc = append(esc, l.Tier+" error: "+l.Error)
			} else {
				esc = append(esc, l.Tier+": "+strings.Join(l.Failures, "; "))
			}
		}
		entry.Reason = strings.TrimPrefix(entry.Reason+" | escalated from "+strings.Join(esc, " | "), " | ")
	}
	if !redactedSent {
		entry.Redacted = nil // nothing masked was actually sent anywhere
	}
	entry.TotalMS = time.Since(start).Milliseconds()
	if best == nil {
		var errs []string
		for _, l := range entry.Legs {
			if l.Error != "" {
				errs = append(errs, l.Tier+": "+l.Error)
			}
		}
		res := &Result{Mode: req.Mode, Entry: entry}
		return res, fmt.Errorf("%w: %s", ErrNoRoute, strings.Join(errs, "; "))
	}
	// If the final tier failed outright, say so on the answer we fall back to.
	if lastLeg := entry.Legs[len(entry.Legs)-1]; lastLeg.Error != "" && best.Tier != lastLeg.Tier {
		best.Gate.Warnings = append(best.Gate.Warnings, lastLeg.Tier+" escalation failed: "+lastLeg.Error)
	}
	entry.Accepted = best.Gate.Passed
	best.Entry = entry
	return best, nil
}

func usable(r *Result) bool {
	if r.Mode == ModeCommand {
		return r.Command.Command != ""
	}
	return r.Text != ""
}

func (r *Router) callTier(ctx context.Context, t tier, mode Mode, msgs []llm.Message) (*llm.Response, error) {
	client, release, err := t.open(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	req := llm.Request{Messages: msgs, MaxTokens: maxTokens(mode), Temperature: 0.2}
	if t.constrained {
		req.Temperature = 0.1
		req.Logprobs = true
		req.CachePrompt = true
		if mode == ModeCommand {
			req.JSONSchema = commandSchema
		}
	} else if mode == ModeCommand && r.Cfg.Cloud.JSONMode {
		req.JSONObject = true
	}
	return client.Chat(ctx, req)
}

// check runs the gates. On the last tier, a missing binary and low
// confidence are only warnings: there is no stronger model left to ask.
func (r *Router) check(mode Mode, out *llm.Response, canEscalate bool) *Result {
	res := &Result{Mode: mode}
	minConf := r.Cfg.Policy.MinConfidence
	if !canEscalate {
		minConf = 0
	}
	if mode == ModeCommand {
		res.Command, res.Gate = gate.CheckCommand(out.Text, out.Logprobs, gate.CommandOptions{
			MinConfidence:        minConf,
			RequireKnownBinaries: r.Cfg.Policy.RequireKnownBinaries && canEscalate,
		})
	} else {
		res.Text = strings.TrimSpace(out.Text)
		res.Gate = gate.CheckText(out.Text, out.Logprobs, minConf)
	}
	return res
}

var commandSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"command":     map[string]any{"type": "string"},
		"explanation": map[string]any{"type": "string"},
	},
	"required": []string{"command", "explanation"},
}

func maxTokens(m Mode) int {
	if m == ModeCommand {
		return 256
	}
	return 1024
}

func shellName() string {
	if s := os.Getenv("SHELL"); s != "" {
		return filepath.Base(s)
	}
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	return "sh"
}

func systemPrompt(m Mode) string {
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macOS"
	}
	if m == ModeCommand {
		return fmt.Sprintf(`You are Komet, a terminal assistant. Turn the user's request into ONE shell command for %s on %s.
Reply with JSON only: {"command": "<the command>", "explanation": "<one short sentence>"}.
Use standard tools that ship with the OS. Never invent tools or flags. Prefer safe, non-destructive forms.`, shellName(), osName)
	}
	return fmt.Sprintf(`You are Komet, a concise terminal assistant on %s (%s shell).
Answer directly. Prefer short answers; include commands or code when they help.`, osName, shellName())
}

func userMessage(prompt, ctx string) string {
	if strings.TrimSpace(ctx) == "" {
		return prompt
	}
	return prompt + "\n\nContext (piped input):\n```\n" + ctx + "\n```"
}
