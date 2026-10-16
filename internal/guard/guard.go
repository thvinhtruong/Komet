// Package guard finds secrets in text before it is allowed to leave the
// machine, and masks them.
package guard

import (
	"regexp"
	"sort"
)

type Finding struct {
	Kind  string
	Start int
	End   int
}

type rule struct {
	kind string
	re   *regexp.Regexp
	// group selects the submatch to mask (0 = whole match). Lets us keep
	// "password=" visible and only hide the value.
	group int
}

var rules = []rule{
	{"private_key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), 0},
	{"aws_access_key", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`), 0},
	{"github_token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,}\b|\bgithub_pat_[A-Za-z0-9_]{40,}\b`), 0},
	{"api_key", regexp.MustCompile(`\bsk-(?:[A-Za-z0-9_-]{20,})\b`), 0},
	{"slack_token", regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}\b`), 0},
	{"google_api_key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`), 0},
	{"jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`), 0},
	{"connection_string", regexp.MustCompile(`\b[a-z][a-z0-9+]*://[^\s:/@]+:([^\s@/]+)@`), 1},
	{"credential", regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|client[_-]?secret)\b\s*[:=]\s*["']?([^\s"']{4,})`), 1},
}

// Scan returns non-overlapping findings ordered by position.
func Scan(text string) []Finding {
	var out []Finding
	for _, r := range rules {
		for _, m := range r.re.FindAllStringSubmatchIndex(text, -1) {
			s, e := m[2*r.group], m[2*r.group+1]
			if s < 0 {
				continue
			}
			out = append(out, Finding{Kind: r.kind, Start: s, End: e})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Start != out[j].Start {
			return out[i].Start < out[j].Start
		}
		return out[i].End > out[j].End
	})
	// Drop findings nested inside an earlier, wider one.
	var merged []Finding
	for _, f := range out {
		if n := len(merged); n > 0 && f.Start < merged[n-1].End {
			continue
		}
		merged = append(merged, f)
	}
	return merged
}

// Redact masks every finding as [REDACTED:<kind>].
func Redact(text string) (string, []Finding) {
	fs := Scan(text)
	if len(fs) == 0 {
		return text, nil
	}
	b := make([]byte, 0, len(text))
	last := 0
	for _, f := range fs {
		b = append(b, text[last:f.Start]...)
		b = append(b, "[REDACTED:"+f.Kind+"]"...)
		last = f.End
	}
	b = append(b, text[last:]...)
	return string(b), fs
}

// Kinds lists the distinct finding kinds, for reporting.
func Kinds(fs []Finding) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range fs {
		if !seen[f.Kind] {
			seen[f.Kind] = true
			out = append(out, f.Kind)
		}
	}
	return out
}
