package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A live-layout smoke, not a fixture: when this test runs inside a real
// linked worktree, an allowlist naming the PRIMARY checkout must capture the
// worktree. Skips anywhere else, so CI and fixture-only machines lose nothing.
func TestLiveWorktreeLayoutMatchesPrimary(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Skip("no cwd")
	}
	common, ok := gitCommonDir(cwd)
	if !ok {
		t.Skip("not in a git checkout")
	}
	primary := filepath.Dir(common)
	if underPath(filepath.Clean(cwd), primary) {
		t.Skip("this checkout IS the primary; nothing linked to prove")
	}
	c := valid()
	c.CaptureOnlyPaths = []string{primary}
	if ok, why := c.ShouldCapture(cwd); !ok {
		t.Fatalf("a linked worktree of the allowed primary was refused: %s", why)
	}
}
