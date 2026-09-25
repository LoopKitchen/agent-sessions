//go:build !darwin

package backfill

import (
	"os"
	"time"
)

// birthTime falls back to the modification time where the platform's stat
// carries no creation time. An origin that is still being written to is
// "younger" than its copy by this measure, so on these platforms it is only
// the tie-break: index decides a fork's origin by the promptId rule first
// and consults this only when neither file's promptId settles it.
func birthTime(fi os.FileInfo) time.Time { return fi.ModTime().UTC() }
