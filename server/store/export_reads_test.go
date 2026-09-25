package store

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// TestExportProjectionsAreShapedForBigQuery pins the SQL the export copies
// with, without a database: every table the run names has a projection,
// every projection carries viewer_emails and exported_at, the events
// projection drops the raw record, the day is read from a setting rather
// than spliced in, and the COPY uses the CSV form that leaves row_to_json
// output unescaped.
func TestExportProjectionsAreShapedForBigQuery(t *testing.T) {
	for _, table := range []string{"turns", "sessions", "messages", "events", "health_hourly"} {
		sql, err := exportPartitionSQL(table, testAliases)
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		for _, want := range []string{"COPY (", "row_to_json(r)", "AS viewer_emails", "now() AS exported_at",
			"current_setting('export.day', true)", "AT TIME ZONE 'UTC'", exportCopyFormat} {
			if !strings.Contains(sql, want) {
				t.Errorf("%s projection lacks %q", table, want)
			}
		}
		// The one ORDER BY allowed is the array_agg inside viewer_emails.
		if strings.Contains(strings.ReplaceAll(sql, "ORDER BY v)", ""), "ORDER BY") {
			t.Errorf("%s partition sorts; a sort over a day of bodies spills work_mem for nothing BigQuery needs", table)
		}
	}
	events, _ := exportPartitionSQL("events", testAliases)
	if !strings.Contains(events, "e.body - 'raw' AS body") {
		t.Error("the events projection exports the raw transcript line")
	}
	if !strings.Contains(events, "e.ingested_at >= b.lo AND e.ingested_at < b.hi") {
		t.Error("events are not partitioned by ingested_at")
	}
	for _, table := range []string{"turns", "messages"} {
		sql, _ := exportPartitionSQL(table, testAliases)
		if !strings.Contains(sql, "s.started_at AS session_started_at") || !strings.Contains(sql, "s.started_at >= b.lo AND s.started_at < b.hi") {
			t.Errorf("%s is not partitioned by the session's start day", table)
		}
	}
	sessions, _ := exportPartitionSQL("sessions", testAliases)
	for _, leaked := range []string{"mirror_request", "derive_dirty"} {
		if strings.Contains(sessions, leaked) {
			t.Errorf("sessions projection exports %s", leaked)
		}
	}
	if _, err := exportPartitionSQL("principals", testAliases); err == nil {
		t.Error("an unknown table got a projection")
	}
	for _, want := range []string{"'session' AS record", "'turn' AS record", "'event' AS record", "current_setting('export.sid', true)", "ORDER BY part, k1, k2, k3",
		// The bundle leaves out the harness's own text (review-1 M3); the
		// partition SQL does not.
		"e.type <> 'compaction'", "m.kind = 'system_reminder'"} {
		if !strings.Contains(exportBundleSQL(testAliases), want) {
			t.Errorf("bundle SQL lacks %q", want)
		}
	}
	if strings.Contains(events, "compaction") || strings.Contains(events, "system_reminder") {
		t.Error("the events partition excludes rows; only the bundle does")
	}
	if ExportStatementTimeout != 10*time.Minute {
		t.Errorf("export statement ceiling is %v, want the documented 600 s", ExportStatementTimeout)
	}
}

// testAliases is the DOMAIN_ALIASES the projection tests are built with:
// one pair, so the alias branch is present and pinned.
var testAliases = []DomainAlias{{A: "alpha.example", B: "beta.example"}}

