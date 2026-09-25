package web

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The skills page and the session strip on the fake: a member sees the
// fleet aggregates and only their own person row, an admin sees everyone
// and the two admin panels, the default query hides internal and
// automation rows and keeps claimed rows apart, the rebuild banner renders
// from the step's progress, the strip renders derived rows only, and the
// nav carries the link after Analytics.

func skillsFixture() *fakeData {
	f := newFake()
	f.seedSession("s1", owner.Email)
	last := fixedNow.Add(-time.Hour)
	f.skills = &fakeSkills{
		summary: SkillSummary{From: fixedNow.Add(-30 * 24 * time.Hour), To: fixedNow,
			Totals: SkillTotals{Invocations: 12, Skills: 3, People: 2, Sessions: 5, User: 4, Agent: 8, Error: 1, Claimed: 2},
			ByBucket: []SkillBucketRow{
				{Bucket: fixedNow.Add(-24 * time.Hour), Platform: "claude_code", Trigger: "agent", Trust: "device", Invocations: 7},
				{Bucket: fixedNow, Platform: "devin", Trigger: "agent", Trust: "claimed", Invocations: 5},
			},
			BySkill: []SkillSkillRow{{Lineage: "example-skills/engg:git", SourceRepo: "example-skills", Plugin: "engg", Skill: "git", Invocations: 9, People: 2, Sessions: 4,
				User: 3, Agent: 6, Platforms: []string{"claude_code"}, Repos: []string{"example-app"}, LastUsedAt: &last,
				Copies: []SkillCopy{{SourceRepo: "example-skills", Plugin: "engg", Skill: "git", Invocations: 8}, {SourceRepo: "example-app", Plugin: "engg", Skill: "git", Invocations: 1}}}},
			ByPlatform: []SkillPlatformRow{{Platform: "claude_code", Origin: "derived", Trust: "device", Invocations: 12, Skills: 3, People: 2}},
			ByPerson: []SkillPersonRow{
				{Email: owner.Email, Invocations: 8, Skills: 2, TopSkill: "engg:git", Platforms: []string{"claude_code"}},
				{Email: other.Email, Invocations: 4, Skills: 1, TopSkill: "engg:git", Platforms: []string{"claude_code"}},
			},
			ByRepo: []SkillRepoRow{{Repo: "example-app", Lineage: "example-skills/engg:git", Invocations: 12}},
		},
		unused:     []SkillUnusedRow{{SourceRepo: "example-skills", Plugin: "engg", Skill: "temporal", AuthoredBy: "unknown", Mirrored: true, Installable: true, FirstSeenAt: fixedNow.Add(-100 * 24 * time.Hour), DaysInCatalog: 100}},
		unknown:    []SkillUnknownRow{{RawName: "/plan", Platform: "claude_code", Origin: "derived", Count: 4, FirstSeen: last, LastSeen: last, Suggested: &SkillSuggestion{SourceRepo: "example-skills", Plugin: "engg", Skill: "plan"}}},
		pruning:    []SkillPruningRow{{SourceRepo: "example-skills", Plugin: "engg", Skill: "temporal", AuthoredBy: "agent", DaysUnused: 100, DaysInCatalog: 100, ProposedAction: "archive_pr"}},
		compliance: []SkillComplianceRow{{Day: fixedNow, Platform: "devin", Lineage: "example-skills/engg:git", BeaconRows: 4, ReconcilerRows: ptrInt64(5), ExactJoins: 3, CompliancePct: ptrFloat(0.8), LastRunAt: &last}},
		sessionSkills: map[string][]SkillInvocation{"s1": {
			{OccurredAt: last, Origin: "derived", AgentPlatform: "claude_code", Trust: "device", RawName: "engg:git", Plugin: "engg", Skill: "git", Trigger: "agent", Outcome: "success", SessionRef: "s1"},
			// A row the shared cloud token posted under this session's id,
			// which the store never returns and the template never draws.
			{OccurredAt: last, Origin: "hook", AgentPlatform: "claude_code", Trust: "claimed", RawName: "cloud:leaked-skill", Plugin: "cloud", Skill: "leaked-skill", Trigger: "agent", Outcome: "started", SessionRef: "s1"},
		}},
	}
	return f
}

func ptrInt64(n int64) *int64     { return &n }
func ptrFloat(f float64) *float64 { return &f }

