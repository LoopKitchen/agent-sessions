package web

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func analyticsFixture() *fakeData {
	f := newFake()
	day := fixedNow.Add(-24 * time.Hour)
	f.usage = []DayUsage{
		{Day: day, Sessions: 12, People: 3, TokensIn: 1_000_000, TokensOut: 50_000,
			CostUSD: 12.5, ToolCalls: 400, Errors: 8},
		{Day: fixedNow, Sessions: 5, People: 2, TokensIn: 400_000, TokensOut: 20_000,
			CostUSD: 4.5, ToolCalls: 150, Errors: 2},
		// Per-person series rows for the comparison chart.
		{Day: day, Email: "ann@example.com", Sessions: 8, TokensIn: 700_000},
		{Day: day, Email: "bea@example.com", Sessions: 4, TokensIn: 300_000},
	}
	f.perPerson = []PersonUsage{
		{Email: "ann@example.com", DisplayName: "Ann", Sessions: 8, TokensIn: 700_000,
			TokensOut: 30_000, CostUSD: 9, ToolCalls: 250, Errors: 5, LastActive: fixedNow},
		{Email: "bea@example.com", DisplayName: "Bea", Sessions: 4, TokensIn: 300_000,
			TokensOut: 20_000, CostUSD: 3.5, ToolCalls: 150, Errors: 3, LastActive: day},
	}
	f.peopleOptions = []string{"ann@example.com", "bea@example.com"}
	f.repoOptions = []string{"acme/api"}
	return f
}

// The page exists to answer "what did this cost and who spent it"; the KPI row
// and the person table are that answer.
func TestAnalyticsShowsTotalsAndThePersonTable(t *testing.T) {
	f := analyticsFixture()
	srv := newServer(t, f, Viewer{Email: "admin@example.com", Admin: true})

	rec := get(t, srv, "/analytics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"17",         // total sessions across both days
		"1.4M",       // tokens in, compressed the way the site writes them
		"$17.00",     // total cost
		"Ann", "Bea", // the people
		"tool failure",        // the failure-rate card
		`class="combo-panel"`, // the people filter, themed and pruning
		"<svg",                // the charts are actually drawn
		"Viewing by person",   // the person component
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the analytics page does not contain %q", want)
		}
	}
	// The tabular chart views were removed on request: the person table is the
	// tabular view that earns its space, and three more collapsed tables were
	// furniture.
	if strings.Contains(body, "As a table") {
		t.Error("a system chart still offers a tabular view")
	}
}

// A member's analytics is their own. The fake enforces scope the way the store
// does, so this test fails if the handler ever bypasses the scoped read.
func TestAnalyticsForAMemberShowsOnlyTheirOwnNumbers(t *testing.T) {
	f := analyticsFixture()
	srv := newServer(t, f, Viewer{Email: "ann@example.com"})

	rec := get(t, srv, "/analytics")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "Bea") || strings.Contains(body, "bea@example.com") {
		t.Error("a member's analytics page names a colleague")
	}
	if !strings.Contains(body, "Your own usage") {
		t.Error("the page does not say it is scoped to the member")
	}
}

// The person chart is part of the table's component: one pre-rendered line per
// top row, visibility driven by the row checkboxes in CSS. The server's half of
// that contract is drawing every line and checking the first five boxes.
func TestThePersonChartIsAttachedToTheTable(t *testing.T) {
	f := analyticsFixture()
	srv := newServer(t, f, Viewer{Email: "admin@example.com", Admin: true})

	body := get(t, srv, "/analytics").Body.String()
	if !strings.Contains(body, "Viewing by person") {
		t.Fatal("the person component did not render")
	}
	// Every top row has a line, whatever is checked: CSS shows and hides them.
	if !strings.Contains(body, `class="line c0"`) || !strings.Contains(body, `class="line c1"`) {
		t.Error("the person chart does not pre-render one line per row")
	}
	// Rows carry the classes the stylesheet's :has() rules key on.
	if !strings.Contains(body, `class="prow0"`) || !strings.Contains(body, `class="prow1"`) {
		t.Error("table rows are missing their toggle classes")
	}
	// The old flow's button must be gone: ticking a box IS the interaction.
	if strings.Contains(body, "Compare selected") {
		t.Error("the compare button survived; selection is supposed to sync by itself")
	}
	// Two people exist, both inside the default five, so both arrive checked.
	if strings.Count(body, `type="checkbox" checked`) != 2 {
		t.Errorf("want both rows checked by default, body has %d checked boxes",
			strings.Count(body, `type="checkbox" checked`))
	}
	// The metric tabs survive with the rest of the state.
	if !strings.Contains(body, "metric=cost") {
		t.Error("the metric tabs are missing")
	}
}

