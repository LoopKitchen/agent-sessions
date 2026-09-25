package normalize

import (
	"encoding/json"
	"strings"
)

// SlashCommand reports the command a user record invoked, for the skill
// derivation: the name without its leading slash, whether the name came
// from the transcript's <command-name> envelope, and whether the record is
// a slash command at all.
//
// It is a wrapper over ClassifyUser rather than over slashCommand so the
// prefilters run first: a skill body carries the command envelope of the
// command that loaded it and is told apart only by its isMeta flag, which
// lives in raw, and an interruption or a compaction summary that happens
// to open with a slash is not a command. The caller builds text: the hook
// copy's is the typed line; a transcript copy's is its message.content
// (ContentText), never Event.Text, which the walker sets to the person's
// words when the command carried arguments.
//
// envelope is the trust bit. A name from the envelope is the harness's own
// record of what ran and always yields a row; a name read by the typed-line
// shape rule ("/api endpoint returns 500" parses as a command named api)
// yields one only when the catalog confirms it, which is the caller's
// business.
func SlashCommand(text string, raw []byte) (name string, envelope bool, ok bool) {
	m := ClassifyUser(text, raw, false, false)
	if m.Kind != KindSlashCommand {
		return "", false, false
	}
	name = strings.TrimPrefix(m.Command, "/")
	if name == "" {
		return "", false, false
	}
	return name, m.Envelope, true
}

// ContentText is a transcript record's message.content as prose: the
// string itself for a typed prompt, or the text blocks of a content array
// joined by newlines. Empty for a record with no message or a content of
// another shape. It reads the raw record line, so the caller need not know
// the record's layout.
func ContentText(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var r struct {
		Message *struct {
			Content json.RawMessage `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &r) != nil || r.Message == nil {
		return ""
	}
	return FlattenContent(r.Message.Content)
}

// FlattenContent renders a content value, which is a string in most
// records and an array of blocks in the rest: the string as it is, the
// blocks' text joined by newlines, or the raw bytes for a shape that is
// neither. The transcript walker's tool_result rendering calls this too,
// so the server and the client read one record the same way.
func FlattenContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var bs []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &bs); err == nil {
		var parts []string
		for _, b := range bs {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(raw)
}

// builtinCommands are the harness's own slash commands (design 3.5): typed
// as a command, recorded as a command, and not a skill anybody wrote. A
// typed /compact is not a skill invocation and never becomes a row.
//
// Eight names are both a harness command and a skill somewhere: plan and
// sandbox are skill directories in the organisation's repositories, and init, review,
// loop, schedule, simplify and security-review are harness-bundled skills.
// All eight stay off this list, so a typed /plan is a row that the unknown
// report shows until the catalog names it; a list that carried them would
// silently drop real usage. The list applies to bare names only: a typed
// /ralph-loop:help survives the help entry because its plugin is not empty,
// which the caller checks before asking.
var builtinCommands = map[string]bool{
	"login": true, "logout": true, "effort": true, "model": true, "rate-limit-options": true,
	"reload-skills": true, "reload-plugins": true, "insights": true, "compact": true, "clear": true,
	"help": true, "status": true, "plugin": true, "mcp": true, "resume": true, "rename": true,
	"permissions": true, "btw": true, "fast": true, "exit": true, "cost": true, "doctor": true,
	"config": true, "memory": true, "hooks": true, "agents": true, "context": true,
	"terminal-setup": true, "vim": true, "bug": true, "release-notes": true, "upgrade": true,
	"privacy-settings": true, "stats": true, "usage": true, "add-dir": true, "ide": true,
	"install-github-app": true,
}

// IsBuiltinCommand reports whether a bare command name is one of the
// harness's own. The name is taken as the derivation stores it: lowercase,
// no leading slash; both are normalised here anyway so a caller that
// forgot is not silently wrong.
func IsBuiltinCommand(name string) bool {
	return builtinCommands[strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "/"))]
}
