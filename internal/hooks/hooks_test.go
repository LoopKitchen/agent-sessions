package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tmpSettings(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.json")
	if body != "" {
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return p
}

func opts(path string) Options {
	return Options{SettingsPath: path, Binary: "/usr/local/bin/loop-sessions"}
}

func readJSON(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("settings is not valid JSON after our write: %v\n%s", err, b)
	}
	return m
}

func hookCommands(t *testing.T, path, event string) []string {
	t.Helper()
	m := readJSON(t, path)
	raw, ok := m["hooks"]
	if !ok {
		return nil
	}
	var hooks map[string][]matcherGroup
	if err := json.Unmarshal(raw, &hooks); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, g := range hooks[event] {
		for _, h := range g.Hooks {
			out = append(out, h.Command)
		}
	}
	return out
}

func TestRegisterOnFreshMachineCreatesSettings(t *testing.T) {
	p := tmpSettings(t, "")
	plan, err := Register(opts(p))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Created {
		t.Fatal("expected the plan to report it created the file")
	}
	if len(plan.Add) != len(Events) {
		t.Fatalf("added %d hooks, want %d", len(plan.Add), len(Events))
	}
	for _, ev := range Events {
		cmds := hookCommands(t, p, ev.Name)
		if len(cmds) != 1 || !strings.Contains(cmds[0], Marker) {
			t.Fatalf("%s not registered: %v", ev.Name, cmds)
		}
	}
}

// The single most important property: a user's own hooks must survive. The
// reference machine has four unrelated hook events implementing personal
// enforcement gates, and destroying those would be a serious breach.
func TestRegisterPreservesForeignHooks(t *testing.T) {
	existing := `{
      "hooks": {
        "PreToolUse": [{"hooks":[{"type":"command","command":"~/.claude/hooks/my-own-guard"}]}],
        "Stop": [{"hooks":[{"type":"command","command":"/usr/bin/say done"}]}]
      }
    }`
	p := tmpSettings(t, existing)
	if _, err := Register(opts(p)); err != nil {
		t.Fatal(err)
	}

	pre := hookCommands(t, p, "PreToolUse")
	if len(pre) != 2 {
		t.Fatalf("PreToolUse has %d hooks, want the user's plus ours: %v", len(pre), pre)
	}
	var sawUser bool
	for _, c := range pre {
		if strings.Contains(c, "my-own-guard") {
			sawUser = true
		}
	}
	if !sawUser {
		t.Fatal("the user's own hook was destroyed")
	}
	stop := hookCommands(t, p, "Stop")
	if len(stop) != 2 {
		t.Fatalf("Stop hooks = %v, want the user's plus ours", stop)
	}
}

// Everything outside the hooks section belongs to the user and must round-trip
// untouched.
func TestRegisterPreservesUnrelatedSettings(t *testing.T) {
	existing := `{
      "model": "opus",
      "permissions": {"allow": ["Bash(git status)"]},
      "env": {"FOO": "bar"},
      "hooks": {}
    }`
	p := tmpSettings(t, existing)
	if _, err := Register(opts(p)); err != nil {
		t.Fatal(err)
	}
	m := readJSON(t, p)
	for _, key := range []string{"model", "permissions", "env"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("unrelated key %q was dropped", key)
		}
	}
	var model string
	_ = json.Unmarshal(m["model"], &model)
	if model != "opus" {
		t.Fatalf("model = %q, want it unchanged", model)
	}
}

func TestRegisterIsIdempotent(t *testing.T) {
	p := tmpSettings(t, "")
	if _, err := Register(opts(p)); err != nil {
		t.Fatal(err)
	}
	plan, err := Register(opts(p))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Add) != 0 {
		t.Fatalf("second register wanted to add %v", plan.Add)
	}
	// Re-running an installer must not accumulate duplicate hook entries.
	for _, ev := range Events {
		if got := hookCommands(t, p, ev.Name); len(got) != 1 {
			t.Fatalf("%s has %d entries after two installs: %v", ev.Name, len(got), got)
		}
	}
}

// A settings file that fails to parse may stop the harness from starting.
// Overwriting it would turn installing telemetry into an outage.
func TestInvalidSettingsIsRefusedNotOverwritten(t *testing.T) {
	p := tmpSettings(t, `{"hooks": {broken`)
	if _, err := Register(opts(p)); err == nil {
		t.Fatal("expected registration to refuse an unparseable settings file")
	}
	b, _ := os.ReadFile(p)
	if string(b) != `{"hooks": {broken` {
		t.Fatal("the user's file was modified despite being unparseable")
	}
}

func TestUnparseableHooksSectionIsRefused(t *testing.T) {
	p := tmpSettings(t, `{"hooks": "not-an-object"}`)
	if _, err := Register(opts(p)); err == nil {
		t.Fatal("expected refusal when the hooks section has an unexpected shape")
	}
}

// A relative binary path is a latent failure: the hook works until the harness
// runs with a different PATH, then capture silently stops.
func TestRelativeBinaryPathIsRejected(t *testing.T) {
	o := opts(tmpSettings(t, ""))
	o.Binary = "loop-sessions"
	_, err := Register(o)
	if err == nil {
		t.Fatal("expected a relative binary path to be rejected")
	}
	if !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("error should explain why: %v", err)
	}
}

func TestMissingBinaryIsRejected(t *testing.T) {
	o := Options{SettingsPath: tmpSettings(t, "")}
	if _, err := Register(o); err == nil {
		t.Fatal("expected an error when Binary is empty")
	}
}

