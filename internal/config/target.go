package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Target is a remote machine Komet can wake, provision and use as a tier:
// a VM you already have (kind "ssh"), a RunPod pod ("runpod") or an EC2
// instance ("aws"). The model runs there; Komet reaches it only through an
// SSH tunnel, so the inference port is never exposed to the internet.
type Target struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // ssh | runpod | aws

	// Trust is "private" (default): data stays in infrastructure you
	// control, so redaction is not applied. Set "public" for shared hosts.
	Trust string `json:"trust,omitempty"`

	// SSH access. For runpod/aws, host and port are discovered from the
	// provider API on every start, because they change between boots.
	Host    string `json:"host,omitempty"`
	Port    int    `json:"port,omitempty"`
	User    string `json:"user,omitempty"`
	KeyPath string `json:"key_path,omitempty"`

	// RunPod: an existing pod. Stopping a pod keeps its /workspace volume,
	// which is where Komet keeps the engine build and model weights.
	PodID     string `json:"pod_id,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"` // default RUNPOD_API_KEY
	APIBase   string `json:"api_base,omitempty"`    // default https://api.runpod.io/v2
	// SelfStopKeyEnv names an env var holding a RunPod key that is copied
	// to the pod so it can stop itself when idle, even if this computer is
	// offline. Use a separate, restricted key. Without it, only this
	// computer stops the pod.
	SelfStopKeyEnv string `json:"self_stop_key_env,omitempty"`

	// AWS: an existing EBS-backed EC2 instance, driven through the aws CLI
	// (so SSO, profiles and roles all work).
	InstanceID string `json:"instance_id,omitempty"`
	Region     string `json:"region,omitempty"`
	Profile    string `json:"profile,omitempty"`
	AWSBin     string `json:"aws_bin,omitempty"`

	// Engine on the remote machine.
	Engine     string        `json:"engine,omitempty"`      // llama (default) | vllm
	WorkDir    string        `json:"work_dir,omitempty"`    // persistent dir; default /workspace/komet (runpod) or ~/komet
	ServerBin  string        `json:"server_bin,omitempty"`  // llama-server path; default <work_dir>/llama.cpp/build/bin/llama-server
	InstallCmd string        `json:"install_cmd,omitempty"` // run once if the engine is missing; default builds llama.cpp with CUDA
	Models     []RemoteModel `json:"models"`
	Ctx        int           `json:"ctx,omitempty"`
	Parallel   int           `json:"parallel,omitempty"` // concurrent request slots
	GPULayers  int           `json:"gpu_layers,omitempty"`

	// Lifecycle and money.
	CostPerHour    float64 `json:"cost_per_hour,omitempty"`     // overrides the provider-reported price
	AutostopIdle   float64 `json:"autostop_idle_min,omitempty"` // stop after this many idle minutes (default 15)
	MaxUptimeHours float64 `json:"max_uptime_hours,omitempty"`  // hard cap per session (default 8)
	OnIdle         string  `json:"on_idle,omitempty"`           // stop (default) | keep: only close the tunnel
	// UseWhenRunning lets automatic routing use this target whenever it is
	// already up (default true). Automatic routing never wakes a machine
	// for a single request; use `komet up` or --target for that.
	UseWhenRunning *bool `json:"use_when_running,omitempty"`
}

type RemoteModel struct {
	Name string `json:"name"`
	// Path is the GGUF file on the remote machine (llama) or a Hugging Face
	// repo id / local dir (vllm).
	Path string `json:"path"`
	// URL is downloaded to Path when the file is missing (llama only).
	URL    string `json:"url,omitempty"`
	VRAMMB int    `json:"vram_mb"` // GPU memory needed incl. KV cache
}

// Budget caps spend on machines billed by the hour.
type Budget struct {
	MaxUSDPerDay float64 `json:"max_usd_per_day,omitempty"`
}

func (t *Target) normalize() error {
	t.Kind = strings.ToLower(t.Kind)
	switch t.Kind {
	case "ssh":
		if t.Host == "" {
			return fmt.Errorf("host is required for kind ssh")
		}
	case "runpod":
		if t.PodID == "" {
			return fmt.Errorf("pod_id is required for kind runpod")
		}
		if t.APIKeyEnv == "" {
			t.APIKeyEnv = "RUNPOD_API_KEY"
		}
		if t.APIBase == "" {
			t.APIBase = "https://api.runpod.io/v2"
		}
		if t.User == "" {
			t.User = "root"
		}
		if t.WorkDir == "" {
			t.WorkDir = "/workspace/komet"
		}
	case "aws":
		if t.InstanceID == "" {
			return fmt.Errorf("instance_id is required for kind aws")
		}
		if t.User == "" {
			t.User = "ubuntu"
		}
		if t.AWSBin == "" {
			t.AWSBin = "aws"
		}
	default:
		return fmt.Errorf("kind must be ssh, runpod or aws (got %q)", t.Kind)
	}
	if t.Trust == "" {
		t.Trust = "private"
	}
	if t.Trust != "private" && t.Trust != "public" {
		return fmt.Errorf("trust must be private or public")
	}
	if t.Port == 0 {
		t.Port = 22
	}
	if t.User == "" {
		t.User = os.Getenv("USER")
	}
	if t.KeyPath != "" && strings.HasPrefix(t.KeyPath, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			t.KeyPath = filepath.Join(h, t.KeyPath[2:])
		}
	}
	if t.Engine == "" {
		t.Engine = "llama"
	}
	if t.Engine != "llama" && t.Engine != "vllm" {
		return fmt.Errorf("engine must be llama or vllm")
	}
	if t.WorkDir == "" {
		t.WorkDir = "~/komet"
	}
	if t.Ctx == 0 {
		t.Ctx = 8192
	}
	if t.Parallel == 0 {
		t.Parallel = 4
	}
	if t.GPULayers == 0 {
		t.GPULayers = 999
	}
	if t.AutostopIdle == 0 {
		t.AutostopIdle = 15
	}
	if t.MaxUptimeHours == 0 {
		t.MaxUptimeHours = 8
	}
	if t.OnIdle == "" {
		t.OnIdle = "stop"
	}
	if t.OnIdle != "stop" && t.OnIdle != "keep" {
		return fmt.Errorf("on_idle must be stop or keep")
	}
	if len(t.Models) == 0 {
		return fmt.Errorf("at least one model is required")
	}
	return nil
}

// AutoUse reports whether automatic routing may use the target when it is
// already running.
func (t *Target) AutoUse() bool { return t.UseWhenRunning == nil || *t.UseWhenRunning }