// TestExportViewerEmailsFollowsTheConfiguredAliases pins the alias clause to
// the configuration and nothing else. With a pair declared, viewer_emails is
// the owner, its same-local-part address on the other domain of its pair,
// and principals rows with that local part on either domain of the pair,
// and no other domain is named; with a second pair, each owner is matched
// within its own pair only; with no pairs there is no CASE and no principals
// read at all, and the value is still an array. A domain outside the pairs
// never appears in the clause, because a same-local-part row there could be
// another person.
func TestExportViewerEmailsFollowsTheConfiguredAliases(t *testing.T) {
	v := exportViewerEmails("s.email", testAliases)
	for _, want := range []string{
		"split_part(s.email, '@', 2) = 'alpha.example' THEN split_part(s.email, '@', 1) || '@' || 'beta.example'",
		"split_part(s.email, '@', 2) = 'beta.example' THEN split_part(s.email, '@', 1) || '@' || 'alpha.example'",
		"FROM principals p",
		"split_part(p.email, '@', 1) = split_part(s.email, '@', 1)",
		"split_part(s.email, '@', 2) IN ('alpha.example', 'beta.example') AND split_part(p.email, '@', 2) IN ('alpha.example', 'beta.example')",
		"WHERE v IS NOT NULL",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("viewer_emails lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "gamma.example") {
		t.Error("viewer_emails names a domain that is not in any pair")
	}

	two := exportViewerEmails("s.email", append(testAliases, DomainAlias{A: "gamma.example", B: "delta.example"}))
	for _, want := range []string{
		"= 'gamma.example' THEN split_part(s.email, '@', 1) || '@' || 'delta.example'",
		"IN ('alpha.example', 'beta.example') AND split_part(p.email, '@', 2) IN ('alpha.example', 'beta.example')) OR (split_part(s.email, '@', 2) IN ('gamma.example', 'delta.example')",
	} {
		if !strings.Contains(two, want) {
			t.Errorf("two pairs: viewer_emails lacks %q:\n%s", want, two)
		}
	}

	none := exportViewerEmails("s.email", nil)
	if none != "ARRAY[s.email]" {
		t.Errorf("with no aliases viewer_emails = %q, want the owner alone as an array", none)
	}
	for _, table := range []string{"turns", "sessions", "messages", "events", "health_hourly"} {
		sql, err := exportPartitionSQL(table, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(sql, "CASE") || strings.Contains(sql, "FROM principals") {
			t.Errorf("%s: with no aliases the projection still carries the alias branch:\n%s", table, sql)
		}
		if !strings.Contains(sql, "AS viewer_emails") {
			t.Errorf("%s: no viewer_emails column without aliases", table)
		}
	}
	if strings.Contains(exportBundleSQL(nil), "FROM principals") {
		t.Error("the bundle reads principals with no aliases configured")
	}

	// The domains reach the SQL as literals, so a quote in one cannot end
	// the literal. Config refuses such a value before it gets here; this is
	// the second fence.
	quoted := exportViewerEmails("s.email", []DomainAlias{{A: "a'b.example", B: "c.example"}})
	if !strings.Contains(quoted, "'a''b.example'") {
		t.Errorf("a quote in a domain is not doubled:\n%s", quoted)
	}

	// SetDeployment is what hands the pairs to the copies, and it drops the
	// pairs that could not mean anything.
	s := NewWithDB(&fakeDB{}, nil)
	s.SetDeployment(Deployment{
		AllowedDomains: []string{" Alpha.Example ", "", "beta.example"},
		DomainAliases:  []DomainAlias{{A: " Alpha.Example", B: "BETA.example "}, {A: "same.example", B: "same.example"}, {A: "", B: "x.example"}},
	})
	if got := s.deployment.AllowedDomains; len(got) != 2 || got[0] != "alpha.example" || got[1] != "beta.example" {
		t.Errorf("allowed domains = %q", got)
	}
	if got := s.deployment.DomainAliases; len(got) != 1 || got[0] != testAliases[0] {
		t.Errorf("aliases = %+v, want the one usable pair, lower-cased and trimmed", got)
	}
}

// TestExportPlanRunsInsideOneBoundedReadOnlyTransaction (review-1 C1): the
// planning reads share one transaction that is read-only and carries the
// export ceiling, set before any of them runs; on a pool connection they
// would run under the pool's 30 s ceiling, which the first run's events-day
// read (a scan of the whole table without an index on ingested_at) does
// not fit in. And while health_hourly is absent the plan says so instead of
// querying it.
func TestExportPlanRunsInsideOneBoundedReadOnlyTransaction(t *testing.T) {
	since := time.Date(2026, 9, 12, 2, 0, 0, 0, time.UTC)
	run := func(healthExists bool) (*fakeDB, ExportPlanReads) {
		db := &fakeDB{stubs: []*stub{
			{match: "to_regclass('health_hourly')", rows: [][]any{{healthExists}}},
			{match: "FROM sessions WHERE updated_at > $1", rows: [][]any{{"s1", "a@example.org", since.Add(-time.Hour), since.Add(time.Hour)}}},
			{match: "FROM events WHERE ingested_at > $1", rows: [][]any{{"2026-09-12"}}},
			{match: "FROM health_hourly WHERE hour > $1", rows: [][]any{{"2026-09-11"}, {"2026-09-12"}}},
		}}
		s := NewWithDB(db, nil)
		reads, err := s.ExportPlan(context.Background(), ExportPlanRequest{
			SessionsSince: since, SessionsLimit: 2001, EventsSince: since, EventsLimit: 7, HealthSince: since.Add(-24 * time.Hour),
		})
		if err != nil {
			t.Fatalf("plan (health %v): %v", healthExists, err)
		}
		return db, reads
	}

	db, reads := run(true)
	if len(reads.Sessions) != 1 || reads.Sessions[0].SessionID != "s1" || len(reads.EventDays) != 1 || reads.EventDays[0] != "2026-09-12" ||
		!reads.HealthTableExists || len(reads.HealthDays) != 2 {
		t.Errorf("reads = %+v", reads)
	}
	if db.begun != 1 || db.committed != 1 {
		t.Errorf("begun %d committed %d, want one committed transaction:\n%s", db.begun, db.committed, db.summary())
	}
	// Order: the access mode first (so no read runs outside it; Postgres
	// would accept it after a query, this code does not), the ceiling
	// second, then every read, all inside the transaction.
	position := func(match string) int {
		for i, c := range db.calls {
			if strings.Contains(c.sql, match) {
				if !c.inTx {
					t.Errorf("%q ran outside the transaction:\n%s", match, db.summary())
				}
				return i
			}
		}
		t.Fatalf("no statement containing %q:\n%s", match, db.summary())
		return -1
	}
	readOnly := position("SET TRANSACTION READ ONLY")
	ceiling := position("SET LOCAL statement_timeout = '600000ms'")
	if readOnly != 0 || ceiling != 1 {
		t.Errorf("read-only at %d and the ceiling at %d, want the first two statements:\n%s", readOnly, ceiling, db.summary())
	}
	for _, read := range []string{"FROM sessions WHERE updated_at > $1", "FROM events WHERE ingested_at > $1", "to_regclass('health_hourly')", "FROM health_hourly WHERE hour > $1"} {
		if position(read) <= ceiling {
			t.Errorf("%q ran before the ceiling was set", read)
		}
	}
	// The limits are passed through, one more than the caps.
	if c := db.find(t, "FROM sessions WHERE updated_at > $1"); len(c.args) != 2 || c.args[1] != 2001 {
		t.Errorf("touched sessions args %v, want since and 2001", c.args)
	}
	if c := db.find(t, "FROM events WHERE ingested_at > $1"); len(c.args) != 2 || c.args[1] != 7 {
		t.Errorf("event days args %v, want since and 7", c.args)
	}
	// The events-day read is a range on the bare column, so an index on
	// ingested_at can serve it; the expression is on the output only.
	if c := db.find(t, "FROM events WHERE ingested_at > $1"); strings.Contains(c.sql, "WHERE to_char") || strings.Contains(c.sql, "WHERE (ingested_at AT TIME ZONE") {
		t.Errorf("the events-day predicate wraps the column in an expression:\n%s", c.sql)
	}

	db, reads = run(false)
	if reads.HealthTableExists || reads.HealthDays != nil {
		t.Errorf("health absent: %+v", reads)
	}
	if db.count("FROM health_hourly") != 0 {
		t.Errorf("health_hourly was queried while absent:\n%s", db.summary())
	}
	if db.begun != 1 || db.committed != 1 {
		t.Errorf("health absent: begun %d committed %d", db.begun, db.committed)
	}

	// A failing read ends the transaction without a commit.
	failing := &fakeDB{stubs: []*stub{{match: "FROM sessions WHERE updated_at > $1", err: errors.New("canceling statement due to statement timeout")}}}
	if _, err := NewWithDB(failing, nil).ExportPlan(context.Background(), ExportPlanRequest{SessionsLimit: 1, EventsLimit: 1}); err == nil || !strings.Contains(err.Error(), "statement timeout") {
		t.Errorf("err = %v, want the read's failure", err)
	}
	if failing.committed != 0 || failing.rolled != 1 {
		t.Errorf("failed plan: committed %d rolled %d", failing.committed, failing.rolled)
	}
}

// TestExportWatermarksMoveTogetherInOneTransaction: the three streams'
// positions are upserted inside one transaction, so a run's watermarks are
// never half recorded; a stream with no position (zero) writes no row.
func TestExportWatermarksMoveTogetherInOneTransaction(t *testing.T) {
	db := &fakeDB{}
	s := NewWithDB(db, nil)
	at := time.Date(2026, 9, 12, 3, 55, 0, 0, time.UTC)
	err := s.ExportAdvanceWatermarks(context.Background(), map[string]time.Time{
		ExportWatermarkSessions: at, ExportWatermarkEvents: at, ExportWatermarkHealth: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	if db.count("INSERT INTO export_watermarks") != 3 {
		t.Errorf("%d watermark statements, want 3:\n%s", db.count("INSERT INTO export_watermarks"), db.summary())
	}
	if db.begun != 1 || db.committed != 1 {
		t.Errorf("begun %d committed %d, want one transaction", db.begun, db.committed)
	}
	c := db.find(t, "INSERT INTO export_watermarks")
	if !c.inTx || !strings.Contains(c.sql, "ON CONFLICT (name) DO UPDATE SET watermark = EXCLUDED.watermark") {
		t.Errorf("watermark write is not an upsert inside the transaction: %+v", c)
	}

	// health_hourly has no position yet (the table does not exist): no row,
	// or the next run would read a year-1 watermark as the stream's age.
	db = &fakeDB{}
	if err := NewWithDB(db, nil).ExportAdvanceWatermarks(context.Background(), map[string]time.Time{
		ExportWatermarkSessions: at, ExportWatermarkEvents: at, ExportWatermarkHealth: {},
	}); err != nil {
		t.Fatal(err)
	}
	if db.count("INSERT INTO export_watermarks") != 2 {
		t.Errorf("%d watermark statements with one zero stream, want 2:\n%s", db.count("INSERT INTO export_watermarks"), db.summary())
	}
	for _, c := range db.calls {
		if strings.Contains(c.sql, "INSERT INTO export_watermarks") && c.args[0] == ExportWatermarkHealth {
			t.Error("a zero health watermark was written")
		}
	}
}

// TestExportCopyNeedsAPooledConnection: a DB that cannot pin a connection
// (the fake, and any future non-pool DB) is refused before any statement
// runs, rather than half-running the COPY through Exec.
func TestExportCopyNeedsAPooledConnection(t *testing.T) {
	db := &fakeDB{}
	s := NewWithDB(db, nil)
	_, err := s.ExportCopyPartition(context.Background(), "turns", "2026-09-10", io.Discard)
	if !errors.Is(err, errExportNeedsPool) {
		t.Errorf("err = %v, want errExportNeedsPool", err)
	}
	if len(db.calls) != 0 {
		t.Errorf("statements ran without a pool:\n%s", db.summary())
	}
	if _, err := s.ExportCopyPartition(context.Background(), "turns", "10/09/2026", io.Discard); err == nil || !strings.Contains(err.Error(), "export day") {
		t.Errorf("a malformed day was accepted: %v", err)
	}
	if _, err := s.ExportCopyBundle(context.Background(), " ", io.Discard); err == nil {
		t.Error("an empty session id was accepted")
	}
}

// TestExportMigrationIsTheContractsTable parses 0021: the watermark table
// and nothing else, catalog-only.
func TestExportMigrationIsTheContractsTable(t *testing.T) {
	body, err := migrations.ReadFile("migrations/0021_export_watermarks.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{"CREATE TABLE IF NOT EXISTS export_watermarks", "name       TEXT PRIMARY KEY", "watermark  TIMESTAMPTZ NOT NULL", "updated_at TIMESTAMPTZ NOT NULL DEFAULT now()", "Rollback floor"} {
		if !strings.Contains(text, want) {
			t.Errorf("0021 lacks %q", want)
		}
	}
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		upper := strings.ToUpper(trimmed)
		for _, forbidden := range []string{"CREATE INDEX", "DROP ", "UPDATE ", "FROM EVENTS", "FROM MESSAGES", "ALTER TABLE"} {
			if strings.Contains(upper, forbidden) {
				t.Errorf("0021 statement %q is not catalog-only for a new table", trimmed)
			}
		}
	}
}
