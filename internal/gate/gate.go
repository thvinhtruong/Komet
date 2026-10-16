// Package gate holds the quality gates that decide whether a draft from a
// cheap model is good enough to accept, or whether the request escalates.
//
// Gates are deliberately cheap and deterministic: parse the output, check
// shell syntax, confirm the binaries exist, run shellcheck if installed, and
// look at the model's own token confidence. None of them call a model.
package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Command is the structured answer expected in command mode.
type Command struct {
	Command     string `json:"command"`
	Explanation string `json:"explanation"`
}

type Result struct {
	Passed   bool
	Failures []string // reasons the draft was rejected
	Warnings []string // non-blocking notes shown to the user
	Danger   []string // destructive patterns; require explicit confirmation to run
}

func (r *Result) fail(format string, a ...any) {
	r.Passed = false
	r.Failures = append(r.Failures, fmt.Sprintf(format, a...))
}

// ParseCommand extracts the JSON answer, tolerating code fences and prose
// around it (cloud models without JSON mode often add them).
func ParseCommand(text string) (Command, error) {
	var c Command
	t := strings.TrimSpace(text)
	if err := json.Unmarshal([]byte(t), &c); err == nil && c.Command != "" {
		c.Command = strings.TrimSpace(c.Command)
		return c, nil
	}
	if i, j := strings.Index(t, "{"), strings.LastIndex(t, "}"); i >= 0 && j > i {
		if err := json.Unmarshal([]byte(t[i:j+1]), &c); err == nil && c.Command != "" {
			c.Command = strings.TrimSpace(c.Command)
			return c, nil
		}
	}
	return c, errors.New("answer is not the expected JSON {command, explanation}")
}

// Confidence turns per-token log-probabilities into a geometric-mean
// probability in [0,1]. ok is false when no logprobs were returned.
func Confidence(logprobs []float64) (p float64, ok bool) {
	if len(logprobs) == 0 {
		return 0, false
	}
	var sum float64
	for _, lp := range logprobs {
		sum += lp
	}
	return math.Exp(sum / float64(len(logprobs))), true
}

type CommandOptions struct {
	MinConfidence        float64
	RequireKnownBinaries bool
}

// CheckCommand runs all command-mode gates.
func CheckCommand(raw string, logprobs []float64, opt CommandOptions) (Command, Result) {
	res := Result{Passed: true}
	cmd, err := ParseCommand(raw)
	if err != nil {
		res.fail("%v", err)
		return cmd, res
	}
	if strings.Contains(cmd.Command, "\n") && !strings.Contains(cmd.Command, "\\\n") {
		res.Warnings = append(res.Warnings, "multi-line command")
	}
	if p, ok := Confidence(logprobs); ok && opt.MinConfidence > 0 && p < opt.MinConfidence {
		res.fail("low model confidence %.2f < %.2f", p, opt.MinConfidence)
	}
	if err := bashSyntax(cmd.Command); err != nil {
		res.fail("shell syntax error: %v", err)
	}
	if msgs, ran := shellcheck(cmd.Command); ran && len(msgs) > 0 {
		res.fail("shellcheck: %s", strings.Join(msgs, "; "))
	}
	if missing := MissingBinaries(cmd.Command); len(missing) > 0 {
		msg := "not found on this machine: " + strings.Join(missing, ", ")
		if opt.RequireKnownBinaries {
			res.fail("%s", msg)
		} else {
			res.Warnings = append(res.Warnings, msg)
		}
	}
	res.Danger = Dangerous(cmd.Command)
	return cmd, res
}

// CheckText is the gate for free-form answers: non-empty and confident.
func CheckText(raw string, logprobs []float64, minConfidence float64) Result {
	res := Result{Passed: true}
	if strings.TrimSpace(raw) == "" {
		res.fail("empty answer")
	}
	if p, ok := Confidence(logprobs); ok && minConfidence > 0 && p < minConfidence {
		res.fail("low model confidence %.2f < %.2f", p, minConfidence)
	}
	return res
}

func bashSyntax(command string) error {
	bash, err := exec.LookPath("bash")
	if err != nil {
		return nil // cannot check; not a failure
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, bash, "-n")
	c.Stdin = strings.NewReader(command)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		msg = strings.TrimPrefix(msg, "bash: ")
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(msg)
	}
	return nil
}

// shellcheck reports error-level findings only; style nits do not escalate.
func shellcheck(command string) (msgs []string, ran bool) {
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, sc, "-s", "bash", "-S", "error", "-f", "gcc", "-")
	c.Stdin = strings.NewReader(command)
	out, _ := c.Output()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		if i := strings.Index(line, "error:"); i >= 0 {
			line = strings.TrimSpace(line[i+len("error:"):])
		}
		msgs = append(msgs, line)
	}
	return msgs, true
}

