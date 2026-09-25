package store

// The catalog's properties that need no database: the alias oracle is one
// indexed SELECT and both constructors wire it, a typed command the shape
// rule read is a row only when the catalog confirms it while an envelope
// name never asks, a publish of a recorded commit issues one statement and
// commits nothing, the statements of a publish and their arguments, the
// body checks that turn a CHECK into a field error, and the reconciler run
// statements with the line they write. What only Postgres can answer (the
// upsert's ON CONFLICT arms, the alias replacement across repositories, the
// UNIQUE on a retried run) is in skill_catalog_integration_test.go.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestConfirmSkillAliasIsOneIndexedSelect(t *testing.T) {
	db := &fakeDB{stubs: []*stub{{match: "FROM skill_catalog_aliases", rows: [][]any{{true}}}}}
	s, _ := skillStore(db)
	ok, err := s.ConfirmSkillAlias(context.Background(), db, "engg", "git")
	if err != nil || !ok {
		t.Fatalf("ConfirmSkillAlias = %v, %v", ok, err)
	}
	if len(db.calls) != 1 || db.calls[0].kind != "queryrow" || db.calls[0].sql != confirmAliasSQL {
		t.Fatalf("calls = %+v, want one QueryRow of the alias statement", db.calls)
	}
	if args := db.calls[0].args; len(args) != 2 || args[0] != "engg" || args[1] != "git" {
		t.Errorf("args = %v, want (plugin, skill)", args)
	}
	for _, want := range []string{"alias_plugin = $1", "alias_skill = $2", "EXISTS"} {
		if !strings.Contains(confirmAliasSQL, want) {
			t.Errorf("the statement lacks %q", want)
		}
	}
	// A false is a false, not an error, and a failed read is an error.
	db = &fakeDB{stubs: []*stub{{match: "FROM skill_catalog_aliases", rows: [][]any{{false}}}}}
	s, _ = skillStore(db)
	if ok, err := s.ConfirmSkillAlias(context.Background(), db, "", "git"); err != nil || ok {
		t.Errorf("an unknown alias = %v, %v", ok, err)
	}
	db = &fakeDB{stubs: []*stub{{match: "FROM skill_catalog_aliases", err: errors.New("connection refused")}}}
	s, _ = skillStore(db)
	if _, err := s.ConfirmSkillAlias(context.Background(), db, "", "git"); err == nil {
		t.Error("a failed read was answered as a verdict")
	}
}

// Both constructors leave the oracle wired to ConfirmSkillAlias (design 4a,
// CA-1): calling the field issues the alias read.
func TestBothConstructorsWireTheAliasOracle(t *testing.T) {
	if New(nil, nil).AliasOracle == nil {
		t.Error("New leaves AliasOracle nil")
	}
	db := &fakeDB{stubs: []*stub{{match: "FROM skill_catalog_aliases", rows: [][]any{{true}}}}}
	s := NewWithDB(db, nil)
	if s.AliasOracle == nil {
		t.Fatal("NewWithDB leaves AliasOracle nil")
	}
	if ok, err := s.AliasOracle(context.Background(), db, "engg", "git"); err != nil || !ok {
		t.Errorf("the wired oracle = %v, %v", ok, err)
	}
	if db.count("FROM skill_catalog_aliases") != 1 {
		t.Error("the wired oracle did not read the aliases")
	}
}

// The oracle reads the Queryer it is handed and never the store's own
// connection source. At ingest that Queryer IS the events transaction, so
// a read on s.db would take a second pool connection while the first is
// still held; under a burst every batch would hold two and the pool would
// wedge against itself (adversarial iteration 5, finding 1).
func TestConfirmSkillAliasReadsTheGivenQueryerNotThePool(t *testing.T) {
	pool := &fakeDB{stubs: []*stub{{match: "FROM skill_catalog_aliases", rows: [][]any{{true}}}}}
	txq := &fakeDB{stubs: []*stub{{match: "FROM skill_catalog_aliases", rows: [][]any{{true}}}}}
	s, _ := skillStore(pool)
	if ok, err := s.ConfirmSkillAlias(context.Background(), txq, "engg", "git"); err != nil || !ok {
		t.Fatalf("ConfirmSkillAlias = %v, %v", ok, err)
	}
	if got := txq.count("FROM skill_catalog_aliases"); got != 1 {
		t.Errorf("the given Queryer saw %d alias reads, want 1", got)
	}
	if got := pool.count("FROM skill_catalog_aliases"); got != 0 {
		t.Errorf("the pool saw %d alias reads, want none", got)
	}
}

