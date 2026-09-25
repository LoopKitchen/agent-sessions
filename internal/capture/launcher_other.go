//go:build !darwin && !linux

package capture

// parentComm has no non-forking source on this platform; the field stays
// empty rather than costing a process spawn on every session start.
func parentComm(int) (string, bool) { return "", false }
