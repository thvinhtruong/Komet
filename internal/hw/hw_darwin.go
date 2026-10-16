//go:build darwin

package hw

import (
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

var vmStatLine = regexp.MustCompile(`^Pages (free|inactive|speculative|purgeable):\s+(\d+)`)

func probe(s *Snapshot) {
	if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
			s.TotalMB = int(v / (1 << 20))
		}
	}
	pageSize := int64(16384)
	if out, err := exec.Command("sysctl", "-n", "hw.pagesize").Output(); err == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
			pageSize = v
		}
	}
	// macOS has no MemAvailable; free + inactive + speculative + purgeable
	// pages is the usual approximation of memory obtainable without pressure.
	if out, err := exec.Command("vm_stat").Output(); err == nil {
		var pages int64
		for _, line := range strings.Split(string(out), "\n") {
			if m := vmStatLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
				v, _ := strconv.ParseInt(m[2], 10, 64)
				pages += v
			}
		}
		s.AvailableMB = int(pages * pageSize / (1 << 20))
	}
	if out, err := exec.Command("sysctl", "-n", "vm.loadavg").Output(); err == nil {
		f := strings.Fields(strings.Trim(strings.TrimSpace(string(out)), "{}"))
		if len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				s.Load1 = v
			}
		}
	}
}
