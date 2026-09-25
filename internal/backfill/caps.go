package backfill

import "unicode/utf8"

// Capture-side size caps.
//
// The server refuses a single item over 4 MiB, whole. A tool that printed a
// few megabytes therefore did not merely lose its output: the turn it belonged
// to was rejected and quarantined on the laptop, and nine machines carried
// quarantines that were traced to exactly this. Cutting the text here and
// saying so is the difference between "output truncated" on a page and a turn
// that never arrives. The caps are shared by the live path and the walker so a
// hook copy and a transcript copy of the same result are cut at the same byte.
const (
	// MaxToolOutputBytes bounds Tool.Output. Half a megabyte keeps a whole
	// compiler log or test run while staying far under the item limit even
	// with a diff beside it.
	MaxToolOutputBytes = 512 << 10
	// MaxDiffSideBytes bounds each of Diff.Before and Diff.After. A megabyte
	// each is a generated file; anything larger is a lockfile or a bundle
	// nobody reads in a session viewer.
	MaxDiffSideBytes = 1 << 20
)

// CapText cuts s to at most limit bytes on a rune boundary and reports whether
// it did. A cut inside a multi-byte rune would leave the stored text invalid
// UTF-8, which Postgres refuses as a text value.
func CapText(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut], true
}