// The bucket width follows the range so the segment count stays small and the
// chart fills the page: hours for a day, days for a week, two-day chunks for a
// month, weeks for a quarter.
func TestAnalyticsBucketsFollowTheRange(t *testing.T) {
	f := analyticsFixture()
	srv := newServer(t, f, Viewer{Email: "admin@example.com", Admin: true})

	for rng, noun := range map[string]string{
		"1d":  "Sessions per hour",
		"7d":  "Sessions per day",
		"30d": "Sessions per 2 days",
		"90d": "Sessions per week",
	} {
		body := get(t, srv, "/analytics?range="+rng).Body.String()
		if !strings.Contains(body, noun) {
			t.Errorf("range %s: caption %q missing", rng, noun)
		}
	}
	// Every bucket gets a hover column: crosshair rule plus value panel, the
	// standard shared-tooltip shape. 90 days in weekly buckets is 13 of them.
	weekly := get(t, srv, "/analytics?range=90d").Body.String()
	if got := strings.Count(weekly, `class="hit-zone"`); got == 0 {
		t.Fatal("charts have no hover columns")
	}
	if !strings.Contains(weekly, `class="tipbox"`) || !strings.Contains(weekly, `class="xline"`) {
		t.Error("hover columns are missing the value panel or the crosshair rule")
	}
	// And the axis captions cannot collide: at most one caption per bucket.
	if lbls := strings.Count(weekly, `class="xlab"`); lbls > 13*4 {
		t.Errorf("suspiciously many x captions: %d", lbls)
	}
}

// The range tabs change the window and say so.
func TestAnalyticsRangeSelection(t *testing.T) {
	f := analyticsFixture()
	srv := newServer(t, f, Viewer{Email: "admin@example.com", Admin: true})

	for _, rng := range []string{"1d", "7d", "30d", "90d"} {
		rec := get(t, srv, "/analytics?range="+rng)
		if rec.Code != http.StatusOK {
			t.Fatalf("range %s: status = %d", rng, rec.Code)
		}
	}
	// Nonsense falls back to the default rather than erroring.
	rec := get(t, srv, "/analytics?range=999d")
	if rec.Code != http.StatusOK {
		t.Fatalf("bad range: status = %d, want 200 with the default window", rec.Code)
	}
}

// The prefix filter narrows the table.
func TestAnalyticsPrefixSearchNarrowsThePersonTable(t *testing.T) {
	f := analyticsFixture()
	srv := newServer(t, f, Viewer{Email: "admin@example.com", Admin: true})

	body := get(t, srv, "/analytics?q=ann").Body.String()
	if !strings.Contains(body, "Ann") {
		t.Error("the matching person is missing")
	}
	// The display name only renders in table rows, so its absence is the row's
	// absence. The email cannot be asserted on: the datalist legitimately lists
	// everyone whatever the filter says, that being what an autocomplete is.
	if strings.Contains(body, "Bea") {
		t.Error("a non-matching person survived the prefix filter")
	}
}

// The charts stay a CSS-only construction even though this page may now load a
// script. combo.js exists to prune one dropdown; it does not touch the SVG, and
// the crosshair, the tooltips and the per-person line toggles are still drawn
// server-side and revealed by :hover and :has(). This test is what stops "the
// page has a script budget now" from becoming a charting library: the budget is
// one file, and it is this one.
func TestAnalyticsChartsNeedNoScriptOfTheirOwn(t *testing.T) {
	f := analyticsFixture()
	srv := newServer(t, f, Viewer{Email: "admin@example.com", Admin: true})

	rec := get(t, srv, "/analytics")
	body := rec.Body.String()
	if n := strings.Count(body, "<script"); n != 1 {
		t.Errorf("the analytics page carries %d script tags, want exactly the combo one", n)
	}
	if !strings.Contains(body, "/static/combo.js") {
		t.Error("the one script is not combo.js")
	}
	// Inline script is refused by the policy, so a chart that grew one would
	// fail in the browser rather than here. Assert it never gets written.
	if strings.Contains(body, "<script>") {
		t.Error("the analytics page carries inline script, which the policy refuses")
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("analytics CSP = %q, want same-origin script", csp)
	}
}

