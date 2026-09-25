package store

// The skill reads: the summary the /skills page and
// GET /v1/skills/summary draw, the row listing, the catalog's unused and
// pruning reports, the unknown-name report and the reconciler compliance
// report. Every read takes the Viewer, reads skill_invocations and the 0023
// tables (plus the sessions join that resolves a derived row's type and the
// derive ledger the rebuild banner reads), and scopes the person dimension
// in the store, so a handler that forgot to scope could not leak the fleet's
// people to a member.
//
// Owner decision D5 (G9): fleet aggregates go to every member; per-person
// breakdowns and the row listing are the member's own rows; rows with no
// person (beacon, reconciler) reach person panels for admins only. An
// aggregate can still isolate one person, so for a member every by_skill
// and by_platform cell with fewer than three people folds into one "other"
// row without last_used_at, by_bucket drops the rows of such a cell, and
// the repo filter covers the member's own rows only. The unused and pruning
// reports carry the same rule on their last_used_at (and on days_unused,
// which is that fact at a day's resolution): the row itself is a fleet
// aggregate every member may read, but an exact last use on an entry one
// person touched names that person's day.
//
// Trust: the proven predicate is the one exported constant below and
// nothing else spells it, since an lsd_ API row is device-verified but its
// skill and ids are payload claims (design 4b, 6.5).

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ProvenSkillRow is the one predicate that makes a row count as proven: a
// device-verified derived copy of the laptop's own events. Every other row
// is claimed. Pinned by a guard test: no other spelling of the device-trust
// literal exists under the skill files or the read API.
const ProvenSkillRow = "trust = 'device' AND origin = 'derived'"

// SkillFilter is every query parameter of the 6.2 routes, Go-cased, plus
// Limit; the page adds Sort and Q for its top-skills panel. Unknown range
// and since keys fall back to their defaults; types is validated against
// the sessions lattice, nil meaning the default exclusion.
type SkillFilter struct {
	// Range is 1d, 7d, 30d or 90d; 30d when empty or unknown.
	Range          string
	Platform, Repo string
	Types          []string
	Trigger, Trust string
	IncludeClaimed bool
	// TZ buckets the per-bucket series and the compliance days; UTC when
	// empty.
	TZ string
	// The listing's own: a skill (plugin:skill or bare), a person, a
	// session (derived rows only), the keyset cursor and the page size.
	Skill, Email, SessionRef string
	Cursor                   string
	Limit                    int
	// Since is the unused and pruning window, 90d by default.
	Since      string
	SourceRepo string
	// Sort orders the top-skills panel: invocations, people, sessions, user
	// or recent; Q is a prefix on the skill name.
	Sort, Q string
}

// SkillTotals are the KPI figures over the window.
type SkillTotals struct {
	Invocations, Skills, People, Sessions         int64
	User, Agent, Success, Error, Started, Claimed int64
}

// SkillBucketRow is one bucket of one (platform, trigger, trust) series.
type SkillBucketRow struct {
	Bucket                   time.Time
	Platform, Trigger, Trust string
	Invocations              int64
}

// SkillCopy is one catalog copy of a lineage, for the drill-down.
type SkillCopy struct {
	SourceRepo, Plugin, Skill string
	Invocations               int64
}

// SkillSkillRow is one lineage of the top-skills panel: the head's identity
// and the figures over every copy.
type SkillSkillRow struct {
	Lineage, SourceRepo, Plugin, Skill              string
	Invocations, People, Sessions, User, Agent, Err int64
	Platforms, Repos                                []string
	LastUsedAt                                      *time.Time
	Copies                                          []SkillCopy
}

// SkillPlatformRow is one (platform, origin, trust) cell.
type SkillPlatformRow struct {
	Platform, Origin, Trust     string
	Invocations, Skills, People int64
}

// SkillPersonRow is one person's figures; Email is ” for the rows with no
// person, which only an admin sees.
type SkillPersonRow struct {
	Email               string
	Invocations, Skills int64
	TopSkill            string
	Platforms           []string
}

// SkillRepoRow is one (repo, lineage) cell.
type SkillRepoRow struct {
	Repo, Lineage string
	Invocations   int64
}

// SkillSummary is the 6.2 summary document.
type SkillSummary struct {
	From, To   time.Time
	Totals     SkillTotals
	ByBucket   []SkillBucketRow
	BySkill    []SkillSkillRow
	ByPlatform []SkillPlatformRow
	ByPerson   []SkillPersonRow
	ByRepo     []SkillRepoRow
}

// SkillInvocationRow is one listed row: the table's columns plus the
// lineage it resolves to, minus the credential columns (dedupe_key,
// device_id, source_token_id, and the two preempted ids, which are
// credentials of the same kind). SessionRef is blank on a source-token row
// of claude_code or codex, since only the events path binds a session to
// its owner and such a row could carry a colleague's session id
// (SECURITY3-2).
type SkillInvocationRow struct {
	ID                                           int64
	OccurredAt, ReceivedAt                       time.Time
	TimeClamped                                  bool
	Origin, AgentPlatform, Trust                 string
	RawName, Plugin                              string
	Skill                                        *string
	SkillSource, Trigger, Outcome                string
	ErrorClass, ActorEmail                       *string
	ActorKnown                                   bool
	Repo, SessionRef                             string
	LinkRef                                      *string
	SessionType                                  string
	EventID, PromptID, ToolUseID, IdempotencyKey *string
	ArgsPresent                                  bool
	ArgsBytes                                    int
	HarnessVersion                               string
	Lineage                                      string
}

// SkillInvocationPage is one page of the listing.
type SkillInvocationPage struct {
	Invocations []SkillInvocationRow
	NextCursor  string
}

// SkillUnusedRow is one present entry no row reached in the window.
type SkillUnusedRow struct {
	SourceRepo, Plugin, Skill, Lineage, AuthoredBy string
	Mirrored, Installable                          bool
	FirstSeenAt                                    time.Time
	DaysInCatalog                                  int
	LastUsedAt                                     *time.Time
	StaleMirror                                    bool
}

// SkillSuggestion is the exact bare-name alias an unknown name matches.
type SkillSuggestion struct {
	SourceRepo, Plugin, Skill string
}

// SkillUnknownRow is one (raw_name, platform, origin) the alias join leaves
// unresolved; for a member the name and the suggestion are blank and the
// row is one (platform, origin) count.
type SkillUnknownRow struct {
	RawName, Platform, Origin string
	Count                     int64
	FirstSeen, LastSeen       time.Time
	Suggested                 *SkillSuggestion
}

