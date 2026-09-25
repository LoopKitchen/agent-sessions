package api

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// sourceClaudeCode is the only harness whose resume procedure is known. Other
// sources are still handed their transcript; they are not handed instructions
// that were never tried.
const sourceClaudeCode = "claude_code"

// ResumeBundle is the response of GET /v1/sessions/{id}/resume: a colleague's
// conversation, plus what to do with it.
//
// These types are exported, unlike the envelopes on the other routes, because
// this is the one response a program consumes rather than a page renders. The
// client that writes the fork onto somebody's laptop can share these
// definitions instead of re-deriving them from a sample response.
type ResumeBundle struct {
	Session    Session          `json:"session"`
	Manifest   ResumeManifest   `json:"manifest"`
	Transcript ResumeTranscript `json:"transcript"`
}

// ResumeManifest describes how to continue the conversation locally, and what
// continuing it does not get you.
//
// The honest framing belongs in the response rather than in a wiki page nobody
// opens: you resume the conversation, not the environment it ran in. Everything
// in NotRestored is something a reader would otherwise assume came along,
// discover missing halfway through a task, and blame on the transcript.
type ResumeManifest struct {
	// Resumable is false when the session came from a harness whose resume
	// procedure this server does not know. The transcript is still delivered.
	Resumable bool   `json:"resumable"`
	Harness   string `json:"harness"`

	// NewSessionID is the identifier the fork takes on the recipient's machine.
	// It is minted here and never reuses the origin's, because the two files
	// would collide in the same directory if the recipient is also the owner,
	// and because a fork is a new session rather than a second writer to an old
	// one: the transcript is an append-only local file with no multi-writer
	// protocol, so concurrent resume cannot be made correct.
	//
	// It is stable across the pages of one bundle, carried in the cursor, so a
	// transcript delivered in several requests assembles into one file.
	NewSessionID string `json:"new_session_id"`
	Filename     string `json:"filename"`

	// ProjectDir is filled in only when the caller says where its project lives,
	// by passing ?cwd=. The server does not know the recipient's layout and will
	// not guess at it.
	ProjectDir     string `json:"project_dir,omitempty"`
	ProjectDirRule string `json:"project_dir_rule"`

	Command string   `json:"command,omitempty"`
	Steps   []string `json:"steps"`

	Origin      ResumeOrigin `json:"origin"`
	CarriesOver []string     `json:"carries_over"`
	NotRestored []string     `json:"not_restored"`
	Caveats     []string     `json:"caveats"`

	UnsupportedReason string `json:"unsupported_reason,omitempty"`
}

// ResumeOrigin records where the conversation came from. It is part of the
// bundle rather than something the recipient has to remember, because a forked
// session that cannot be traced back to the work it continues is where the
// lineage between one logical task and its five files gets lost.
type ResumeOrigin struct {
	SessionID string `json:"session_id"`
	Owner     string `json:"owner"`
	Source    string `json:"source"`
	Cwd       string `json:"cwd,omitempty"`
	Repo      string `json:"repo,omitempty"`
	GitBranch string `json:"git_branch,omitempty"`
	// StartedAt is the event time the original session began, never the moment
	// it was uploaded, so a fork of a session backfilled from March is dated to
	// March.
	StartedAt       time.Time `json:"started_at"`
	HarnessVersions []string  `json:"harness_versions,omitempty"`
}

// ResumeTranscript is the conversation itself, as the harness's own records.
//
// Lines are the original records, passed through rather than rebuilt from this
// platform's canonical event shape. A fork has to load in the harness that
// wrote it, and a record this server reconstructed would be this server's idea
// of the conversation rather than the conversation.
type ResumeTranscript struct {
	Format string            `json:"format"`
	Lines  []json.RawMessage `json:"lines"`

	// Events is how many stored events this page covered, which is larger than
	// the number of lines whenever something was left out. The three counts
	// below say why, so a short transcript is explained rather than merely
	// short.
	Events                  int `json:"events"`
	OmittedNoOriginalRecord int `json:"omitted_no_original_record"`
	OmittedSubagent         int `json:"omitted_subagent_events"`

	// Complete is false when the conversation continues beyond this page. The
	// fork should not be started until every page has been appended: a
	// transcript cut off mid-conversation resumes into a session that is
	// missing its own recent history and cannot tell.
	Complete   bool   `json:"complete"`
	NextCursor string `json:"next_cursor,omitempty"`
	// TruncatedBy names which bound ended this page, for an operator wondering
	// why a bundle arrived in eleven parts.
	TruncatedBy string `json:"truncated_by,omitempty"`
}

