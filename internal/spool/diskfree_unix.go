//go:build unix

package spool

import "syscall"

// diskFree reports free and total bytes for the filesystem holding dir.
// syscall keeps this dependency-free; the client must install without a module
// cache or network, so every avoidable dependency is one more failure mode.
func diskFree(dir string) (free, total uint64, err error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, 0, err
	}
	bs := uint64(st.Bsize)
	return uint64(st.Bavail) * bs, st.Blocks * bs, nil
}