func TestSkillsPageRendersEveryPanelForAnAdmin(t *testing.T) {
	f := skillsFixture()
	rec := get(t, newServer(t, f, admin), "/skills")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Invocations per day", "Top skills", "By platform", "By repository", "By person", "Unknown names", "Never used", "Pruning", "Compliance",
		owner.Email, other.Email, // the admin sees everyone
		"/plan",                              // the unknown name, admins only
		"example-skills/engg:plan",           // its suggestion
		"example-app/engg:git",               // the per-copy drill-down
		"<svg",                               // the chart is drawn
		"internal and automation hidden",     // the default type selection, named
		"claimed rows (apart)",               // claimed kept apart by default
		"archive_pr", "80%", "engg:temporal", // pruning, compliance, never used
		`href="/skills?range=7d"`, `href="/skills?sort=people#top"`, // the range and sort links
		"typed by a person", "error rate",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the skills page does not contain %q", want)
		}
	}
	if strings.Contains(body, "History rebuilding") {
		t.Error("the rebuild banner shows while no step is open")
	}
	// The compliance row's reconciler count, rendered. The fixture's
	// ReconcilerRows is *int64(5) and the cell used to print the heap
	// address behind it, because num takes any and a template never
	// indirects a pointer that is already assignable to it; the suite went
	// green over it because only the ratio was asserted (adversarial
	// finding 2). The beacon count and the ratio pin the row around it.
	if !strings.Contains(body, `<td class="n">4</td>`) || !strings.Contains(body, `<td class="n">5</td>`) {
		t.Errorf("the compliance row does not render 4 beacon and 5 reconciler rows:\n%s", complianceRow(body))
	}
	// And the general form of the same bug: no Go pointer anywhere in the
	// page. A rendered address is always a template calling an any-taking
	// helper on a pointer field.
	if i := strings.Index(body, "0x"); i >= 0 {
		t.Errorf("the page renders a pointer address: %q", body[max(0, i-60):min(len(body), i+20)])
	}
}

// complianceRow is the compliance table's first row, for a readable
// failure message.
func complianceRow(body string) string {
	i := strings.Index(body, `id="compliance"`)
	if i < 0 {
		return body
	}
	j := strings.Index(body[i:], "<tbody>")
	if j < 0 {
		return body[i:]
	}
	end := i + j + 400
	return body[i+j : min(len(body), end)]
}

// A panel whose read failed says unavailable instead of stating a positive
// fact about the data: "Every name in this window resolves." on a
// statement timeout told an admin the catalog was clean, and "No beacon or
// reconciler rows in this window." told them the reconcilers had nothing
// to reconcile (adversarial finding 3). One case per panel, and the page
// itself still renders: a failure costs the panel, not the page.
func TestSkillsPagePanelSaysUnavailableWhenItsReadFailed(t *testing.T) {
	boom := errors.New("canceling statement due to statement timeout")
	cases := []struct {
		name        string
		fail        func(*fakeSkills)
		gone, shown string
	}{
		{"unknown names", func(s *fakeSkills) { s.unknownErr = boom }, "Every name in this window resolves.", "Unknown names"},
		{"never used", func(s *fakeSkills) { s.unusedErr = boom }, "Every present entry was used in the window.", "Never used"},
		{"pruning", func(s *fakeSkills) { s.pruningErr = boom }, "No candidates.", "Pruning"},
		{"compliance", func(s *fakeSkills) { s.complianceErr = boom }, "No beacon or reconciler rows in this window.", "Compliance"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := skillsFixture()
			// Every panel empty, so only the failing one differs from a
			// genuinely empty window.
			f.skills.unknown, f.skills.unused, f.skills.pruning, f.skills.compliance = nil, nil, nil, nil
			c.fail(f.skills)
			rec := get(t, newServer(t, f, admin), "/skills")
			if rec.Code != http.StatusOK {
				t.Fatalf("a failed panel took the page down: %d", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, c.shown) {
				t.Errorf("the %s panel is gone from the page", c.name)
			}
			if strings.Contains(body, c.gone) {
				t.Errorf("the %s panel states %q although its read failed", c.name, c.gone)
			}
			if !strings.Contains(body, "unavailable") {
				t.Errorf("the %s panel does not say unavailable:\n%s", c.name, body)
			}
			// The other three still say their empty-state fact, so the flag
			// is per panel and not a page-wide switch.
			for _, o := range cases {
				if o.name == c.name {
					continue
				}
				if !strings.Contains(body, o.gone) {
					t.Errorf("the %s panel lost its empty-state note while %s failed", o.name, c.name)
				}
			}
		})
	}
}

