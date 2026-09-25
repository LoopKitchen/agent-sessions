package store

import (
	"strings"
	"testing"
)

// The three derive migrations are columns and empty tables only. No index
// build under the migration lock (every events index is the runner's, built
// CONCURRENTLY), nothing dropped, no read of events or messages, and a
// stated rollback floor per file, which is what lets an operator roll a
// revision back without reading the SQL first.
func TestDeriveMigrationsAreAdditiveAndBounded(t *testing.T) {
	for _, name := range []string{"0017_event_keys.sql", "0018_turns.sql", "0019_derive_jobs.sql"} {
		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		sql := strings.ToUpper(string(body))
		for _, forbidden := range []string{"CREATE INDEX", "CREATE UNIQUE INDEX", "DROP COLUMN", "DROP TABLE", "FROM EVENTS", "FROM MESSAGES", "UPDATE EVENTS", "UPDATE MESSAGES"} {
			if strings.Contains(sql, forbidden) {
				t.Errorf("%s contains %s", name, forbidden)
			}
		}
		if !strings.Contains(string(body), "Rollback floor") {
			t.Errorf("%s does not state its rollback floor", name)
		}
	}

	// 0017 adds exactly the seven columns the contract names, each a bare
	// nullable TEXT with no default, so every statement is a catalog change
	// and not a rewrite. Judged on the statements, not the prose around them.
	body, _ := migrations.ReadFile("migrations/0017_event_keys.sql")
	statements := 0
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ALTER TABLE") {
			continue
		}
		statements++
		fields := strings.Fields(strings.TrimSuffix(line, ";"))
		// ALTER TABLE events ADD COLUMN IF NOT EXISTS <name> TEXT
		if len(fields) != 10 || fields[2] != "events" || fields[8] == "" || fields[9] != "TEXT" {
			t.Errorf("0017 statement is not a bare nullable TEXT column: %q", line)
		}
	}
	if statements != 7 {
		t.Errorf("0017 has %d ALTER TABLE statements, want 7", statements)
	}
	for _, col := range []string{"prompt_id", "record_uuid", "parent_record_uuid", "request_id", "message_id", "tool_use_id", "superseded_by"} {
		want := "ALTER TABLE events ADD COLUMN IF NOT EXISTS " + col
		if !strings.Contains(string(body), want) {
			t.Errorf("0017 lacks %q", want)
		}
	}

	// 0018 carries the two invariants the fold relies on: the turn key is
	// unique per thread, and turn_events can only name one of four roles.
	body, _ = migrations.ReadFile("migrations/0018_turns.sql")
	for _, want := range []string{
		"PRIMARY KEY (session_id, thread, turn_index)",
		"UNIQUE (session_id, thread, turn_key)",
		"CHECK (role IN ('prompt', 'final', 'work', 'superseded'))",
		"REFERENCES sessions(session_id) ON DELETE CASCADE",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("0018 lacks %q", want)
		}
	}
	for _, outcome := range []string{"answered", "interrupted", "no_answer_captured", "no_work", "in_progress"} {
		if !strings.Contains(string(body), "'"+outcome+"'") {
			t.Errorf("0018 outcome CHECK lacks %q", outcome)
		}
	}

	// 0019 is the ledger the runner resumes from: keyed by (version, step).
	body, _ = migrations.ReadFile("migrations/0019_derive_jobs.sql")
	for _, want := range []string{"PRIMARY KEY (version, step)", "cursor", "processed", "attempts", "last_error", "finished_at"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("0019 lacks %q", want)
		}
	}
}