// A typed command the shape rule read (the hook copy of /git, no envelope)
// is a row when the catalog holds the alias and nothing, counted
// unconfirmed, when it does not.
func TestShapeRuleNamesAreRowsOnlyWhenTheCatalogConfirmsThem(t *testing.T) {
	for _, c := range []struct {
		name      string
		confirmed bool
	}{{"confirmed", true}, {"unconfirmed", false}} {
		t.Run(c.name, func(t *testing.T) {
			db := &fakeDB{stubs: []*stub{{match: "FROM skill_catalog_aliases", rows: [][]any{{c.confirmed}}}, insertedRow()}}
			s, _ := skillStore(db)
			st, err := s.insertSkillInvocations(context.Background(), db, []Ingest{typedHook("evt-h", "sess-o", "p1", "/git ship it", 1)})
			if err != nil {
				t.Fatal(err)
			}
			ask := db.find(t, "FROM skill_catalog_aliases")
			if len(ask.args) != 2 || ask.args[0] != "" || ask.args[1] != "git" {
				t.Errorf("the oracle was asked %v, want ('', git)", ask.args)
			}
			if c.confirmed {
				if st.RowsUser != 1 || st.Unconfirmed != 0 || db.count("INSERT INTO skill_invocations") != 1 {
					t.Errorf("confirmed: stats %+v, %d merges", st, db.count("INSERT INTO skill_invocations"))
				}
				return
			}
			if st.RowsUser != 0 || st.Unconfirmed != 1 || db.count("INSERT INTO skill_invocations") != 0 {
				t.Errorf("unconfirmed: stats %+v, %d merges", st, db.count("INSERT INTO skill_invocations"))
			}
		})
	}
}

// A name from the transcript's own <command-name> envelope is the harness's
// record and never consults the catalog, under the wired oracle.
func TestEnvelopeNamesNeverReadTheCatalog(t *testing.T) {
	db := &fakeDB{stubs: []*stub{insertedRow()}}
	s, _ := skillStore(db)
	st, err := s.insertSkillInvocations(context.Background(), db, []Ingest{typedTranscript("evt-t", "sess-e", "p1", "engg:git", "", 1)})
	if err != nil {
		t.Fatal(err)
	}
	if st.RowsUser != 1 || st.Unconfirmed != 0 {
		t.Errorf("stats = %+v", st)
	}
	if db.count("skill_catalog_aliases") != 0 {
		t.Error("an envelope name reached the catalog")
	}
}

// ---------------------------------------------------------------- publishes

const catalogTokenID = "33333333-3333-4333-8333-333333333333"

func testCatalog() CatalogBody {
	skip := "skip_skills"
	return CatalogBody{Schema: 1, GeneratedAt: time.Date(2026, 9, 21, 6, 0, 0, 0, time.UTC), SourceRepo: "example-skills", Commit: "abc123", Skills: []CatalogSkill{
		{Slug: "git", Dir: "git", Name: "git", Plugin: "engg", Path: "plugins/engg/skills/git/SKILL.md", Sha256Tree: "h1", Installable: true, Mirrored: true,
			AuthoredBy: AuthoredByUnknown, Aliases: []string{"engg:git", "git"}},
		{Slug: "customer-call-analyzer", Dir: "customer-call-analyzer", Name: "call-analyzer", Plugin: "gtm", Path: "plugins/gtm/skills/customer-call-analyzer/SKILL.md",
			Sha256Tree: "h2", SkipReason: &skip, AuthoredBy: AuthoredByUnknown, Aliases: []string{"gtm:customer-call-analyzer", "customer-call-analyzer", "gtm:call-analyzer", "call-analyzer"}},
	}}
}

// measured stubs the shrink measurement: what the repository holds present
// and how many of those the body would retire (design 4d as amended).
func measured(present, retiring int) *stub {
	return &stub{match: measureMatch, rows: [][]any{{present, retiring}}}
}

// measureMatch is the fragment only the shrink measurement carries; the
// stale count also reads count(*) FROM skill_catalog_entries.
const measureMatch = "count(*) FILTER (WHERE NOT EXISTS"

