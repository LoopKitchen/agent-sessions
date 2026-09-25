package web

import (
	"strings"
	"testing"
	"time"
)

// The list page defaults to sessions people typed. The automations that watch
// this fleet outnumber the people, and a default that mixes them buries every
// real session under hourly pings.
// TestSessionListDefaultsToTheStoreDefault: with no types parameter the list
// hands the store nil and gets its default view (people and automations,
// plus capture-loss rows of the hidden classes); an explicit selection
// narrows, a mixed view marks each row, and an unknown value falls back to
// the default rather than erroring or leaking.
func TestSessionListDefaultsToTheStoreDefault(t *testing.T) {
	f := newFilterData()
	f.all = append(f.all, Session{
		ID: "s-x", Email: "ann@example.com", Repo: "cron", Source: "claude_code",
		Type: "automation", StartedAt: fltDay(6, 9), FirstPrompt: "Reply with exactly: pong",
	})
	s := fltServer(t, f, Viewer{Email: "ann@example.com", Admin: true})

	rec := fltGet(t, s, "/sessions")
	if got := f.lastQuery.Types; got != nil {
		t.Fatalf("default query types = %v, want nil (the store default)", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "s-x") {
		t.Error("an automation session is missing from the store's default view")
	}
	// In the mixed default view the row says which kind it is.
	if !strings.Contains(body, "tag-auto") {
		t.Error("the automation row carries no marker in a mixed listing")
	}
	// The menu offers the hidden classes as toggle links.
	if !strings.Contains(body, `href="/sessions?types=user%2Cinternal%2Cautomation"`) || !strings.Contains(body, `href="/sessions?types=user%2Cautomation%2Cempty"`) {
		t.Error("no links to add internal or empty to the selection")
	}

	rec = fltGet(t, s, "/sessions?types=user")
	if got := f.lastQuery.Types; len(got) != 1 || got[0] != "user" {
		t.Fatalf("types=user reached the query as %v", got)
	}
	if body = rec.Body.String(); strings.Contains(body, "s-x") {
		t.Error("an automation session is listed under the user-only filter")
	}

	rec = fltGet(t, s, "/sessions?types=automation")
	body = rec.Body.String()
	if strings.Contains(body, "s-a") || !strings.Contains(body, "s-x") {
		t.Error("the automation-only view listed the wrong sessions")
	}

	// A typed search obeys the same selection: a type the list suppresses must
	// not resurface as a transcript hit on the same page.
	fltGet(t, s, "/sessions?q=pong&types=user")
	if n := len(f.searches); n == 0 {
		t.Fatal("a typed query ran no search")
	} else if got := f.searches[n-1].Types; len(got) != 1 || got[0] != "user" {
		t.Errorf("the search ran with types %v, want the list's [user]", got)
	}

	// Unknown values fall back to the default rather than erroring or leaking.
	fltGet(t, s, "/sessions?types=robot")
	if got := f.lastQuery.Types; got != nil {
		t.Errorf("an unknown types value reached the query as %v", got)
	}
}

// Deselecting the last type flips to the others rather than selecting nothing:
// a filter that can select nothing renders a page that can only explain itself
// with an empty table.
func TestToggleTypeNeverGoesEmpty(t *testing.T) {
	got := toggleType([]string{"user"}, "user", sessionTypeOptions)
	if !typesEqual(got, []string{"internal", "automation", "empty"}) {
		t.Errorf("deselecting the only type gave %v, want the other three", got)
	}
	got = toggleType([]string{"user"}, "automation", sessionTypeOptions)
	if !typesEqual(got, []string{"user", "automation"}) {
		t.Errorf("adding a second type gave %v, want [user automation] in menu order", got)
	}
	got = toggleType([]string{"user", "automation"}, "automation", sessionTypeOptions)
	if len(got) != 1 || got[0] != "user" {
		t.Errorf("removing one of two gave %v, want [user]", got)
	}
}

// The menu offers exactly the lattice the store holds, in its order, and the
// labels stay short enough for a row tag.
func TestSessionTypeOptionsCoverTheLattice(t *testing.T) {
	if !typesEqual(SessionTypeOptions(), []string{"user", "internal", "automation", "empty"}) {
		t.Errorf("options = %v", SessionTypeOptions())
	}
	for _, tc := range []struct{ in, want string }{
		{"user", "user-initiated"}, {"internal", "internal"}, {"automation", "automation"}, {"empty", "empty"},
	} {
		if got := SessionTypeLabel(tc.in); got != tc.want {
			t.Errorf("SessionTypeLabel(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Analytics defaults to every type: the page answers "what did the fleet
// spend", and automation is half that answer. The sessions and cost charts
// stack the split; the tokens chart and the person component carry their own
// selections.
func TestAnalyticsTypeSelections(t *testing.T) {
	f := newFake()
	day := time.Date(2026, 8, 3, 0, 0, 0, 0, time.Local)
	f.usage = []DayUsage{{Day: day, Sessions: 10, TokensIn: 1000, TokensOut: 200, CostUSD: 5}}
	f.byType = []DayUsage{
		{Day: day, SessionType: "user", Sessions: 4, CostUSD: 3},
		{Day: day, SessionType: "automation", Sessions: 6, CostUSD: 2},
	}
	f.perPerson = []PersonUsage{{Email: "ann@example.com", Sessions: 4, TokensIn: 1000, LastActive: day}}
	s := newServer(t, f, Viewer{Email: "ann@example.com", Admin: true})

	rec := fltGet(t, s, "/analytics")
	body := rec.Body.String()
	// The full selection reaches the store as the full list, never as an
	// empty one: an empty list is the store's own default, which is narrower
	// than "all types", and a panel whose menu says all types must not
	// quietly count fewer.
	if !typesEqual(f.personTypes, sessionTypeOptions) {
		t.Errorf("the default person selection reached the store as %v, want every type", f.personTypes)
	}
	if !typesEqual(f.tokenTypes, sessionTypeOptions) {
		t.Errorf("the headline totals and tokens series were read with types %v, want every type", f.tokenTypes)
	}
	// Both stacked series drew: bars in the user colour and the automation
	// colour, and a legend naming them.
	if !strings.Contains(body, `class="bar c0"`) || !strings.Contains(body, `class="bar c1"`) {
		t.Error("the sessions chart did not stack both types")
	}
	if !strings.Contains(body, "user-initiated") || !strings.Contains(body, "automation") {
		t.Error("the stacked charts carry no legend naming the types")
	}
	// Both panels offer their own type menu.
	if !strings.Contains(body, `id="tokens"`) || strings.Count(body, "menu-mini") < 2 {
		t.Error("the tokens panel and the person component do not each carry a type menu")
	}

	fltGet(t, s, "/analytics?tt=automation")
	if len(f.tokenTypes) != 1 || f.tokenTypes[0] != "automation" {
		t.Errorf("tt=automation reached the tokens query as %v", f.tokenTypes)
	}

	fltGet(t, s, "/analytics?pt=user")
	if len(f.personTypes) != 1 || f.personTypes[0] != "user" {
		t.Errorf("pt=user reached the person query as %v", f.personTypes)
	}
	if len(f.personSeriesTypes) != 1 || f.personSeriesTypes[0] != "user" {
		t.Errorf("pt=user reached the person series query as %v", f.personSeriesTypes)
	}
}

// Every navigation on the analytics page carries the type selections, so
// switching range or metric does not silently reset a narrowed panel.
func TestAnalyticsURLCarriesTypeSelections(t *testing.T) {
	v := analyticsView{Range: "7d", Sort: "tokens", Top: 25, PersonMetric: "tokens",
		TokenTypes: []string{"automation"}, PersonTypes: SessionTypeOptions()}
	url := v.RangeURL("30d")
	if !strings.Contains(url, "tt=automation") {
		t.Errorf("switching range dropped the tokens selection: %s", url)
	}
	// The full set is the default and stays out of the URL.
	if strings.Contains(url, "pt=") {
		t.Errorf("the default person selection leaked into the URL: %s", url)
	}
}

// The stacked layout accumulates segments and scales to the bucket sum; a
// max taken per-series would clip the stack at the tallest single type.
func TestLayoutChartStacksBars(t *testing.T) {
	days := []time.Time{time.Now(), time.Now().Add(24 * time.Hour)}
	cv := layoutChart(chartSpec{
		Days: days, Kind: "day", Bars: true, Stacked: true,
		Ser: []series{
			{Label: "user", Points: []float64{60, 0}, Color: 0},
			{Label: "automation", Points: []float64{40, 0}, Color: 1},
		},
	})
	if len(cv.Bars) != 3 {
		t.Fatalf("drew %d bars, want 2 stacked segments plus 1 zero sliver", len(cv.Bars))
	}
	// The stack sums to 100, so the top gridline label must fit 100, not 60.
	if cv.YTop != "100" {
		t.Errorf("y scale label %q, want the bucket sum 100", cv.YTop)
	}
	// Segments abut: the lower segment's top is the upper segment's bottom.
	lower, upper := cv.Bars[0], cv.Bars[1]
	if lower.Color == upper.Color {
		t.Fatal("both segments drew in one colour")
	}
	if got, want := upper.Y+upper.H, lower.Y; got != want {
		t.Errorf("segments do not abut: upper bottom %v, lower top %v", got, want)
	}
}
