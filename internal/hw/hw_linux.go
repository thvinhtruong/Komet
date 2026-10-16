//go:build linux

package hw

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

func probe(s *Snapshot) {
	if f, err := os.Open("/proc/meminfo"); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) < 2 {
				continue
			}
			kb, err := strconv.Atoi(fields[1])
			if err != nil {
				continue
			}
			switch fields[0] {
			case "MemTotal:":
				s.TotalMB = kb / 1024
			case "MemAvailable:":
				s.AvailableMB = kb / 1024
			}
		}
	}
	// Respect a cgroup v2 memory limit (containers, systemd slices).
	if b, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		if lim, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			limMB := int(lim / (1 << 20))
			if cur, err := os.ReadFile("/sys/fs/cgroup/memory.current"); err == nil {
				if used, err := strconv.ParseInt(strings.TrimSpace(string(cur)), 10, 64); err == nil {
					if free := limMB - int(used/(1<<20)); s.AvailableMB < 0 || free < s.AvailableMB {
						s.AvailableMB = free
					}
				}
			}
			if limMB < s.TotalMB {
				s.TotalMB = limMB
			}
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if v, err := strconv.ParseFloat(f[0], 64); err == nil {
				s.Load1 = v
			}
		}
	}
}