// SkillPruningRow is one entry with no proven row in the window.
type SkillPruningRow struct {
	SourceRepo, Plugin, Skill, AuthoredBy, AuthorEvidence string
	LastUsedAt                                            *time.Time
	DaysUnused, DaysInCatalog                             int
	ProposedAction, ExemptReason                          string
	StaleMirror                                           bool
}

// SkillComplianceRow is one (day, platform, lineage) of the reconciler
// comparison. ReconcilerRows is nil on a platform with no reconciler;
// LastRunAt is the greatest window_end among the runs COVERING the day
// (every local day between window_start and window_end inclusive), and nil
// on a day no run covered, which reads "no run", never 0.
type SkillComplianceRow struct {
	Day               time.Time
	Platform, Lineage string
	BeaconRows        int64
	ReconcilerRows    *int64
	ExactJoins        int64
	CompliancePct     *float64
	LastRunAt         *time.Time
}

// SkillRebuild is the versioned step's progress for the page banner:
// rebuilding while the skill_invocations step of the current version has
// started and not finished.
type SkillRebuild struct {
	Rebuilding       bool
	Processed, Total int64
}

// ---------------------------------------------------------------- the window

// skillRangeDays are the offered windows (design 6.2).
var skillRangeDays = map[string]int{"1d": 1, "7d": 7, "30d": 30, "90d": 90}

const (
	defaultSkillRange = "30d"
	defaultSkillSince = "90d"
	// pruningAge is how long an entry must have been in the catalog, and
	// how long it must have gone unused, to be a candidate (design 6.5).
	pruningAge = 90
	// foldBelow is the people count under which a member's aggregate cell
	// folds into other (design 6.1, SECURITY2-5).
	foldBelow = 3
	// otherCell is the folded row's name.
	otherCell = "other"
)

// skillWindow resolves a range key to [from, to) ending now and the bucket
// unit that keeps its chart under a screen: hours for a day, days beyond.
func skillWindow(key string, now time.Time) (from, to time.Time, unit string) {
	days, ok := skillRangeDays[key]
	if !ok {
		days = skillRangeDays[defaultSkillRange]
	}
	unit = "day"
	if days == 1 {
		unit = "hour"
	}
	return now.Add(-time.Duration(days) * 24 * time.Hour), now, unit
}

// sinceWindow resolves the unused and pruning window's start.
func sinceWindow(key string, now time.Time) time.Time {
	days, ok := skillRangeDays[key]
	if !ok {
		days = skillRangeDays[defaultSkillSince]
	}
	return now.Add(-time.Duration(days) * 24 * time.Hour)
}

// skillSorts are the top-skills orderings, applied in Go over the grouped
// rows; the map is the whitelist.
var skillSorts = map[string]func(a, b SkillSkillRow) bool{
	"invocations": func(a, b SkillSkillRow) bool { return a.Invocations > b.Invocations },
	"people":      func(a, b SkillSkillRow) bool { return a.People > b.People },
	"sessions":    func(a, b SkillSkillRow) bool { return a.Sessions > b.Sessions },
	"user":        func(a, b SkillSkillRow) bool { return a.User > b.User },
	"recent": func(a, b SkillSkillRow) bool {
		switch {
		case a.LastUsedAt == nil:
			return false
		case b.LastUsedAt == nil:
			return true
		}
		return a.LastUsedAt.After(*b.LastUsedAt)
	},
}

// ---------------------------------------------------------------- the shared CTEs

// skillQuery accumulates a statement and its positional parameters. The
// slice is not named args on purpose: the skillusage guard walks this
// package's skill files for that identifier, since the one thing it must
// never see reaching a statement is a Skill call's arguments.
type skillQuery struct {
	sql    strings.Builder
	params []any
}

func (q *skillQuery) param(v any) string {
	q.params = append(q.params, v)
	return fmt.Sprintf("$%d", len(q.params))
}

// skillLineage is the lineage a catalog entry belongs to: its head's key
// when it has one, its own otherwise.
func skillLineage(e string) string {
	return "COALESCE(" + e + ".lineage_of, " + lineageKey(e) + ")"
}

// aliasJoinSQL resolves one row to its catalog copy (design 6.1): the
// aliases on (plugin, skill), and when several repositories match one bare
// alias, the copy in the row's own repository for a project or mirror
// source, else the lineage head, else the first repository by name.
func aliasJoinSQL(r string) string {
	return `LEFT JOIN LATERAL (
    SELECT e.source_repo, e.plugin, e.skill, ` + skillLineage("e") + ` AS lineage
    FROM skill_catalog_aliases a
    JOIN skill_catalog_entries e ON (e.source_repo, e.plugin, e.skill) = (a.source_repo, a.plugin, a.skill)
    WHERE a.alias_plugin = ` + r + `.plugin AND a.alias_skill = ` + r + `.skill
    ORDER BY (e.source_repo = ` + r + `.repo AND ` + r + `.skill_source IN ('project', 'mirror')) DESC, (e.lineage_of IS NULL) DESC, e.source_repo
    LIMIT 1) c ON true`
}

// skillBase opens the statement every summary read shares: base is the
// window under the common filters with each row's session type resolved
// through the sessions join (a derived row's from its session, a
// reconciler's its own); typed applies the default exclusion or the
// caller's types; rows keeps the proven ones unless claimed rows are
// included; resolved attaches the catalog copy. A member's repo filter
// covers their own rows only.
func (s *Store) skillBase(v Viewer, f SkillFilter, now time.Time) (*skillQuery, time.Time, time.Time, string) {
	from, to, unit := skillWindow(f.Range, now)
	q := &skillQuery{}
	scope := scopeEmails(v, nil)
	scoped := !v.IsAdmin() && f.Repo != ""
	q.params = []any{from, to, f.Platform, f.Repo, f.Trigger, f.Trust, scoped, scope, validSessionTypes(f.Types), f.IncludeClaimed}
	q.sql.WriteString(`WITH base AS (
  SELECT si.*, COALESCE(NULLIF(si.session_type, ''), s.session_type, '') AS resolved_type
  FROM skill_invocations si
  LEFT JOIN sessions s ON s.session_id = si.session_ref AND si.origin = 'derived'
  WHERE si.occurred_at >= $1 AND si.occurred_at < $2
    AND ($3::text = '' OR si.agent_platform = $3)
    AND ($4::text = '' OR si.repo = $4)
    AND ($5::text = '' OR si.trigger = $5)
    AND ($6::text = '' OR si.trust = $6)
    AND (NOT $7::bool OR si.actor_email = ANY($8::text[]))
), typed AS (
  SELECT * FROM base
  WHERE (CASE WHEN $9::text[] IS NULL THEN resolved_type NOT IN ('internal', 'automation') ELSE resolved_type = ANY($9::text[]) END)
), rows AS (
  SELECT * FROM typed WHERE (` + ProvenSkillRow + `) OR $10::bool
), resolved AS (
  SELECT r.*, c.lineage, c.source_repo AS copy_repo, c.plugin AS copy_plugin, c.skill AS copy_skill
  FROM rows r
  ` + aliasJoinSQL("r") + `
)
`)
	return q, from, to, unit
}