var (
	quoted       = regexp.MustCompile(`'[^']*'|"(?:[^"\\]|\\.)*"`)
	segmentSplit = regexp.MustCompile(`\|\||&&|[|;&\n]|\$\(|` + "`" + `|\(|\)|\{|\}`)
	commandName  = regexp.MustCompile(`^[A-Za-z0-9_./][A-Za-z0-9_.+/-]*$`)
	assignment   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	wrappers     = map[string]bool{"sudo": true, "doas": true, "env": true, "nohup": true, "time": true, "exec": true, "command": true, "nice": true, "ionice": true, "watch": true, "!": true}
	keywords     = map[string]bool{"if": true, "then": true, "else": true, "elif": true, "fi": true, "for": true, "while": true, "until": true, "do": true, "done": true, "case": true, "esac": true, "in": true, "function": true, "select": true, "[[": true, "]]": true}
	builtins     = map[string]bool{"cd": true, "echo": true, "printf": true, "read": true, "export": true, "set": true, "unset": true, "source": true, ".": true, "[": true, "test": true, "true": true, "false": true, "eval": true, "exit": true, "return": true, "local": true, "declare": true, "alias": true, "trap": true, "wait": true, "shift": true, "pwd": true, "type": true, "hash": true, "ulimit": true, "umask": true, "jobs": true, "fg": true, "bg": true, "kill": true, "let": true, "pushd": true, "popd": true, "history": true, "mapfile": true, "readarray": true, ":": true, "break": true, "continue": true}
)

// MissingBinaries returns command names in the pipeline that are neither
// shell builtins nor found on PATH. Small models invent tools and flags;
// this is the cheapest way to catch the tool half of that.
func MissingBinaries(command string) []string {
	var missing []string
	seen := map[string]bool{}
	// Quoted strings (awk programs, messages) are arguments, never commands.
	unquoted := quoted.ReplaceAllString(command, "ARG")
	for _, seg := range segmentSplit.Split(unquoted, -1) {
		for _, tok := range strings.Fields(seg) {
			if tok == "for" || tok == "select" || tok == "case" {
				break // loop/case header: the next word is a variable, not a command
			}
			if wrappers[tok] || keywords[tok] || assignment.MatchString(tok) {
				continue
			}
			if strings.HasPrefix(tok, "-") { // flag of a wrapper, e.g. sudo -u x
				continue
			}
			name := strings.Trim(tok, `"'\`)
			if !commandName.MatchString(name) || builtins[name] {
				break
			}
			if !seen[name] {
				seen[name] = true
				if strings.Contains(name, "/") {
					break // explicit path: script the user may be about to create
				}
				if _, err := exec.LookPath(name); err != nil {
					missing = append(missing, name)
				}
			}
			break // only the first word of each segment is a command
		}
	}
	return missing
}

var dangerRules = []struct {
	re   *regexp.Regexp
	desc string
}{
	{regexp.MustCompile(`\brm\s+(-[a-zA-Z]*[rR][a-zA-Z]*\s+|-[a-zA-Z]*f[a-zA-Z]*\s+)*(-[a-zA-Z]*\s+)*(/|~|\$HOME|/\*|\*)(\s|$)`), "recursive delete of a root, home or wildcard path"},
	{regexp.MustCompile(`\brm\s+-[a-zA-Z]*[rR]`), "recursive delete"},
	{regexp.MustCompile(`\bmkfs(\.\w+)?\b`), "formats a filesystem"},
	{regexp.MustCompile(`\bdd\b.*\bof=/dev/`), "writes raw data to a device"},
	{regexp.MustCompile(`>\s*/dev/(sd|nvme|hd|disk)`), "overwrites a block device"},
	{regexp.MustCompile(`:\(\)\s*\{\s*:\|:&\s*\};:`), "fork bomb"},
	{regexp.MustCompile(`\bchmod\s+(-R\s+)?[0-7]*777\s+/`), "world-writable permissions on system paths"},
	{regexp.MustCompile(`\b(curl|wget)\b[^|]*\|\s*(sudo\s+)?(ba|z)?sh\b`), "pipes a download straight into a shell"},
	{regexp.MustCompile(`\bgit\s+push\b.*(--force\b|-f\b)`), "force-push rewrites remote history"},
	{regexp.MustCompile(`\bgit\s+(reset\s+--hard|clean\s+-[a-zA-Z]*f)`), "discards local changes"},
	{regexp.MustCompile(`\b(shutdown|reboot|halt|poweroff)\b`), "shuts down or restarts the machine"},
	{regexp.MustCompile(`\bDROP\s+(TABLE|DATABASE)\b`), "drops database objects"},
}

// Dangerous lists destructive patterns found in the command.
func Dangerous(command string) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range dangerRules {
		if r.re.MatchString(command) && !seen[r.desc] {
			// The generic "recursive delete" is redundant after the specific one.
			if r.desc == "recursive delete" && seen["recursive delete of a root, home or wildcard path"] {
				continue
			}
			seen[r.desc] = true
			out = append(out, r.desc)
		}
	}
	return out
}