// handleResume assembles the fork-on-resume bundle.
//
// It is two audited reads, the rollup and the transcript window, so the access
// log carries both. That is not tidy but it is correct: each is a read of a
// colleague's work in its own right, and collapsing them would mean one of the
// two happening without a record.
func (h *Handler) handleResume(w http.ResponseWriter, r *http.Request) {
	v, ok := h.viewer(w, r)
	if !ok {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeNotFound(w)
		return
	}
	q := r.URL.Query()
	scope := scopeOf(string(cursorResume), v.Email, id)
	cur, err := decodeCursor(q.Get("cursor"), cursorResume, scope)
	if err != nil {
		h.storeFailure(w, r, "resume", err)
		return
	}

	sess, err := h.store.GetSession(r.Context(), v, id)
	if err != nil {
		h.storeFailure(w, r, "resume", err)
		return
	}
	// Every stored row, the superseded hook copies included: the lines are
	// rebuilt from transcript rows, which are never superseded, so the
	// transcript is the same either way; the manifest's counts are not, and
	// a bundle whose counts change when the runner folds the session is a
	// bundle that does not round-trip.
	page, err := h.store.GetEvents(r.Context(), v, id, EventRange{
		AfterSeq:          cur.AfterSeq,
		Limit:             h.resumeMaxN,
		IncludeSuperseded: true,
	})
	if err != nil {
		h.storeFailure(w, r, "resume", err)
		return
	}

	scan := scanTranscript(page.Events, h.resumeMaxB)
	fork := cur.Fork
	if fork == "" {
		fork = newSessionID()
	}

	transcript := ResumeTranscript{
		Format:                  transcriptFormat(sess.Source),
		Lines:                   scan.lines,
		Events:                  scan.consumed,
		OmittedNoOriginalRecord: scan.noRecord,
		OmittedSubagent:         scan.subagent,
		TruncatedBy:             scan.truncatedBy,
	}
	if transcript.Lines == nil {
		transcript.Lines = []json.RawMessage{}
	}
	// The page is complete when everything the store handed over was consumed
	// and the store had nothing more to hand over.
	more := scan.truncatedBy != "" || page.HasMore
	transcript.Complete = !more
	if more {
		after := scan.lastSeq
		if scan.truncatedBy == "" && page.NextAfter != nil {
			after = page.NextAfter
		}
		if after != nil {
			transcript.NextCursor = encodeCursor(cursor{
				Kind: cursorResume, Scope: scope, AfterSeq: after, Fork: fork,
			})
		} else {
			// Nothing to page from means the transcript cannot be continued,
			// and saying "there is more, here is nowhere to get it" would leave
			// the caller in a loop. Report the page as what it is.
			transcript.Complete = true
			transcript.TruncatedBy = ""
		}
	}

	writeJSON(w, http.StatusOK, ResumeBundle{
		Session:    sess,
		Manifest:   h.manifest(sess, fork, strings.TrimSpace(q.Get("cwd")), transcript),
		Transcript: transcript,
	})
}

// manifest builds the instructions and the limits that go with them.
func (h *Handler) manifest(sess Session, fork, cwd string, t ResumeTranscript) ResumeManifest {
	m := ResumeManifest{
		Resumable:    sess.Source == sourceClaudeCode,
		Harness:      sess.Source,
		NewSessionID: fork,
		Filename:     fork + ".jsonl",
		ProjectDirRule: "The harness keeps one directory per project under ~/.claude/projects " +
			"(or under $CLAUDE_CONFIG_DIR/projects when that is set), named after the project's " +
			"absolute path with every character outside A-Z a-z 0-9 _ - replaced by a hyphen. " +
			"Pass your own project directory as ?cwd= to have this computed, and check the " +
			"directory exists before writing into it.",
		Origin: ResumeOrigin{
			SessionID:       sess.SessionID,
			Owner:           sess.Email,
			Source:          sess.Source,
			Cwd:             sess.Cwd,
			Repo:            sess.Repo,
			GitBranch:       sess.GitBranch,
			StartedAt:       sess.StartedAt,
			HarnessVersions: sess.HarnessVersions,
		},
		CarriesOver: []string{
			"The conversation: the prompts, the replies, and the tool calls and results between them.",
			"The link back to the original session, which stays inside the records and is what lets " +
				"this fork be traced to the work it continues.",
		},
		NotRestored: []string{
			"Tool servers. Whatever MCP servers the original machine had configured are not " +
				"configured here, and the fork runs with yours.",
			"Additional working directories added to the original session.",
			"Background work. Long-running shells, watchers and monitors from the original " +
				"session are not running, and nothing will restart them.",
			"Permission mode. The fork starts under your own default, not the mode the original " +
				"session was granted.",
			"The originating machine's settings, hooks and plugins. Yours apply instead.",
		},
		Caveats: []string{
			"Absolute paths from the originating machine are embedded in the conversation text. " +
				"They read as real paths and will not resolve on your machine.",
			"The transcript format is internal to the harness and changes between versions. A fork " +
				"written by a version far from yours may not load.",
		},
	}

	if cwd != "" {
		m.ProjectDir = "~/.claude/projects/" + projectSlug(cwd)
	}
	if !m.Resumable {
		m.UnsupportedReason = fmt.Sprintf(
			"the resume procedure is only established for %s sessions; this one was captured from %q, "+
				"so the records below are delivered without instructions rather than with guessed ones",
			sourceClaudeCode, sess.Source)
	}

	// The counts drive the caveats, so a reader is told what is missing from
	// their copy rather than having to compare two numbers in the transcript
	// block and work it out.
	if t.OmittedSubagent > 0 {
		m.Caveats = append(m.Caveats, fmt.Sprintf(
			"%d subagent records are not included. Subagent transcripts live in their own files "+
				"under the session directory and are not part of the main conversation.",
			t.OmittedSubagent))
	}
	if t.OmittedNoOriginalRecord > 0 {
		m.Caveats = append(m.Caveats, fmt.Sprintf(
			"%d events carried no original harness record and could not be replayed. The fork "+
				"continues without them, so its history is not identical to the original's.",
			t.OmittedNoOriginalRecord))
	}
	if !t.Complete {
		m.Caveats = append(m.Caveats,
			"This is one page of a longer conversation. Fetch the remaining pages with "+
				"transcript.next_cursor and append them to the same file before starting the fork.")
	}

	if m.Resumable {
		target := m.ProjectDir
		if target == "" {
			target = "<your project directory under ~/.claude/projects>"
		}
		m.Command = "claude --resume " + fork
		m.Steps = []string{
			"Create " + target + " if it does not already exist.",
			"Write every element of transcript.lines to " + target + "/" + m.Filename +
				", one JSON object per line, in the order given.",
			"If transcript.complete is false, fetch the remaining pages with " +
				"transcript.next_cursor and append their lines to the same file first.",
			"From your own project directory, run: " + m.Command,
		}
	} else {
		m.Steps = []string{
			"Keep transcript.lines as the record of the conversation. This server does not know " +
				"how to load them back into " + sess.Source + ".",
		}
	}
	return m
}