// A member's page: the fleet aggregates, their own person row alone, no
// colleague's name anywhere, nameless unknowns, and neither admin panel.
func TestSkillsPageScopesAMember(t *testing.T) {
	f := skillsFixture()
	rec := get(t, newServer(t, f, owner), "/skills")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Top skills", "By platform", "By person", "Your own row", owner.Email, "Unknown names", "Never used", "the names are for admins"} {
		if !strings.Contains(body, want) {
			t.Errorf("a member's skills page does not contain %q", want)
		}
	}
	for _, banned := range []string{other.Email, "/plan", "engg:plan", "Pruning", "Compliance", "archive_pr"} {
		if strings.Contains(body, banned) {
			t.Errorf("a member's skills page contains %q", banned)
		}
	}
	if !strings.Contains(body, "claude_code") {
		t.Error("a member's unknown panel lost the platform count")
	}
}

// The default query is the store's default: no types (internal and
// automation hidden), 30 days, claimed apart; every knob reaches the port.
func TestSkillsPageDefaultsAndKnobsReachThePort(t *testing.T) {
	f := skillsFixture()
	srv := newServer(t, f, admin)
	get(t, srv, "/skills")
	q := f.skills.lastQuery
	if q.Types != nil || q.Range != "30d" || q.IncludeClaimed || q.Trigger != "" || q.Sort != "invocations" || q.Q != "" {
		t.Errorf("default query = %+v", q)
	}
	get(t, srv, "/skills?range=7d&types=automation,internal&include_claimed=1&trigger=user&sort=people&q=gi")
	q = f.skills.lastQuery
	if q.Range != "7d" || strings.Join(q.Types, ",") != "internal,automation" || !q.IncludeClaimed || q.Trigger != "user" || q.Sort != "people" || q.Q != "gi" {
		t.Errorf("query = %+v", q)
	}
	// An unknown range, sort or trigger falls back rather than 400: the
	// page is a page, and a stale bookmark should still open.
	get(t, srv, "/skills?range=1y&sort=cost&trigger=robot")
	q = f.skills.lastQuery
	if q.Range != "30d" || q.Sort != "invocations" || q.Trigger != "" {
		t.Errorf("fallback query = %+v", q)
	}
}

func TestSkillsPageShowsTheRebuildBanner(t *testing.T) {
	f := skillsFixture()
	f.skills.rebuild = SkillRebuild{Rebuilding: true, Processed: 3, Total: 10}
	body := get(t, newServer(t, f, owner), "/skills").Body.String()
	if !strings.Contains(body, "History rebuilding, 3 of 10 sessions") {
		t.Error("the rebuild banner did not render from the step's progress")
	}
}

func TestSkillsPageRequiresSignIn(t *testing.T) {
	rec := get(t, newServer(t, skillsFixture(), nobody), "/skills")
	if rec.Code != http.StatusFound || !strings.Contains(rec.Header().Get("Location"), "next=%2Fskills") {
		t.Errorf("an anonymous request: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

// The nav: Skills after Analytics, for everyone.
func TestNavLinksSkillsAfterAnalytics(t *testing.T) {
	for _, v := range []Viewer{owner, admin} {
		body := get(t, newServer(t, skillsFixture(), v), "/skills").Body.String()
		a, s := strings.Index(body, `href="/analytics"`), strings.Index(body, `href="/skills"`)
		if a < 0 || s < 0 || s < a {
			t.Errorf("%s: analytics at %d, skills at %d", v.Email, a, s)
		}
		if !strings.Contains(body, `href="/skills" class="on" aria-current="page"`) {
			t.Errorf("%s: the skills link is not marked current on its own page", v.Email)
		}
	}
}

// The session strip: derived rows only, never a cloud-token row carrying
// the session's id; absent when the session has none; and a panel failure
// costs the strip, not the page.
func TestSessionStripRendersDerivedRowsOnly(t *testing.T) {
	f := skillsFixture()
	rec := get(t, newServer(t, f, owner), "/sessions/s1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `class="skill-strip"`) || !strings.Contains(body, "engg:git") {
		t.Error("the strip did not render the derived row")
	}
	if strings.Contains(body, "leaked-skill") {
		t.Error("the strip rendered a row the cloud token posted under this session's id")
	}
	f.skills.sessionSkills = map[string][]SkillInvocation{}
	if body := get(t, newServer(t, f, owner), "/sessions/s1").Body.String(); strings.Contains(body, `class="skill-strip"`) {
		t.Error("an empty strip rendered")
	}
	// The other member cannot open the session at all, so no strip leaks.
	if rec := get(t, newServer(t, f, other), "/sessions/s1"); rec.Code != http.StatusNotFound {
		t.Errorf("a colleague's session page = %d", rec.Code)
	}
}