// A commit already recorded: the publish lock and one statement, nothing
// committed, duplicate reported (design 4d).
func TestPublishSkillCatalogRecordedCommitTouchesNothing(t *testing.T) {
	db := &fakeDB{}
	s, _ := skillStore(db)
	res, err := s.PublishSkillCatalog(context.Background(), catalogTokenID, "example-skills", testCatalog())
	if err != nil || !res.Duplicate || res.Skills != 0 || res.Aliases != 0 {
		t.Fatalf("result = %+v, %v, want duplicate", res, err)
	}
	// The lock is taken before the read-then-write alias rule, so two
	// repositories cannot publish one alias at the same moment.
	if len(db.calls) != 2 || !strings.Contains(db.calls[0].sql, "pg_advisory_xact_lock") ||
		db.calls[0].args[0] != catalogPublishLockKey || !strings.Contains(db.calls[1].sql, "INSERT INTO skill_catalog_publishes") {
		t.Errorf("calls = %+v, want the publish lock and the publish insert alone", db.calls)
	}
	if db.committed != 0 || db.rolled != 1 {
		t.Errorf("committed %d, rolled back %d", db.committed, db.rolled)
	}
}

// A fresh commit: the publish row, the entries upserted from arrays, the
// absent entries flipped, the aliases replaced by the deduped union of the
// body's with the composite and bare forms, the stale count, one commit.
func TestPublishSkillCatalogStatementsAndArguments(t *testing.T) {
	db := &fakeDB{stubs: []*stub{
		{match: "INSERT INTO skill_catalog_publishes", rows: [][]any{{true}}},
		measured(2, 0),
		{match: "INSERT INTO skill_catalog_aliases", affected: 6},
		{match: "count(*) FROM skill_catalog_entries e", rows: [][]any{{int(1)}}},
	}}
	s, _ := skillStore(db)
	res, err := s.PublishSkillCatalog(context.Background(), catalogTokenID, "example-skills", testCatalog())
	if err != nil {
		t.Fatal(err)
	}
	if res.Duplicate || res.Skills != 2 || res.Aliases != 6 || res.StaleEntries != 1 {
		t.Errorf("result = %+v", res)
	}
	if db.committed != 1 || db.rolled != 0 {
		t.Errorf("committed %d, rolled back %d", db.committed, db.rolled)
	}
	var order []string
	for _, c := range db.calls {
		for _, m := range []string{"INSERT INTO skill_catalog_publishes", "INSERT INTO skill_catalog_entries", "SET present = false", "DELETE FROM skill_catalog_aliases", "FROM skill_catalog_aliases a", "INSERT INTO skill_catalog_aliases", "count(*) FROM skill_catalog_entries e"} {
			if strings.Contains(c.sql, m) {
				order = append(order, m)
			}
		}
		if !c.inTx {
			t.Errorf("a publish statement ran outside the transaction: %s", c.sql)
		}
	}
	want := []string{"INSERT INTO skill_catalog_publishes", "INSERT INTO skill_catalog_entries", "SET present = false", "DELETE FROM skill_catalog_aliases", "FROM skill_catalog_aliases a", "INSERT INTO skill_catalog_aliases", "count(*) FROM skill_catalog_entries e"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("statement order = %v, want %v", order, want)
	}
	pub := db.find(t, "INSERT INTO skill_catalog_publishes")
	if pub.args[0] != "example-skills" || pub.args[1] != "abc123" || argText(pub.args[3]) != catalogTokenID || pub.args[4] != 2 {
		t.Errorf("publish args = %v", pub.args)
	}
	ent := db.find(t, "INSERT INTO skill_catalog_entries")
	if ent.args[0] != "example-skills" || ent.args[1] != "abc123" || argText(ent.args[2]) != "engg gtm" || argText(ent.args[3]) != "git customer-call-analyzer" {
		t.Errorf("entry args = %v", ent.args)
	}
	if !strings.Contains(ent.sql, "present = true, last_seen_at = now()") || strings.Contains(ent.sql, "first_seen_at = ") {
		t.Error("the upsert does not keep first_seen_at while moving last_seen_at")
	}
	flip := db.find(t, "SET present = false")
	if flip.args[0] != "example-skills" || flip.args[1] != "abc123" || !strings.Contains(flip.sql, "commit <> $2") {
		t.Errorf("the present flip = %s %v", flip.sql, flip.args)
	}
	al := db.find(t, "INSERT INTO skill_catalog_aliases")
	if argText(al.args[1]) != "engg  gtm  gtm " || argText(al.args[2]) != "git git customer-call-analyzer customer-call-analyzer call-analyzer call-analyzer" {
		t.Errorf("alias args = %q %q", argText(al.args[1]), argText(al.args[2]))
	}
	if argText(al.args[4]) != "git git customer-call-analyzer customer-call-analyzer customer-call-analyzer customer-call-analyzer" {
		t.Errorf("alias targets = %q", argText(al.args[4]))
	}
	if !strings.Contains(al.sql, "ON CONFLICT DO NOTHING") {
		t.Error("the alias insert does not tolerate a repeat")
	}
	// No lineage in the body: the lineage check is not issued.
	if db.count("names no entry") != 0 && db.count("unnest($1::text[]) AS u(l)") != 0 {
		t.Error("a body without lineage_of ran the lineage check")
	}
}

