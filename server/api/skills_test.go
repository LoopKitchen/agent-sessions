package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The skill read routes' properties, observed without a database: which
// status a storage answer becomes, that the filter reaches the store as
// parsed and a bad value is refused at the edge, that the admin-only route
// is a 404 to a member before any store call, that the listing's cursor
// is scoped to the viewer and the query, and that the arrays are never
// null. The scoping itself is the store's and is tested there; the fake
// mimics it only far enough to show the handlers pass the viewer through.

// The fake's skill half. Recorded filters let a test read what the
// handler decided; the rows are fixtures the fake narrows by role the way
// the store does.
type fakeSkills struct {
	summary    SkillSummary
	rows       []SkillInvocation
	unused     []SkillUnusedRow
	unknown    []SkillUnknownRow
	pruning    []SkillPruningRow
	compliance []SkillComplianceRow
	err        error
	lastFilter SkillFilter
	calls      int

	publish    PublishResult
	publishErr error
	published  []CatalogBody
	publishTok string
}

// skillFixtures attaches the skill half to a fakeStore from outside its
// file: the fake's struct belongs to api_test.go and a field cannot be
// added to it from here, so the fixtures live in a registry keyed by the
// fake's pointer.
var (
	skillFixturesMu sync.Mutex
	skillFixtures   = map[*fakeStore]*fakeSkills{}
)

func (f *fakeStore) skills() *fakeSkills {
	skillFixturesMu.Lock()
	defer skillFixturesMu.Unlock()
	s, ok := skillFixtures[f]
	if !ok {
		s = &fakeSkills{}
		skillFixtures[f] = s
	}
	return s
}

// withSkills seeds the fake's skill half.
func (f *fakeStore) withSkills(s *fakeSkills) *fakeStore {
	skillFixturesMu.Lock()
	defer skillFixturesMu.Unlock()
	skillFixtures[f] = s
	return f
}

func (f *fakeStore) SkillSummary(_ context.Context, v Viewer, q SkillFilter) (SkillSummary, error) {
	s := f.skills()
	s.lastFilter, s.calls = q, s.calls+1
	if s.err != nil {
		return SkillSummary{}, s.err
	}
	out := s.summary
	if !v.IsAdmin() {
		var own []SkillPersonRow
		for _, p := range out.ByPerson {
			if p.Email == v.Email {
				own = append(own, p)
			}
		}
		out.ByPerson = own
	}
	return out, nil
}

func (f *fakeStore) SkillInvocations(_ context.Context, v Viewer, q SkillFilter) (SkillInvocationPage, error) {
	s := f.skills()
	s.lastFilter, s.calls = q, s.calls+1
	if s.err != nil {
		return SkillInvocationPage{}, s.err
	}
	if q.Cursor == "bad" {
		return SkillInvocationPage{}, ErrInvalidCursor
	}
	var out []SkillInvocation
	for _, r := range s.rows {
		if !v.IsAdmin() && (r.ActorEmail == nil || *r.ActorEmail != v.Email) {
			continue
		}
		out = append(out, r)
	}
	page := SkillInvocationPage{Invocations: out}
	if q.Limit > 0 && len(out) > q.Limit {
		page.Invocations, page.NextCursor = out[:q.Limit], "store-cursor"
	}
	return page, nil
}

func (f *fakeStore) SkillUnused(_ context.Context, _ Viewer, q SkillFilter) ([]SkillUnusedRow, error) {
	s := f.skills()
	s.lastFilter, s.calls = q, s.calls+1
	return s.unused, s.err
}

