package store

// Reads over the derived tables: the turns page the reader renders, the
// session facets the list badges need, and the repair list a laptop asks
// for. Every read here is a read of what the runner wrote (turns,
// turn_events, the lattice columns), never a fold of its own: the runner is
// the one fold (design.md D5), and a reader that re-derived a turn from
// events would be a second answer to "what did the person ask".

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	// turnPageSize is the reader's page: twenty-five turns, a turn never
	// split across pages. A page is bounded by turns rather than by events
	// because a turn is the unit a person reads, and a page boundary inside
	// one would separate a question from its answer.
	turnPageSize = 25
	// turnPageEventCap bounds the events one page loads, for the reason
	// every transcript limit exists: a single turn of a pathological session
	// holds thousands of tool rows, and a page that loads them all is a page
	// nobody can open. The page says when it hit the cap.
	turnPageEventCap = 5000
	// turnToolOutputCap is where a tool result's output is cut in SQL before
	// it leaves the database. The reader shows eight kilobytes and links to
	// the event for the rest, so shipping a megabyte to cut it to eight
	// kilobytes in Go is transfer for nothing.
	turnToolOutputCap = 64 << 10
)

// TurnRange selects a page of one session's turns.
type TurnRange struct {
	// Thread is "" for the main conversation. A canonical agent id narrows
	// the page to that subagent's own turns, top-level, for a reader who
	// followed a link into one thread of a long session.
	Thread string
	// After is the exclusive lower bound on turn_index; nil means from the
	// first turn. A pointer because turn indexes start at zero.
	After *int
	Limit int
	// Terms is unused by the store and carried for symmetry with the
	// timeline's options; highlighting is the reader's.
}

// TurnRow is one turns row with the events that belong to it, canonical
// copies only: the superseded role is filtered in SQL, which is what makes a
// dual-origin session read once per moment on this path.
type TurnRow struct {
	Thread        string
	Index         int
	Key           string
	Kind          string
	PromptEventID string
	FinalEventID  string
	Outcome       string
	Inherited     bool

	StartedAt       time.Time
	FirstActivityAt *time.Time
	LastActivityAt  time.Time
	AnsweredAt      *time.Time

	WallMS            int64
	ActiveMS          int64
	IdleMS            int64
	WaitingForHumanMS int64

	Origins      []string
	Merged       int
	Prompts      int
	ToolCalls    int
	Errors       int
	Subagents    int
	FilesChanged int

	TokensInput      int64
	TokensOutput     int64
	TokensCacheRead  int64
	TokensCacheWrite int64
	CostUSD          float64
	Model            string

	Events []TurnEventRow
}

// TurnEventRow is one event's membership in a turn: the stored row, the
// role the fold gave it, and the message kind the normalizer assigned.
type TurnEventRow struct {
	StoredEvent
	Role string
	Kind string
}

// TurnPage is one page of a session's turns.
type TurnPage struct {
	Turns []TurnRow
	// Agents are the subagent-thread turns that started inside this page's
	// span, for the reader to nest under the main turn in progress when each
	// started. Empty when the range names a thread.
	Agents  []TurnRow
	HasMore bool
	// NextAfter feeds straight back into TurnRange.After.
	NextAfter *int
	// EventsCapped reports that the page's events were cut at
	// turnPageEventCap and the later turns on it are incomplete.
	EventsCapped bool
}

const turnColumns = `thread, turn_index, turn_key, kind, coalesce(prompt_event_id, ''), coalesce(final_event_id, ''),
	outcome, inherited, started_at, first_activity_at, last_activity_at, answered_at,
	wall_ms, active_ms, idle_ms, waiting_for_human_ms, origins, merged, prompts,
	tool_calls, errors, subagents, files_changed,
	tokens_input, tokens_output, tokens_cache_read, tokens_cache_write, cost_usd::float8, model`

