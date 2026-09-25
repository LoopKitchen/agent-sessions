package capture

import (
	"os"
	"path/filepath"
	"testing"
)

// The hook payload never says how the harness was started; the transcript's
// own records do. Without this read-through, every live-captured session
// classifies as a person — including the hourly cron runs the automation
// filter exists to separate.
func TestTranscriptEntrypointReadsTheHead(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, lines ...string) string {
		p := filepath.Join(dir, name)
		var body string
		for _, l := range lines {
			body += l + "\n"
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// A real headless transcript opens with bookkeeping lines that carry no
	// entrypoint; the first substantive record does.
	cron := write("cron.jsonl",
		`{"type":"queue-operation","operation":"enqueue","sessionId":"s-1"}`,
		`{"type":"queue-operation","operation":"dequeue","sessionId":"s-1"}`,
		`{"parentUuid":null,"type":"attachment","entrypoint":"sdk-cli","sessionId":"s-1"}`,
	)
	if got := transcriptEntrypoint(cron); got != "sdk-cli" {
		t.Errorf("cron transcript read as %q, want sdk-cli", got)
	}

	interactive := write("interactive.jsonl",
		`{"type":"last-prompt","sessionId":"s-2"}`,
		`{"type":"user","entrypoint":"cli","sessionId":"s-2"}`,
	)
	if got := transcriptEntrypoint(interactive); got != "cli" {
		t.Errorf("interactive transcript read as %q, want cli", got)
	}

	// A brand-new session whose file has nothing yet, and a path that does not
	// exist: both are an empty answer, never an error.
	if got := transcriptEntrypoint(write("empty.jsonl")); got != "" {
		t.Errorf("empty transcript read as %q", got)
	}
	if got := transcriptEntrypoint(filepath.Join(dir, "missing.jsonl")); got != "" {
		t.Errorf("missing transcript read as %q", got)
	}
	if got := transcriptEntrypoint(""); got != "" {
		t.Errorf("no path read as %q", got)
	}
}

// SessionStart and SessionEnd both stamp the entrypoint, so a session whose
// start raced an empty transcript file is still classified by its end.
func TestSessionEventsCarryTheTranscriptEntrypoint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"user","entrypoint":"sdk-cli","sessionId":"s-1"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	r := &recorder{}
	c := newCap(t, r)
	if _, err := c.Handle(HookEvent{
		HookEventName: "SessionStart", SessionID: "s-1", TranscriptPath: path, Source: "startup",
	}); err != nil {
		t.Fatalf("SessionStart: %v", err)
	}
	if _, err := c.Handle(HookEvent{
		HookEventName: "SessionEnd", SessionID: "s-1", TranscriptPath: path, Reason: "exit",
	}); err != nil {
		t.Fatalf("SessionEnd: %v", err)
	}
	if len(r.events) != 2 {
		t.Fatalf("captured %d events, want 2", len(r.events))
	}
	for _, e := range r.events {
		if e.Entrypoint != "sdk-cli" {
			t.Errorf("%s carries entrypoint %q, want sdk-cli", e.Type, e.Entrypoint)
		}
	}
}