// transcriptScan is the result of turning a page of stored events into the
// harness records a fork is written from.
type transcriptScan struct {
	lines    []json.RawMessage
	bytes    int
	consumed int
	noRecord int
	subagent int
	// lastSeq is the sequence number of the last event this scan consumed, and
	// therefore where the next page starts. It is tracked over every event, not
	// only the ones that produced a line, so that a run of records with nothing
	// to replay still advances the position instead of paging over them forever.
	lastSeq     *int64
	truncatedBy string
}

func (s *transcriptScan) consume(seq int64) {
	v := seq
	s.lastSeq = &v
	s.consumed++
}

// scanTranscript extracts the original harness records from a page of events,
// stopping at the byte budget.
//
// Two events are deliberately left out. A subagent record belongs to a separate
// file under the session directory, and folding it into the main transcript
// would produce a file the harness has never seen. An event with no original
// record cannot be replayed at all: this platform's canonical event is derived
// from the harness's record and cannot stand in for it.
func scanTranscript(events []StoredEvent, maxBytes int) transcriptScan {
	var s transcriptScan
	for _, e := range events {
		if e.AgentID != "" {
			s.subagent++
			s.consume(e.Seq)
			continue
		}
		raw := originalRecord(e.Body)
		if len(raw) == 0 {
			s.noRecord++
			s.consume(e.Seq)
			continue
		}
		// The first line is always taken, whatever it costs. A budget applied
		// without that exception turns one oversized record into a page that
		// makes no progress, and the caller retries it forever.
		if len(s.lines) > 0 && s.bytes+len(raw) > maxBytes {
			s.truncatedBy = "bytes"
			return s
		}
		s.lines = append(s.lines, raw)
		s.bytes += len(raw)
		s.consume(e.Seq)
	}
	return s
}

// originalRecord pulls the harness's own record out of a stored event body, or
// reports that there is none.
//
// The result is compacted because the destination is a file with one record per
// line: a body that was stored pretty-printed would otherwise arrive as a JSON
// value spanning several lines and corrupt every record after it.
func originalRecord(body json.RawMessage) json.RawMessage {
	if len(body) == 0 {
		return nil
	}
	var envelope struct {
		Raw json.RawMessage `json:"raw"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil
	}
	raw := bytes.TrimSpace(envelope.Raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil
	}
	return json.RawMessage(buf.Bytes())
}

// transcriptFormat names what the lines are, so a client does not have to infer
// it from the shape of the first record.
func transcriptFormat(source string) string {
	if source == sourceClaudeCode {
		return "claude_code_jsonl"
	}
	return source + "_records"
}

// projectSlug derives the harness's per-project directory name from an absolute
// path.
//
// The rule is the harness's, read off directories it has already created rather
// than documented anywhere: every character outside the unreserved set becomes
// a hyphen, which is why a path segment beginning with a dot contributes two of
// them. It is offered as a convenience and the manifest says so, because the
// authority on a machine's layout is the client running on it.
func projectSlug(path string) string {
	var b strings.Builder
	b.Grow(len(path))
	for _, r := range path {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// newSessionID mints the identifier the fork takes locally. It is a version 4
// UUID because that is the shape the harness expects of a session file name,
// and it is random because the fork must not collide with anything already in
// the recipient's project directory.
func newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A system with no entropy cannot mint an identifier that is safe to
		// use, and continuing with a predictable one would put a colleague's
		// conversation in a file somebody else can name in advance.
		panic("api: system entropy unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