// scopePredicate is the person-panel scope: everything for an admin, the
// viewer's own rows for a member.
func scopePredicate(q *skillQuery, v Viewer, col string) string {
	if v.IsAdmin() {
		return "true"
	}
	return col + " = ANY(" + q.param(scopeEmails(v, nil)) + "::text[])"
}

// ---------------------------------------------------------------- the summary

// SkillSummary is GET /v1/skills/summary and the page's first five panels.
func (s *Store) SkillSummary(ctx context.Context, v Viewer, f SkillFilter) (SkillSummary, error) {
	now := time.Now().UTC()
	tz := f.TZ
	if tz == "" {
		tz = "UTC"
	}
	if err := validZone(tz); err != nil {
		return SkillSummary{}, err
	}
	out := SkillSummary{}
	var err error
	if out.Totals, out.From, out.To, err = s.skillTotals(ctx, v, f, now); err != nil {
		return SkillSummary{}, err
	}
	if out.ByBucket, err = s.skillByBucket(ctx, v, f, now, tz); err != nil {
		return SkillSummary{}, err
	}
	if out.BySkill, err = s.skillBySkill(ctx, v, f, now); err != nil {
		return SkillSummary{}, err
	}
	if out.ByPlatform, err = s.skillByPlatform(ctx, v, f, now); err != nil {
		return SkillSummary{}, err
	}
	if out.ByPerson, err = s.skillByPerson(ctx, v, f, now); err != nil {
		return SkillSummary{}, err
	}
	if out.ByRepo, err = s.skillByRepo(ctx, v, f, now); err != nil {
		return SkillSummary{}, err
	}
	return out, nil
}

func (s *Store) skillTotals(ctx context.Context, v Viewer, f SkillFilter, now time.Time) (SkillTotals, time.Time, time.Time, error) {
	q, from, to, _ := s.skillBase(v, f, now)
	q.sql.WriteString(`SELECT count(*), count(DISTINCT (plugin, skill)) FILTER (WHERE skill IS NOT NULL), count(DISTINCT actor_email),
  count(DISTINCT session_ref) FILTER (WHERE session_ref <> ''),
  count(*) FILTER (WHERE trigger = 'user'), count(*) FILTER (WHERE trigger = 'agent'),
  count(*) FILTER (WHERE outcome = 'success'), count(*) FILTER (WHERE outcome = 'error'), count(*) FILTER (WHERE outcome = 'started'),
  (SELECT count(*) FROM typed WHERE NOT (` + ProvenSkillRow + `))
FROM rows`)
	var t SkillTotals
	if err := s.db.QueryRow(ctx, q.sql.String(), q.params...).Scan(&t.Invocations, &t.Skills, &t.People, &t.Sessions,
		&t.User, &t.Agent, &t.Success, &t.Error, &t.Started, &t.Claimed); err != nil {
		return SkillTotals{}, from, to, fmt.Errorf("store: skill totals: %w", err)
	}
	return t, from, to, nil
}