// A lineage_of naming no entry rolls the publish back with the name.
func TestPublishSkillCatalogRefusesAnUnknownLineage(t *testing.T) {
	db := &fakeDB{stubs: []*stub{
		{match: "INSERT INTO skill_catalog_publishes", rows: [][]any{{true}}},
		measured(0, 0),
		{match: "AS u(l)", rows: [][]any{{"example-skills/engg:nowhere"}}},
	}}
	s, _ := skillStore(db)
	body := testCatalog()
	body.SourceRepo = "backend"
	nowhere := "example-skills/engg:nowhere"
	body.Skills[0].LineageOf = &nowhere
	_, err := s.PublishSkillCatalog(context.Background(), catalogTokenID, "backend", body)
	var le *LineageError
	if !errors.As(err, &le) || le.Lineage != nowhere {
		t.Fatalf("err = %v, want LineageError", err)
	}
	if db.committed != 0 || db.rolled != 1 || db.count("INSERT INTO skill_catalog_aliases") != 0 {
		t.Error("a refused lineage committed, or reached the aliases")
	}
}

// An alias another repository holds, with no lineage either way, is a
// conflict; with the lineage naming the holder it is allowed.
func TestPublishSkillCatalogAliasConflictNeedsLineage(t *testing.T) {
	holder := func() *stub {
		return &stub{match: "FROM skill_catalog_aliases a", rows: [][]any{{"engg", "git", "example-skills", "engg", "git", (*string)(nil)}}}
	}
	db := &fakeDB{stubs: []*stub{{match: "INSERT INTO skill_catalog_publishes", rows: [][]any{{true}}}, measured(0, 0), holder()}}
	s, _ := skillStore(db)
	body := testCatalog()
	body.SourceRepo = "backend"
	_, err := s.PublishSkillCatalog(context.Background(), catalogTokenID, "backend", body)
	var ce *AliasConflictError
	if !errors.As(err, &ce) || ce.Alias != "engg:git" || ce.HeldBy != "example-skills" {
		t.Fatalf("err = %v, want AliasConflictError on engg:git", err)
	}
	if db.committed != 0 || db.rolled != 1 {
		t.Error("a refused alias committed")
	}

	db = &fakeDB{stubs: []*stub{
		{match: "INSERT INTO skill_catalog_publishes", rows: [][]any{{true}}},
		measured(0, 0),
		holder(),
		{match: "count(*) FROM skill_catalog_entries e", rows: [][]any{{int(0)}}},
	}}
	s, _ = skillStore(db)
	head := "example-skills/engg:git"
	body.Skills[0].LineageOf = &head
	if _, err := s.PublishSkillCatalog(context.Background(), catalogTokenID, "backend", body); err != nil {
		t.Fatalf("a lineage publish was refused: %v", err)
	}
	if db.committed != 1 {
		t.Error("the lineage publish did not commit")
	}
	lin := db.find(t, "AS u(l)")
	if argText(lin.args[0]) != head {
		t.Errorf("the lineage check asked %v", lin.args)
	}
}

