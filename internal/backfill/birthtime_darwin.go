//go:build darwin

package backfill

import (
	"os"
	"syscall"
	"time"
)

// birthTime is when a file was created. APFS records it, and it orders a
// fork and its origin whatever has been appended to either since: the copy is
// always the younger file. index still asks the promptId first, so the two
// platforms agree on the origin whenever the promptId can say.
func birthTime(fi os.FileInfo) time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Birthtimespec.Sec, st.Birthtimespec.Nsec).UTC()
	}
	return fi.ModTime().UTC()
}
