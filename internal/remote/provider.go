// Package remote turns machines you rent or own into Komet tiers.
//
// A Provider knows how to start, stop and locate a machine (a RunPod pod,
// an EC2 instance, or a plain SSH host). Everything else - probing the
// hardware, installing the engine, fetching the model, running the server
// and the idle watchdog - happens over SSH with the same bootstrap script on
// every provider, so adding a provider means implementing four methods.
package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/komet-cli/komet/internal/config"
)

const (
	StateRunning  = "running"
	StateStopped  = "stopped"
	StatePending  = "pending"
	StateStopping = "stopping"
	StateGone     = "gone" // terminated / deleted
	StateUnknown  = "unknown"
)

type Info struct {
	State       string
	Host        string // SSH host, empty until the machine has an address
	Port        int    // SSH port as seen from the outside
	CostPerHour float64
	Hardware    string // e.g. "1x NVIDIA RTX 4090" or "g5.xlarge"
}

type Provider interface {
	Info(ctx context.Context) (Info, error)
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
	// StopMode tells the remote watchdog how to stop the machine itself:
	// "aws", "runpod" or "none".
	StopMode() string
}

func NewProvider(t *config.Target) (Provider, error) {
	switch t.Kind {
	case "ssh":
		return &sshHost{t: t}, nil
	case "runpod":
		return &runPod{t: t, http: &http.Client{Timeout: 30 * time.Second}}, nil
	case "aws":
		return &awsEC2{t: t}, nil
	}
	return nil, fmt.Errorf("unknown target kind %q", t.Kind)
}

// ---------------------------------------------------------------- ssh host

// sshHost is a machine that is always on (your own server or a VM you
// manage elsewhere). Komet only starts and stops the model server on it.
type sshHost struct{ t *config.Target }

func (s *sshHost) Info(context.Context) (Info, error) {
	return Info{State: StateRunning, Host: s.t.Host, Port: s.t.Port, CostPerHour: s.t.CostPerHour}, nil
}
func (s *sshHost) Start(context.Context) error { return nil }
func (s *sshHost) Stop(context.Context) error  { return nil }
func (s *sshHost) StopMode() string            { return "none" }

// ------------------------------------------------------------------ RunPod

// runPod drives an existing pod through RunPod's REST API v2
// (https://api.runpod.io/v2). v1 is retired on 2026-11-15.
type runPod struct {
	t    *config.Target
	http *http.Client
}

func (r *runPod) StopMode() string { return "runpod" }

func (r *runPod) key() (string, error) {
	k := os.Getenv(r.t.APIKeyEnv)
	if k == "" {
		return "", fmt.Errorf("set $%s to your RunPod API key", r.t.APIKeyEnv)
	}
	return k, nil
}

func (r *runPod) do(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	key, err := r.key()
	if err != nil {
		return nil, 0, err
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(r.t.APIBase, "/")+path, rd)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return b, resp.StatusCode, err
}

// pod is the subset of the v2 pod object Komet uses.
type pod struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Status string  `json:"status"` // RUNNING, EXITED, TERMINATED, ...
	Cost   float64 `json:"cost"`   // USD/hour, 0 when not running
	GPU    struct {
		ID    string `json:"id"`
		Count int    `json:"count"`
	} `json:"gpu"`
	SSH struct {
		Direct struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		} `json:"direct"`
	} `json:"ssh"`
	Runtime struct {
		Ports []struct {
			IP      string `json:"ip"`
			Private int    `json:"private"`
			Public  int    `json:"public"`
			Type    string `json:"type"`
		} `json:"ports"`
	} `json:"runtime"`
}

func (r *runPod) find(ctx context.Context) (*pod, error) {
	// Single-pod GET first; fall back to paging through the list.
	if b, code, err := r.do(ctx, http.MethodGet, "/pods/"+r.t.PodID, nil); err == nil && code == http.StatusOK {
		var p pod
		if json.Unmarshal(b, &p) == nil && p.ID == r.t.PodID {
			return &p, nil
		}
	} else if err != nil {
		return nil, err
	}
	cursor := ""
	for page := 0; page < 20; page++ {
		path := "/pods?limit=1000"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		b, code, err := r.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		if code != http.StatusOK {
			return nil, fmt.Errorf("runpod: list pods: HTTP %d: %s", code, short(b))
		}
		var list struct {
			Pods       []pod `json:"pods"`
			Pagination struct {
				NextCursor string `json:"nextCursor"`
			} `json:"pagination"`
		}
		if err := json.Unmarshal(b, &list); err != nil {
			return nil, fmt.Errorf("runpod: decode pods: %w", err)
		}
		for i := range list.Pods {
			if list.Pods[i].ID == r.t.PodID {
				return &list.Pods[i], nil
			}
		}
		if list.Pagination.NextCursor == "" {
			break
		}
		cursor = list.Pagination.NextCursor
	}
	return nil, fmt.Errorf("runpod: pod %s not found on this account", r.t.PodID)
}

