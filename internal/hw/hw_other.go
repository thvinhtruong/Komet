//go:build !linux && !darwin

package hw

// probe leaves values unknown on platforms without a probe yet (Windows).
// The router treats unknown availability as "do not gate on it".
func probe(s *Snapshot) {}
