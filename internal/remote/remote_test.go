package remote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/komet-cli/komet/internal/config"
)

func runpodServer(t *testing.T, status string, withSSH bool) (*httptest.Server, *[]string) {
	t.Helper()
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch {
		case r.URL.Path == "/v2/pods":
			p := map[string]any{"id": "p1", "status": status, "cost": 0.44,
				"gpu": map[string]any{"id": "NVIDIA A40", "count": 2}}
			if withSSH {
				p["runtime"] = map[string]any{"ports": []any{
					map[string]any{"ip": "1.2.3.4", "private": 8888, "public": 40000, "type": "http"},
					map[string]any{"ip": "1.2.3.4", "private": 22, "public": 40022, "type": "tcp"},
				}}
			}
			// second page holds the pod, to exercise pagination
			if r.URL.Query().Get("cursor") == "" {
				json.NewEncoder(w).Encode(map[string]any{"pods": []any{map[string]any{"id": "other"}}, "pagination": map[string]any{"nextCursor": "c2"}})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"pods": []any{p}, "pagination": map[string]any{}})
		case strings.HasSuffix(r.URL.Path, "/action"):
			var b struct{ Action string }
			json.NewDecoder(r.Body).Decode(&b)
			actions = append(actions, b.Action)
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r) // no single-pod GET: client must fall back to listing
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &actions
}

func rpTarget(base string) *config.Target {
	return &config.Target{Name: "rp", Kind: "runpod", PodID: "p1", APIKeyEnv: "KOMET_TEST_RP_KEY", APIBase: base + "/v2"}
}

func TestRunPodInfoAndActions(t *testing.T) {
	t.Setenv("KOMET_TEST_RP_KEY", "k")
	srv, actions := runpodServer(t, "RUNNING", true)
	p, _ := NewProvider(rpTarget(srv.URL))
	in, err := p.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if in.State != StateRunning || in.Host != "1.2.3.4" || in.Port != 40022 || in.CostPerHour != 0.44 || in.Hardware != "2x NVIDIA A40" {
		t.Fatalf("unexpected info %+v", in)
	}
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(*actions, ",") != "stop,start" {
		t.Fatalf("actions %v", *actions)
	}
}

func TestRunPodStates(t *testing.T) {
	t.Setenv("KOMET_TEST_RP_KEY", "k")
	for status, want := range map[string]string{"EXITED": StateStopped, "TERMINATED": StateGone, "RESTARTING": StatePending} {
		srv, _ := runpodServer(t, status, false)
		p, _ := NewProvider(rpTarget(srv.URL))
		if in, _ := p.Info(context.Background()); in.State != want {
			t.Errorf("%s: got %s want %s", status, in.State, want)
		}
	}
}

func TestRunPodRunningWithoutPublicSSH(t *testing.T) {
	t.Setenv("KOMET_TEST_RP_KEY", "k")
	srv, _ := runpodServer(t, "RUNNING", false)
	p, _ := NewProvider(rpTarget(srv.URL))
	if _, err := p.Info(context.Background()); err == nil || !strings.Contains(err.Error(), "TCP port 22") {
		t.Fatalf("expected a clear error about exposing port 22, got %v", err)
	}
}

func TestRunPodMissingKey(t *testing.T) {
	os.Unsetenv("KOMET_TEST_RP_KEY")
	p, _ := NewProvider(rpTarget("http://127.0.0.1:1"))
	if _, err := p.Info(context.Background()); err == nil || !strings.Contains(err.Error(), "KOMET_TEST_RP_KEY") {
		t.Fatalf("got %v", err)
	}
}

func TestAWSViaCLI(t *testing.T) {
	bin, err := filepath.Abs("../../tools/fakeaws/aws")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("FAKE_AWS_STATE", filepath.Join(dir, "state"))
	t.Setenv("FAKE_AWS_BOOT_S", "0")
	tg := &config.Target{Name: "ec2", Kind: "aws", InstanceID: "i-0123456789abcdef0", AWSBin: bin, Port: 22, CostPerHour: 1.0}
	p, _ := NewProvider(tg)
	ctx := context.Background()
	if in, err := p.Info(ctx); err != nil || in.State != StateStopped || in.Hardware != "g5.xlarge" {
		t.Fatalf("stopped: %+v %v", in, err)
	}
	if err := p.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if in, err := p.Info(ctx); err != nil || in.State != StateRunning || in.Host != "127.0.0.1" {
		t.Fatalf("running: %+v %v", in, err)
	}
	tg.InstanceID = "i-nope"
	if _, err := p.Info(ctx); err == nil || !strings.Contains(err.Error(), "InvalidInstanceID") {
		t.Fatalf("expected aws error to surface, got %v", err)
	}
}

func TestChooseModel(t *testing.T) {
	tg := &config.Target{Engine: "llama", Models: []config.RemoteModel{
		{Name: "70b", VRAMMB: 46000}, {Name: "14b", VRAMMB: 12000}, {Name: "8b", VRAMMB: 7000},
	}}
	cases := []struct {
		vram int
		want string
	}{{81920, "70b"}, {49152, "70b"}, {24564, "14b"}, {12288, "8b"}, {0, "8b"}}
	for _, c := range cases {
		m, _ := chooseModel(tg, c.vram)
		if m == nil || m.Name != c.want {
			t.Errorf("vram %d: got %v want %s", c.vram, m, c.want)
		}
	}
	if m, note := chooseModel(tg, 4096); m != nil || !strings.Contains(note, "no configured model fits") {
		t.Errorf("4GB should fit nothing: %v %s", m, note)
	}
	tg.Engine = "vllm"
	if m, _ := chooseModel(tg, 0); m != nil {
		t.Error("vllm without GPU should refuse")
	}
}

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{"": "''", "a b": "'a b'", "it's": `'it'\''s'`} {
		if got := shellQuote(in); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
}
