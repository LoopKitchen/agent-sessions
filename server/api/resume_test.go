package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// storedWithRecord builds an event whose stored body carries the harness's own
// record, which is what a fork is written from.
func storedWithRecord(seq int64, raw string) StoredEvent {
	body := fmt.Sprintf(`{"id":"e%d","session_id":"own-1","type":"assistant_turn","raw":%s}`, seq, raw)
	return StoredEvent{
		ID: fmt.Sprintf("e%d", seq), SessionID: "own-1", Email: "member@example.com",
		Seq: seq, Type: "assistant_turn", Origin: "transcript",
		OccurredAt: testNow.Add(time.Duration(seq) * time.Second),
		Body:       json.RawMessage(body),
	}
}

func resumeStore(t *testing.T, events ...StoredEvent) *fakeStore {
	t.Helper()
	f := seeded()
	f.events["own-1"] = events
	return f
}

func TestResumeDeliversTheConversationAndSaysWhatItDoesNotCarry(t *testing.T) {
	f := resumeStore(t,
		storedWithRecord(1, `{"type":"user","text":"start the migration"}`),
		storedWithRecord(2, `{"type":"assistant","text":"running it"}`),
	)
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	w := do(t, h, http.MethodGet, "/v1/sessions/own-1/resume?cwd=/home/jordan/work/api", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body)
	}
	got := decodeBody[ResumeBundle](t, w)

	if len(got.Transcript.Lines) != 2 || !got.Transcript.Complete {
		t.Fatalf("transcript = %+v", got.Transcript)
	}
	if got.Transcript.Format != "claude_code_jsonl" {
		t.Fatalf("format = %q", got.Transcript.Format)
	}
	// The lines are the harness's records, not this platform's canonical event.
	var first map[string]any
	if err := json.Unmarshal(got.Transcript.Lines[0], &first); err != nil {
		t.Fatalf("line is not JSON: %v", err)
	}
	if first["text"] != "start the migration" {
		t.Fatalf("line = %v, want the original record", first)
	}

	m := got.Manifest
	if !m.Resumable || m.NewSessionID == "" {
		t.Fatalf("manifest = %+v", m)
	}
	// A fresh identifier, never the origin's: the two files would collide in
	// one directory, and a fork is a new session rather than a second writer.
	if m.NewSessionID == "own-1" {
		t.Fatal("the fork reused the origin's session id")
	}
	if m.Filename != m.NewSessionID+".jsonl" {
		t.Fatalf("filename = %q", m.Filename)
	}
	if m.Command != "claude --resume "+m.NewSessionID {
		t.Fatalf("command = %q", m.Command)
	}
	if m.ProjectDir != "~/.claude/projects/-home-jordan-work-api" {
		t.Fatalf("project dir = %q", m.ProjectDir)
	}
	if m.Origin.SessionID != "own-1" || m.Origin.Owner != "member@example.com" {
		t.Fatalf("origin = %+v", m.Origin)
	}
	if len(m.Steps) == 0 {
		t.Fatal("no steps")
	}

	// The limits are the point of the manifest. Each of these is something a
	// reader would otherwise assume came along and discover missing halfway
	// through a task.
	limits := strings.ToLower(strings.Join(m.NotRestored, " "))
	for _, want := range []string{"tool server", "working director", "background", "permission mode"} {
		if !strings.Contains(limits, want) {
			t.Fatalf("not_restored does not mention %q: %v", want, m.NotRestored)
		}
	}
	if !strings.Contains(strings.ToLower(strings.Join(m.Caveats, " ")), "absolute path") {
		t.Fatalf("caveats do not mention embedded absolute paths: %v", m.Caveats)
	}
}

func TestResumeOmitsWhatCannotBeReplayedAndSaysSo(t *testing.T) {
	subagent := storedWithRecord(2, `{"type":"assistant","text":"subagent work"}`)
	subagent.AgentID = "agent-explore"
	noRecord := StoredEvent{
		ID: "e3", SessionID: "own-1", Email: "member@example.com", Seq: 3,
		Type: "tool_call", Origin: "hook", OccurredAt: testNow,
		Body: json.RawMessage(`{"id":"e3","session_id":"own-1"}`),
	}
	f := resumeStore(t,
		storedWithRecord(1, `{"type":"user","text":"hello"}`),
		subagent,
		noRecord,
	)
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	got := decodeBody[ResumeBundle](t, do(t, h, http.MethodGet, "/v1/sessions/own-1/resume", ""))
	if len(got.Transcript.Lines) != 1 {
		t.Fatalf("lines = %d, want only the main-transcript record", len(got.Transcript.Lines))
	}
	if got.Transcript.Events != 3 {
		t.Fatalf("events = %d, want every event on the page counted", got.Transcript.Events)
	}
	if got.Transcript.OmittedSubagent != 1 || got.Transcript.OmittedNoOriginalRecord != 1 {
		t.Fatalf("omissions = %+v", got.Transcript)
	}
	caveats := strings.ToLower(strings.Join(got.Manifest.Caveats, " "))
	if !strings.Contains(caveats, "subagent") || !strings.Contains(caveats, "could not be replayed") {
		t.Fatalf("caveats do not explain the omissions: %v", got.Manifest.Caveats)
	}
}