// Tooltips carry query-string-derived labels (the compared emails), so the SVG
// path is exactly where an escaping bypass would land. The guard test bans
// template.HTML; this proves the render path escapes for real.
func TestAnalyticsChartTooltipsAreEscaped(t *testing.T) {
	f := analyticsFixture()
	// A hostile display name lands in the table AND in the chart tooltips,
	// which is the path an escaping bypass would take now that labels come
	// from the top rows rather than the query string.
	evil := `<script>alert(1)</script>`
	f.perPerson = append(f.perPerson, PersonUsage{
		Email: "evil@example.com", DisplayName: evil, Sessions: 99, TokensIn: 9_000_000,
	})
	f.usage = append(f.usage, DayUsage{
		Day: fixedNow, Email: "evil@example.com", Sessions: 99, TokensIn: 9_000_000,
	})
	srv := newServer(t, f, Viewer{Email: "admin@example.com", Admin: true})

	body := get(t, srv, "/analytics").Body.String()
	if strings.Contains(body, evil) {
		t.Fatal("a person-derived label reached the page as live markup")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("the escaped name is not present at all")
	}
}

// TestFilterDropdownsAcrossThePages pins which widget the filter bars use.
//
// This assertion has now flipped twice and the history is the point. The bars
// began as themed .combo panels, which looked right and could not do the one
// thing a person typing expects, which is prune the options to what matches.
// On 2026-08-12 they became native input+datalist: the browser prunes for
// free, and free was the only price payable under script-src 'none'. That
// bought function and sold the theme, because a datalist popup is drawn by the
// browser and no stylesheet can reach it.
//
// The owner's call on 2026-08-13 was that both were required. Neither widget
// gives both, so the third answer is a themed panel plus the smallest script
// that can prune it: static/combo.js, ~150 lines, no dependencies, and the
// filter pages alone move to script-src 'self' to load it. Transcript pages
// keep script-src 'none' and ship nothing, which is what
// TestScriptedPagesAndTheirPolicyAgree holds in place.
//
// So: a combo panel on every filter bar, no datalist on any of them, and every
// option still a real link that applies itself with the script blocked.
//
// One datalist survives on purpose, and it is not on a filter bar. The session
// page's mirror-group picker (session.html) sits on a transcript body, which
// keeps script-src 'none' and therefore cannot load combo.js. A themed panel
// there would look right and prune nothing, so that picker keeps the native
// widget: on that one page the browser's popup is the only thing that prunes.
// This test covers the filter pages and deliberately does not reach it.
func TestFilterDropdownsAcrossThePages(t *testing.T) {
	f := analyticsFixture()
	f.seedSession("s1", "admin@example.com")
	srv := newServer(t, f, Viewer{Email: "admin@example.com", Admin: true})

	sessions := get(t, srv, "/sessions").Body.String()
	for _, want := range []string{
		`class="combo"`, `class="combo-panel"`,
		// Options are apply-self links, not <option> values: this is what
		// keeps the component working with the script blocked.
		`>ann@example.com</a>`, `>acme/api</a>`,
		// The reset option has to survive pruning's arrival, because it is
		// how somebody clears a filter from inside the panel.
		`>anyone</a>`, `>any repo</a>`,
	} {
		if !strings.Contains(sessions, want) {
			t.Errorf("the session list is missing %q", want)
		}
	}

	access := get(t, srv, "/admin/access").Body.String()
	if !strings.Contains(access, `class="combo-panel"`) {
		t.Error("the access log filters are not themed combo panels")
	}

	principals := get(t, srv, "/admin/principals").Body.String()
	fleet := get(t, srv, "/admin/fleet").Body.String()
	analytics := get(t, srv, "/analytics").Body.String()

	pages := map[string]string{
		"/sessions": sessions, "/admin/access": access, "/admin/principals": principals,
		"/admin/fleet": fleet, "/analytics": analytics,
	}
	for page, body := range pages {
		// The native popup is gone from every one of them. A single datalist
		// left behind is the failure this names, because it renders as a
		// second, differently-styled dropdown next to the themed one.
		//
		// Matched on the tag alone. An earlier version also looked for the
		// list= attribute, which the session list can fail on for a reason
		// that has nothing to do with dropdowns: its rows print prompt text
		// and search snippets out of real transcripts, so any session whose
		// prompt quoted a URL carrying ?list= would trip a guard about
		// markup using a string that arrived as escaped content. The tag is
		// escaped in that path and cannot be forged from it.
		if strings.Contains(body, "<datalist") {
			t.Errorf("%s still carries a native datalist", page)
		}
		// No Apply buttons anywhere on the reworked filter bars: options apply
		// on click, text applies on Enter.
		if strings.Contains(body, ">Apply</button>") {
			t.Errorf("%s still has an Apply button", page)
		}
	}
}

