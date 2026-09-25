//go:build linux

package capture

import (
	"os"
	"strconv"
	"strings"
)

// parentComm reads /proc/<pid>/comm: the kernel's own record of the command
// name, one file read and no process.
func parentComm(pid int) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/comm")
	if err != nil {
		return "", false
	}
	s := strings.TrimSpace(string(b))
	return s, s != ""
}
