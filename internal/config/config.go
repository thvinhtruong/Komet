// Package config loads Komet's configuration from ~/.komet/config.json.
//
// The config describes three things: the local tier (which models may run on
// this machine and inside which resource envelope), the cloud tier (an
// OpenAI-compatible endpoint used for escalation), and the routing policy
// (quality bar, privacy handling, budget).
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	Local   LocalConfig `json:"local"`
	Targets []Target    `json:"targets,omitempty"`
	Cloud   CloudConfig `json:"cloud"`
	Policy  Policy      `json:"policy"`
	Budget  Budget      `json:"budget"`

	home string
}

// LocalConfig is the resource envelope for on-device inference.
type LocalConfig struct {
	Enabled bool `json:"enabled"`

	// Endpoint points at an already-running OpenAI-compatible server
	// (Ollama, LM Studio, a llama-server you manage yourself). When set,
	// Komet does not provision anything and just uses it.
	Endpoint      string `json:"endpoint,omitempty"`
	EndpointModel string `json:"endpoint_model,omitempty"`

	// ServerBin is the llama-server binary used for managed provisioning.
	// Empty means "find llama-server on PATH".
	ServerBin string `json:"server_bin,omitempty"`

	// Models are candidate local models, largest first. Komet picks the
	// first one whose RAM requirement fits the machine's free memory minus
	// HeadroomMB.
	Models []LocalModel `json:"models"`

	Threads      int     `json:"threads"`        // CPU threads the model may use
	Ctx          int     `json:"ctx"`            // context window (bounds KV-cache RAM)
	HeadroomMB   int     `json:"headroom_mb"`    // RAM that must stay free for other apps
	MaxLoad      float64 `json:"max_load"`       // per-core load average above which local is skipped
	Nice         int     `json:"nice"`           // scheduling priority of the model process (0..19)
	KeepAliveSec int     `json:"keep_alive_sec"` // unload the model after this many idle seconds
	CgroupLimits bool    `json:"cgroup_limits"`  // Linux: hard-cap RAM/CPU via systemd-run --user --scope
	GPULayers    int     `json:"gpu_layers"`     // layers offloaded to GPU (0 = CPU only)
}

type LocalModel struct {
	Name  string `json:"name"`
	Path  string `json:"path"`   // GGUF file
	RAMMB int    `json:"ram_mb"` // resident memory estimate incl. KV cache at Ctx
}

// CloudConfig is any OpenAI-compatible chat completions endpoint.
type CloudConfig struct {
	Enabled   bool    `json:"enabled"`
	BaseURL   string  `json:"base_url"`
	Model     string  `json:"model"`
	APIKeyEnv string  `json:"api_key_env"`
	PriceIn   float64 `json:"price_in_per_mtok"`  // USD per 1M input tokens
	PriceOut  float64 `json:"price_out_per_mtok"` // USD per 1M output tokens
	JSONMode  bool    `json:"json_mode"`          // send response_format=json_object
}

type Policy struct {
	// MinConfidence is the minimum geometric-mean token probability (0..1)
	// a local answer must reach. 0 disables the confidence gate.
	MinConfidence float64 `json:"min_confidence"`
	// Privacy controls what happens when secrets are found in the input:
	//   "redact"       - mask them before anything goes to a public API (default)
	//   "private_only" - only this machine and your own (private) targets may see them
	//   "local_only"   - never let such a request leave this machine
	//   "allow"        - send as-is
	Privacy string `json:"privacy"`
	// HardPromptChars: prompts (plus piped context) longer than this skip
	// the local tier and go straight to the cloud.
	HardPromptChars int `json:"hard_prompt_chars"`
	// MaxCloudUSD is a per-request ceiling on estimated cloud spend.
	MaxCloudUSD float64 `json:"max_cloud_usd"`
	// RequireKnownBinaries makes "command not found on this machine" a gate
	// failure, which catches hallucinated tools from small models.
	RequireKnownBinaries bool `json:"require_known_binaries"`
}