func scanTurn(rows Rows) (TurnRow, error) {
	var t TurnRow
	err := rows.Scan(&t.Thread, &t.Index, &t.Key, &t.Kind, &t.PromptEventID, &t.FinalEventID,
		&t.Outcome, &t.Inherited, &t.StartedAt, &t.FirstActivityAt, &t.LastActivityAt, &t.AnsweredAt,
		&t.WallMS, &t.ActiveMS, &t.IdleMS, &t.WaitingForHumanMS, &t.Origins, &t.Merged, &t.Prompts,
		&t.ToolCalls, &t.Errors, &t.Subagents, &t.FilesChanged,
		&t.TokensInput, &t.TokensOutput, &t.TokensCacheRead, &t.TokensCacheWrite, &t.CostUSD, &t.Model)
	return t, err
}

// GetTurns reads one page of a session's turns with their events. Audited
// like GetTimeline: the transcript is the sensitive read, and a page of
// turns is the transcript.
func (s *Store) GetTurns(ctx context.Context, v Viewer, sessionID string, r TurnRange) (TurnPage, error) {
	limit := r.Limit
	if limit <= 0 {
		limit = turnPageSize
	}
	if limit > 200 {
		limit = 200
	}

	tx, err := s.db.Begin(ctx)
	if err != nil {
		return TurnPage{}, fmt.Errorf("store: begin turns read: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	sess, err := authorizeSession(ctx, tx, v, sessionID)
	if err != nil {
		return TurnPage{}, err
	}
	if err := recordAccess(ctx, tx, []Access{{
		Viewer: v.Email, SessionID: sess.SessionID, Owner: sess.Email, Via: viaFor(v, sess.Email),
	}}); err != nil {
		return TurnPage{}, err
	}

	after := -1
	if r.After != nil {
		after = *r.After
	}
	page := TurnPage{}
	page.Turns, err = readTurnRows(ctx, tx, `
		SELECT `+turnColumns+`
		FROM turns
		WHERE session_id = $1 AND thread = $2 AND turn_index > $3
		ORDER BY turn_index
		LIMIT $4`, sessionID, r.Thread, after, limit+1)
	if err != nil {
		return TurnPage{}, err
	}
	if len(page.Turns) > limit {
		page.Turns = page.Turns[:limit]
		page.HasMore = true
		next := page.Turns[limit-1].Index
		page.NextAfter = &next
	}
	if len(page.Turns) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return TurnPage{}, fmt.Errorf("store: commit turns read: %w", err)
		}
		return page, nil
	}

	// Subagent turns that started inside this page's span. The fold hangs a
	// subagent on the main turn in progress when it started (derive.
	// attachTarget), which the reader reproduces by time; a page that ends
	// before the session does bounds the span by the first turn beyond it.
	if r.Thread == "" {
		var until any
		if page.HasMore {
			var next time.Time
			if err := tx.QueryRow(ctx, `
				SELECT started_at FROM turns
				WHERE session_id = $1 AND thread = '' AND turn_index = $2`,
				sessionID, *page.NextAfter+1).Scan(&next); err != nil && !noRows(err) {
				return TurnPage{}, fmt.Errorf("store: read the next turn's start: %w", err)
			} else if err == nil {
				until = next
			}
		}
		page.Agents, err = readTurnRows(ctx, tx, `
			SELECT `+turnColumns+`
			FROM turns
			WHERE session_id = $1 AND thread <> ''
			  AND started_at >= $2
			  AND ($3::timestamptz IS NULL OR started_at < $3)
			ORDER BY started_at, thread, turn_index`, sessionID, page.Turns[0].StartedAt, until)
		if err != nil {
			return TurnPage{}, err
		}
	}

	// The events of every turn on the page in one read, canonical copies
	// only, time-ordered within the turn. raw is dropped in SQL: the reader
	// renders the derived fields, and the record of truth is the event
	// page's to show. Tool output is cut at the transfer cap for the same
	// reason.
	var threads []string
	var indexes []int32
	byKey := map[string]*TurnRow{}
	for i := range page.Turns {
		t := &page.Turns[i]
		threads = append(threads, t.Thread)
		indexes = append(indexes, int32(t.Index))
		byKey[t.Thread+"\x1f"+fmt.Sprint(t.Index)] = t
	}
	for i := range page.Agents {
		t := &page.Agents[i]
		threads = append(threads, t.Thread)
		indexes = append(indexes, int32(t.Index))
		byKey[t.Thread+"\x1f"+fmt.Sprint(t.Index)] = t
	}
	rows, err := tx.Query(ctx, `
		SELECT te.thread, te.turn_index, te.role, coalesce(m.kind, ''),
		       e.id, e.session_id, e.email, e.seq, e.type, e.origin, e.occurred_at, e.ingested_at,
		       e.agent_id, e.workflow_id, e.model, e.tool_name,
		       CASE WHEN length(e.body->'tool'->>'output') > $4
		            THEN jsonb_set(e.body - 'raw', '{tool,output}', to_jsonb(left(e.body->'tool'->>'output', $4)))
		            ELSE e.body - 'raw' END
		FROM turn_events te
		JOIN unnest($2::text[], $3::int[]) AS u(thread, turn_index)
		  ON u.thread = te.thread AND u.turn_index = te.turn_index
		JOIN events e ON e.id = te.event_id
		LEFT JOIN messages m ON m.event_id = e.id
		WHERE te.session_id = $1 AND te.role <> 'superseded'
		ORDER BY te.thread, te.turn_index, e.occurred_at, coalesce(e.agent_id, ''), e.seq
		LIMIT $5`, sessionID, threads, indexes, turnToolOutputCap, turnPageEventCap+1)
	if err != nil {
		return TurnPage{}, fmt.Errorf("store: read turn events: %w", err)
	}
	n := 0
	for rows.Next() {
		var (
			te                           TurnEventRow
			thread                       string
			index                        int32
			agent, workflow, model, tool *string
			body                         []byte
		)
		if err := rows.Scan(&thread, &index, &te.Role, &te.Kind,
			&te.ID, &te.SessionID, &te.Email, &te.Seq, &te.Type, &te.Origin, &te.OccurredAt, &te.IngestedAt,
			&agent, &workflow, &model, &tool, &body); err != nil {
			rows.Close()
			return TurnPage{}, fmt.Errorf("store: scan turn event: %w", err)
		}
		n++
		if n > turnPageEventCap {
			page.EventsCapped = true
			break
		}
		te.AgentID = deref(agent)
		te.WorkflowID = deref(workflow)
		te.Model = deref(model)
		te.ToolName = deref(tool)
		te.Body = append(json.RawMessage(nil), body...)
		if t := byKey[thread+"\x1f"+fmt.Sprint(index)]; t != nil {
			t.Events = append(t.Events, te)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return TurnPage{}, fmt.Errorf("store: read turn events: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TurnPage{}, fmt.Errorf("store: commit turns read: %w", err)
	}
	return page, nil
}

func readTurnRows(ctx context.Context, q Queryer, sql string, args ...any) ([]TurnRow, error) {
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: read turns: %w", err)
	}
	defer rows.Close()
	var out []TurnRow
	for rows.Next() {
		t, err := scanTurn(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan turn: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read turns: %w", err)
	}
	return out, nil
}

// SessionFacet is the lattice, provenance and lineage state of one session:
// the columns 0014 and 0015 added, which the shared session projection does
// not carry. Read for a page of ids at once, never per row.
type SessionFacet struct {
	SessionID       string
	Type            string
	EmptyKind       string
	HeadState       string
	LineageSource   string
	ParentSessionID string
	TitleSource     string
	HumanTurns      int
	HarnessTitle    string
	ContentEvents   int
	Entrypoint      string
	// CaptureLossDrops is the device's dropped-event count around a session
	// whose head_state is capture_loss, so the banner can name the loss.
	CaptureLossDrops int64
	// FirstAnswer is the opening of the first main-thread turn's final
	// answer, the list row's snippet; "" when the first turn has none, and
	// never a later turn's (the first turn is the one the title came from).
	FirstAnswer string
}

// SessionFacets reads the facets of the given sessions in one query, scoped
// to what the viewer may read. Not audited: it is metadata for rows a list
// or a detail read has already authorised and, where it matters, audited.
func (s *Store) SessionFacets(ctx context.Context, v Viewer, sessionIDs []string) (map[string]SessionFacet, error) {
	out := map[string]SessionFacet{}
	if len(sessionIDs) == 0 {
		return out, nil
	}
	rows, err := s.db.Query(ctx, `
		SELECT s.session_id, s.session_type, coalesce(s.empty_kind, ''), s.head_state,
		       s.lineage_source, coalesce(s.parent_session_id, ''), s.title_source, s.human_turns,
		       coalesce(s.harness_title, ''), s.content_events, s.entrypoint,
		       CASE WHEN s.head_state = 'capture_loss' THEN
		         (SELECT coalesce(sum(h.drops), 0) FROM health_hourly h
		           WHERE h.email = s.email AND h.device_id IS NOT DISTINCT FROM s.device_id
		             AND h.hour >= date_trunc('hour', s.started_at - interval '5 minutes')
		             AND h.hour <= coalesce(s.ended_at, now()) + interval '5 minutes')
		       ELSE 0 END,
		       coalesce((SELECT left(e.body->>'text', 400) FROM turns t
		                   LEFT JOIN events e ON e.id = t.final_event_id
		                  WHERE t.session_id = s.session_id AND t.thread = ''
		                  ORDER BY t.turn_index LIMIT 1), '')
		FROM sessions s
		WHERE s.session_id = ANY($3::text[]) AND `+canRead("s", "$1::bool", "$2"),
		v.IsAdmin(), v.Email, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("store: session facets: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var f SessionFacet
		if err := rows.Scan(&f.SessionID, &f.Type, &f.EmptyKind, &f.HeadState,
			&f.LineageSource, &f.ParentSessionID, &f.TitleSource, &f.HumanTurns,
			&f.HarnessTitle, &f.ContentEvents, &f.Entrypoint, &f.CaptureLossDrops, &f.FirstAnswer); err != nil {
			return nil, fmt.Errorf("store: scan session facet: %w", err)
		}
		out[f.SessionID] = f
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read session facets: %w", err)
	}
	return out, nil
}

// RepairEntry is one session a laptop should re-walk.
type RepairEntry struct {
	SessionID string
	Source    string
	Cwd       string
	StartedAt time.Time
	// Reason is missing_answers (hook-only turns whose answer was never
	// captured and no transcript row can stand in) or codex_tokens (a Codex
	// session stored before the walker read token counts).
	Reason string
	// Turns counts the answerless turns for the missing_answers reason.
	Turns int
}

// RepairList names the sessions owned by a device that a re-walk of the
// transcript still on that laptop would complete, newest first, at most
// limit of them, as of now.
//
// A missing answer is a main-thread turn the fold could not close: hook
// origin only, outcome no_answer_captured (so a turn still in progress and
// an interrupted one are not on the list), in a session with no
// transcript-origin assistant row at all. A session that has any transcript
// answer has been walked already; what the walk did not recover, a second
// walk will not either. Codex sessions with content and no input tokens are
// the ones stored before the walker read token_count; the walk re-emits the
// usage under the same record identity and the ledger credits it once.
//
// Only a settled session is listed: one that ended, or one nothing has
// touched for a day (updated_at, which every fold of new events bumps; a
// two-day-old session still in use started long ago and is not settled). A
// transcript still being written is walked tomorrow, whole.
func (s *Store) RepairList(ctx context.Context, deviceID string, now time.Time, limit int) ([]RepairEntry, error) {
	if !isUUID(deviceID) {
		return nil, fmt.Errorf("store: repair list needs a device id")
	}
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := s.db.Query(ctx, `
		SELECT s.session_id, s.source, coalesce(s.cwd, ''), s.started_at,
		       CASE WHEN s.source = 'codex' AND s.tokens_input = 0 THEN 'codex_tokens' ELSE 'missing_answers' END,
		       (SELECT count(*) FROM turns t
		         WHERE t.session_id = s.session_id AND t.thread = ''
		           AND t.origins = '{hook}' AND t.outcome = 'no_answer_captured')::int
		FROM sessions s
		WHERE s.device_id = $1::uuid
		  AND s.session_type <> 'empty'
		  AND (s.ended OR s.updated_at < $3::timestamptz - interval '24 hours')
		  AND (
		    (s.source = 'codex' AND s.tokens_input = 0 AND s.content_events > 0)
		    OR (
		      EXISTS (SELECT 1 FROM turns t
		               WHERE t.session_id = s.session_id AND t.thread = ''
		                 AND t.origins = '{hook}' AND t.outcome = 'no_answer_captured')
		      AND NOT EXISTS (SELECT 1 FROM events e
		                       WHERE e.session_id = s.session_id
		                         AND e.origin = 'transcript' AND e.type = 'assistant_turn')
		    )
		  )
		ORDER BY s.started_at DESC
		LIMIT $2`, deviceID, limit, now)
	if err != nil {
		return nil, fmt.Errorf("store: repair list: %w", err)
	}
	defer rows.Close()
	var out []RepairEntry
	for rows.Next() {
		var e RepairEntry
		if err := rows.Scan(&e.SessionID, &e.Source, &e.Cwd, &e.StartedAt, &e.Reason, &e.Turns); err != nil {
			return nil, fmt.Errorf("store: scan repair entry: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read repair list: %w", err)
	}
	return out, nil
}

// ClaudeTranscriptPath is where Claude Code keeps the transcript of a
// session started in cwd: <home>/.claude/projects/<slug>/<session id>.jsonl,
// where the slug is the cwd with every character outside [A-Za-z0-9]
// replaced by '-'. Pinned against this machine's own project directory on
// 2026-09-11 (ls ~/.claude/projects against the cwd values of its sessions):
// /home/x/work/api/apps/ai_ingestion is -home-x-work-api-apps-ai-ingestion,
// /home/x/.claude/projects/-home-x-work-api/memory is
// -home-x--claude-projects--home-x-work-api-memory, so '/', '.' and '_'
// all become '-' and a segment that already starts with '-' yields '--'.
//
// The home directory is read off the cwd: a clean absolute path under a
// per-user home tree (the macOS Users directory or /home), /<tree>/<u>,
// belongs to that user. Anything else (a cwd of
// "/", a temp directory outside the home, a path with a "." or ".."
// segment) yields "", as does a session id that is not the harness's uuid,
// and the caller says so in the hint, because the client skips an entry
// with no path rather than guess. Both strings came from a laptop, and the
// laptop walks the directory of the path it gets back: a ".." in either
// would have sent that walk somewhere the session never was.
func ClaudeTranscriptPath(cwd, sessionID string) string {
	home := homeOf(cwd)
	if home == "" || !isUUID(sessionID) {
		return ""
	}
	return home + "/.claude/projects/" + ClaudeProjectSlug(cwd) + "/" + sessionID + ".jsonl"
}

// ClaudeProjectSlug is the harness's directory name for a cwd. The rule
// runs over the JavaScript string, one dash per UTF-16 unit, so a rune the
// slug drops is one dash and a rune above U+FFFF (a surrogate pair there)
// is two; a byte-wise rule made café into caf-- and named a directory that
// is not there.
func ClaudeProjectSlug(cwd string) string {
	var b strings.Builder
	for _, c := range cwd {
		switch {
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'):
			b.WriteRune(c)
		case c > 0xFFFF:
			b.WriteString("--")
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// homeOf returns the user's home directory a cwd lies under, or "". Only a
// clean absolute path qualifies: no empty, "." or ".." segment anywhere,
// because every segment becomes part of the slug and the user segment
// becomes a directory the client walks.
func homeOf(cwd string) string {
	if !strings.HasPrefix(cwd, "/") {
		return ""
	}
	segs := strings.Split(cwd[1:], "/")
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." {
			return ""
		}
	}
	if len(segs) < 2 || (segs[0] != "Users" && segs[0] != "home") {
		return ""
	}
	return "/" + segs[0] + "/" + segs[1]
}