// The bundle is byte-identical before and after the derive runner folds
// the session, manifest counts included (contract C criterion g as amended
// after review-1 finding 11): the fold marks the hook copies of a
// dual-origin session superseded, and a resume that read the page as the
// timeline does would count fewer events and fewer omissions afterwards.
func TestResumeCountsTheSameEventsBeforeAndAfterAFold(t *testing.T) {
	hookCopy := StoredEvent{
		ID: "e3", SessionID: "own-1", Email: "member@example.com", Seq: 3,
		Type: "user_prompt", Origin: "hook", OccurredAt: testNow,
		Body: json.RawMessage(`{"id":"e3","session_id":"own-1","text":"hello"}`),
	}
	f := resumeStore(t,
		storedWithRecord(1, `{"type":"user","text":"hello"}`),
		storedWithRecord(2, `{"type":"assistant","text":"hi"}`),
		hookCopy,
	)
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})
	before := decodeBody[ResumeBundle](t, do(t, h, http.MethodGet, "/v1/sessions/own-1/resume", ""))

	// The runner folds the session: the hook prompt is superseded by e1.
	f.superseded = map[string]bool{"e3": true}
	after := decodeBody[ResumeBundle](t, do(t, h, http.MethodGet, "/v1/sessions/own-1/resume", ""))

	if !f.lastEventRange.IncludeSuperseded {
		t.Errorf("resume read the page without IncludeSuperseded; the manifest would count the fold")
	}
	if before.Transcript.Events != 3 || before.Transcript.OmittedNoOriginalRecord != 1 {
		t.Fatalf("before the fold: events %d omitted %d, want 3 and 1", before.Transcript.Events, before.Transcript.OmittedNoOriginalRecord)
	}
	// Each response mints its own fork id; everything else must match.
	masked := func(b ResumeBundle) string {
		return strings.ReplaceAll(fmt.Sprintf("%+v", b.Manifest), b.Manifest.NewSessionID, "<fork>")
	}
	if fmt.Sprintf("%+v", before.Transcript) != fmt.Sprintf("%+v", after.Transcript) {
		t.Errorf("the transcript changed across the fold:\nbefore %+v\nafter  %+v", before.Transcript, after.Transcript)
	}
	if masked(before) != masked(after) {
		t.Errorf("the manifest changed across the fold:\nbefore %s\nafter  %s", masked(before), masked(after))
	}
}

