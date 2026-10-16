package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/komet-cli/komet/internal/config"
	"github.com/komet-cli/komet/internal/hw"
)

type fakeLLM struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
	reply  func(body string) (string, float64)
}

func newFake(reply func(string) (string, float64)) *fakeLLM {
	f := &fakeLLM{reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.bodies = append(f.bodies, string(b))
		f.mu.Unlock()
		text, lp := f.reply(string(b))
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message":  map[string]any{"content": text},
				"logprobs": map[string]any{"content": []any{map[string]any{"logprob": lp}, map[string]any{"logprob": lp}}},
			}},
			"usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 20},
		})
	}))
	return f
}

func (f *fakeLLM) calls() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.bodies) }
func (f *fakeLLM) last() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[len(f.bodies)-1]
}

type stubLocal struct {
	url   string
	model *config.LocalModel
	why   string
}

func (s stubLocal) Select(hw.Snapshot) (*config.LocalModel, string) { return s.model, s.why }
func (s stubLocal) Ensure(context.Context, *config.LocalModel, func(string)) (string, func(), error) {
	return s.url, func() {}, nil
}

func cmdJSON(c string) string {
	b, _ := json.Marshal(map[string]string{"command": c, "explanation": "test"})
	return string(b)
}

func setup(t *testing.T, local, cloud *fakeLLM, withLocal bool) *Router {
	t.Helper()
	cfg := config.Default(t.TempDir())
	cfg.Cloud.BaseURL = cloud.srv.URL
	cfg.Cloud.Model = "fake-cloud"
	t.Cleanup(local.srv.Close)
	t.Cleanup(cloud.srv.Close)
	sl := stubLocal{url: local.srv.URL, why: "no model fits"}
	if withLocal {
		sl.model = &config.LocalModel{Name: "fake-local"}
	}
	return &Router{Cfg: cfg, Local: sl, CloudKey: "k", Probe: func() hw.Snapshot { return hw.Snapshot{} }}
}

var goodCloud = func(string) (string, float64) { return cmdJSON("ls -la"), -0.05 }