// TestScriptedPagesAndTheirPolicyAgree walks both halves of the 2026-08-13
// split at once.
//
// The policy lives in Secure's scriptedPaths and the <script> tag lives in each
// template's "scripts" block, which are two files that have to say the same
// thing. Either mismatch is quiet in production: a page that loads the script
// under script-src 'none' gets a console error nobody reads and a dropdown that
// silently stops pruning, and a page that carries 'self' without needing it has
// given up the guarantee for nothing.
//
// It ranges over scriptedPaths itself rather than a copy. A hand-written list
// is a third place to keep in step and it drifts the same day somebody adds a
// page: the first version of this test copied five of the six entries, and the
// one it missed was the settings page, which is exactly the page whose route
// needs a port wired up and is therefore the easiest to leave out.
func TestScriptedPagesAndTheirPolicyAgree(t *testing.T) {
	f := analyticsFixture()
	f.seedSession("s1", "admin@example.com")
	// Slack has to be wired or /settings/notifications is not a route at all,
	// and a 404 would pass the strict half of this test while proving nothing.
	srv, err := New(Options{
		Data:    f,
		Viewer:  func(*http.Request) (Viewer, bool) { return Viewer{Email: "admin@example.com", Admin: true}, true },
		CSRFKey: []byte("test-key-for-signing-form-tokens"),
		Now:     func() time.Time { return fixedNow },
		Slack:   &fakeSlackSettings{view: SlackSettingsView{DigestMode: "off"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Every path the policy relaxes must be a page that actually wants it.
	for path := range scriptedPaths {
		rec := get(t, srv, path)
		if rec.Code != http.StatusOK {
			t.Errorf("%s answered %d; a scripted path that does not render proves nothing", path, rec.Code)
			continue
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") {
			t.Errorf("%s is in scriptedPaths but its policy is %q", path, csp)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "/static/combo.js") {
			t.Errorf("%s carries 'self' but loads no combo.js", path)
		}
		// Exactly one, on every scripted page. The budget these pages gave up
		// script-src 'none' for is one file, and counting here is what makes
		// that a property of the split rather than of the analytics page,
		// which is where the assertion used to live alone. It also narrows the
		// gap where a template body grows a script tag of its own: the page
		// would still load combo.js and still carry the right policy, and
		// nothing but the count would notice.
		if n := strings.Count(body, "<script"); n != 1 {
			t.Errorf("%s carries %d script tags, want exactly combo.js", path, n)
		}
	}

	// And the other direction: no template may define the block without a
	// path to go with it. Counting is enough to catch the drift, because the
	// loop above already proved each scripted path renders one, so a template
	// beyond that count is a page carrying a script under script-src 'none'.
	files, err := filepath.Glob(filepath.Join("templates", "*.html"))
	if err != nil {
		t.Fatal(err)
	}
	var withBlock []string
	for _, name := range files {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `{{define "scripts"}}`) {
			withBlock = append(withBlock, filepath.Base(name))
		}
	}
	if len(withBlock) != len(scriptedPaths) {
		t.Errorf("%d templates define a scripts block %v, but scriptedPaths has %d entries; "+
			"a template with the block and no path loads a script under script-src 'none'",
			len(withBlock), withBlock, len(scriptedPaths))
	}

	// The strict half. Both of these print what an agent read back from
	// somewhere else, and neither may run anything. ev2 is the seeded prompt
	// containing a literal <script> tag, so it is the page the strict half of
	// the policy exists for.
	for _, path := range []string{"/sessions/s1", "/sessions/s1/events/ev2"} {
		rec := get(t, srv, path)
		// Without this a 404 passes: error.html defines no scripts block and
		// would satisfy everything below while naming no real page.
		if rec.Code != http.StatusOK {
			t.Errorf("%s answered %d, so the assertions below prove nothing", path, rec.Code)
			continue
		}
		if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'none'") {
			t.Errorf("%s renders a transcript body under %q", path, csp)
		}
		if strings.Contains(rec.Body.String(), "<script") {
			t.Errorf("%s ships a script tag under script-src 'none'", path)
		}
	}
}

// The fleet page judges every machine against the serving build: latest, stale
// with the date its version arrived, or unknown when it has never reported.
// This replaces a bare hash rollup nobody could act on — a hash answers which
// build, a badge and a date answer the question people actually have, which is
// whether the upgrade has reached everyone and how long the stragglers have
// been out.
func TestFleetJudgesVersionsAgainstTheServingBuild(t *testing.T) {
	f := analyticsFixture()
	f.seedFleet()
	f.fleet.Machines[0].Report.AgentVersion = "f9228f7" // matches the server
	f.fleet.VersionFirstSeen = map[string]time.Time{
		"f9228f7": fixedNow.Add(-2 * time.Hour),
	}
	srv, err := New(Options{
		Data:      f,
		Viewer:    func(*http.Request) (Viewer, bool) { return Viewer{Email: "admin@x", Admin: true}, true },
		CSRFKey:   []byte("test-key-for-signing-form-tokens"),
		Now:       func() time.Time { return fixedNow },
		Version:   "f9228f7",
		BuildDate: fixedNow.Add(-3 * time.Hour),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	body := get(t, srv, "/admin/fleet").Body.String()
	if !strings.Contains(body, "Latest build") || !strings.Contains(body, "f9228f7") {
		t.Fatal("the fleet page does not name the serving build")
	}
	// The strip is just the latest build and its timestamp — the per-version
	// chips were hashes nobody could act on, and the rows carry the verdicts.
	if strings.Contains(body, "not yet reported by any machine") {
		t.Error("the strip still carries the not-yet-reported clause")
	}
	if !strings.Contains(body, "built 2026-08-04") {
		t.Error("the strip does not carry the build timestamp")
	}
	if !strings.Contains(body, `Running the serving build.">f9228f7<`) {
		t.Error("the latest machine's tablet does not carry its hash; the colour is the verdict and the hash is the content")
	}
	// The silent machine has never reported a version; it must read unknown
	// rather than stale, because stale is a claim about a version we know.
	if !strings.Contains(body, `pill-ver-unknown`) {
		t.Error("a never-reporting machine is not badged unknown")
	}
	// And no Apply button here either.
	if strings.Contains(body, ">Apply</button>") {
		t.Error("the fleet page still has an Apply button")
	}
}

// The review findings, pinned. Each of these was found by adversarial review
// after the first version shipped the defect, so each gets a test that fails
// against that version.

// fillBuckets must bucket by calendar, not by wall-clock division: a DST zone
// has 23- and 25-hour days, and the arithmetic version dropped the last hour
// of a fall-back day and folded two days into one bucket after spring-forward.
func TestFillBucketsSurvivesDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}

	// Fall back: 2026-11-01 is a 25-hour day. A row at clock-face 23:00 must
	// land in the day's bucket, not be dropped past the end of the range.
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, ny)
	row := DayUsage{Day: time.Date(2026, 11, 1, 23, 0, 0, 0, time.UTC), Sessions: 5}
	got := fillBuckets([]DayUsage{row}, from, 24, time.Hour)
	var kept int64
	for _, b := range got {
		kept += b.Sessions
	}
	if kept != 5 {
		t.Errorf("fall-back day: %d of 5 sessions survived bucketing", kept)
	}

	// Spring forward: 2026-03-08 is a 23-hour day. With daily buckets over a
	// week, the day after the transition must land in its own bucket, not fold
	// into its neighbour, and every later day must not shift.
	from = time.Date(2026, 3, 6, 0, 0, 0, 0, ny)
	rows := []DayUsage{
		{Day: time.Date(2026, 3, 9, 0, 0, 0, 0, time.UTC), Sessions: 3},  // day 3
		{Day: time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC), Sessions: 7}, // day 6
	}
	got = fillBuckets(rows, from, 7, 24*time.Hour)
	if got[3].Sessions != 3 {
		t.Errorf("post-transition day landed in bucket %v, want index 3 to hold 3 sessions: %+v",
			got, got[3])
	}
	if got[6].Sessions != 7 {
		t.Errorf("newest day shifted: bucket 6 holds %d, want 7", got[6].Sessions)
	}
}

