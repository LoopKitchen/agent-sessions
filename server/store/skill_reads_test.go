package store

// The reads' properties that need no database: the one proven predicate
// and where it may be spelled, the statements each read issues and the
// arguments the window and the filters become, the person scope a member
// gets, the folding a member's aggregates go through, the keyset cursor,
// the verdicts the pruning report assigns, and the not-found collapse on
// the session strip. The SQL itself is exercised in
// skill_reads_integration_test.go.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	readAdmin  = Viewer{Email: "boss@example.com", Role: RoleAdmin}
	readMember = Viewer{Email: "ann@example.com", Role: RoleMember}
)

// totalsMatch is a fragment only the totals statement carries; the shared
// CTEs put "FROM rows" in every read.
const totalsMatch = "SELECT count(*) FROM typed WHERE NOT ("

// TestProvenPredicateIsSingleHomed is the 10.3 guard: trust = 'device' is
// spelled once, in ProvenSkillRow, across the store's skill files and the
// read API, so the pruning, zero-use and compliance reads cannot drift to
// "any device row", which an lsd_ API row would satisfy (SECURITY3-4).
func TestProvenPredicateIsSingleHomed(t *testing.T) {
	var files []string
	for _, pattern := range []string{"skill*.go", filepath.Join("..", "api", "*.go")} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range matches {
			if !strings.HasSuffix(m, "_test.go") {
				files = append(files, m)
			}
		}
	}
	if len(files) < 5 {
		t.Fatalf("walked %v; the glob missed the skill files or the api package", files)
	}
	hits := 0
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(body), "\n") {
			if !strings.Contains(line, "trust = 'device'") {
				continue
			}
			hits++
			if filepath.Base(f) != "skill_reads.go" || !strings.Contains(line, "ProvenSkillRow =") {
				t.Errorf("%s spells the proven predicate outside ProvenSkillRow: %s", f, strings.TrimSpace(line))
			}
		}
	}
	if hits != 1 {
		t.Errorf("trust = 'device' occurs %d times, want once (the ProvenSkillRow definition)", hits)
	}
	if ProvenSkillRow != "trust = 'device' AND origin = 'derived'" {
		t.Errorf("ProvenSkillRow = %q", ProvenSkillRow)
	}
}

