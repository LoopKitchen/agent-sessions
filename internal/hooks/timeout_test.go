package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// SessionEnd is the one hook the harness times out. Its registered timeout
// has to cover the transcript-exists check, the title scan and the spool
// write on a laptop that is shutting down, and stay under the 60 s ceiling
// the harness clamps to.
func TestSessionEndDefaultTimeoutIsThirtySeconds(t *testing.T) {
	if DefaultSessionEndTimeout != 30 {
		t.Fatalf("DefaultSessionEndTimeout = %d, want 30", DefaultSessionEndTimeout)
	}
	if got := (Options{}).withDefaults().Timeout; got != 30 {
		t.Fatalf("default Timeout = %d, want 30", got)
	}

	path := filepath.Join(t.TempDir(), "settings.json")
	if _, err := Register(Options{SettingsPath: path, Binary: "/usr/local/bin/loop-sessions"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Timeout int  `json:"timeout"`
				Async   bool `json:"async"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	for name, groups := range s.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				switch {
				case name == "SessionEnd" && h.Timeout != 30:
					t.Errorf("SessionEnd registered with timeout %d, want 30", h.Timeout)
				case name != "SessionEnd" && h.Timeout != 0:
					t.Errorf("%s registered with a timeout; async hooks are never timed out", name)
				}
				if !h.Async {
					t.Errorf("%s is not async", name)
				}
			}
		}
	}
}

// A machine registered by an earlier release carries the timeout that
// release wrote. The self-upgrade replaces the binary and nothing else, so
// unless registration rewrites an entry it finds, the fleet keeps the old
// budget forever and only fresh installs get the new one.
func TestRegisterRaisesAnExistingSessionEndTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if _, err := Register(Options{SettingsPath: path, Binary: "/usr/local/bin/loop-sessions", Timeout: 10}); err != nil {
		t.Fatal(err)
	}
	// The upgraded binary's `install --hooks-only`.
	plan, err := Register(Options{SettingsPath: path, Binary: "/usr/local/bin/loop-sessions"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Add) != 0 || len(plan.Update) != 1 || plan.Update[0] != "SessionEnd" {
		t.Fatalf("plan = %+v, want SessionEnd updated and nothing added", plan)
	}
	if len(plan.AlreadySet) != len(Events) {
		t.Fatalf("AlreadySet = %v; an entry that needs its timeout raised is still registered", plan.AlreadySet)
	}
	if got := sessionEndTimeouts(t, path); len(got) != 1 || got[0] != DefaultSessionEndTimeout {
		t.Fatalf("SessionEnd timeout after re-register = %v, want [%d]", got, DefaultSessionEndTimeout)
	}

	// The daemon's version-gated refresh does the same to an entry it finds
	// and adds nothing for a binary the hooks do not name.
	if _, err := Register(Options{SettingsPath: path, Binary: "/usr/local/bin/loop-sessions", Timeout: 10}); err != nil {
		t.Fatal(err)
	}
	if got := sessionEndTimeouts(t, path); len(got) != 1 || got[0] != 10 {
		t.Fatalf("setup: timeout = %v, want [10]", got)
	}
	updated, err := Refresh(Options{SettingsPath: path, Binary: "/opt/dev/loop-sessions"})
	if err != nil || len(updated) != 0 {
		t.Fatalf("refresh from a foreign binary updated %v (%v); it must touch nothing", updated, err)
	}
	if got := sessionEndTimeouts(t, path); len(got) != 1 || got[0] != 10 {
		t.Fatalf("a foreign binary's refresh changed the file: %v", got)
	}
	updated, err = Refresh(Options{SettingsPath: path, Binary: "/usr/local/bin/loop-sessions"})
	if err != nil || len(updated) != 1 || updated[0] != "SessionEnd" {
		t.Fatalf("refresh updated %v (%v), want SessionEnd", updated, err)
	}
	if got := sessionEndTimeouts(t, path); len(got) != 1 || got[0] != DefaultSessionEndTimeout {
		t.Fatalf("SessionEnd timeout after refresh = %v, want [%d]", got, DefaultSessionEndTimeout)
	}
	again, err := Refresh(Options{SettingsPath: path, Binary: "/usr/local/bin/loop-sessions"})
	if err != nil || len(again) != 0 {
		t.Fatalf("a second refresh updated %v; the file was already current", again)
	}
	for _, ev := range Events {
		if n := len(hookCommands(t, path, ev.Name)); n != 1 {
			t.Fatalf("%s has %d entries after register, register and refresh; want 1", ev.Name, n)
		}
	}
}

func sessionEndTimeouts(t *testing.T, path string) []int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Timeout int `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	var out []int
	for _, g := range s.Hooks["SessionEnd"] {
		for _, h := range g.Hooks {
			out = append(out, h.Timeout)
		}
	}
	return out
}
