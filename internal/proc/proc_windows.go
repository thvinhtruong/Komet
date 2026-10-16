//go:build windows

package proc

import (
	"os"
	"os/exec"
)

// Managed provisioning is Unix-only in this prototype. On Windows, point
// local.endpoint at a server you run yourself (Ollama, LM Studio, Foundry
// Local, llama-server).

func Detach(cmd *exec.Cmd) {}

func Alive(pid int) bool {
	p, err := os.FindProcess(pid)
	return err == nil && p != nil && pid > 0
}

func Terminate(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

func SetNice(pid, nice int) error { return nil }

func Lock(path string) (func(), error) { return func() {}, nil }