// TestSkillSummaryStatementsAndWindow pins the shared CTEs: the sessions
// join that resolves a derived row's type, the default exclusion, the
// proven predicate under include_claimed, the window as the first two
// arguments, and the person scope on the person and repo panels.
func TestSkillSummaryStatementsAndWindow(t *testing.T) {
	db := &fakeDB{stubs: []*stub{{match: totalsMatch, rows: [][]any{{int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)}}}}}
	s, _ := skillStore(db)
	before := time.Now().UTC()
	out, err := s.SkillSummary(context.Background(), readMember, SkillFilter{Platform: "devin", Trigger: "agent", Types: []string{"user", "bogus"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(db.calls) != 7 {
		t.Fatalf("%d statements, want totals, buckets, lineages, copies, platforms, people, repos:\n%s", len(db.calls), db.summary())
	}
	for _, c := range db.calls {
		for _, want := range []string{
			"LEFT JOIN sessions s ON s.session_id = si.session_ref AND si.origin = 'derived'",
			"COALESCE(NULLIF(si.session_type, ''), s.session_type, '') AS resolved_type",
			"resolved_type NOT IN ('internal', 'automation')",
			"resolved_type = ANY($9::text[])",
			"(" + ProvenSkillRow + ") OR $10::bool",
			"skill_catalog_aliases a",
		} {
			if !strings.Contains(c.sql, want) {
				t.Errorf("a summary statement lacks %q:\n%s", want, c.sql)
			}
		}
		from, to := c.args[0].(time.Time), c.args[1].(time.Time)
		if d := to.Sub(from); d != 30*24*time.Hour {
			t.Errorf("window = %v, want the 30d default", d)
		}
		if to.Before(before) || to.After(time.Now().UTC().Add(time.Second)) {
			t.Errorf("the window ends %v, want now", to)
		}
		if c.args[2] != "devin" || c.args[3] != "" || c.args[4] != "agent" || c.args[5] != "" || c.args[6] != false || c.args[9] != false {
			t.Errorf("filter args = %v", c.args[2:10])
		}
		if types, ok := c.args[8].([]string); !ok || len(types) != 1 || types[0] != "user" {
			t.Errorf("types arg = %v, want the validated [user]", c.args[8])
		}
	}
	if !out.From.Equal(db.calls[0].args[0].(time.Time)) || !out.To.Equal(db.calls[0].args[1].(time.Time)) {
		t.Error("the summary does not report its window")
	}
	// The person and repo panels are scoped to the member; the fleet panels
	// are not.
	people := db.find(t, "coalesce(actor_email, ''), agent_platform, plugin")
	if !strings.Contains(people.sql, "actor_email = ANY($11::text[])") || argText(people.args[10]) != readMember.Email {
		t.Errorf("the person panel is not scoped to the member: %s %v", people.sql, people.args)
	}
	repos := db.find(t, "SELECT repo, coalesce(lineage, '')")
	if !strings.Contains(repos.sql, "actor_email = ANY($11::text[])") {
		t.Error("the repo panel is not scoped to the member")
	}
	platforms := db.find(t, "SELECT agent_platform, origin, trust")
	if strings.Contains(platforms.sql, "ANY($11") || !strings.Contains(platforms.sql, "FROM typed GROUP BY") {
		t.Error("the platform panel is scoped, or reads one trust only")
	}
	if !strings.Contains(db.find(t, "JOIN cells c").sql, "FROM typed r JOIN cells") {
		t.Error("the bucket panel reads one trust only")
	}
	if !strings.Contains(db.find(t, "count(DISTINCT (plugin, skill))").sql, "SELECT count(*) FROM typed WHERE NOT ("+ProvenSkillRow+")") {
		t.Error("the totals do not count the claimed rows")
	}

	// An admin's person panel is the whole fleet, the window follows the
	// range, a member's repo filter scopes the base to their own rows.
	db = &fakeDB{stubs: []*stub{{match: totalsMatch, rows: [][]any{{int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)}}}}}
	s, _ = skillStore(db)
	if _, err := s.SkillSummary(context.Background(), readAdmin, SkillFilter{Range: "7d", Repo: "backend"}); err != nil {
		t.Fatal(err)
	}
	people = db.find(t, "coalesce(actor_email, ''), agent_platform, plugin")
	if !strings.Contains(people.sql, "WHERE true") || people.args[6] != false {
		t.Errorf("an admin's person panel = %s scoped %v", people.sql, people.args[6])
	}
	if d := people.args[1].(time.Time).Sub(people.args[0].(time.Time)); d != 7*24*time.Hour {
		t.Errorf("window = %v, want 7d", d)
	}
	db = &fakeDB{stubs: []*stub{{match: totalsMatch, rows: [][]any{{int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)}}}}}
	s, _ = skillStore(db)
	if _, err := s.SkillSummary(context.Background(), readMember, SkillFilter{Repo: "backend"}); err != nil {
		t.Fatal(err)
	}
	if c := db.calls[0]; c.args[6] != true || argText(c.args[7]) != readMember.Email || !strings.Contains(c.sql, "NOT $7::bool OR si.actor_email = ANY($8::text[])") {
		t.Errorf("a member's repo filter is not scoped: %v", c.args[6:8])
	}
	// A bad zone is refused before any statement.
	db = &fakeDB{}
	s, _ = skillStore(db)
	if _, err := s.SkillSummary(context.Background(), readAdmin, SkillFilter{TZ: "Mars/Olympus"}); err == nil || len(db.calls) != 0 {
		t.Error("an unusable zone reached a statement")
	}
}

