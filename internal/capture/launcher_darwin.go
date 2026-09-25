//go:build darwin

package capture

import (
	"bytes"
	"strconv"
	"syscall"
)

// parentComm reads the parent's executable path through the kern.procargs2
// sysctl: four bytes of argc, then the executable path, NUL-terminated. A
// sysctl is a system call, not a process, so it is cheap enough for a hook.
// It answers only for processes the caller may inspect (its own user's), and
// an empty answer is exactly what a hook launched by something else reports.
func parentComm(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	raw, err := syscall.Sysctl("kern.procargs2." + strconv.Itoa(pid))
	if err != nil || len(raw) <= 4 {
		return "", false
	}
	// syscall.Sysctl trims one trailing NUL only; interior NULs survive, so
	// the exec path is the first NUL-terminated string after the argc word.
	rest := []byte(raw[4:])
	if i := bytes.IndexByte(rest, 0); i >= 0 {
		rest = rest[:i]
	}
	if len(rest) == 0 {
		return "", false
	}
	return string(rest), true
}
