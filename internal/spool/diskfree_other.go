//go:build !unix

package spool

// diskFree is unavailable on this platform. Returning total=0 makes guardDisk
// skip the free-space check rather than block capture, which is the right
// failure direction: not knowing the disk state is not a reason to lose events.
func diskFree(dir string) (free, total uint64, err error) {
	return 0, 0, nil
}