func (f *fakeStore) SkillUnknown(_ context.Context, v Viewer, q SkillFilter) ([]SkillUnknownRow, error) {
	s := f.skills()
	s.lastFilter, s.calls = q, s.calls+1
	if s.err != nil {
		return nil, s.err
	}
	if v.IsAdmin() {
		return s.unknown, nil
	}
	var out []SkillUnknownRow
	for _, r := range s.unknown {
		r.RawName, r.Suggested = "", nil
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeStore) SkillPruning(_ context.Context, _ Viewer, q SkillFilter) ([]SkillPruningRow, error) {
	s := f.skills()
	s.lastFilter, s.calls = q, s.calls+1
	return s.pruning, s.err
}

func (f *fakeStore) SkillCompliance(_ context.Context, v Viewer, q SkillFilter) ([]SkillComplianceRow, error) {
	s := f.skills()
	s.lastFilter, s.calls = q, s.calls+1
	if !v.IsAdmin() {
		return nil, ErrNotFound
	}
	return s.compliance, s.err
}

func (f *fakeStore) PublishSkillCatalog(_ context.Context, tokenID, sourceRepo string, body CatalogBody) (PublishResult, error) {
	s := f.skills()
	s.publishTok = tokenID
	s.published = append(s.published, body)
	if s.publishErr != nil {
		return PublishResult{}, s.publishErr
	}
	return s.publish, nil
}

func skillSeeded() *fakeStore {
	f := seeded()
	last := testNow.Add(-time.Hour)
	ann, bea := "member@example.com", "other@example.com"
	f.withSkills(&fakeSkills{
		summary: SkillSummary{From: testNow.Add(-30 * 24 * time.Hour), To: testNow,
			Totals:     SkillTotals{Invocations: 12, Skills: 3, People: 2, Claimed: 4},
			BySkill:    []SkillSkillRow{{Lineage: "example-skills/engg:git", SourceRepo: "example-skills", Plugin: "engg", Skill: "git", Invocations: 9, People: 2, Platforms: []string{"claude_code"}, Repos: []string{"backend"}, LastUsedAt: &last}},
			ByPlatform: []SkillPlatformRow{{Platform: "claude_code", Origin: "derived", Trust: "device", Invocations: 12, Skills: 3, People: 2}},
			ByPerson:   []SkillPersonRow{{Email: ann, Invocations: 8, Skills: 2, TopSkill: "engg:git", Platforms: []string{"claude_code"}}, {Email: bea, Invocations: 4, Skills: 1, TopSkill: "engg:git", Platforms: []string{"claude_code"}}},
			ByRepo:     []SkillRepoRow{{Repo: "backend", Lineage: "example-skills/engg:git", Invocations: 12}},
		},
		rows: []SkillInvocation{
			{ID: 3, OccurredAt: last, Origin: "derived", AgentPlatform: "claude_code", Trust: "device", RawName: "engg:git", Plugin: "engg", ActorEmail: &ann, SessionRef: "own-1", Lineage: "example-skills/engg:git"},
			{ID: 2, OccurredAt: last.Add(-time.Minute), Origin: "derived", AgentPlatform: "claude_code", Trust: "device", RawName: "engg:git", Plugin: "engg", ActorEmail: &ann, SessionRef: "own-1"},
			{ID: 1, OccurredAt: last.Add(-2 * time.Minute), Origin: "derived", AgentPlatform: "claude_code", Trust: "device", RawName: "engg:git", Plugin: "engg", ActorEmail: &bea, SessionRef: "other-1"},
		},
		unused:     []SkillUnusedRow{{SourceRepo: "example-skills", Plugin: "engg", Skill: "temporal", Lineage: "example-skills/engg:temporal", AuthoredBy: "unknown", Mirrored: true, Installable: true, FirstSeenAt: testNow.Add(-100 * 24 * time.Hour), DaysInCatalog: 100}},
		unknown:    []SkillUnknownRow{{RawName: "/plan", Platform: "claude_code", Origin: "derived", Count: 4, FirstSeen: last, LastSeen: last, Suggested: &SkillSuggestion{SourceRepo: "example-skills", Plugin: "engg", Skill: "plan"}}},
		pruning:    []SkillPruningRow{{SourceRepo: "example-skills", Plugin: "engg", Skill: "temporal", AuthoredBy: "agent", DaysUnused: 100, DaysInCatalog: 100, ProposedAction: "archive_pr"}},
		compliance: []SkillComplianceRow{{Day: testNow.Truncate(24 * time.Hour), Platform: "devin", Lineage: "example-skills/engg:git", BeaconRows: 4}},
	})
	return f
}

func skillGet(t *testing.T, f *fakeStore, email, target string) (int, map[string]any) {
	t.Helper()
	h := newHandler(t, f, &fakeAuth{email: email})
	w := do(t, h, http.MethodGet, target, "")
	if w.Code != http.StatusOK {
		return w.Code, nil
	}
	return w.Code, decodeBody[map[string]any](t, w)
}

// Every route answers its 6.2 document to an admin, with every array an
// array.
func TestSkillRoutesAnswerTheirDocuments(t *testing.T) {
	f := skillSeeded()
	for _, c := range []struct{ target, key string }{
		{"/v1/skills/summary", "by_bucket"},
		{"/v1/skills/invocations", "invocations"},
		{"/v1/skills/catalog/unused", "entries"},
		{"/v1/skills/unknown", "names"},
		{"/v1/skills/pruning", "candidates"},
		{"/v1/skills/compliance", "days"},
	} {
		t.Run(c.target, func(t *testing.T) {
			code, body := skillGet(t, f, "admin@example.com", c.target)
			if code != http.StatusOK {
				t.Fatalf("status = %d", code)
			}
			if _, ok := body[c.key].([]any); !ok {
				t.Errorf("%s is %T, want an array", c.key, body[c.key])
			}
		})
	}
	_, sum := skillGet(t, f, "admin@example.com", "/v1/skills/summary")
	totals := sum["totals"].(map[string]any)
	for _, k := range []string{"invocations", "skills", "people", "sessions", "user", "agent", "success", "error", "started", "claimed"} {
		if _, ok := totals[k]; !ok {
			t.Errorf("totals lacks %s", k)
		}
	}
	for _, k := range []string{"from", "to", "by_bucket", "by_skill", "by_platform", "by_person", "by_repo"} {
		if _, ok := sum[k]; !ok {
			t.Errorf("the summary lacks %s", k)
		}
	}
	row := sum["by_skill"].([]any)[0].(map[string]any)
	for _, k := range []string{"lineage", "source_repo", "plugin", "skill", "invocations", "people", "sessions", "user", "agent", "error", "platforms", "repos", "last_used_at"} {
		if _, ok := row[k]; !ok {
			t.Errorf("a by_skill row lacks %s", k)
		}
	}
	if _, ok := row["copies"]; ok {
		t.Error("a by_skill row carries copies, which the 6.2 shape does not")
	}
	if people := sum["by_person"].([]any); len(people) != 2 {
		t.Errorf("an admin's by_person has %d rows, want everyone", len(people))
	}
	_, inv := skillGet(t, f, "admin@example.com", "/v1/skills/invocations")
	first := inv["invocations"].([]any)[0].(map[string]any)
	for _, banned := range []string{"dedupe_key", "device_id", "source_token_id", "preempted_by", "preempted_device"} {
		if _, ok := first[banned]; ok {
			t.Errorf("a listed row carries %s", banned)
		}
	}
	if _, ok := first["lineage"]; !ok {
		t.Error("a listed row lacks lineage")
	}
	// A summary with nothing in it is arrays, not nulls.
	f.skills().summary = SkillSummary{}
	_, empty := skillGet(t, f, "admin@example.com", "/v1/skills/summary")
	for _, k := range []string{"by_bucket", "by_skill", "by_platform", "by_person", "by_repo"} {
		if _, ok := empty[k].([]any); !ok {
			t.Errorf("an empty summary's %s is %T", k, empty[k])
		}
	}
}

// A member: the fleet aggregates, their own person row, their own listed
// rows, nameless unknowns, and a 404 on compliance before any store call.
func TestSkillRoutesScopeAMember(t *testing.T) {
	f := skillSeeded()
	_, sum := skillGet(t, f, "member@example.com", "/v1/skills/summary")
	if sum["totals"].(map[string]any)["invocations"] != float64(12) {
		t.Error("a member does not get the fleet totals")
	}
	people := sum["by_person"].([]any)
	if len(people) != 1 || people[0].(map[string]any)["email"] != "member@example.com" {
		t.Errorf("a member's by_person = %v, want their own row alone", people)
	}
	_, inv := skillGet(t, f, "member@example.com", "/v1/skills/invocations")
	rows := inv["invocations"].([]any)
	if len(rows) != 2 {
		t.Errorf("a member lists %d rows, want their own two", len(rows))
	}
	_, unk := skillGet(t, f, "member@example.com", "/v1/skills/unknown")
	names := unk["names"].([]any)
	if len(names) != 1 || names[0].(map[string]any)["raw_name"] != "" || names[0].(map[string]any)["suggested"] != nil {
		t.Errorf("a member's unknown names = %v, want nameless counts", names)
	}
	_, unk = skillGet(t, f, "admin@example.com", "/v1/skills/unknown")
	if unk["names"].([]any)[0].(map[string]any)["raw_name"] != "/plan" {
		t.Error("an admin's unknown names lack the name")
	}
	calls := f.skills().calls
	h := newHandler(t, f, &fakeAuth{email: "member@example.com"})
	w := do(t, h, http.MethodGet, "/v1/skills/compliance", "")
	if w.Code != http.StatusNotFound || decodeBody[errorBody](t, w).Error.Code != "not_found" {
		t.Fatalf("a member's compliance = %d %s", w.Code, w.Body.String())
	}
	if f.skills().calls != calls {
		t.Error("a member's compliance read reached the store")
	}
	// The pruning report is a catalog fact and open to members.
	if code, _ := skillGet(t, f, "member@example.com", "/v1/skills/pruning"); code != http.StatusOK {
		t.Errorf("a member's pruning = %d", code)
	}
}

// The common filter reaches the store as parsed; a bad value is refused at
// the edge and never reaches the store.
func TestSkillFilterParsingAndRefusals(t *testing.T) {
	f := skillSeeded()
	code, _ := skillGet(t, f, "admin@example.com", "/v1/skills/summary?range=7d&platform=devin&repo=backend&types=user,automation&trigger=agent&trust=device&include_claimed=1&tz=Asia/Kolkata")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	got := f.skills().lastFilter
	want := SkillFilter{Range: "7d", Platform: "devin", Repo: "backend", Types: []string{"user", "automation"}, Trigger: "agent", Trust: "device", IncludeClaimed: true, TZ: "Asia/Kolkata"}
	if got.Range != want.Range || got.Platform != want.Platform || got.Repo != want.Repo || strings.Join(got.Types, ",") != "user,automation" || got.Trigger != want.Trigger || got.Trust != want.Trust || !got.IncludeClaimed || got.TZ != want.TZ {
		t.Errorf("filter = %+v, want %+v", got, want)
	}
	code, _ = skillGet(t, f, "admin@example.com", "/v1/skills/invocations?skill=engg:git&email=Other@Example.com&session_ref=own-1&limit=1")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	got = f.skills().lastFilter
	if got.Skill != "engg:git" || got.Email != "other@example.com" || got.SessionRef != "own-1" || got.Limit != 1 {
		t.Errorf("listing filter = %+v", got)
	}
	code, _ = skillGet(t, f, "admin@example.com", "/v1/skills/pruning?since=30d&source_repo=backend")
	if code != http.StatusOK || f.skills().lastFilter.Since != "30d" || f.skills().lastFilter.SourceRepo != "backend" {
		t.Errorf("pruning filter = %d %+v", code, f.skills().lastFilter)
	}
	calls := f.skills().calls
	for _, c := range []struct{ target, field string }{
		{"/v1/skills/summary?range=1y", "range"},
		{"/v1/skills/summary?include_claimed=2", "include_claimed"},
		{"/v1/skills/summary?tz=Mars/Olympus", "tz"},
		{"/v1/skills/catalog/unused?since=2w", "since"},
		{"/v1/skills/invocations?limit=0", "limit"},
	} {
		h := newHandler(t, f, &fakeAuth{email: "admin@example.com"})
		w := do(t, h, http.MethodGet, c.target, "")
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", c.target, w.Code)
			continue
		}
		if msg := decodeBody[errorBody](t, w).Error.Message; !strings.HasPrefix(msg, c.field+":") {
			t.Errorf("%s: message %q does not name %s", c.target, msg, c.field)
		}
	}
	if f.skills().calls != calls {
		t.Error("a refused filter reached the store")
	}
}