// The body checks: every shape the CHECKs would refuse is a field error
// before any statement.
func TestPublishSkillCatalogRefusesBadShapes(t *testing.T) {
	long := strings.Repeat("x", 201)
	bad := "Bad Name"
	cases := []struct {
		name, field string
		mutate      func(*CatalogBody)
	}{
		{"path mismatch", "source_repo", func(b *CatalogBody) { b.SourceRepo = "other" }},
		{"no commit", "commit", func(b *CatalogBody) { b.Commit = "" }},
		{"no generated_at", "generated_at", func(b *CatalogBody) { b.GeneratedAt = time.Time{} }},
		{"dir off shape", "skills[0].dir", func(b *CatalogBody) { b.Skills[0].Dir = "Git" }},
		{"plugin off shape", "skills[0].plugin", func(b *CatalogBody) { b.Skills[0].Plugin = "En gg" }},
		{"long path", "skills[0].path", func(b *CatalogBody) { b.Skills[0].Path = long }},
		{"long skip reason", "skills[1].skip_reason", func(b *CatalogBody) { b.Skills[1].SkipReason = &long }},
		{"authored_by off enum", "skills[0].authored_by", func(b *CatalogBody) { b.Skills[0].AuthoredBy = "robot" }},
		{"author_evidence off enum", "skills[0].author_evidence", func(b *CatalogBody) { b.Skills[0].AuthorEvidence = "guess" }},
		{"lineage off shape", "skills[0].lineage_of", func(b *CatalogBody) { b.Skills[0].LineageOf = &bad }},
		{"alias off shape", "skills[0].aliases", func(b *CatalogBody) { b.Skills[0].Aliases = []string{"engg:Git"} }},
		{"repeated entry", "skills[1].dir", func(b *CatalogBody) { b.Skills[1] = b.Skills[0] }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := &fakeDB{}
			s, _ := skillStore(db)
			body := testCatalog()
			c.mutate(&body)
			_, err := s.PublishSkillCatalog(context.Background(), catalogTokenID, "example-skills", body)
			var fe *CatalogFieldError
			if !errors.As(err, &fe) || fe.Field != c.field {
				t.Fatalf("err = %v, want a field error on %s", err, c.field)
			}
			if len(db.calls) != 0 {
				t.Error("a refused body reached a statement")
			}
		})
	}
	// The lead's devin-builtin body (contracts section 9): plugin '',
	// authored_by vendor, no lineage, is accepted by the checks.
	db := &fakeDB{stubs: []*stub{{match: "INSERT INTO skill_catalog_publishes", rows: [][]any{{true}}}, measured(0, 0), {match: "count(*) FROM skill_catalog_entries e", rows: [][]any{{int(0)}}}}}
	s, _ := skillStore(db)
	builtin := CatalogBody{Schema: 1, GeneratedAt: time.Now(), SourceRepo: "devin-builtin", Commit: "hand-1", Skills: []CatalogSkill{
		{Slug: "ask-devin", Dir: "ask-devin", Name: "ask-devin", Path: "", AuthoredBy: AuthoredByVendor, Aliases: []string{"ask-devin"}},
	}}
	if _, err := s.PublishSkillCatalog(context.Background(), "", "devin-builtin", builtin); err != nil {
		t.Errorf("the devin-builtin body was refused: %v", err)
	}
	al := db.find(t, "INSERT INTO skill_catalog_aliases")
	if argText(al.args[1]) != "" || argText(al.args[2]) != "ask-devin" {
		t.Errorf("a bare entry's aliases = %q %q, want the bare form once", argText(al.args[1]), argText(al.args[2]))
	}
}

// ---------------------------------------------------------------- reconciler runs

func testRun() ReconcilerRun {
	return ReconcilerRun{AgentPlatform: PlatformDevin, IdempotencyKey: "2026-09-21", WindowStart: time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
		WindowEnd: time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC), SessionsScanned: 5, SessionsWithEvents: 2, RowsPosted: 2}
}