func TestResumePagesALongTranscriptUnderOneIdentifier(t *testing.T) {
	line := `{"type":"assistant","text":"` + strings.Repeat("x", 60) + `"}`
	f := resumeStore(t,
		storedWithRecord(1, line),
		storedWithRecord(2, line),
		storedWithRecord(3, line),
	)
	h, err := New(Options{
		Store: f, Auth: &fakeAuth{email: "member@example.com"},
		ResumeMaxBytes: 100, Now: func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first := decodeBody[ResumeBundle](t, do(t, h, http.MethodGet, "/v1/sessions/own-1/resume", ""))
	if len(first.Transcript.Lines) != 1 {
		t.Fatalf("lines = %d, want the byte budget to stop after one", len(first.Transcript.Lines))
	}
	if first.Transcript.Complete {
		t.Fatal("a page that stopped early must not report itself complete")
	}
	if first.Transcript.TruncatedBy != "bytes" || first.Transcript.NextCursor == "" {
		t.Fatalf("transcript = %+v", first.Transcript)
	}
	if !strings.Contains(strings.ToLower(strings.Join(first.Manifest.Caveats, " ")), "one page") {
		t.Fatalf("caveats do not warn that the bundle is partial: %v", first.Manifest.Caveats)
	}

	second := decodeBody[ResumeBundle](t, do(t, h, http.MethodGet,
		"/v1/sessions/own-1/resume?cursor="+url.QueryEscape(first.Transcript.NextCursor), ""))
	// The pages of one bundle assemble into one file, so the identifier has to
	// survive the round trip rather than being minted again.
	if second.Manifest.NewSessionID != first.Manifest.NewSessionID {
		t.Fatalf("fork id changed between pages: %q then %q",
			first.Manifest.NewSessionID, second.Manifest.NewSessionID)
	}
	if len(second.Transcript.Lines) != 1 {
		t.Fatalf("second page lines = %d", len(second.Transcript.Lines))
	}

	third := decodeBody[ResumeBundle](t, do(t, h, http.MethodGet,
		"/v1/sessions/own-1/resume?cursor="+url.QueryEscape(second.Transcript.NextCursor), ""))
	if !third.Transcript.Complete || third.Transcript.NextCursor != "" {
		t.Fatalf("final page = %+v", third.Transcript)
	}
}

// TestResumeAlwaysMakesProgress covers the trap in every budgeted pager: a
// single record larger than the whole budget must still be delivered, or the
// caller retries the same empty page forever.
func TestResumeAlwaysMakesProgress(t *testing.T) {
	huge := `{"type":"assistant","text":"` + strings.Repeat("y", 4096) + `"}`
	f := resumeStore(t, storedWithRecord(1, huge), storedWithRecord(2, huge))
	h, err := New(Options{
		Store: f, Auth: &fakeAuth{email: "member@example.com"},
		ResumeMaxBytes: 16, Now: func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := decodeBody[ResumeBundle](t, do(t, h, http.MethodGet, "/v1/sessions/own-1/resume", ""))
	if len(got.Transcript.Lines) != 1 {
		t.Fatalf("lines = %d, want the oversized record delivered anyway", len(got.Transcript.Lines))
	}
	if got.Transcript.NextCursor == "" {
		t.Fatal("no way to reach the rest of the transcript")
	}
}

func TestResumeOfAnUnsupportedHarnessSaysSoRatherThanGuessing(t *testing.T) {
	f := resumeStore(t, storedWithRecord(1, `{"type":"user","text":"hello"}`))
	f.sessions[0].Source = "codex"
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	got := decodeBody[ResumeBundle](t, do(t, h, http.MethodGet, "/v1/sessions/own-1/resume", ""))
	if got.Manifest.Resumable {
		t.Fatal("claimed a resume procedure it has never tried")
	}
	if got.Manifest.Command != "" {
		t.Fatalf("command = %q, want none", got.Manifest.Command)
	}
	if got.Manifest.UnsupportedReason == "" {
		t.Fatal("no explanation of why this cannot be resumed")
	}
	// The conversation is still handed over: it is readable even where it is
	// not replayable.
	if len(got.Transcript.Lines) != 1 {
		t.Fatalf("lines = %d", len(got.Transcript.Lines))
	}
}

func TestResumeOfAnEmptySessionIsComplete(t *testing.T) {
	f := resumeStore(t)
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	got := decodeBody[ResumeBundle](t, do(t, h, http.MethodGet, "/v1/sessions/own-1/resume", ""))
	if !got.Transcript.Complete || got.Transcript.NextCursor != "" {
		t.Fatalf("transcript = %+v", got.Transcript)
	}
	if got.Transcript.Lines == nil {
		t.Fatal("lines decoded as null, want an empty array")
	}
}

func TestResumeRecordsAreOneLineEach(t *testing.T) {
	pretty := "{\n  \"type\": \"user\",\n  \"text\": \"multi\\nline prompt\"\n}"
	f := resumeStore(t, storedWithRecord(1, pretty))
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})

	got := decodeBody[ResumeBundle](t, do(t, h, http.MethodGet, "/v1/sessions/own-1/resume", ""))
	if len(got.Transcript.Lines) != 1 {
		t.Fatalf("lines = %d", len(got.Transcript.Lines))
	}
	// The destination is a file with one record per line, so a record stored
	// pretty-printed has to arrive compacted or it corrupts every record after
	// it.
	if strings.ContainsAny(string(got.Transcript.Lines[0]), "\n\r") {
		t.Fatalf("record spans lines: %q", got.Transcript.Lines[0])
	}
}

func TestResumeIsAuditedLikeAnyOtherReadOfSomebodyElsesWork(t *testing.T) {
	f := resumeStore(t, storedWithRecord(1, `{"type":"user","text":"hello"}`))
	f.events["other-1"] = []StoredEvent{storedWithRecord(1, `{"type":"user","text":"theirs"}`)}
	h := newHandler(t, f, &fakeAuth{email: "admin@example.com"})

	if w := do(t, h, http.MethodGet, "/v1/sessions/other-1/resume", ""); w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body)
	}
	if len(f.audits) == 0 {
		t.Fatal("handing over a colleague's whole transcript was not recorded")
	}
	for _, a := range f.audits {
		if a.viewer != "admin@example.com" || a.owner != "other@example.com" {
			t.Fatalf("audit = %+v", a)
		}
	}
}

func TestProjectSlugFollowsTheHarnessLayout(t *testing.T) {
	cases := map[string]string{
		"/home/jordan/work/api":               "-home-jordan-work-api",
		"/home/jordan/.claude/projects":       "-home-jordan--claude-projects",
		"/home/jordan/work/apps/ai_ingestion": "-home-jordan-work-apps-ai_ingestion",
		"/home/jordan/loop-sessions":          "-home-jordan-loop-sessions",
	}
	for in, want := range cases {
		if got := projectSlug(in); got != want {
			t.Fatalf("projectSlug(%q) = %q, want %q", in, got, want)
		}
	}
}
