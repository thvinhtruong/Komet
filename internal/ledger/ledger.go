// Package ledger records every workflow leg (the "complete accounting"
// principle): which tier ran, how long it took, what it cost, and whether its
// output passed the gates. The ledger is also the dataset you will later use
// to tune routing thresholds on your own workload.
package ledger

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Leg struct {
	Tier      string   `json:"tier"`           // local, a target name, or cloud
	Kind      string   `json:"kind,omitempty"` // local | remote | cloud
	Model     string   `json:"model"`
	LatencyMS int64    `json:"latency_ms"`
	InTokens  int      `json:"in_tokens"`
	OutTokens int      `json:"out_tokens"`
	CostUSD   float64  `json:"cost_usd"`
	Passed    bool     `json:"passed"`
	Failures  []string `json:"failures,omitempty"`
	Error     string   `json:"error,omitempty"`
}

type Entry struct {
	Time     time.Time `json:"time"`
	Mode     string    `json:"mode"`
	Route    string    `json:"route"` // e.g. "local", "cloud", "local→cloud"
	Reason   string    `json:"reason,omitempty"`
	Redacted []string  `json:"redacted,omitempty"`
	Legs     []Leg     `json:"legs"`
	TotalMS  int64     `json:"total_ms"`
	CostUSD  float64   `json:"cost_usd"`
	// CloudOnlyUSD estimates what the request would have cost had every
	// local leg's tokens gone to the cloud tier instead.
	CloudOnlyUSD float64 `json:"cloud_only_usd"`
	Accepted     bool    `json:"accepted"`
}

func Append(path string, e Entry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

func Read(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// Summary renders `komet stats`.
func Summary(entries []Entry) string {
	if len(entries) == 0 {
		return "No requests recorded yet."
	}
	routes := map[string]int{}
	var cost, cloudOnly float64
	var localLegs, localPassed int
	var lat []int64
	failures := map[string]int{}
	for _, e := range entries {
		routes[e.Route]++
		cost += e.CostUSD
		cloudOnly += e.CloudOnlyUSD
		lat = append(lat, e.TotalMS)
		for _, l := range e.Legs {
			if l.Tier == "local" {
				localLegs++
				if l.Passed {
					localPassed++
				}
				for _, f := range l.Failures {
					if i := strings.IndexAny(f, ":0123456789"); i > 0 {
						f = strings.TrimSpace(f[:i])
					}
					failures[f]++
				}
			}
		}
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	pct := func(p float64) int64 { return lat[int(p*float64(len(lat)-1))] }

	var b strings.Builder
	fmt.Fprintf(&b, "Requests:        %d\n", len(entries))
	keys := make([]string, 0, len(routes))
	for k := range routes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-14s %d (%.0f%%)\n", k, routes[k], 100*float64(routes[k])/float64(len(entries)))
	}
	if localLegs > 0 {
		fmt.Fprintf(&b, "Local accept rate: %.0f%% (%d/%d drafts passed the gates)\n", 100*float64(localPassed)/float64(localLegs), localPassed, localLegs)
	}
	if len(failures) > 0 {
		fmt.Fprintf(&b, "Top local failure reasons:\n")
		type kv struct {
			k string
			v int
		}
		var fl []kv
		for k, v := range failures {
			fl = append(fl, kv{k, v})
		}
		sort.Slice(fl, func(i, j int) bool { return fl[i].v > fl[j].v })
		for i, f := range fl {
			if i == 5 {
				break
			}
			fmt.Fprintf(&b, "  %-30s %d\n", f.k, f.v)
		}
	}
	fmt.Fprintf(&b, "Latency:         p50 %dms, p90 %dms\n", pct(0.5), pct(0.9))
	fmt.Fprintf(&b, "Cloud spend:     $%.4f\n", cost)
	if cloudOnly > 0 {
		fmt.Fprintf(&b, "Cloud-only est.: $%.4f (saved ≈ $%.4f, %.0f%%)\n", cloudOnly, cloudOnly-cost, 100*(cloudOnly-cost)/cloudOnly)
	}
	return b.String()
}