func TestRecordReconcilerRunStatementsAndLine(t *testing.T) {
	db := &fakeDB{stubs: []*stub{
		{match: "INSERT INTO reconciler_runs", rows: [][]any{{int64(7)}}},
		{match: "count(*) FROM skill_invocations", rows: [][]any{{int64(2)}}},
	}}
	s, buf := skillStore(db)
	dup, conflict, err := s.RecordReconcilerRun(context.Background(), skillTestToken, testRun())
	if err != nil || dup || conflict {
		t.Fatalf("first run = %v %v %v", dup, conflict, err)
	}
	ins := db.find(t, "INSERT INTO reconciler_runs")
	if !strings.Contains(ins.sql, "ON CONFLICT (agent_platform, idempotency_key) DO NOTHING") || !strings.Contains(ins.sql, "RETURNING id") {
		t.Errorf("the insert is not keyed on the platform and the run key: %s", ins.sql)
	}
	if argText(ins.args[0]) != skillTestToken || ins.args[1] != PlatformDevin || ins.args[2] != "2026-09-21" || ins.args[5] != 5 || ins.args[6] != 2 || ins.args[7] != 2 || ins.args[8] != 0 {
		t.Errorf("insert args = %v", ins.args)
	}
	// rows_seen is counted by the run's platform, not by the caller's
	// token: a retry under a rotated token is the one case 0023 advertises,
	// and scoping the count to the caller made it read 0 and page the
	// on-call (adversarial finding 4).
	seen := db.find(t, "count(*) FROM skill_invocations")
	if seen.args[0] != PlatformDevin || !strings.Contains(seen.sql, "origin = 'reconciler'") || !strings.Contains(seen.sql, "WHERE agent_platform = $1") {
		t.Errorf("the rows_seen read = %s %v", seen.sql, seen.args)
	}
	if strings.Contains(seen.sql, "source_token_id") {
		t.Error("rows_seen is still scoped to the caller's token, so a rotated-token retry reads zero")
	}
	for _, a := range seen.args {
		if argText(a) == skillTestToken {
			t.Errorf("the rows_seen read still takes the caller's token: %v", seen.args)
		}
	}
	// It is counted over the run's OWN window by occurred_at, never by
	// arrival between the previous run and this one: received_at does not
	// move on the idempotent upsert, so a row the platform re-posted
	// arrived long before the window it belongs to was reconciled, and an
	// arrival window read that re-post as zero and paged (adversarial
	// iteration 5, finding 5).
	if !strings.Contains(seen.sql, "occurred_at >= $2") || !strings.Contains(seen.sql, "occurred_at <= $3") {
		t.Errorf("the rows_seen read does not close on the run's window: %s", seen.sql)
	}
	if strings.Contains(seen.sql, "received_at") || strings.Contains(seen.sql, "FROM reconciler_runs") {
		t.Errorf("the rows_seen read still counts arrivals against the previous run: %s", seen.sql)
	}
	if want := testRun(); seen.args[1] != want.WindowStart || seen.args[2] != want.WindowEnd {
		t.Errorf("the rows_seen window = %v..%v, want the run's own", seen.args[1], seen.args[2])
	}
	lines := linesWithMessage(skillLines(t, buf), lineReconcilerRun)
	if len(lines) != 1 {
		t.Fatalf("%d run lines, want 1", len(lines))
	}
	l := lines[0]
	for k, v := range map[string]any{"level": "INFO", "platform": PlatformDevin, "source_token_id": skillTestToken, "idempotency_key": "2026-09-21", "mismatch": false, "rows_posted": float64(2), "rows_seen": float64(2), "duplicate": false, "soft_revoked": false} {
		if l[k] != v {
			t.Errorf("line %s = %v, want %v", k, l[k], v)
		}
	}

	// The same key with equal counts: no insert row, the stored row read
	// back, a duplicate, and the line says so.
	db = &fakeDB{stubs: []*stub{
		{match: "SELECT id, sessions_scanned", rows: [][]any{{int64(7), 5, 2, 2, 0}}},
		{match: "count(*) FROM skill_invocations", rows: [][]any{{int64(2)}}},
	}}
	s, buf = skillStore(db)
	dup, conflict, err = s.RecordReconcilerRun(context.Background(), skillTestToken, testRun())
	if err != nil || !dup || conflict {
		t.Fatalf("retried run = %v %v %v, want a duplicate", dup, conflict, err)
	}
	if l := linesWithMessage(skillLines(t, buf), lineReconcilerRun); len(l) != 1 || l[0]["duplicate"] != true {
		t.Errorf("duplicate lines = %v", l)
	}
	// A duplicate stored nothing, so it can never mismatch: the retry below
	// sees one row where the stored run posted two, and stays INFO.
	db = &fakeDB{stubs: []*stub{
		{match: "SELECT id, sessions_scanned", rows: [][]any{{int64(7), 5, 2, 2, 0}}},
		{match: "count(*) FROM skill_invocations", rows: [][]any{{int64(1)}}},
	}}
	s, buf = skillStore(db)
	if dup, _, err := s.RecordReconcilerRun(context.Background(), skillTestToken, testRun()); err != nil || !dup {
		t.Fatalf("the retry = %v %v", dup, err)
	}
	if l := linesWithMessage(skillLines(t, buf), lineReconcilerRun); len(l) != 1 || l[0]["mismatch"] != false || l[0]["level"] != "INFO" {
		t.Errorf("a duplicate whose window moved = %v, want mismatch false at INFO", l)
	}

	// Differing counts: a conflict, no line.
	db = &fakeDB{stubs: []*stub{{match: "SELECT id, sessions_scanned", rows: [][]any{{int64(7), 5, 2, 3, 0}}}}}
	s, buf = skillStore(db)
	dup, conflict, err = s.RecordReconcilerRun(context.Background(), skillTestToken, testRun())
	if err != nil || dup || !conflict {
		t.Fatalf("a retry with other counts = %v %v %v, want a conflict", dup, conflict, err)
	}
	if n := len(linesWithMessage(skillLines(t, buf), lineReconcilerRun)); n != 0 {
		t.Errorf("a conflict wrote %d run lines", n)
	}

	// A mismatch between the rows the token posted and rows_posted is the
	// WARNING the skill_reconciler_mismatch metric counts.
	db = &fakeDB{stubs: []*stub{
		{match: "INSERT INTO reconciler_runs", rows: [][]any{{int64(8)}}},
		{match: "count(*) FROM skill_invocations", rows: [][]any{{int64(1)}}},
	}}
	s, buf = skillStore(db)
	if _, _, err := s.RecordReconcilerRun(context.Background(), skillTestToken, testRun()); err != nil {
		t.Fatal(err)
	}
	if l := linesWithMessage(skillLines(t, buf), lineReconcilerRun); len(l) != 1 || l[0]["level"] != "WARN" || l[0]["mismatch"] != true || l[0]["rows_seen"] != float64(1) {
		t.Errorf("mismatch lines = %v", l)
	}

	// The limit-0 post: the same line, no statement at all, and the keys a
	// filter needs to tell it from a recorded run (review-1 finding 3).
	db = &fakeDB{}
	s, buf = skillStore(db)
	s.LogSoftRevokedReconcilerRun(context.Background(), skillTestToken, testRun())
	if len(db.calls) != 0 {
		t.Errorf("a soft-revoked run ran %d statements", len(db.calls))
	}
	l = linesWithMessage(skillLines(t, buf), lineReconcilerRun)[0]
	for k, v := range map[string]any{"level": "INFO", "platform": PlatformDevin, "source_token_id": skillTestToken, "idempotency_key": "2026-09-21",
		"mismatch": false, "rows_posted": float64(2), "rows_seen": float64(0), "duplicate": true, "soft_revoked": true} {
		if l[k] != v {
			t.Errorf("soft-revoked line %s = %v, want %v", k, l[k], v)
		}
	}

	// A platform the reconciler CHECK refuses, or a token id that is not a
	// uuid, is an error before any statement.
	db = &fakeDB{}
	s, _ = skillStore(db)
	bad := testRun()
	bad.AgentPlatform = PlatformClaudeCode
	if _, _, err := s.RecordReconcilerRun(context.Background(), skillTestToken, bad); err == nil || len(db.calls) != 0 {
		t.Error("a claude_code run reached a statement")
	}
	if _, _, err := s.RecordReconcilerRun(context.Background(), "not-a-uuid", testRun()); err == nil || len(db.calls) != 0 {
		t.Error("a junk token id reached a statement")
	}
}