func Home() string {
	if h := os.Getenv("KOMET_HOME"); h != "" {
		return h
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, ".komet")
}

func Default(home string) *Config {
	models := filepath.Join(home, "models")
	return &Config{
		home: home,
		Local: LocalConfig{
			Enabled: true,
			Models: []LocalModel{
				{Name: "qwen2.5-coder-1.5b-q4", Path: filepath.Join(models, "qwen2.5-coder-1.5b-instruct-q4_k_m.gguf"), RAMMB: 1600},
				{Name: "qwen2.5-coder-0.5b-q4", Path: filepath.Join(models, "qwen2.5-coder-0.5b-instruct-q4_k_m.gguf"), RAMMB: 700},
			},
			Threads:      2,
			Ctx:          2048,
			HeadroomMB:   2048,
			MaxLoad:      0.85,
			Nice:         10,
			KeepAliveSec: 120,
		},
		Cloud: CloudConfig{
			Enabled:   true,
			BaseURL:   "https://api.deepseek.com/v1",
			Model:     "deepseek-chat",
			APIKeyEnv: "DEEPSEEK_API_KEY",
			// Example prices only. Set these to your provider's current rates.
			PriceIn:  0.30,
			PriceOut: 1.20,
			JSONMode: true,
		},
		Policy: Policy{
			MinConfidence:        0.80,
			Privacy:              "redact",
			HardPromptChars:      4000,
			MaxCloudUSD:          0.05,
			RequireKnownBinaries: true,
		},
	}
}

// Target returns the remote target with the given name.
func (c *Config) Target(name string) *Target {
	for i := range c.Targets {
		if c.Targets[i].Name == name {
			return &c.Targets[i]
		}
	}
	return nil
}

func (c *Config) Home() string    { return c.home }
func (c *Config) Path() string    { return filepath.Join(c.home, "config.json") }
func (c *Config) RunDir() string  { return filepath.Join(c.home, "run") }
func (c *Config) LogPath() string { return filepath.Join(c.home, "ledger.jsonl") }

// Load reads the config, falling back to defaults when no file exists.
func Load() (*Config, error) {
	home := Home()
	cfg := Default(home)
	b, err := os.ReadFile(cfg.Path())
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", cfg.Path(), err)
	}
	cfg.home = home
	return cfg, cfg.Validate()
}

func (c *Config) Validate() error {
	switch c.Policy.Privacy {
	case "", "redact", "private_only", "local_only", "allow":
	default:
		return fmt.Errorf("policy.privacy must be redact, private_only, local_only or allow (got %q)", c.Policy.Privacy)
	}
	seen := map[string]bool{"local": true, "cloud": true}
	for i := range c.Targets {
		t := &c.Targets[i]
		if t.Name == "" || seen[t.Name] {
			return fmt.Errorf("targets[%d]: name must be set, unique, and not local/cloud", i)
		}
		seen[t.Name] = true
		if err := t.normalize(); err != nil {
			return fmt.Errorf("target %s: %w", t.Name, err)
		}
	}
	if c.Policy.Privacy == "" {
		c.Policy.Privacy = "redact"
	}
	if c.Local.Threads <= 0 {
		c.Local.Threads = 2
	}
	if c.Local.Ctx <= 0 {
		c.Local.Ctx = 2048
	}
	if c.Local.KeepAliveSec <= 0 {
		c.Local.KeepAliveSec = 120
	}
	if c.Local.Nice < 0 || c.Local.Nice > 19 {
		return fmt.Errorf("local.nice must be between 0 and 19")
	}
	return nil
}

// Save writes the config (used by `komet init`).
func (c *Config) Save() error {
	if err := os.MkdirAll(c.home, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(c.Path(), append(b, '\n'), 0o600)
}

// CloudKey returns the API key from the configured environment variable.
func (c *Config) CloudKey() string {
	if c.Cloud.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(c.Cloud.APIKeyEnv)
}