// Thirteen weekly buckets tile 91 days; the window must therefore span 91, or
// the newest week is permanently one day underweight — a built-in ~14%% dip on
// the newest bar that reads as a real decline.
func TestNinetyDayWindowTilesItsWeeklyBucketsExactly(t *testing.T) {
	var plan rangeDef
	for _, rd := range analyticsRanges {
		if rd.Key == "90d" {
			plan = rd
		}
	}
	if plan.Count*int(plan.Step/(24*time.Hour)) != plan.Days {
		t.Fatalf("%d buckets of %v do not tile a %d-day window",
			plan.Count, plan.Step, plan.Days)
	}
}

// Enter has to actually submit. WHATWG implicit submission does nothing in a
// form with no submit button and more than one text-like field, and both these
// filter bars have five — the first version promised "Enter applies" in its
// placeholders while Enter was a no-op.
func TestMultiFieldFilterBarsCanSubmitOnEnter(t *testing.T) {
	f := analyticsFixture()
	f.seedSession("s1", "admin@example.com")
	srv := newServer(t, f, Viewer{Email: "admin@example.com", Admin: true})

	for _, path := range []string{"/sessions", "/admin/access"} {
		body := get(t, srv, path).Body.String()
		if !strings.Contains(body, `class="sr-submit"`) {
			t.Errorf("%s: no hidden submit button, so Enter applies nothing", path)
		}
		// And no native date popup: dates are typed or picked from presets.
		if strings.Contains(body, `type="date"`) {
			t.Errorf("%s: a native date picker survived", path)
		}
	}
}