// The shrink gate (design 4d as amended 2026-09-22 from the adversarial
// finding): an empty body, or one retiring more than half of what the
// repository holds present, is refused with both counts and nothing
// written; half exactly is accepted; allow_shrink overrides both; and the
// measurement is taken inside the transaction, under the publish lock,
// before the first write.
func TestPublishSkillCatalogRefusesAShrink(t *testing.T) {
	cases := []struct {
		name              string
		skills            int
		present, retiring int
		allow             bool
		refused           bool
	}{
		{name: "an empty body against a full catalog", skills: 0, present: 40, retiring: 40, refused: true},
		{name: "an empty body against an empty one", skills: 0, present: 0, retiring: 0, refused: true},
		{name: "past half", skills: 2, present: 40, retiring: 21, refused: true},
		{name: "half exactly", skills: 2, present: 40, retiring: 20},
		{name: "a growing catalog", skills: 2, present: 1, retiring: 0},
		{name: "an empty body with the override", skills: 0, present: 40, retiring: 40, allow: true},
		{name: "past half with the override", skills: 2, present: 40, retiring: 40, allow: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db := &fakeDB{stubs: []*stub{
				{match: "INSERT INTO skill_catalog_publishes", rows: [][]any{{true}}},
				measured(c.present, c.retiring),
				{match: "INSERT INTO skill_catalog_aliases", affected: 6},
				{match: "count(*) FROM skill_catalog_entries e", rows: [][]any{{int(0)}}},
			}}
			s, _ := skillStore(db)
			body := testCatalog()
			body.AllowShrink = c.allow
			if c.skills == 0 {
				body.Skills = nil
			}
			res, err := s.PublishSkillCatalog(context.Background(), catalogTokenID, "example-skills", body)
			var se *CatalogShrinkError
			if !c.refused {
				if err != nil {
					t.Fatalf("result = %+v, %v, want an accepted publish", res, err)
				}
				if res.PresentBefore != c.present {
					t.Errorf("present_before = %d, want %d", res.PresentBefore, c.present)
				}
				if db.committed != 1 {
					t.Errorf("committed %d", db.committed)
				}
				return
			}
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want a CatalogShrinkError", err)
			}
			if se.Present != c.present || se.Incoming != c.skills {
				t.Errorf("refusal = %+v, want present %d incoming %d", se, c.present, c.skills)
			}
			// Nothing written: the entries, the flip and the aliases are
			// never reached, and the transaction rolls back.
			for _, m := range []string{"INSERT INTO skill_catalog_entries", "SET present = false", "DELETE FROM skill_catalog_aliases"} {
				if db.count(m) != 0 {
					t.Errorf("a refused publish issued %q", m)
				}
			}
			if db.committed != 0 || db.rolled != 1 {
				t.Errorf("committed %d, rolled back %d", db.committed, db.rolled)
			}
			// The measurement is under the lock and after the publish row,
			// so the numbers it reports are the ones the write would act on.
			var order []string
			for _, call := range db.calls {
				switch {
				case strings.Contains(call.sql, "pg_advisory_xact_lock"):
					order = append(order, "lock")
				case strings.Contains(call.sql, "INSERT INTO skill_catalog_publishes"):
					order = append(order, "publishes")
				case strings.Contains(call.sql, measureMatch):
					order = append(order, "measure")
					if !call.inTx {
						t.Error("the measurement ran outside the transaction")
					}
				}
			}
			if strings.Join(order, ",") != "lock,publishes,measure" {
				t.Errorf("statement order = %v", order)
			}
		})
	}
}