// SessionEnd hooks share a ~1.5s budget; without an explicit timeout our flush
// gets killed partway.
func TestSessionEndCarriesAnExplicitTimeout(t *testing.T) {
	p := tmpSettings(t, "")
	o := opts(p)
	o.Timeout = 30
	if _, err := Register(o); err != nil {
		t.Fatal(err)
	}
	m := readJSON(t, p)
	var hooks map[string][]matcherGroup
	_ = json.Unmarshal(m["hooks"], &hooks)
	var found bool
	for _, g := range hooks["SessionEnd"] {
		for _, h := range g.Hooks {
			if isOurs(h.Command) {
				found = true
				if h.Timeout != 30 {
					t.Fatalf("SessionEnd timeout = %d, want 30", h.Timeout)
				}
			}
		}
	}
	if !found {
		t.Fatal("SessionEnd hook not registered")
	}
}

// Hooks must not block a user's turn.
func TestHooksAreRegisteredAsync(t *testing.T) {
	p := tmpSettings(t, "")
	if _, err := Register(opts(p)); err != nil {
		t.Fatal(err)
	}
	m := readJSON(t, p)
	var hooks map[string][]matcherGroup
	_ = json.Unmarshal(m["hooks"], &hooks)
	for _, g := range hooks["PostToolUse"] {
		for _, h := range g.Hooks {
			if isOurs(h.Command) && !h.Async {
				t.Fatal("our hooks must be async so they cannot delay a turn")
			}
		}
	}
}

func TestUnregisterRemovesOnlyOurs(t *testing.T) {
	existing := `{
      "model": "opus",
      "hooks": {"PreToolUse":[{"hooks":[{"type":"command","command":"~/my-guard"}]}]}
    }`
	p := tmpSettings(t, existing)
	if _, err := Register(opts(p)); err != nil {
		t.Fatal(err)
	}
	removed, err := Unregister(opts(p))
	if err != nil {
		t.Fatal(err)
	}
	if removed != len(Events) {
		t.Fatalf("removed %d, want %d", removed, len(Events))
	}
	pre := hookCommands(t, p, "PreToolUse")
	if len(pre) != 1 || !strings.Contains(pre[0], "my-guard") {
		t.Fatalf("the user's hook did not survive uninstall: %v", pre)
	}
	m := readJSON(t, p)
	if _, ok := m["model"]; !ok {
		t.Fatal("unrelated settings lost during uninstall")
	}
}

// A clean uninstall should not leave empty scaffolding behind.
func TestUnregisterDropsEmptiedEvents(t *testing.T) {
	p := tmpSettings(t, "")
	if _, err := Register(opts(p)); err != nil {
		t.Fatal(err)
	}
	if _, err := Unregister(opts(p)); err != nil {
		t.Fatal(err)
	}
	if got := hookCommands(t, p, "PostToolUse"); len(got) != 0 {
		t.Fatalf("PostToolUse still has entries after uninstall: %v", got)
	}
}

func TestUnregisterOnCleanMachineIsANoop(t *testing.T) {
	p := tmpSettings(t, `{"model":"opus"}`)
	removed, err := Unregister(opts(p))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 0 {
		t.Fatalf("removed %d from a machine we never touched", removed)
	}
}

func TestUnregisterOnMissingFileIsANoop(t *testing.T) {
	p := filepath.Join(t.TempDir(), "does-not-exist.json")
	removed, err := Unregister(opts(p))
	if err != nil {
		t.Fatalf("uninstall on a machine without settings should be quiet: %v", err)
	}
	if removed != 0 {
		t.Fatal("nothing should have been removed")
	}
}

func TestPreviewDoesNotModifyAnything(t *testing.T) {
	p := tmpSettings(t, `{"model":"opus"}`)
	before, _ := os.ReadFile(p)
	plan, err := Preview(opts(p))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Add) != len(Events) {
		t.Fatalf("preview planned %d additions, want %d", len(plan.Add), len(Events))
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("preview modified the file")
	}
}

func TestPreviewCountsForeignHooks(t *testing.T) {
	existing := `{"hooks":{"Stop":[{"hooks":[
        {"type":"command","command":"/a"},
        {"type":"command","command":"/b"}]}]}}`
	p := tmpSettings(t, existing)
	plan, err := Preview(opts(p))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Foreign != 2 {
		t.Fatalf("foreign = %d, want 2 so the installer can tell the user what it will leave alone", plan.Foreign)
	}
}

func TestWriteIsAtomic(t *testing.T) {
	p := tmpSettings(t, "")
	if _, err := Register(opts(p)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p + ".loop-tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file survived; a partial settings write can stop the harness starting")
	}
}

func TestRegisteredCommandIsRecognisableAsOurs(t *testing.T) {
	// Removal depends on this marker, and a user reading their own config
	// should be able to see exactly what we added.
	cmd := Command("/usr/local/bin/loop-sessions")
	if !isOurs(cmd) {
		t.Fatalf("command %q does not carry the ownership marker", cmd)
	}
	if !strings.Contains(cmd, "hook") {
		t.Fatalf("command %q does not invoke the hook subcommand", cmd)
	}
}

func TestDefaultSettingsPathHonoursHarnessOverride(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/custom/claude")
	if got := DefaultSettingsPath(); got != "/custom/claude/settings.json" {
		t.Fatalf("path = %q, want the override to be honoured", got)
	}
}

func TestEveryRegisteredEventHasAStatedReason(t *testing.T) {
	// Each hook costs a process spawn during someone's turn, so an event that
	// cannot justify itself should not be in the list.
	for _, ev := range Events {
		if strings.TrimSpace(ev.Why) == "" {
			t.Fatalf("event %s has no stated reason for being registered", ev.Name)
		}
	}
}
