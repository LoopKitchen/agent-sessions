package store

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// skillMigrations are the skill-usage files: catalog-only, on tables of
// their own, and applied while ingest is live. The sibling test above pins
// the derive files, which forbid every CREATE INDEX because their tables
// are events and messages; these files build indexes on tables they create
// in the same transaction, on empty heaps, which the deploy README allows,
// so the assertion here is narrower: nothing in them may touch events,
// messages or sessions, and every index they build is on a table they make.
var skillMigrations = []string{"0022_skill_invocations.sql", "0023_skill_catalog.sql"}

// stripSQLComments drops every "--" comment. The G2 comments name events
// and REFERENCES events(id) in prose (why there is no foreign key), and a
// model that uppercases comments too would read three false hits
// (CODEBASE1-4), so the statements alone are judged.
func stripSQLComments(body string) string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if i := strings.Index(line, "--"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

var (
	createTableRe = regexp.MustCompile(`(?i)CREATE TABLE IF NOT EXISTS\s+([a-z_]+)`)
	createIndexRe = regexp.MustCompile(`(?i)CREATE (?:UNIQUE )?INDEX IF NOT EXISTS\s+[a-z_]+\s+ON\s+([a-z_]+)`)
)

// TestSkillMigrationsTouchNeitherEventsNorSessions is the 10.3 guard: a
// skill migration that read or referenced the largest table would take a
// lock on it inside the migration transaction at every boot that ships the
// file (gapfill-g3), and one that dropped anything would have no rollback
// floor to state.
func TestSkillMigrationsTouchNeitherEventsNorSessions(t *testing.T) {
	for _, name := range skillMigrations {
		raw, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(raw)
		if !strings.Contains(body, "Rollback floor") {
			t.Errorf("%s does not state its rollback floor", name)
		}
		sql := strings.ToUpper(stripSQLComments(body))
		for _, table := range []string{"EVENTS", "MESSAGES", "SESSIONS"} {
			for _, verb := range []string{"FROM ", "UPDATE ", "REFERENCES ", "ALTER TABLE "} {
				if strings.Contains(sql, verb+table) {
					t.Errorf("%s contains %s%s", name, verb, table)
				}
			}
		}
		if strings.Contains(sql, "DROP") {
			t.Errorf("%s contains DROP", name)
		}
		created := map[string]bool{}
		for _, m := range createTableRe.FindAllStringSubmatch(stripSQLComments(body), -1) {
			created[strings.ToLower(m[1])] = true
		}
		if len(created) == 0 {
			t.Errorf("%s creates no table", name)
		}
		for _, m := range createIndexRe.FindAllStringSubmatch(stripSQLComments(body), -1) {
			if !created[strings.ToLower(m[1])] {
				t.Errorf("%s builds an index on %s, which it does not create", name, m[1])
			}
		}
	}

	// 0022 carries the trust invariant the handler and the merge rely on: a
	// device row can never name a source token, so trust = 'device' is a
	// statement about the credential and not about a payload.
	body, _ := migrations.ReadFile("migrations/0022_skill_invocations.sql")
	for _, want := range []string{
		"CHECK (trust = 'claimed' OR source_token_id IS NULL)",
		"CREATE UNIQUE INDEX IF NOT EXISTS skill_invocations_dedupe_idx",
		"CREATE TABLE IF NOT EXISTS skill_rederive_queue",
		"CREATE TABLE IF NOT EXISTS admin_actions",
		"preempted_by UUID REFERENCES source_tokens(id)",
		"preempted_device UUID",
		"link_ref TEXT CHECK",
		"rate_limit_per_min INTEGER NOT NULL DEFAULT 1200",
		"allowed_origins TEXT[] NOT NULL DEFAULT '{beacon}'",
		"bound_actor_email TEXT",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("0022 lacks %q", want)
		}
	}
}

// enumCheckRe is a column with a closed CHECK: the column name, the name
// the CHECK repeats and the quoted values.
var enumCheckRe = regexp.MustCompile(`(?m)^\s*([a-z_]+)\s+TEXT[^\n]*CHECK \(([a-z_]+) IN \(([^)]*)\)\)`)

// TestClosedEnumsMatchTheMigrationChecks is the 10.3 closed-enum guard:
// every CHECK (... IN (...)) of 0022 and 0023 equals the Go slice for that
// column, in the CHECK's order, so a value added on one side and not the
// other fails the build rather than a statement in production. scope
// belongs to source_tokens and its Go set arrives with the token routes;
// any other enum column without a set fails here. The sets are per file:
// reconciler_runs.agent_platform is the reconciler subset, not the
// invocation table's five.
func TestClosedEnumsMatchTheMigrationChecks(t *testing.T) {
	files := []struct {
		name  string
		sets  map[string][]string
		noSet map[string]bool
	}{
		{"0022_skill_invocations.sql", map[string][]string{
			"origin": Origins, "agent_platform": Platforms, "platform": Platforms, "trust": Trusts,
			"skill_source": SkillSources, "trigger": Triggers, "outcome": Outcomes, "error_class": ErrorClasses,
			"session_type": SkillSessionTypes,
		}, map[string]bool{"scope": true}},
		{"0023_skill_catalog.sql", map[string][]string{
			"authored_by": AuthoredBys, "author_evidence": AuthorEvidences, "agent_platform": ReconcilerPlatforms,
		}, nil},
	}
	for _, file := range files {
		sets, noSet := file.sets, file.noSet
		raw, err := migrations.ReadFile("migrations/" + file.name)
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, m := range enumCheckRe.FindAllStringSubmatch(stripSQLComments(string(raw)), -1) {
			col := m[1]
			if m[2] != col {
				t.Errorf("%s: column %s carries a CHECK on %s", file.name, col, m[2])
			}
			var vals []string
			for _, v := range strings.Split(m[3], ",") {
				vals = append(vals, strings.Trim(strings.TrimSpace(v), "'"))
			}
			want, ok := sets[col]
			if !ok {
				if !noSet[col] {
					t.Errorf("%s: %s has a closed CHECK and no Go set", file.name, col)
				}
				continue
			}
			seen[col] = true
			if !reflect.DeepEqual(vals, want) {
				t.Errorf("%s: %s: migration %v, Go %v", file.name, col, vals, want)
			}
		}
		for col := range sets {
			if !seen[col] {
				t.Errorf("%s has no closed CHECK for %s", file.name, col)
			}
		}
	}
}