// Clear must clear. The first version's Clear kept the q it claimed to drop
// and scrolled to the top of the page besides.
func TestAnalyticsClearDropsTheFilterAndStaysAtTheComponent(t *testing.T) {
	f := analyticsFixture()
	srv := newServer(t, f, Viewer{Email: "admin@example.com", Admin: true})

	body := get(t, srv, "/analytics?q=ann&top=50").Body.String()
	if !strings.Contains(body, `href="/analytics?top=50#viewing"`) {
		t.Error("Clear does not drop q, keep top and return to the component")
	}
	// The combo's option links and the filter form must both carry a
	// non-default top, or filtering resets the table size.
	if !strings.Contains(body, "top=50") || !strings.Contains(body, `name="top" value="50"`) {
		t.Error("a non-default top is dropped by the person filter")
	}
}

// The roster's role control is the page's own component; the native select's
// option popup is browser-drawn and off-theme.
func TestRoleEditorIsNotANativeSelect(t *testing.T) {
	f := analyticsFixture()
	f.people = []Principal{{Email: "ann@example.com", Role: "admin"}, {Email: "bea@example.com", Role: "member"}}
	srv := newServer(t, f, Viewer{Email: "ann@example.com", Admin: true})

	body := get(t, srv, "/admin/principals").Body.String()
	if strings.Contains(body, "<select") {
		t.Error("the role editor is still a native select")
	}
	if !strings.Contains(body, `type="radio" name="role" value="admin" checked`) {
		t.Error("the admin's current role is not reflected in the radio pills")
	}
}