func (r *runPod) Info(ctx context.Context) (Info, error) {
	p, err := r.find(ctx)
	if err != nil {
		return Info{State: StateUnknown}, err
	}
	in := Info{CostPerHour: p.Cost}
	switch strings.ToUpper(p.Status) {
	case "RUNNING":
		in.State = StateRunning
	case "EXITED", "STOPPED":
		in.State = StateStopped
	case "TERMINATED":
		in.State = StateGone
	case "":
		in.State = StateUnknown
	default: // CREATED, STARTING, RESTARTING, ...
		in.State = StatePending
	}
	if p.GPU.ID != "" {
		in.Hardware = fmt.Sprintf("%dx %s", max(1, p.GPU.Count), p.GPU.ID)
	}
	in.Host, in.Port = p.SSH.Direct.Host, p.SSH.Direct.Port
	if in.Host == "" {
		for _, pt := range p.Runtime.Ports {
			if pt.Private == 22 && pt.IP != "" && pt.Public != 0 {
				in.Host, in.Port = pt.IP, pt.Public
			}
		}
	}
	// A running pod without a direct SSH mapping cannot be tunnelled to:
	// the proxied ssh.runpod.io endpoint does not support port forwarding.
	if in.State == StateRunning && in.Host == "" {
		return in, fmt.Errorf("runpod: pod %s has no public IP with TCP port 22 exposed; enable both in the pod settings", r.t.PodID)
	}
	return in, nil
}

func (r *runPod) action(ctx context.Context, action string) error {
	b, code, err := r.do(ctx, http.MethodPost, "/pods/"+r.t.PodID+"/action", map[string]string{"action": action})
	if err != nil {
		return err
	}
	if code/100 != 2 {
		return fmt.Errorf("runpod: %s pod %s: HTTP %d: %s", action, r.t.PodID, code, short(b))
	}
	return nil
}

func (r *runPod) Start(ctx context.Context) error { return r.action(ctx, "start") }
func (r *runPod) Stop(ctx context.Context) error  { return r.action(ctx, "stop") }

// --------------------------------------------------------------------- AWS

// awsEC2 drives an existing EBS-backed instance through the aws CLI, which
// already handles every credential style (env, profiles, SSO, roles).
// Stopping keeps the EBS volume - engine build and weights included - and
// stops compute billing; storage is still billed.
type awsEC2 struct{ t *config.Target }

func (a *awsEC2) StopMode() string { return "aws" }

func (a *awsEC2) run(ctx context.Context, args ...string) ([]byte, error) {
	full := append([]string{"ec2"}, args...)
	full = append(full, "--instance-ids", a.t.InstanceID, "--output", "json")
	if a.t.Region != "" {
		full = append(full, "--region", a.t.Region)
	}
	if a.t.Profile != "" {
		full = append(full, "--profile", a.t.Profile)
	}
	cmd := exec.CommandContext(ctx, a.t.AWSBin, full...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if _, lerr := exec.LookPath(a.t.AWSBin); lerr != nil {
			return nil, fmt.Errorf("aws CLI not found (%s); install it or set aws_bin", a.t.AWSBin)
		}
		return nil, fmt.Errorf("aws %s: %s", args[0], strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (a *awsEC2) Info(ctx context.Context) (Info, error) {
	out, err := a.run(ctx, "describe-instances")
	if err != nil {
		return Info{State: StateUnknown}, err
	}
	var d struct {
		Reservations []struct {
			Instances []struct {
				InstanceType    string `json:"InstanceType"`
				PublicIPAddress string `json:"PublicIpAddress"`
				PublicDNSName   string `json:"PublicDnsName"`
				State           struct {
					Name string `json:"Name"`
				} `json:"State"`
			} `json:"Instances"`
		} `json:"Reservations"`
	}
	if err := json.Unmarshal(out, &d); err != nil {
		return Info{State: StateUnknown}, fmt.Errorf("aws: decode describe-instances: %w", err)
	}
	if len(d.Reservations) == 0 || len(d.Reservations[0].Instances) == 0 {
		return Info{State: StateGone}, fmt.Errorf("aws: instance %s not found", a.t.InstanceID)
	}
	i := d.Reservations[0].Instances[0]
	in := Info{Hardware: i.InstanceType, CostPerHour: a.t.CostPerHour, Port: a.t.Port}
	switch i.State.Name {
	case "running":
		in.State = StateRunning
	case "stopped":
		in.State = StateStopped
	case "pending":
		in.State = StatePending
	case "stopping", "shutting-down":
		in.State = StateStopping
	case "terminated":
		in.State = StateGone
	default:
		in.State = StateUnknown
	}
	// The public IP changes on every start unless an Elastic IP is attached;
	// a configured host (e.g. an Elastic IP or a private IP over VPN) wins.
	switch {
	case a.t.Host != "":
		in.Host = a.t.Host
	case i.PublicIPAddress != "":
		in.Host = i.PublicIPAddress
	default:
		in.Host = i.PublicDNSName
	}
	return in, nil
}

func (a *awsEC2) Start(ctx context.Context) error {
	_, err := a.run(ctx, "start-instances")
	return err
}

func (a *awsEC2) Stop(ctx context.Context) error {
	_, err := a.run(ctx, "stop-instances")
	return err
}

func short(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}