// TestSkillSummaryFoldsAMembersSmallCells is design 6.1's side-channel
// rule: a member's lineage and platform cells under three people fold into
// other without last_used_at, their buckets are dropped, and an admin sees
// every cell as it is.
func TestSkillSummaryFoldsAMembersSmallCells(t *testing.T) {
	when := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	stubs := func() []*stub {
		return []*stub{
			{match: totalsMatch, rows: [][]any{{int64(9), int64(2), int64(4), int64(3), int64(5), int64(4), int64(1), int64(1), int64(7), int64(2)}}},
			{match: "JOIN cells c", rows: [][]any{
				{when, "claude_code", "agent", "device", int64(6), int64(4)},
				{when, "vorflux", "agent", "claimed", int64(3), int64(0)},
			}},
			{match: "GROUP BY 1 ORDER BY 2 DESC, 1", rows: [][]any{
				{"example-skills/engg:git", int64(6), int64(4), int64(3), int64(2), int64(4), int64(0), []string{"claude_code"}, []string{"backend"}, &when},
				{"example-skills/engg:temporal", int64(2), int64(1), int64(1), int64(0), int64(2), int64(1), []string{"claude_code"}, []string{}, &when},
				{"example-skills/engg:standup", int64(1), int64(1), int64(1), int64(1), int64(0), int64(0), []string{"codex"}, []string{}, &when},
			}},
			{match: "GROUP BY 1, 2, 3, 4 ORDER BY 1, 5 DESC, 2", rows: [][]any{
				{"example-skills/engg:git", "example-skills", "engg", "git", int64(5)},
				{"example-skills/engg:git", "backend", "engg", "git", int64(1)},
			}},
			{match: "SELECT agent_platform, origin, trust", rows: [][]any{
				{"claude_code", "derived", "device", int64(6), int64(3), int64(4)},
				{"vorflux", "reconciler", "claimed", int64(3), int64(1), int64(0)},
			}},
		}
	}
	db := &fakeDB{stubs: stubs()}
	s, _ := skillStore(db)
	member, err := s.SkillSummary(context.Background(), readMember, SkillFilter{IncludeClaimed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(member.ByBucket) != 1 || member.ByBucket[0].Platform != "claude_code" {
		t.Errorf("a member's buckets = %+v, want the four-person cell alone", member.ByBucket)
	}
	if len(member.BySkill) != 2 || member.BySkill[0].Lineage != "example-skills/engg:git" || member.BySkill[1].Lineage != otherCell {
		t.Fatalf("a member's skills = %+v, want git and other", member.BySkill)
	}
	other := member.BySkill[1]
	if other.Invocations != 3 || other.LastUsedAt != nil || other.Copies != nil || len(other.Platforms) != 2 {
		t.Errorf("the folded row = %+v", other)
	}
	if git := member.BySkill[0]; len(git.Copies) != 2 || git.SourceRepo != "example-skills" || git.Plugin != "engg" || git.Skill != "git" || git.LastUsedAt == nil {
		t.Errorf("the git row = %+v", git)
	}
	if len(member.ByPlatform) != 2 || member.ByPlatform[1].Platform != otherCell || member.ByPlatform[1].Invocations != 3 {
		t.Errorf("a member's platforms = %+v", member.ByPlatform)
	}
	if member.Totals.Invocations != 9 || member.Totals.Claimed != 2 {
		t.Errorf("totals = %+v", member.Totals)
	}

	db = &fakeDB{stubs: stubs()}
	s, _ = skillStore(db)
	admin, err := s.SkillSummary(context.Background(), readAdmin, SkillFilter{IncludeClaimed: true, Sort: "recent", Q: "eng"})
	if err != nil {
		t.Fatal(err)
	}
	if len(admin.ByBucket) != 2 || len(admin.ByPlatform) != 2 || admin.ByPlatform[1].Platform != "vorflux" {
		t.Errorf("an admin's cells were folded: %+v %+v", admin.ByBucket, admin.ByPlatform)
	}
	if len(admin.BySkill) != 3 {
		t.Errorf("an admin's skills = %+v, want every lineage under the engg prefix", admin.BySkill)
	}
	db = &fakeDB{stubs: stubs()}
	s, _ = skillStore(db)
	admin, _ = s.SkillSummary(context.Background(), readAdmin, SkillFilter{IncludeClaimed: true, Q: "temp"})
	if len(admin.BySkill) != 1 || admin.BySkill[0].Skill != "temporal" {
		t.Errorf("the prefix filter = %+v", admin.BySkill)
	}
}

// TestSkillByPersonFoldsRowsIntoPeople: the per-person panel is folded in
// Go from one (person, platform, skill) row set: totals, distinct skills,
// the top skill and the platforms, the rows with no person as one blank
// row for an admin.
func TestSkillByPersonFoldsRowsIntoPeople(t *testing.T) {
	db := &fakeDB{stubs: []*stub{{match: "coalesce(actor_email, ''), agent_platform, plugin", rows: [][]any{
		{"ann@example.com", "claude_code", "engg", "git", int64(5)},
		{"ann@example.com", "codex", "", "git", int64(1)},
		{"ann@example.com", "claude_code", "engg", "temporal", int64(2)},
		{"", "devin", "", "ask-devin", int64(4)},
	}}}}
	s, _ := skillStore(db)
	rows, err := s.skillByPerson(context.Background(), readAdmin, SkillFilter{}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Email != "ann@example.com" || rows[0].Invocations != 8 || rows[0].Skills != 3 || rows[0].TopSkill != "engg:git" || len(rows[0].Platforms) != 2 {
		t.Errorf("rows = %+v", rows)
	}
	if rows[1].Email != "" || rows[1].Invocations != 4 || rows[1].TopSkill != "ask-devin" {
		t.Errorf("the person-less row = %+v", rows[1])
	}
}

// TestSkillInvocationsStatementAndCursor pins the listing: the projection
// without the credential columns and with the session_ref blanking, the
// person scope, the skill and session filters, the keyset cursor and the
// page cut.
func TestSkillInvocationsStatementAndCursor(t *testing.T) {
	at := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	row := func(id int64, when time.Time) []any {
		return []any{id, when, when, false, "derived", "claude_code", "device", "engg:git", "engg", strPtr("git"), "plugin", "agent", "success",
			(*string)(nil), strPtr("ann@example.com"), true, "backend", "sess-1", (*string)(nil), "user", strPtr("evt-1"), (*string)(nil), strPtr("toolu_1"), (*string)(nil), false, 0, "2.1.273", "example-skills/engg:git"}
	}
	db := &fakeDB{stubs: []*stub{{match: "FROM resolved r", rows: [][]any{row(3, at), row(2, at.Add(-time.Minute)), row(1, at.Add(-2*time.Minute))}}}}
	s, _ := skillStore(db)
	page, err := s.SkillInvocations(context.Background(), readMember, SkillFilter{Limit: 2, Skill: "engg:git", SessionRef: "sess-1"})
	if err != nil {
		t.Fatal(err)
	}
	c := db.find(t, "FROM resolved r")
	for _, want := range []string{
		"CASE WHEN r.source_token_id IS NOT NULL AND r.agent_platform IN ('claude_code', 'codex') THEN '' ELSE r.session_ref END",
		"WHERE r.actor_email = ANY($11::text[])",
		"r.plugin = $12 AND r.skill = $13",
		"r.actor_email = ANY($14::text[])",
		"r.session_ref = $15 AND r.origin = 'derived'",
		"ORDER BY r.occurred_at DESC, r.id DESC LIMIT $16",
	} {
		if !strings.Contains(c.sql, want) {
			t.Errorf("the listing lacks %q:\n%s", want, c.sql)
		}
	}
	for _, banned := range []string{"r.dedupe_key", "r.device_id,", "r.source_token_id,", "r.preempted_by", "r.preempted_device"} {
		if strings.Contains(skillInvocationColumns, banned) {
			t.Errorf("the projection carries %s", banned)
		}
	}
	if c.args[11] != "engg" || c.args[12] != "git" || argText(c.args[13]) != readMember.Email || c.args[14] != "sess-1" || c.args[15] != 3 {
		t.Errorf("listing args = %v", c.args[11:])
	}
	if len(page.Invocations) != 2 || page.Invocations[0].ID != 3 || page.Invocations[0].Lineage != "example-skills/engg:git" || page.NextCursor == "" {
		t.Errorf("page = %+v", page)
	}
	cur, err := decodeSkillCursor(page.NextCursor)
	if err != nil || cur.ID != 2 || !cur.At.Equal(at.Add(-time.Minute)) {
		t.Errorf("cursor = %+v, %v", cur, err)
	}
	// The cursor feeds back as the keyset bound; a bare skill matches under
	// any plugin; an admin naming a person filters to them without a scope.
	db = &fakeDB{}
	s, _ = skillStore(db)
	if _, err := s.SkillInvocations(context.Background(), readAdmin, SkillFilter{Cursor: page.NextCursor, Skill: "git", Email: "bea@example.com"}); err != nil {
		t.Fatal(err)
	}
	c = db.find(t, "FROM resolved r")
	if !strings.Contains(c.sql, "WHERE true") || !strings.Contains(c.sql, "AND r.skill = $11") || !strings.Contains(c.sql, "(r.occurred_at, r.id) < ($13, $14)") || argText(c.args[11]) != "bea@example.com" || c.args[13] != int64(2) {
		t.Errorf("the second page = %s %v", c.sql, c.args[10:])
	}
	if _, err := s.SkillInvocations(context.Background(), readAdmin, SkillFilter{Cursor: "not-a-cursor"}); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("a junk cursor = %v", err)
	}
}

// TestSessionSkillsCollapsesDenialIntoNotFound: the strip starts with the
// session's own authorizing read, so a colleague's session and a missing
// one are one ErrNotFound with no skill statement issued.
func TestSessionSkillsCollapsesDenialIntoNotFound(t *testing.T) {
	db := &fakeDB{}
	s, _ := skillStore(db)
	if _, err := s.SessionSkills(context.Background(), readMember, "sess-x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if db.count("skill_invocations") != 0 {
		t.Error("a denied strip read the skill rows")
	}
	if !strings.Contains(db.calls[0].sql, "FROM sessions s") {
		t.Errorf("the strip does not start with the authorizing read: %s", db.calls[0].sql)
	}
}

// TestSkillUnusedAndPruningStatementsAndVerdicts pins the 6.4 zero-use
// shape and the 6.5 verdicts: archive_pr for an agent-authored,
// unexempt, fresh entry; flag_only otherwise, with the exemption named.
func TestSkillUnusedAndPruningStatementsAndVerdicts(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)
	young := now.Add(-10 * 24 * time.Hour)
	used := now.Add(-95 * 24 * time.Hour)
	db := &fakeDB{stubs: []*stub{{match: "FROM skill_catalog_entries e", rows: [][]any{
		{"example-skills", "engg", "git", "example-skills/engg:git", "human", true, true, old, (*time.Time)(nil), int64(0), false},
	}}}}
	s, _ := skillStore(db)
	unused, err := s.SkillUnused(context.Background(), readMember, SkillFilter{Since: "30d", SourceRepo: "example-skills"})
	if err != nil {
		t.Fatal(err)
	}
	c := db.calls[0]
	for _, want := range []string{
		"WHERE e.present AND ($3::text = '' OR e.source_repo = $3)",
		"NOT EXISTS (SELECT 1 FROM skill_invocations si JOIN skill_catalog_aliases a ON a.alias_plugin = si.plugin AND a.alias_skill = si.skill",
		"(a.source_repo, a.plugin, a.skill) = (e.source_repo, e.plugin, e.skill) AND si.occurred_at >= $1 AND ((" + ProvenSkillRow + ") OR $2::bool)",
		"e.lineage_of = h.source_repo || '/' || h.plugin || ':' || h.skill",
		// The last use and the people behind it come from one pass, so the
		// D5 fold below cannot be applied to a count that disagrees with
		// the timestamp it is guarding (adversarial finding 7).
		"SELECT max(si.occurred_at) AS last_used_at, count(DISTINCT si.actor_email) AS people",
	} {
		if !strings.Contains(c.sql, want) {
			t.Errorf("the unused read lacks %q:\n%s", want, c.sql)
		}
	}
	if d := now.Sub(c.args[0].(time.Time)); d < 30*24*time.Hour-time.Minute || d > 30*24*time.Hour+time.Minute {
		t.Errorf("since = %v ago, want 30d", d)
	}
	if c.args[1] != false || c.args[2] != "example-skills" {
		t.Errorf("unused args = %v", c.args[1:])
	}
	if len(unused) != 1 || unused[0].DaysInCatalog != 100 || unused[0].Lineage != "example-skills/engg:git" {
		t.Errorf("unused = %+v", unused)
	}

	db = &fakeDB{stubs: []*stub{{match: "FROM skill_catalog_entries e", rows: [][]any{
		{"example-skills", "engg", "old-agent", "agent", "frontmatter", true, true, old, (*time.Time)(nil), int64(0), false},
		// Three people behind the last use, so a member keeps it.
		{"example-skills", "engg", "old-human", "human", "git_first_commit", true, true, old, &used, int64(3), false},
		{"example-skills", "engg", "young-agent", "agent", "", true, true, young, (*time.Time)(nil), int64(0), false},
		{"example-skills", "gtm", "unmirrored", "agent", "", false, true, old, (*time.Time)(nil), int64(0), false},
		{"example-skills", "gtm", "uninstallable", "agent", "", true, false, old, (*time.Time)(nil), int64(0), false},
		{"backend", "", "pr-review", "agent", "", true, true, old, (*time.Time)(nil), int64(0), false},
		{"backend", "engg", "stale-agent", "agent", "", true, true, old, (*time.Time)(nil), int64(0), true},
	}}}}
	s, _ = skillStore(db)
	pruning, err := s.SkillPruning(context.Background(), readMember, SkillFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if d := now.Sub(db.calls[0].args[0].(time.Time)); d < 90*24*time.Hour-time.Minute || d > 90*24*time.Hour+time.Minute {
		t.Errorf("the pruning window starts %v ago, want 90d", d)
	}
	want := map[string][2]string{
		"old-agent": {PruneArchivePR, ""}, "old-human": {PruneFlagOnly, ""}, "young-agent": {PruneFlagOnly, ExemptTooNew},
		"unmirrored": {PruneFlagOnly, ExemptNotMirrored}, "uninstallable": {PruneFlagOnly, ExemptNotInstallable},
		"pr-review": {PruneFlagOnly, ExemptAdapter}, "stale-agent": {PruneFlagOnly, ""},
	}
	if len(pruning) != len(want) {
		t.Fatalf("%d pruning rows, want %d", len(pruning), len(want))
	}
	for _, r := range pruning {
		w := want[r.Skill]
		if r.ProposedAction != w[0] || r.ExemptReason != w[1] {
			t.Errorf("%s: %s %q, want %s %q", r.Skill, r.ProposedAction, r.ExemptReason, w[0], w[1])
		}
		switch r.Skill {
		case "old-human":
			if r.DaysUnused != 95 || r.DaysInCatalog != 100 {
				t.Errorf("old-human days = %d unused, %d in catalog", r.DaysUnused, r.DaysInCatalog)
			}
		case "old-agent":
			if r.DaysUnused != 100 {
				t.Errorf("a never-used entry's days_unused = %d, want its days in the catalog", r.DaysUnused)
			}
		case "stale-agent":
			if !r.StaleMirror {
				t.Error("the stale entry does not read stale")
			}
		}
	}
}

// TestSkillUnknownNamesAreForAdmins: the unresolved names, with the bare
// suggestion, for an admin; nameless (platform, origin) counts for a
// member; and every row of the window counts, claimed included.
func TestSkillUnknownNamesAreForAdmins(t *testing.T) {
	at := time.Date(2026, 9, 21, 9, 0, 0, 0, time.UTC)
	stubs := func() []*stub {
		return []*stub{{match: "WHERE r.lineage IS NULL", rows: [][]any{
			{"/plan", "claude_code", "derived", int64(4), at.Add(-time.Hour), at, (*string)(nil), (*string)(nil), (*string)(nil)},
			{"foo:git", "claude_code", "derived", int64(2), at.Add(-2 * time.Hour), at.Add(-time.Hour), strPtr("example-skills"), strPtr("engg"), strPtr("git")},
			{"mystery", "devin", "beacon", int64(1), at, at, (*string)(nil), (*string)(nil), (*string)(nil)},
		}}}
	}
	db := &fakeDB{stubs: stubs()}
	s, _ := skillStore(db)
	admin, err := s.SkillUnknown(context.Background(), readAdmin, SkillFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if db.calls[0].args[9] != true {
		t.Error("the unknown report hides the claimed rows")
	}
	if len(admin) != 3 || admin[0].RawName != "/plan" || admin[1].Suggested == nil || admin[1].Suggested.Skill != "git" || admin[1].Suggested.SourceRepo != "example-skills" {
		t.Errorf("admin rows = %+v", admin)
	}
	db = &fakeDB{stubs: stubs()}
	s, _ = skillStore(db)
	member, err := s.SkillUnknown(context.Background(), readMember, SkillFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(member) != 2 {
		t.Fatalf("member rows = %+v, want one per (platform, origin)", member)
	}
	for _, r := range member {
		if r.RawName != "" || r.Suggested != nil {
			t.Errorf("a member's row carries a name: %+v", r)
		}
	}
	if member[0].Platform != "claude_code" || member[0].Count != 6 || !member[0].FirstSeen.Equal(at.Add(-2*time.Hour)) {
		t.Errorf("the folded claude_code row = %+v", member[0])
	}
}

// TestSkillComplianceIsAdminOnlyAndReadsRuns: a member is refused before
// any statement; an admin gets one row per (day, platform, lineage) with
// the reconciler count, the ratio capped at one, a null count on a
// platform with no reconciler, and the day's last run. The runs statement
// is pinned on its covering shape: every day the window touches, clamped
// to the report's own days, and max(window_end) rather than the moment the
// POST arrived (review-3 finding 1).
func TestSkillComplianceIsAdminOnlyAndReadsRuns(t *testing.T) {
	db := &fakeDB{}
	s, _ := skillStore(db)
	if _, err := s.SkillCompliance(context.Background(), readMember, SkillFilter{}); !errors.Is(err, ErrNotAdmin) || len(db.calls) != 0 {
		t.Fatalf("a member's compliance = %v with %d statements", err, len(db.calls))
	}
	day := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	ran := day.Add(23 * time.Hour)
	db = &fakeDB{stubs: []*stub{
		{match: "r.origin IN ('beacon', 'reconciler')", rows: [][]any{
			{day, "devin", "example-skills/engg:git", "beacon", int64(5), int64(4)},
			{day, "devin", "example-skills/engg:git", "reconciler", int64(4), int64(0)},
			{day, "capy", "example-skills/engg:git", "beacon", int64(2), int64(0)},
			{day, "vorflux", "", "reconciler", int64(3), int64(0)},
		}},
		// One run covering the day, plus a second platform whose clamped
		// range is empty: the LEFT JOIN keeps the row with a null day, so
		// the platform still counts as having a reconciler.
		{match: "FROM reconciler_runs r", rows: [][]any{{"devin", day, ran}, {"codex", nil, nil}}},
	}}
	s, _ = skillStore(db)
	rows, err := s.SkillCompliance(context.Background(), readAdmin, SkillFilter{TZ: "Asia/Kolkata"})
	if err != nil {
		t.Fatal(err)
	}
	if db.calls[0].args[9] != true || argText(db.calls[0].args[10]) != "Asia/Kolkata" {
		t.Errorf("compliance args = %v", db.calls[0].args[9:])
	}
	runsSQL := db.find(t, "FROM reconciler_runs r").sql
	for _, want := range []string{
		"generate_series(",
		"greatest((r.window_start AT TIME ZONE $3)::date, ($1 AT TIME ZONE $3)::date)",
		"least((r.window_end AT TIME ZONE $3)::date, ($2 AT TIME ZONE $3)::date)",
		"max(r.window_end)",
		"LEFT JOIN LATERAL",
	} {
		if !strings.Contains(runsSQL, want) {
			t.Errorf("the runs statement does not carry %q:\n%s", want, runsSQL)
		}
	}
	if strings.Contains(runsSQL, "max(received_at)") {
		t.Error("the runs statement still keys last_run_at on received_at")
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	devin := rows[0]
	if devin.BeaconRows != 5 || devin.ReconcilerRows == nil || *devin.ReconcilerRows != 4 || devin.ExactJoins != 4 || devin.CompliancePct == nil || *devin.CompliancePct != 1 || devin.LastRunAt == nil || !devin.LastRunAt.Equal(ran) {
		t.Errorf("devin = %+v", devin)
	}
	capy := rows[1]
	if capy.ReconcilerRows != nil || capy.CompliancePct != nil || capy.LastRunAt != nil {
		t.Errorf("capy, with no reconciler, = %+v", capy)
	}
	vorflux := rows[2]
	if vorflux.BeaconRows != 0 || vorflux.ReconcilerRows == nil || *vorflux.ReconcilerRows != 3 || vorflux.CompliancePct == nil || *vorflux.CompliancePct != 0 {
		t.Errorf("vorflux = %+v", vorflux)
	}
}

// TestSkillRebuildProgressReadsTheStep: rebuilding while the version's
// step has no finished_at, with the sessions counted; nothing while the
// step is absent or done, with no sessions count paid for.
func TestSkillRebuildProgressReadsTheStep(t *testing.T) {
	db := &fakeDB{stubs: []*stub{
		{match: "FROM derive_jobs", rows: [][]any{{int64(3), false}}},
		{match: "count(*) FROM sessions", rows: [][]any{{int64(10)}}},
	}}
	s, _ := skillStore(db)
	rb, err := s.SkillRebuildProgress(context.Background())
	if err != nil || !rb.Rebuilding || rb.Processed != 3 || rb.Total != 10 {
		t.Errorf("rebuild = %+v, %v", rb, err)
	}
	if c := db.find(t, "FROM derive_jobs"); c.args[0] != DerivedSchema || !strings.Contains(c.sql, "step = 'skill_invocations'") {
		t.Errorf("the step read = %s %v", c.sql, c.args)
	}
	db = &fakeDB{stubs: []*stub{{match: "FROM derive_jobs", rows: [][]any{{int64(12600), true}}}}}
	s, _ = skillStore(db)
	if rb, err := s.SkillRebuildProgress(context.Background()); err != nil || rb.Rebuilding || db.count("FROM sessions") != 0 {
		t.Errorf("a finished step = %+v, %v, sessions counted %d times", rb, err, db.count("FROM sessions"))
	}
	db = &fakeDB{}
	s, _ = skillStore(db)
	if rb, err := s.SkillRebuildProgress(context.Background()); err != nil || rb.Rebuilding {
		t.Errorf("an absent step = %+v, %v", rb, err)
	}
}

// TestSkillWindowAndSorts: the range keys, their bucket unit, the since
// default and the sort whitelist.
func TestSkillWindowAndSorts(t *testing.T) {
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	for key, want := range map[string]struct {
		days int
		unit string
	}{"1d": {1, "hour"}, "7d": {7, "day"}, "30d": {30, "day"}, "90d": {90, "day"}, "": {30, "day"}, "1y": {30, "day"}} {
		from, to, unit := skillWindow(key, now)
		if to.Sub(from) != time.Duration(want.days)*24*time.Hour || unit != want.unit || !to.Equal(now) {
			t.Errorf("range %q = %v to %v by %s", key, from, to, unit)
		}
	}
	if since := sinceWindow("", now); now.Sub(since) != 90*24*time.Hour {
		t.Errorf("since default = %v", now.Sub(since))
	}
	for _, key := range []string{"invocations", "people", "sessions", "user", "recent"} {
		if _, ok := skillSorts[key]; !ok {
			t.Errorf("sort %q is not offered", key)
		}
	}
	if _, ok := skillSorts["cost"]; ok {
		t.Error("an unknown sort is offered")
	}
}

// The unused and pruning reads take the Viewer and use it: an entry nobody
// used in ninety days is one or two people's, so an exact last_used_at on
// it names when a particular colleague last ran a particular skill. Design
// 6.1 folds any cell under three people and drops its last_used_at; these
// two reports had the Viewer in their signature and never read it, which
// handed a member the fleet's figure on a surface with no fold at all
// (adversarial finding 7). days_unused goes with it, being the same fact at
// a day's resolution, while the verdict does not, so an entry's
// proposed_action is the same for every viewer.
func TestSkillUnusedAndPruningFoldLastUsedForAMember(t *testing.T) {
	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)
	used := now.Add(-95 * 24 * time.Hour)
	unusedRows := func() [][]any {
		return [][]any{
			{"example-skills", "engg", "lonely", "example-skills/engg:lonely", "agent", true, true, old, &used, int64(1), false},
			{"example-skills", "engg", "shared", "example-skills/engg:shared", "agent", true, true, old, &used, int64(3), false},
		}
	}
	pruningRows := func() [][]any {
		return [][]any{
			{"example-skills", "engg", "lonely", "agent", "", true, true, old, &used, int64(1), false},
			{"example-skills", "engg", "shared", "agent", "", true, true, old, &used, int64(3), false},
		}
	}
	for _, c := range []struct {
		name          string
		v             Viewer
		wantLonelyAt  bool
		wantLonelyDay int
	}{
		{"a member", readMember, false, 100},
		{"an admin", readAdmin, true, 95},
	} {
		t.Run(c.name, func(t *testing.T) {
			db := &fakeDB{stubs: []*stub{{match: "FROM skill_catalog_entries e", rows: unusedRows()}}}
			s, _ := skillStore(db)
			unused, err := s.SkillUnused(context.Background(), c.v, SkillFilter{})
			if err != nil || len(unused) != 2 {
				t.Fatalf("unused = %+v, %v", unused, err)
			}
			if (unused[0].LastUsedAt != nil) != c.wantLonelyAt {
				t.Errorf("%s sees the one-person entry's last use as %v", c.name, unused[0].LastUsedAt)
			}
			if unused[1].LastUsedAt == nil || !unused[1].LastUsedAt.Equal(used) {
				t.Errorf("the three-person entry's last use = %v, want it kept for everyone", unused[1].LastUsedAt)
			}

			db = &fakeDB{stubs: []*stub{{match: "FROM skill_catalog_entries e", rows: pruningRows()}}}
			s, _ = skillStore(db)
			pruning, err := s.SkillPruning(context.Background(), c.v, SkillFilter{})
			if err != nil || len(pruning) != 2 {
				t.Fatalf("pruning = %+v, %v", pruning, err)
			}
			if (pruning[0].LastUsedAt != nil) != c.wantLonelyAt {
				t.Errorf("%s sees the one-person entry's last use as %v", c.name, pruning[0].LastUsedAt)
			}
			if pruning[0].DaysUnused != c.wantLonelyDay {
				t.Errorf("%s sees days_unused = %d, want %d", c.name, pruning[0].DaysUnused, c.wantLonelyDay)
			}
			if pruning[1].DaysUnused != 95 {
				t.Errorf("the three-person entry's days_unused = %d, want 95 for everyone", pruning[1].DaysUnused)
			}
			// The verdict is the same for both viewers: it reads the age,
			// the flags and the authorship, never the folded figure.
			for _, r := range pruning {
				if r.ProposedAction != PruneArchivePR || r.ExemptReason != "" {
					t.Errorf("%s sees %s's verdict as %s %q", c.name, r.Skill, r.ProposedAction, r.ExemptReason)
				}
			}
		})
	}
}
