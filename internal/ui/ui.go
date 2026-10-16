// Package ui handles terminal presentation: colors and a progress spinner
// on stderr, so stdout stays clean for piping (e.g. `$(komet "...")`).
package ui

import (
	"fmt"
	"os"
	"sync"
	"time"
)

func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

var (
	StdoutTTY = isTTY(os.Stdout)
	StderrTTY = isTTY(os.Stderr)
	StdinTTY  = isTTY(os.Stdin)
	noColor   = os.Getenv("NO_COLOR") != ""
)

func paint(code string, tty bool, s string) string {
	if !tty || noColor {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func Bold(s string) string   { return paint("1", StdoutTTY, s) }
func Dim(s string) string    { return paint("2", StderrTTY, s) }
func Green(s string) string  { return paint("32", StderrTTY, s) }
func Yellow(s string) string { return paint("33", StderrTTY, s) }
func Red(s string) string    { return paint("31", StderrTTY, s) }
func Cyan(s string) string   { return paint("36", StderrTTY, s) }

// Spinner shows the current stage ("loading model", "local · …") on stderr.
type Spinner struct {
	mu     sync.Mutex
	label  string
	stop   chan struct{}
	done   chan struct{}
	active bool
}

func NewSpinner() *Spinner { return &Spinner{} }

func (s *Spinner) Set(label string) {
	if !StderrTTY {
		return
	}
	s.mu.Lock()
	s.label = label
	start := !s.active
	if start {
		s.active = true
		s.stop, s.done = make(chan struct{}), make(chan struct{})
	}
	s.mu.Unlock()
	if start {
		go s.loop()
	}
}

func (s *Spinner) loop() {
	frames := []rune("⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏")
	t := time.NewTicker(80 * time.Millisecond)
	defer t.Stop()
	defer close(s.done)
	begin := time.Now()
	for i := 0; ; i++ {
		select {
		case <-s.stop:
			fmt.Fprint(os.Stderr, "\r\x1b[2K")
			return
		case <-t.C:
			s.mu.Lock()
			l := s.label
			s.mu.Unlock()
			fmt.Fprintf(os.Stderr, "\r\x1b[2K%s %s %s", Cyan(string(frames[i%len(frames)])), l,
				Dim(fmt.Sprintf("%.1fs", time.Since(begin).Seconds())))
		}
	}
}

func (s *Spinner) Stop() {
	s.mu.Lock()
	active := s.active
	s.active = false
	s.mu.Unlock()
	if active {
		close(s.stop)
		<-s.done
	}
}