func TestLocalAccepted(t *testing.T) {
	local := newFake(func(string) (string, float64) { return cmdJSON("find . -type f -size +100M"), -0.05 })
	cloud := newFake(goodCloud)
	r := setup(t, local, cloud, true)
	res, err := r.Run(context.Background(), Request{Mode: ModeCommand, Prompt: "files over 100MB"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Entry.Route != "local" || cloud.calls() != 0 || res.Entry.CostUSD != 0 {
		t.Fatalf("route=%s cloudCalls=%d cost=%v", res.Entry.Route, cloud.calls(), res.Entry.CostUSD)
	}
	if res.Entry.CloudOnlyUSD <= 0 {
		t.Error("ledger should estimate the cloud-only counterfactual")
	}
	if !strings.Contains(local.last(), `"json_schema"`) || !strings.Contains(local.last(), `"cache_prompt":true`) {
		t.Errorf("local request should use grammar-constrained JSON and prompt cache: %s", local.last())
	}
}

func TestEscalatesWithFeedback(t *testing.T) {
	local := newFake(func(string) (string, float64) { return cmdJSON("frobnicate --todo"), -0.05 })
	cloud := newFake(goodCloud)
	r := setup(t, local, cloud, true)
	res, err := r.Run(context.Background(), Request{Mode: ModeCommand, Prompt: "find TODOs"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Entry.Route != "local→cloud" || res.Tier != "cloud" || res.Command.Command != "ls -la" {
		t.Fatalf("got route=%s tier=%s cmd=%q", res.Entry.Route, res.Tier, res.Command.Command)
	}
	if !strings.Contains(cloud.last(), "failed automatic checks") || !strings.Contains(cloud.last(), "frobnicate") {
		t.Errorf("cloud should receive the draft and the gate failures: %s", cloud.last())
	}
}

func TestLowConfidenceEscalates(t *testing.T) {
	local := newFake(func(string) (string, float64) { return cmdJSON("ls -la"), -1.0 })
	cloud := newFake(goodCloud)
	r := setup(t, local, cloud, true)
	res, err := r.Run(context.Background(), Request{Mode: ModeCommand, Prompt: "list"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Entry.Route != "local→cloud" {
		t.Fatalf("route=%s", res.Entry.Route)
	}
}

func TestSecretsRedactedBeforeCloud(t *testing.T) {
	local := newFake(goodCloud)
	cloud := newFake(func(string) (string, float64) { return "it failed because of auth", -0.1 })
	r := setup(t, local, cloud, true)
	secret := "sk-abcdefghijklmnopqrstuvwxyz0123"
	res, err := r.Run(context.Background(), Request{Mode: ModeAsk, Prompt: "why does this fail?",
		Context: "OPENAI_API_KEY=" + secret + "\nerror 401", Force: "cloud"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(cloud.last(), secret) {
		t.Fatal("secret reached the cloud")
	}
	if len(res.Entry.Redacted) == 0 {
		t.Error("redaction not recorded")
	}
}

func TestLocalOnlyPrivacyBlocksCloud(t *testing.T) {
	local := newFake(goodCloud)
	cloud := newFake(goodCloud)
	r := setup(t, local, cloud, true)
	r.Cfg.Policy.Privacy = "local_only"
	_, err := r.Run(context.Background(), Request{Mode: ModeAsk, Prompt: "x", Context: "password=hunter2xyz", Force: "cloud"})
	if err == nil || cloud.calls() != 0 {
		t.Fatalf("expected refusal without cloud call, err=%v calls=%d", err, cloud.calls())
	}
}

func TestNoLocalModelGoesToCloud(t *testing.T) {
	local := newFake(goodCloud)
	cloud := newFake(goodCloud)
	r := setup(t, local, cloud, false)
	res, err := r.Run(context.Background(), Request{Mode: ModeCommand, Prompt: "list files"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Entry.Route != "cloud" || local.calls() != 0 || !strings.Contains(res.Entry.Reason, "no model fits") {
		t.Fatalf("route=%s reason=%s", res.Entry.Route, res.Entry.Reason)
	}
}

func TestHardQuestionSkipsLocal(t *testing.T) {
	local := newFake(goodCloud)
	cloud := newFake(func(string) (string, float64) { return "deep answer", -0.1 })
	r := setup(t, local, cloud, true)
	res, err := r.Run(context.Background(), Request{Mode: ModeAsk, Prompt: "Design a sharded architecture for our queue"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Entry.Route != "cloud" || local.calls() != 0 {
		t.Fatalf("route=%s localCalls=%d", res.Entry.Route, local.calls())
	}
}

func TestBudgetCeiling(t *testing.T) {
	local := newFake(goodCloud)
	cloud := newFake(goodCloud)
	r := setup(t, local, cloud, false)
	r.Cfg.Policy.MaxCloudUSD = 0.0000001
	if _, err := r.Run(context.Background(), Request{Mode: ModeCommand, Prompt: "list"}); err == nil || cloud.calls() != 0 {
		t.Fatalf("budget should block the call, err=%v", err)
	}
}

func TestNoRoute(t *testing.T) {
	local := newFake(goodCloud)
	cloud := newFake(goodCloud)
	r := setup(t, local, cloud, false)
	r.CloudKey = ""
	if _, err := r.Run(context.Background(), Request{Mode: ModeCommand, Prompt: "list"}); err == nil {
		t.Fatal("expected ErrNoRoute")
	}
}

type stubRemote struct {
	url     string
	running bool
	ensured int
}

func (s *stubRemote) Running(string) bool { return s.running }
func (s *stubRemote) Ensure(context.Context, string, func(string)) (*Endpoint, error) {
	s.ensured++
	s.running = true
	return &Endpoint{BaseURL: s.url, APIKey: "rk", Model: "remote-14b", Release: func() {}}, nil
}

func setupRemote(t *testing.T, running bool) (*Router, *fakeLLM, *fakeLLM, *fakeLLM, *stubRemote) {
	local := newFake(func(string) (string, float64) { return cmdJSON("frobnicate"), -0.05 })
	pod := newFake(func(string) (string, float64) { return cmdJSON("ls -la"), -0.05 })
	cloud := newFake(goodCloud)
	t.Cleanup(pod.srv.Close)
	r := setup(t, local, cloud, true)
	r.Cfg.Targets = []config.Target{{Name: "gpu", Kind: "ssh", Trust: "private"}}
	rs := &stubRemote{url: pod.srv.URL, running: running}
	r.Remote = rs
	return r, local, pod, cloud, rs
}

func TestRunningTargetSitsBetweenLocalAndCloud(t *testing.T) {
	r, local, pod, cloud, _ := setupRemote(t, true)
	res, err := r.Run(context.Background(), Request{Mode: ModeCommand, Prompt: "list"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Entry.Route != "local→gpu" || res.Tier != "gpu" || cloud.calls() != 0 || local.calls() != 1 || pod.calls() != 1 {
		t.Fatalf("route=%s tier=%s calls local=%d pod=%d cloud=%d", res.Entry.Route, res.Tier, local.calls(), pod.calls(), cloud.calls())
	}
	if !strings.Contains(pod.last(), "frobnicate") {
		t.Error("the pod should get the failed local draft as feedback")
	}
}

func TestStoppedTargetIsNotWokenAutomatically(t *testing.T) {
	r, _, pod, _, rs := setupRemote(t, false)
	res, err := r.Run(context.Background(), Request{Mode: ModeCommand, Prompt: "list"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Entry.Route != "local→cloud" || rs.ensured != 0 || pod.calls() != 0 {
		t.Fatalf("route=%s ensured=%d", res.Entry.Route, rs.ensured)
	}
	if !strings.Contains(res.Entry.Reason, "komet up gpu") {
		t.Errorf("reason should explain how to start the target: %s", res.Entry.Reason)
	}
}

func TestForcedTargetWakes(t *testing.T) {
	r, local, _, cloud, rs := setupRemote(t, false)
	res, err := r.Run(context.Background(), Request{Mode: ModeCommand, Prompt: "list", Force: "gpu"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Entry.Route != "gpu" || rs.ensured != 1 || local.calls() != 0 || cloud.calls() != 0 {
		t.Fatalf("route=%s ensured=%d", res.Entry.Route, rs.ensured)
	}
}

func TestPrivateTargetSeesRawPublicSeesMasked(t *testing.T) {
	r, _, pod, cloud, _ := setupRemote(t, true)
	r.Cfg.Targets[0].Trust = "private"
	secret := "postgres://app:Sup3rSecretPw@db/prod"
	// pod fails the gate (empty answer) so the request escalates to the cloud
	pod.reply = func(string) (string, float64) { return "", -0.05 }
	r.Local = stubLocal{why: "off"}
	_, err := r.Run(context.Background(), Request{Mode: ModeAsk, Prompt: "why?", Context: secret})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(pod.last(), "Sup3rSecretPw") {
		t.Error("private target should receive the input unmasked")
	}
	if strings.Contains(cloud.last(), "Sup3rSecretPw") {
		t.Fatal("secret reached the public cloud")
	}
}

func TestPrivateOnlyPolicy(t *testing.T) {
	r, _, pod, cloud, _ := setupRemote(t, true)
	r.Cfg.Policy.Privacy = "private_only"
	pod.reply = func(string) (string, float64) { return "", -0.05 }
	r.Local = stubLocal{why: "off"}
	res, err := r.Run(context.Background(), Request{Mode: ModeAsk, Prompt: "why?", Context: "password=hunter2xyz"})
	if err == nil && res.Entry.Route != "gpu" {
		t.Fatalf("route=%s", res.Entry.Route)
	}
	if cloud.calls() != 0 {
		t.Fatal("private_only must keep secrets off the public cloud")
	}
}

func TestUnknownForcedTier(t *testing.T) {
	r, _, _, _, _ := setupRemote(t, true)
	if _, err := r.Run(context.Background(), Request{Mode: ModeCommand, Prompt: "x", Force: "nope"}); err == nil {
		t.Fatal("expected error for unknown tier")
	}
}