// The listing's cursor is scoped to the viewer and the query: a page's
// cursor continues the same query, and one from another query or another
// person is refused as invalid.
func TestSkillInvocationsCursorIsScoped(t *testing.T) {
	f := skillSeeded()
	h := newHandler(t, f, &fakeAuth{email: "admin@example.com"})
	w := do(t, h, http.MethodGet, "/v1/skills/invocations?limit=2", "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Body.String())
	}
	page := decodeBody[skillInvocationsResponse](t, w)
	if len(page.Invocations) != 2 || page.NextCursor == "" {
		t.Fatalf("page = %+v", page)
	}
	w = do(t, h, http.MethodGet, "/v1/skills/invocations?limit=2&cursor="+page.NextCursor, "")
	if w.Code != http.StatusOK || f.skills().lastFilter.Cursor != "store-cursor" {
		t.Errorf("the next page: %d, store cursor %q", w.Code, f.skills().lastFilter.Cursor)
	}
	for _, target := range []string{
		"/v1/skills/invocations?limit=2&platform=devin&cursor=" + page.NextCursor,
		"/v1/skills/invocations?limit=2&cursor=not-a-cursor",
	} {
		w = do(t, h, http.MethodGet, target, "")
		if w.Code != http.StatusBadRequest || decodeBody[errorBody](t, w).Error.Code != "invalid_cursor" {
			t.Errorf("%s: %d %s", target, w.Code, w.Body.String())
		}
	}
	other := newHandler(t, f, &fakeAuth{email: "member@example.com"})
	w = do(t, other, http.MethodGet, "/v1/skills/invocations?limit=2&cursor="+page.NextCursor, "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("a colleague's cursor was accepted: %d", w.Code)
	}
}

// A storage failure is a 500 with no detail; a store's not-found is a 404;
// no cookie is 401.
func TestSkillRoutesTranslateStoreAnswers(t *testing.T) {
	f := skillSeeded()
	f.skills().err = errors.New("connection refused")
	h := newHandler(t, f, &fakeAuth{email: "admin@example.com"})
	for _, target := range []string{"/v1/skills/summary", "/v1/skills/invocations", "/v1/skills/catalog/unused", "/v1/skills/unknown", "/v1/skills/pruning", "/v1/skills/compliance"} {
		w := do(t, h, http.MethodGet, target, "")
		if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "connection refused") {
			t.Errorf("%s: %d %s", target, w.Code, w.Body.String())
		}
	}
	f.skills().err = ErrNotFound
	if w := do(t, h, http.MethodGet, "/v1/skills/summary", ""); w.Code != http.StatusNotFound {
		t.Errorf("a store not-found = %d", w.Code)
	}
	h = newHandler(t, f, &fakeAuth{err: ErrNoIdentity})
	if w := do(t, h, http.MethodGet, "/v1/skills/summary", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no cookie = %d", w.Code)
	}
}
