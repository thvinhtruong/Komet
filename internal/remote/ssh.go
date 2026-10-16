package remote

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/komet-cli/komet/internal/config"
)

//go:embed bootstrap.sh
var bootstrapScript string

// sshConn runs commands on a target. Host keys are trusted on first use and
// pinned per target in their own known_hosts file. Machines that come back
// from a stop with a new container or a recycled IP get that file reset on
// start (see session.go), which is the usual trade-off for cloud hosts.
type sshConn struct {
	t          *config.Target
	host       string
	port       int
	knownHosts string
}

func (c *sshConn) base(extra ...string) []string {
	args := []string{
		"-p", strconv.Itoa(c.port),
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "UserKnownHostsFile=" + c.knownHosts,
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "LogLevel=ERROR",
	}
	if c.t.KeyPath != "" {
		args = append(args, "-i", c.t.KeyPath, "-o", "IdentitiesOnly=yes")
	}
	args = append(args, extra...)
	return append(args, c.t.User+"@"+c.host)
}

// Ping reports whether SSH login works.
func (c *sshConn) Ping(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "ssh", append(c.base(), "true")...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return err
	}
	return nil
}

// Agent runs one bootstrap command and decodes its JSON result line.
// secrets are prepended to the script as shell variables so they never
// appear on a command line.
func (c *sshConn) Agent(ctx context.Context, secrets map[string]string, out any, args ...string) error {
	var script strings.Builder
	for k, v := range secrets {
		fmt.Fprintf(&script, "%s=%s\n", k, shellQuote(v))
	}
	script.WriteString(bootstrapScript)

	remote := "bash -s --"
	for _, a := range args {
		remote += " " + shellQuote(a)
	}
	cmd := exec.CommandContext(ctx, "ssh", append(c.base(), remote)...)
	cmd.Stdin = strings.NewReader(script.String())
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()

	// The result is the last non-empty stdout line.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(last), &e) == nil && e.Error != "" {
		return fmt.Errorf("%s", e.Error)
	}
	if runErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = runErr.Error()
		}
		return fmt.Errorf("remote %s: %s", args[0], msg)
	}
	if out != nil {
		if err := json.Unmarshal([]byte(last), out); err != nil {
			return fmt.Errorf("remote %s: unexpected output %q", args[0], last)
		}
	}
	return nil
}

// Tunnel returns an `ssh -N -L` command forwarding localPort to the
// remote server port on the target's loopback interface.
func (c *sshConn) Tunnel(localPort, remotePort int) *exec.Cmd {
	fwd := fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", localPort, remotePort)
	return exec.Command("ssh", c.base("-N", "-L", fwd, "-o", "ExitOnForwardFailure=yes")...)
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
