package normalize

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The r2:64 envelope and the typed line are the two shapes a command
// reaches the server in; the wrapper strips one slash from both and says
// which shape it was, and refuses everything ClassifyUser would not call a
// command, the isMeta skill body above all.
func TestSlashCommand(t *testing.T) {
	const envelope = "<command-message>probe-skill</command-message>\n<command-name>/probe-skill</command-name>\n<command-args>hello</command-args>"
	cases := []struct {
		name     string
		text     string
		raw      string
		want     string
		envelope bool
		ok       bool
	}{
		{name: "r2:64 envelope", text: envelope, want: "probe-skill", envelope: true, ok: true},
		{name: "envelope without a slash in the name", text: "<command-name>probe-skill</command-name>\n<command-message>probe-skill</command-message>\n<command-args></command-args>", want: "probe-skill", envelope: true, ok: true},
		{name: "envelope with a transcript record", text: envelope, raw: `{"type":"user","promptId":"p1"}`, want: "probe-skill", envelope: true, ok: true},
		{name: "namespaced envelope", text: "<command-message>platform-engineer:platform-engineer</command-message>\n<command-name>/platform-engineer:platform-engineer</command-name>\n<command-args>Git fetch</command-args>", want: "platform-engineer:platform-engineer", envelope: true, ok: true},
		{name: "typed line", text: "/probe-skill", want: "probe-skill", envelope: false, ok: true},
		{name: "typed line with args", text: "/git --autonomous", want: "git", envelope: false, ok: true},
		{name: "typed namespaced", text: "/ralph-loop:help", want: "ralph-loop:help", envelope: false, ok: true},
		{name: "prose that looks like a command", text: "/api endpoint returns 500", want: "api", envelope: false, ok: true},
		{name: "path is prose", text: "/usr/local/bin/x", ok: false},
		{name: "plain prose", text: "fix the bug", ok: false},
		{name: "skill body is meta", text: "<command-message>x</command-message>\n<command-name>/x</command-name>\n<skill-format>true</skill-format>\n# body", raw: `{"isMeta":true}`, ok: false},
		{name: "empty envelope", text: "<command-name></command-name>\n<command-message>x</command-message>", ok: false},
		{name: "hook payload as raw is ignored", text: "/probe-skill", raw: `{"hook_event_name":"UserPromptSubmit","isMeta":true}`, want: "probe-skill", envelope: false, ok: true},
		{name: "compaction summary", text: "This session is being continued from a previous conversation", ok: false},
		{name: "interrupted", text: "[Request interrupted by user]", ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw []byte
			if tc.raw != "" {
				raw = []byte(tc.raw)
			}
			name, env, ok := SlashCommand(tc.text, raw)
			if ok != tc.ok || name != tc.want || env != tc.envelope {
				t.Errorf("SlashCommand = (%q, %v, %v), want (%q, %v, %v)", name, env, ok, tc.want, tc.envelope, tc.ok)
			}
		})
	}
}

// A string content and a text-block array read the same; a record with no
// message, or with a content of another shape, yields nothing a classifier
// could mistake for a command.
func TestContentText(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "string content", raw: `{"type":"user","message":{"role":"user","content":"<command-name>/probe-skill</command-name>"}}`, want: "<command-name>/probe-skill</command-name>"},
		{name: "text blocks", raw: `{"message":{"content":[{"type":"text","text":"first"},{"type":"image","source":{}},{"type":"text","text":"second"}]}}`, want: "first\nsecond"},
		{name: "no message", raw: `{"type":"user","promptId":"p"}`, want: ""},
		{name: "empty", raw: ``, want: ""},
		{name: "not json", raw: `{`, want: ""},
		{name: "content of another shape", raw: `{"message":{"content":{"weird":1}}}`, want: `{"weird":1}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ContentText([]byte(tc.raw)); got != tc.want {
				t.Errorf("ContentText = %q, want %q", got, tc.want)
			}
		})
	}
	if got := FlattenContent(nil); got != "" {
		t.Errorf("FlattenContent(nil) = %q", got)
	}
}

// Every 3.5 entry is a built-in, the eight names that are also skills are
// not, and the check tolerates the slash and the case a caller might leave
// on.
func TestIsBuiltinCommand(t *testing.T) {
	for name := range builtinCommands {
		if !IsBuiltinCommand(name) {
			t.Errorf("IsBuiltinCommand(%q) = false", name)
		}
	}
	if len(builtinCommands) != 38 {
		t.Errorf("%d built-ins, want the 38 of design 3.5", len(builtinCommands))
	}
	for _, skill := range []string{"plan", "sandbox", "init", "review", "loop", "schedule", "simplify", "security-review", "git", "engg:git", "ralph-loop:help", ""} {
		if IsBuiltinCommand(skill) {
			t.Errorf("IsBuiltinCommand(%q) = true; that name is a skill (or nothing) and must yield a row", skill)
		}
	}
	for _, spelled := range []string{"/compact", "Compact", " clear "} {
		if !IsBuiltinCommand(spelled) {
			t.Errorf("IsBuiltinCommand(%q) = false; the name is normalised before the lookup", spelled)
		}
	}
}

// The list must never name a skill directory in either of the organisation's
// skill repositories: a built-in that is also a skill would drop real usage silently.
// Skipped, with a log line, when the repositories are not on this machine.
func TestBuiltinListIsDisjointFromTheSkillDirectories(t *testing.T) {
	skillsDir, backendDir := os.Getenv("SKILLS_DIR"), os.Getenv("APP_REPO_DIR")
	if skillsDir == "" || backendDir == "" {
		t.Skip("SKILLS_DIR or APP_REPO_DIR is unset; the intersection with the skill directories is not checked on this machine")
	}
	var dirs []string
	plugins, err := filepath.Glob(filepath.Join(skillsDir, "plugins", "*", "skills", "*"))
	if err != nil {
		t.Fatal(err)
	}
	dirs = append(dirs, plugins...)
	backend, err := filepath.Glob(filepath.Join(backendDir, ".claude", "skills", "*"))
	if err != nil {
		t.Fatal(err)
	}
	dirs = append(dirs, backend...)
	if len(dirs) == 0 {
		t.Fatalf("no skill directories under %s or %s; the variables point at the wrong checkouts", skillsDir, backendDir)
	}
	var clash []string
	for _, d := range dirs {
		info, err := os.Stat(d)
		if err != nil || !info.IsDir() {
			continue
		}
		if IsBuiltinCommand(filepath.Base(d)) {
			clash = append(clash, d)
		}
	}
	sort.Strings(clash)
	if len(clash) > 0 {
		t.Errorf("built-in names that are skill directories:\n%s", strings.Join(clash, "\n"))
	}
	t.Logf("checked %d skill directories against %d built-ins", len(dirs), len(builtinCommands))
}