// skillByBucket is the per-bucket series, one row per (bucket, platform,
// trigger, trust), over both trusts since trust is one of its dimensions
// and the claimed rows are what the split shows; for a member the rows of
// a cell with fewer than three people are dropped, since a one-person
// platform's daily volume would isolate that person (SECURITY3-8).
func (s *Store) skillByBucket(ctx context.Context, v Viewer, f SkillFilter, now time.Time, tz string) ([]SkillBucketRow, error) {
	q, _, _, unit := s.skillBase(v, f, now)
	q.sql.WriteString(`, cells AS (
  SELECT agent_platform, trigger, trust, count(DISTINCT actor_email) AS people FROM typed GROUP BY 1, 2, 3
)
SELECT date_trunc(` + q.param(unit) + `, r.occurred_at AT TIME ZONE ` + q.param(tz) + `), r.agent_platform, r.trigger, r.trust, count(*), c.people
FROM typed r JOIN cells c USING (agent_platform, trigger, trust)
GROUP BY 1, 2, 3, 4, c.people ORDER BY 1, 2, 3, 4`)
	rows, err := s.db.Query(ctx, q.sql.String(), q.params...)
	if err != nil {
		return nil, fmt.Errorf("store: skill buckets: %w", err)
	}
	defer rows.Close()
	out := []SkillBucketRow{}
	for rows.Next() {
		var r SkillBucketRow
		var people int64
		if err := rows.Scan(&r.Bucket, &r.Platform, &r.Trigger, &r.Trust, &r.Invocations, &people); err != nil {
			return nil, fmt.Errorf("store: scan skill bucket: %w", err)
		}
		if !v.IsAdmin() && people < foldBelow {
			continue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// skillBySkill groups resolved rows by lineage, with the copies beneath,
// sorted and prefix-filtered as the page asks; for a member the cells with
// fewer than three people fold into other.
func (s *Store) skillBySkill(ctx context.Context, v Viewer, f SkillFilter, now time.Time) ([]SkillSkillRow, error) {
	q, _, _, _ := s.skillBase(v, f, now)
	q.sql.WriteString(`SELECT lineage, count(*), count(DISTINCT actor_email), count(DISTINCT session_ref) FILTER (WHERE session_ref <> ''),
  count(*) FILTER (WHERE trigger = 'user'), count(*) FILTER (WHERE trigger = 'agent'), count(*) FILTER (WHERE outcome = 'error'),
  array_agg(DISTINCT agent_platform), array_remove(array_agg(DISTINCT repo), ''), max(occurred_at)
FROM resolved WHERE lineage IS NOT NULL
GROUP BY 1 ORDER BY 2 DESC, 1`)
	rows, err := s.db.Query(ctx, q.sql.String(), q.params...)
	if err != nil {
		return nil, fmt.Errorf("store: skills by lineage: %w", err)
	}
	var out []SkillSkillRow
	byLineage := map[string]int{}
	for rows.Next() {
		var r SkillSkillRow
		var last *time.Time
		if err := rows.Scan(&r.Lineage, &r.Invocations, &r.People, &r.Sessions, &r.User, &r.Agent, &r.Err, &r.Platforms, &r.Repos, &last); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan skill lineage: %w", err)
		}
		r.LastUsedAt = last
		r.SourceRepo, r.Plugin, r.Skill = splitLineage(r.Lineage)
		byLineage[r.Lineage] = len(out)
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: skills by lineage: %w", err)
	}

	q2, _, _, _ := s.skillBase(v, f, now)
	q2.sql.WriteString(`SELECT lineage, copy_repo, copy_plugin, copy_skill, count(*)
FROM resolved WHERE lineage IS NOT NULL
GROUP BY 1, 2, 3, 4 ORDER BY 1, 5 DESC, 2`)
	copies, err := s.db.Query(ctx, q2.sql.String(), q2.params...)
	if err != nil {
		return nil, fmt.Errorf("store: skill copies: %w", err)
	}
	defer copies.Close()
	for copies.Next() {
		var lineage string
		var c SkillCopy
		if err := copies.Scan(&lineage, &c.SourceRepo, &c.Plugin, &c.Skill, &c.Invocations); err != nil {
			return nil, fmt.Errorf("store: scan skill copy: %w", err)
		}
		if i, ok := byLineage[lineage]; ok {
			out[i].Copies = append(out[i].Copies, c)
		}
	}
	if err := copies.Err(); err != nil {
		return nil, fmt.Errorf("store: skill copies: %w", err)
	}

	if prefix := strings.ToLower(strings.TrimSpace(f.Q)); prefix != "" {
		kept := out[:0]
		for _, r := range out {
			if strings.HasPrefix(r.Skill, prefix) || strings.HasPrefix(r.Plugin+":"+r.Skill, prefix) {
				kept = append(kept, r)
			}
		}
		out = kept
	}
	if !v.IsAdmin() {
		out = foldSkillRows(out)
	}
	less, ok := skillSorts[f.Sort]
	if !ok {
		less = skillSorts["invocations"]
	}
	sort.SliceStable(out, func(i, j int) bool { return less(out[i], out[j]) })
	if out == nil {
		out = []SkillSkillRow{}
	}
	return out, nil
}

// splitLineage reads '<source_repo>/<plugin>:<skill>' back.
func splitLineage(lineage string) (repo, plugin, skill string) {
	repo, rest, _ := strings.Cut(lineage, "/")
	plugin, skill, _ = strings.Cut(rest, ":")
	return repo, plugin, skill
}

// foldSkillRows folds every lineage with fewer than three people into one
// other row: the counts summed, the platforms and repos merged, no
// last_used_at, no copies.
func foldSkillRows(in []SkillSkillRow) []SkillSkillRow {
	var out []SkillSkillRow
	var other *SkillSkillRow
	platforms, repos := map[string]bool{}, map[string]bool{}
	for _, r := range in {
		if r.People >= foldBelow {
			out = append(out, r)
			continue
		}
		if other == nil {
			other = &SkillSkillRow{Lineage: otherCell, Skill: otherCell}
		}
		other.Invocations += r.Invocations
		other.People += r.People
		other.Sessions += r.Sessions
		other.User += r.User
		other.Agent += r.Agent
		other.Err += r.Err
		for _, p := range r.Platforms {
			platforms[p] = true
		}
		for _, p := range r.Repos {
			repos[p] = true
		}
	}
	if other != nil {
		other.Platforms, other.Repos = sortedKeys(platforms), sortedKeys(repos)
		out = append(out, *other)
	}
	return out
}

// skillByPlatform is one row per (platform, origin, trust) over both
// trusts, the trust split being the panel's point; a member's cells with
// fewer than three people fold into other.
func (s *Store) skillByPlatform(ctx context.Context, v Viewer, f SkillFilter, now time.Time) ([]SkillPlatformRow, error) {
	q, _, _, _ := s.skillBase(v, f, now)
	q.sql.WriteString(`SELECT agent_platform, origin, trust, count(*), count(DISTINCT (plugin, skill)) FILTER (WHERE skill IS NOT NULL), count(DISTINCT actor_email)
FROM typed GROUP BY 1, 2, 3 ORDER BY 4 DESC, 1, 2, 3`)
	rows, err := s.db.Query(ctx, q.sql.String(), q.params...)
	if err != nil {
		return nil, fmt.Errorf("store: skills by platform: %w", err)
	}
	defer rows.Close()
	out := []SkillPlatformRow{}
	var other *SkillPlatformRow
	for rows.Next() {
		var r SkillPlatformRow
		if err := rows.Scan(&r.Platform, &r.Origin, &r.Trust, &r.Invocations, &r.Skills, &r.People); err != nil {
			return nil, fmt.Errorf("store: scan skill platform: %w", err)
		}
		if !v.IsAdmin() && r.People < foldBelow {
			if other == nil {
				other = &SkillPlatformRow{Platform: otherCell}
			}
			other.Invocations += r.Invocations
			other.Skills += r.Skills
			other.People += r.People
			continue
		}
		out = append(out, r)
	}
	if other != nil {
		out = append(out, *other)
	}
	return out, rows.Err()
}

// skillByPerson reads one row per (person, platform, skill) under the
// person scope and folds them in Go: the totals, the distinct skills, the
// most used skill and the platforms. The rows with no person are one row
// with an empty email, for admins.
func (s *Store) skillByPerson(ctx context.Context, v Viewer, f SkillFilter, now time.Time) ([]SkillPersonRow, error) {
	q, _, _, _ := s.skillBase(v, f, now)
	q.sql.WriteString(`SELECT coalesce(actor_email, ''), agent_platform, plugin, coalesce(skill, ''), count(*)
FROM rows WHERE ` + scopePredicate(q, v, "actor_email") + `
GROUP BY 1, 2, 3, 4 ORDER BY 1, 5 DESC, 3, 4`)
	rows, err := s.db.Query(ctx, q.sql.String(), q.params...)
	if err != nil {
		return nil, fmt.Errorf("store: skills by person: %w", err)
	}
	defer rows.Close()
	type acc struct {
		row       SkillPersonRow
		skills    map[string]int64
		platforms map[string]bool
		top       int64
	}
	byEmail := map[string]*acc{}
	var order []string
	for rows.Next() {
		var email, platform, plugin, skill string
		var n int64
		if err := rows.Scan(&email, &platform, &plugin, &skill, &n); err != nil {
			return nil, fmt.Errorf("store: scan skill person: %w", err)
		}
		a, ok := byEmail[email]
		if !ok {
			a = &acc{row: SkillPersonRow{Email: email}, skills: map[string]int64{}, platforms: map[string]bool{}}
			byEmail[email] = a
			order = append(order, email)
		}
		name := skill
		if plugin != "" {
			name = plugin + ":" + skill
		}
		a.row.Invocations += n
		a.platforms[platform] = true
		if skill != "" {
			a.skills[name] += n
			if a.skills[name] > a.top {
				a.top, a.row.TopSkill = a.skills[name], name
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: skills by person: %w", err)
	}
	out := []SkillPersonRow{}
	for _, email := range order {
		a := byEmail[email]
		a.row.Skills = int64(len(a.skills))
		a.row.Platforms = sortedKeys(a.platforms)
		out = append(out, a.row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Invocations > out[j].Invocations })
	return out, nil
}

// skillByRepo is one row per (repo, lineage); a member's covers their own
// rows, which is their own data and folds nothing.
func (s *Store) skillByRepo(ctx context.Context, v Viewer, f SkillFilter, now time.Time) ([]SkillRepoRow, error) {
	q, _, _, _ := s.skillBase(v, f, now)
	q.sql.WriteString(`SELECT repo, coalesce(lineage, ''), count(*)
FROM resolved WHERE repo <> '' AND ` + scopePredicate(q, v, "actor_email") + `
GROUP BY 1, 2 ORDER BY 3 DESC, 1, 2 LIMIT 500`)
	rows, err := s.db.Query(ctx, q.sql.String(), q.params...)
	if err != nil {
		return nil, fmt.Errorf("store: skills by repo: %w", err)
	}
	defer rows.Close()
	out := []SkillRepoRow{}
	for rows.Next() {
		var r SkillRepoRow
		if err := rows.Scan(&r.Repo, &r.Lineage, &r.Invocations); err != nil {
			return nil, fmt.Errorf("store: scan skill repo: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- the listing

// skillCursor is the listing's keyset position: (occurred_at DESC, id
// DESC), so rows arriving above the page never move it.
type skillCursor struct {
	At time.Time `json:"t"`
	ID int64     `json:"i"`
}

func encodeSkillCursor(c skillCursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeSkillCursor(s string) (*skillCursor, error) {
	if s == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, ErrInvalidCursor
	}
	var c skillCursor
	if err := json.Unmarshal(b, &c); err != nil || c.At.IsZero() || c.ID <= 0 {
		return nil, ErrInvalidCursor
	}
	return &c, nil
}

// skillInvocationColumns is the listing's projection; the credential
// columns are not in it.
const skillInvocationColumns = `r.id, r.occurred_at, r.received_at, r.time_clamped, r.origin, r.agent_platform, r.trust,
  r.raw_name, r.plugin, r.skill, r.skill_source, r.trigger, r.outcome, r.error_class, r.actor_email, r.actor_known, r.repo,
  CASE WHEN r.source_token_id IS NOT NULL AND r.agent_platform IN ('claude_code', 'codex') THEN '' ELSE r.session_ref END,
  r.link_ref, r.resolved_type, r.event_id, r.prompt_id, r.tool_use_id, r.idempotency_key, r.args_present, r.args_bytes, r.harness_version,
  coalesce(r.lineage, '')`

// SkillInvocations is GET /v1/skills/invocations: the window's rows under
// the common filters and the person scope, newest first, keyset paged. A
// session_ref filter takes derived rows only (design 6.3), and a skill
// filter is plugin:skill or the bare skill under any plugin.
func (s *Store) SkillInvocations(ctx context.Context, v Viewer, f SkillFilter) (SkillInvocationPage, error) {
	now := time.Now().UTC()
	after, err := decodeSkillCursor(f.Cursor)
	if err != nil {
		return SkillInvocationPage{}, err
	}
	limit := clampLimit(f.Limit)
	q, _, _, _ := s.skillBase(v, f, now)
	q.sql.WriteString("SELECT " + skillInvocationColumns + "\nFROM resolved r\nWHERE " + scopePredicate(q, v, "r.actor_email"))
	if f.Skill != "" {
		plugin, skill := SplitAlias(strings.ToLower(strings.TrimPrefix(f.Skill, "/")))
		if plugin != "" {
			q.sql.WriteString(" AND r.plugin = " + q.param(plugin) + " AND r.skill = " + q.param(skill))
		} else {
			q.sql.WriteString(" AND r.skill = " + q.param(skill))
		}
	}
	if emails := scopeEmails(v, emailList(f.Email)); len(emails) > 0 {
		q.sql.WriteString(" AND r.actor_email = ANY(" + q.param(emails) + "::text[])")
	}
	if f.SessionRef != "" {
		q.sql.WriteString(" AND r.session_ref = " + q.param(f.SessionRef) + " AND r.origin = 'derived'")
	}
	if after != nil {
		q.sql.WriteString(" AND (r.occurred_at, r.id) < (" + q.param(after.At) + ", " + q.param(after.ID) + ")")
	}
	q.sql.WriteString(" ORDER BY r.occurred_at DESC, r.id DESC LIMIT " + q.param(limit+1))
	rows, err := s.db.Query(ctx, q.sql.String(), q.params...)
	if err != nil {
		return SkillInvocationPage{}, fmt.Errorf("store: list skill invocations: %w", err)
	}
	defer rows.Close()
	page := SkillInvocationPage{Invocations: []SkillInvocationRow{}}
	for rows.Next() {
		r, err := scanSkillInvocation(rows)
		if err != nil {
			return SkillInvocationPage{}, err
		}
		page.Invocations = append(page.Invocations, r)
	}
	if err := rows.Err(); err != nil {
		return SkillInvocationPage{}, fmt.Errorf("store: list skill invocations: %w", err)
	}
	if len(page.Invocations) > limit {
		page.Invocations = page.Invocations[:limit]
		last := page.Invocations[limit-1]
		page.NextCursor = encodeSkillCursor(skillCursor{At: last.OccurredAt, ID: last.ID})
	}
	return page, nil
}

func emailList(email string) []string {
	if email == "" {
		return nil
	}
	return []string{email}
}

func scanSkillInvocation(row Row) (SkillInvocationRow, error) {
	var r SkillInvocationRow
	if err := row.Scan(&r.ID, &r.OccurredAt, &r.ReceivedAt, &r.TimeClamped, &r.Origin, &r.AgentPlatform, &r.Trust,
		&r.RawName, &r.Plugin, &r.Skill, &r.SkillSource, &r.Trigger, &r.Outcome, &r.ErrorClass, &r.ActorEmail, &r.ActorKnown, &r.Repo,
		&r.SessionRef, &r.LinkRef, &r.SessionType, &r.EventID, &r.PromptID, &r.ToolUseID, &r.IdempotencyKey, &r.ArgsPresent, &r.ArgsBytes, &r.HarnessVersion,
		&r.Lineage); err != nil {
		return SkillInvocationRow{}, fmt.Errorf("store: scan skill invocation: %w", err)
	}
	return r, nil
}

// SessionSkills is the session page's strip: the derived rows of one
// session, in order, for a viewer the session's own predicate admits.
// Denial and absence are one ErrNotFound, through the same authorizing
// read every session page starts with; the page's own read already wrote
// the audit row, so this one adds none.
func (s *Store) SessionSkills(ctx context.Context, v Viewer, sessionID string) ([]SkillInvocationRow, error) {
	if _, err := authorizeSession(ctx, s.db, v, sessionID); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(ctx, `WITH r0 AS (
  SELECT si.*, COALESCE(NULLIF(si.session_type, ''), s.session_type, '') AS resolved_type
  FROM skill_invocations si LEFT JOIN sessions s ON s.session_id = si.session_ref
  WHERE si.session_ref = $1 AND si.origin = 'derived'
), resolved AS (
  SELECT r.*, c.lineage FROM r0 r `+aliasJoinSQL("r")+`
)
SELECT `+skillInvocationColumns+` FROM resolved r ORDER BY r.occurred_at, r.id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store: read session skills: %w", err)
	}
	defer rows.Close()
	out := []SkillInvocationRow{}
	for rows.Next() {
		r, err := scanSkillInvocation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- the catalog reports

// usedViaAliasesSQL is the zero-use subquery of design 6.4, verbatim: a
// row reaching the entry through its aliases inside the window, proven
// unless claimed rows count.
func usedViaAliasesSQL(e, since, includeClaimed string) string {
	return `EXISTS (SELECT 1 FROM skill_invocations si JOIN skill_catalog_aliases a ON a.alias_plugin = si.plugin AND a.alias_skill = si.skill
    WHERE (a.source_repo, a.plugin, a.skill) = (` + e + `.source_repo, ` + e + `.plugin, ` + e + `.skill) AND si.occurred_at >= ` + since + ` AND ((` + ProvenSkillRow + `) OR ` + includeClaimed + `::bool))`
}

// lastUsedViaAliasesJoin is the newest row reaching the entry, ever, with
// the number of distinct people behind that row. The people count is what
// the D5 fold needs: an entry nobody used in ninety days is by definition
// one or two people's, so an exact last_used_at on it names when a
// particular colleague last ran a particular skill. Design 6.1 already
// folds any cell under three people and drops its last_used_at; these two
// reports are the same disclosure on a surface with no fold, so they apply
// the same rule (adversarial finding 7). It is a LATERAL rather than two
// correlated subqueries so the timestamp and the count are read from one
// pass over the same rows and cannot disagree.
func lastUsedViaAliasesJoin(e, includeClaimed, as string) string {
	return `LEFT JOIN LATERAL (
    SELECT max(si.occurred_at) AS last_used_at, count(DISTINCT si.actor_email) AS people
    FROM skill_invocations si JOIN skill_catalog_aliases a ON a.alias_plugin = si.plugin AND a.alias_skill = si.skill
    WHERE (a.source_repo, a.plugin, a.skill) = (` + e + `.source_repo, ` + e + `.plugin, ` + e + `.skill) AND ((` + ProvenSkillRow + `) OR ` + includeClaimed + `::bool)) ` + as + ` ON true`
}

// foldLastUsed applies the design 6.1 rule to an optional last_used_at: an
// admin sees it, a member sees it only when at least three people are
// behind it. Returning nil is the same state an entry no row ever reached
// is in, so the page and the JSON read "never" rather than inventing a
// date.
func foldLastUsed(v Viewer, at *time.Time, people int64) *time.Time {
	if v.IsAdmin() || people >= foldBelow {
		return at
	}
	return nil
}

// SkillUnused is GET /v1/skills/catalog/unused (design 6.4): the present
// entries no row reached through the aliases since the window's start.
func (s *Store) SkillUnused(ctx context.Context, v Viewer, f SkillFilter) ([]SkillUnusedRow, error) {
	now := time.Now().UTC()
	since := sinceWindow(f.Since, now)
	rows, err := s.db.Query(ctx, `SELECT e.source_repo, e.plugin, e.skill, `+skillLineage("e")+`, e.authored_by, e.mirrored, e.installable, e.first_seen_at,
  u.last_used_at, u.people, `+staleMirrorSQL("e")+`
FROM skill_catalog_entries e
`+lastUsedViaAliasesJoin("e", "$2", "u")+`
WHERE e.present AND ($3::text = '' OR e.source_repo = $3)
  AND NOT `+usedViaAliasesSQL("e", "$1", "$2")+`
ORDER BY e.source_repo, e.plugin, e.skill`, since, f.IncludeClaimed, f.SourceRepo)
	if err != nil {
		return nil, fmt.Errorf("store: unused skills: %w", err)
	}
	defer rows.Close()
	out := []SkillUnusedRow{}
	for rows.Next() {
		var (
			r      SkillUnusedRow
			people int64
		)
		if err := rows.Scan(&r.SourceRepo, &r.Plugin, &r.Skill, &r.Lineage, &r.AuthoredBy, &r.Mirrored, &r.Installable, &r.FirstSeenAt, &r.LastUsedAt, &people, &r.StaleMirror); err != nil {
			return nil, fmt.Errorf("store: scan unused skill: %w", err)
		}
		r.DaysInCatalog = wholeDays(r.FirstSeenAt, now)
		r.LastUsedAt = foldLastUsed(v, r.LastUsedAt, people)
		out = append(out, r)
	}
	return out, rows.Err()
}

func wholeDays(from, now time.Time) int {
	if !now.After(from) {
		return 0
	}
	return int(now.Sub(from).Hours() / 24)
}

// SkillUnknown is GET /v1/skills/unknown (design 6.4): the window's rows
// the alias join leaves unresolved, per (raw_name, platform, origin), with
// the exact bare-name alias as the suggestion. raw_name is text an emitter
// chose, so a member gets one nameless count per (platform, origin). Every
// row of the window counts, claimed included: skills from unpublished
// catalogs are what this report exists to find.
func (s *Store) SkillUnknown(ctx context.Context, v Viewer, f SkillFilter) ([]SkillUnknownRow, error) {
	now := time.Now().UTC()
	f.IncludeClaimed = true
	q, _, _, _ := s.skillBase(v, f, now)
	q.sql.WriteString(`SELECT r.raw_name, r.agent_platform, r.origin, count(*), min(r.occurred_at), max(r.occurred_at), s.source_repo, s.plugin, s.skill
FROM resolved r
LEFT JOIN LATERAL (SELECT a.source_repo, a.plugin, a.skill FROM skill_catalog_aliases a WHERE a.alias_plugin = '' AND a.alias_skill = r.skill ORDER BY a.source_repo LIMIT 1) s ON true
WHERE r.lineage IS NULL
GROUP BY 1, 2, 3, 7, 8, 9 ORDER BY 4 DESC, 1, 2, 3 LIMIT 500`)
	rows, err := s.db.Query(ctx, q.sql.String(), q.params...)
	if err != nil {
		return nil, fmt.Errorf("store: unknown skills: %w", err)
	}
	defer rows.Close()
	var out []SkillUnknownRow
	for rows.Next() {
		var r SkillUnknownRow
		var repo, plugin, skill *string
		if err := rows.Scan(&r.RawName, &r.Platform, &r.Origin, &r.Count, &r.FirstSeen, &r.LastSeen, &repo, &plugin, &skill); err != nil {
			return nil, fmt.Errorf("store: scan unknown skill: %w", err)
		}
		if repo != nil && skill != nil {
			r.Suggested = &SkillSuggestion{SourceRepo: *repo, Plugin: deref(plugin), Skill: *skill}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: unknown skills: %w", err)
	}
	if !v.IsAdmin() {
		out = foldUnknownRows(out)
	}
	if out == nil {
		out = []SkillUnknownRow{}
	}
	return out, nil
}

// foldUnknownRows drops the names: one row per (platform, origin), the
// counts summed, the spans merged, no suggestion.
func foldUnknownRows(in []SkillUnknownRow) []SkillUnknownRow {
	byCell := map[string]*SkillUnknownRow{}
	for _, r := range in {
		k := r.Platform + "/" + r.Origin
		c, ok := byCell[k]
		if !ok {
			c = &SkillUnknownRow{Platform: r.Platform, Origin: r.Origin, FirstSeen: r.FirstSeen, LastSeen: r.LastSeen}
			byCell[k] = c
		}
		c.Count += r.Count
		if r.FirstSeen.Before(c.FirstSeen) {
			c.FirstSeen = r.FirstSeen
		}
		if r.LastSeen.After(c.LastSeen) {
			c.LastSeen = r.LastSeen
		}
	}
	var out []SkillUnknownRow
	for _, k := range sortedKeys(byCell) {
		out = append(out, *byCell[k])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

// The pruning verdicts (design 6.5).
const (
	PruneArchivePR = "archive_pr"
	PruneFlagOnly  = "flag_only"

	ExemptTooNew         = "too_new"
	ExemptNotMirrored    = "not_mirrored"
	ExemptNotInstallable = "not_installable"
	ExemptAdapter        = "adapter"
)

// reviewAdapters are the two backend review adapters that stay in the
// backend tree by design (contracts section 7, BE-1) and are never pruned.
var reviewAdapters = map[string]bool{"pr-review": true, "local-pr-review": true}

// SkillPruning is GET /v1/skills/pruning (design 6.5): the present entries
// with no proven row (claimed too when asked) since the window's start,
// with the verdict the archive generator reads. An entry younger than
// ninety days is listed as too_new rather than dropped, so the report says
// why it is not yet a candidate.
func (s *Store) SkillPruning(ctx context.Context, v Viewer, f SkillFilter) ([]SkillPruningRow, error) {
	now := time.Now().UTC()
	since := sinceWindow(f.Since, now)
	rows, err := s.db.Query(ctx, `SELECT e.source_repo, e.plugin, e.skill, e.authored_by, e.author_evidence, e.mirrored, e.installable, e.first_seen_at,
  u.last_used_at, u.people, `+staleMirrorSQL("e")+`
FROM skill_catalog_entries e
`+lastUsedViaAliasesJoin("e", "$2", "u")+`
WHERE e.present AND NOT `+usedViaAliasesSQL("e", "$1", "$2")+`
ORDER BY e.source_repo, e.plugin, e.skill`, since, f.IncludeClaimed)
	if err != nil {
		return nil, fmt.Errorf("store: skill pruning: %w", err)
	}
	defer rows.Close()
	out := []SkillPruningRow{}
	for rows.Next() {
		var (
			r                     SkillPruningRow
			mirrored, installable bool
			firstSeen             time.Time
			people                int64
		)
		if err := rows.Scan(&r.SourceRepo, &r.Plugin, &r.Skill, &r.AuthoredBy, &r.AuthorEvidence, &mirrored, &installable, &firstSeen, &r.LastUsedAt, &people, &r.StaleMirror); err != nil {
			return nil, fmt.Errorf("store: scan skill pruning: %w", err)
		}
		r.DaysInCatalog = wholeDays(firstSeen, now)
		// The fold takes days_unused with it: it is last_used_at at a day's
		// resolution, so leaving it would hand back what the fold hid.
		// The verdict below reads days_in_catalog, the flags and the
		// authorship, never days_unused, so an entry's proposed_action is
		// the same for every viewer.
		r.LastUsedAt = foldLastUsed(v, r.LastUsedAt, people)
		if r.LastUsedAt != nil {
			r.DaysUnused = wholeDays(*r.LastUsedAt, now)
		} else {
			r.DaysUnused = r.DaysInCatalog
		}
		switch {
		case r.DaysInCatalog < pruningAge:
			r.ExemptReason = ExemptTooNew
		case !mirrored:
			r.ExemptReason = ExemptNotMirrored
		case !installable:
			r.ExemptReason = ExemptNotInstallable
		case r.SourceRepo == "backend" && reviewAdapters[r.Skill]:
			r.ExemptReason = ExemptAdapter
		}
		// archive_pr only for an agent-authored, unexempt, fresh entry; a
		// stale mirror is flagged until a fresh publish clears it.
		r.ProposedAction = PruneFlagOnly
		if r.AuthoredBy == AuthoredByAgent && r.ExemptReason == "" && !r.StaleMirror {
			r.ProposedAction = PruneArchivePR
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SkillCompliance is GET /v1/skills/compliance (design 6.5), admin only:
// per (day, platform, lineage) the beacon rows, the reconciler rows, the
// exact joins (a beacon row whose session_ref a reconciler row's link_ref
// names) and the last run of the day. Claimed rows are the subject here,
// so every row of the window counts; a platform without a reconciler
// reads reconciler_rows null.
func (s *Store) SkillCompliance(ctx context.Context, v Viewer, f SkillFilter) ([]SkillComplianceRow, error) {
	if !v.IsAdmin() {
		return nil, ErrNotAdmin
	}
	now := time.Now().UTC()
	tz := f.TZ
	if tz == "" {
		tz = "UTC"
	}
	if err := validZone(tz); err != nil {
		return nil, err
	}
	f.IncludeClaimed = true
	q, from, to, _ := s.skillBase(v, f, now)
	q.sql.WriteString(`SELECT (r.occurred_at AT TIME ZONE ` + q.param(tz) + `)::date, r.agent_platform, coalesce(r.lineage, ''), r.origin, count(*),
  count(*) FILTER (WHERE r.origin = 'beacon' AND EXISTS (SELECT 1 FROM skill_invocations x WHERE x.origin = 'reconciler' AND x.link_ref = r.session_ref))
FROM resolved r WHERE r.origin IN ('beacon', 'reconciler')
GROUP BY 1, 2, 3, 4 ORDER BY 1, 2, 3, 4`)
	rows, err := s.db.Query(ctx, q.sql.String(), q.params...)
	if err != nil {
		return nil, fmt.Errorf("store: skill compliance: %w", err)
	}
	type key struct {
		day               time.Time
		platform, lineage string
	}
	byKey := map[key]*SkillComplianceRow{}
	var order []key
	reconcilers := map[string]bool{}
	for rows.Next() {
		var (
			day                       time.Time
			platform, lineage, origin string
			n, joins                  int64
		)
		if err := rows.Scan(&day, &platform, &lineage, &origin, &n, &joins); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan skill compliance: %w", err)
		}
		k := key{day, platform, lineage}
		c, ok := byKey[k]
		if !ok {
			c = &SkillComplianceRow{Day: day, Platform: platform, Lineage: lineage}
			byKey[k] = c
			order = append(order, k)
		}
		if origin == OriginBeacon {
			c.BeaconRows, c.ExactJoins = n, joins
		} else {
			reconcilers[platform] = true
			c.ReconcilerRows = &n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: skill compliance: %w", err)
	}

	// The runs of the window, by every local day their window COVERS, not
	// by the day it ended (ruled 2026-09-22 from LS-3 review-3 finding 1;
	// design 6.5). An hourly reconciler whose window crosses local midnight
	// reconciled both days, and keying it on window_end alone left the
	// earlier one reading "no run" forever, which is the one thing the
	// column exists to distinguish from a day with no activations. So a run
	// attaches to each day from (window_start AT TIME ZONE tz)::date to
	// (window_end AT TIME ZONE tz)::date inclusive, and a day's last_run_at
	// is the greatest window_end among the runs covering it, which is the
	// coverage the operator is reading for, not the moment a POST arrived.
	//
	// The series is clamped to the report's own days: window_start is the
	// caller's, so a run claiming a window opening in the year 1000 would
	// otherwise generate a row per day since, and no day outside [from, to)
	// can appear in the output anyway. The LEFT JOIN keeps a run whose
	// clamped range is empty, so its platform still counts as having a
	// reconciler.
	runs, err := s.db.Query(ctx, `SELECT r.agent_platform, d::date, max(r.window_end)
FROM reconciler_runs r
LEFT JOIN LATERAL generate_series(
    greatest((r.window_start AT TIME ZONE $3)::date, ($1 AT TIME ZONE $3)::date),
    least((r.window_end AT TIME ZONE $3)::date, ($2 AT TIME ZONE $3)::date),
    interval '1 day') AS d ON true
WHERE r.window_end >= $1 AND r.window_start < $2 GROUP BY 1, 2`, from, to, tz)
	if err != nil {
		return nil, fmt.Errorf("store: reconciler runs: %w", err)
	}
	defer runs.Close()
	lastRun := map[string]time.Time{}
	for runs.Next() {
		var platform string
		var day, at *time.Time
		if err := runs.Scan(&platform, &day, &at); err != nil {
			return nil, fmt.Errorf("store: scan reconciler run: %w", err)
		}
		reconcilers[platform] = true
		if day == nil || at == nil {
			continue
		}
		k := platform + "/" + day.Format("2006-01-02")
		if prev, ok := lastRun[k]; !ok || at.After(prev) {
			lastRun[k] = *at
		}
	}
	if err := runs.Err(); err != nil {
		return nil, fmt.Errorf("store: reconciler runs: %w", err)
	}
	out := []SkillComplianceRow{}
	for _, k := range order {
		c := byKey[k]
		if reconcilers[c.Platform] && c.ReconcilerRows == nil {
			zero := int64(0)
			c.ReconcilerRows = &zero
		}
		if c.ReconcilerRows != nil && *c.ReconcilerRows > 0 {
			pct := float64(c.BeaconRows) / float64(*c.ReconcilerRows)
			if pct > 1 {
				pct = 1
			}
			c.CompliancePct = &pct
		}
		if at, ok := lastRun[c.Platform+"/"+c.Day.Format("2006-01-02")]; ok {
			t := at
			c.LastRunAt = &t
		}
		out = append(out, *c)
	}
	return out, nil
}

// SkillRebuildProgress reads the skill_invocations step of the current
// derive version for the page banner (design 3.6): rebuilding while the
// step has a row with no finished_at, with the sessions it has processed
// against the sessions there are.
func (s *Store) SkillRebuildProgress(ctx context.Context) (SkillRebuild, error) {
	var (
		out      SkillRebuild
		finished bool
	)
	err := s.db.QueryRow(ctx, `SELECT processed, finished_at IS NOT NULL FROM derive_jobs WHERE version = $1 AND step = 'skill_invocations'`, DerivedSchema).Scan(&out.Processed, &finished)
	switch {
	case err == nil:
		out.Rebuilding = !finished
	case noRows(err):
		return out, nil
	default:
		return SkillRebuild{}, fmt.Errorf("store: read the skill rebuild progress: %w", err)
	}
	if !out.Rebuilding {
		return out, nil
	}
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&out.Total); err != nil {
		return SkillRebuild{}, fmt.Errorf("store: count sessions for the skill rebuild banner: %w", err)
	}
	return out, nil
}