// The repair path: a stored publish row recording no entries is not a
// publish worth keeping, so the same (source_repo, commit) is let through
// and overwrites it, which is what makes re-running the last good CI job
// restore a wiped catalog (adversarial finding 1).
func TestPublishSkillCatalogRepairsAZeroSkillRow(t *testing.T) {
	db := &fakeDB{stubs: []*stub{
		{match: "INSERT INTO skill_catalog_publishes", rows: [][]any{{true}}},
		measured(0, 0),
		{match: "INSERT INTO skill_catalog_aliases", affected: 6},
		{match: "count(*) FROM skill_catalog_entries e", rows: [][]any{{int(0)}}},
	}}
	s, _ := skillStore(db)
	res, err := s.PublishSkillCatalog(context.Background(), catalogTokenID, "example-skills", testCatalog())
	if err != nil || res.Duplicate || res.Skills != 2 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	pub := db.find(t, "INSERT INTO skill_catalog_publishes")
	for _, want := range []string{"ON CONFLICT (source_repo, commit) DO UPDATE SET", "WHERE p.skills = 0"} {
		if !strings.Contains(pub.sql, want) {
			t.Errorf("the publish row does not carry %q:\n%s", want, pub.sql)
		}
	}
	if strings.Contains(pub.sql, "DO NOTHING") {
		t.Error("the publish row still refuses every repeat of a commit, so a wiped catalog cannot be repaired by re-running its CI job")
	}
}
