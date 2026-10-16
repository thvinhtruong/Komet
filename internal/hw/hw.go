// Package hw takes a cheap snapshot of the machine's current resources so the
// router can decide whether running a local model right now would hurt the
// user's other applications.
package hw

import (
	"fmt"
	"runtime"
)

type Snapshot struct {
	OS          string
	Arch        string
	CPUs        int
	TotalMB     int     // physical memory
	AvailableMB int     // memory the OS can hand out without swapping (-1 if unknown)
	Load1       float64 // 1-minute load average (-1 if unknown)
}

// LoadPerCore is the 1-minute load divided by CPU count (-1 if unknown).
func (s Snapshot) LoadPerCore() float64 {
	if s.Load1 < 0 || s.CPUs == 0 {
		return -1
	}
	return s.Load1 / float64(s.CPUs)
}

func (s Snapshot) String() string {
	avail := "unknown"
	if s.AvailableMB >= 0 {
		avail = fmt.Sprintf("%d MB", s.AvailableMB)
	}
	load := "unknown"
	if s.LoadPerCore() >= 0 {
		load = fmt.Sprintf("%.2f per core", s.LoadPerCore())
	}
	return fmt.Sprintf("%s/%s, %d CPUs, RAM %d MB total / %s available, load %s",
		s.OS, s.Arch, s.CPUs, s.TotalMB, avail, load)
}

// Probe reads the current snapshot. It never fails; unknown values are -1.
func Probe() Snapshot {
	s := Snapshot{OS: runtime.GOOS, Arch: runtime.GOARCH, CPUs: runtime.NumCPU(), AvailableMB: -1, Load1: -1}
	probe(&s)
	return s
}
