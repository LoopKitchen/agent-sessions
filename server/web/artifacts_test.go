package web

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func artifactFixture(t *testing.T) *fakeData {
	t.Helper()
	f := newFake()
	f.seedSession("s1", "dev@example.com")
	at := fixedNow.Add(-2 * time.Hour)
	f.artifacts = map[string][]Artifact{
		"s1": {{
			ID: 7, SessionID: "s1", Email: "dev@example.com",
			Path: "server/app/app.go", Created: false,
			FirstSeen: at, LastSeen: at.Add(time.Hour),
			VersionCount: 2, LatestSHA: "abcdef0123456789", LatestBytes: 2048,
		}},
	}
	f.versions = map[int64][]ArtifactVersion{
		7: {
			{ArtifactID: 7, EventID: "e2", SHA256: "abcdef0123456789", Bytes: 2048, OccurredAt: at.Add(time.Hour)},
			{ArtifactID: 7, EventID: "e1", SHA256: "0011223344556677", Bytes: 1024, OccurredAt: at},
		},
	}
	f.contents = map[string]string{
		"e2": "package app // newest",
		"e1": "package app // older",
	}
	f.links = map[string][]Link{
		"s1": {
			{SessionID: "s1", URL: "https://github.com/o/r/pull/4", Kind: "pr",
				Host: "github.com", Ref: "o/r#4", Occurrences: 3},
			{SessionID: "s1", URL: "https://notion.so/example/runbook", Kind: "doc",
				Host: "notion.so", Occurrences: 1},
		},
	}
	return f
}

// The panels are the feature: a session page that captured artifacts has to
// link to them, or the whole thing is a table nobody can reach.
func TestTheSessionPageLinksToItsArtifactsAndLinks(t *testing.T) {
	f := artifactFixture(t)
	srv := newServer(t, f, Viewer{Email: "dev@example.com"})

	rec := get(t, srv, "/sessions/s1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		`href="/artifacts/7"`,           // the artifact is reachable
		"app.go",                        // named by its file name
		"server/app/app.go",             // and by its full path
		"2 versions",                    // the history is advertised
		`https://github.com/o/r/pull/4`, // the PR is a real hyperlink
		"o/r#4",                         // rendered by its short ref
		"&times;3",                      // mentioned three times
		"notion.so/example/runbook",     // a doc link keeps its URL as its label
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the session page does not contain %q", want)
		}
	}
}

// Links go to hosts this system did not choose, so they must not hand the
// destination a window handle or a referrer.
func TestOutboundLinksDoNotLeakTheReferrerOrTheOpener(t *testing.T) {
	f := artifactFixture(t)
	srv := newServer(t, f, Viewer{Email: "dev@example.com"})

	rec := get(t, srv, "/sessions/s1")
	body := rec.Body.String()

	i := strings.Index(body, `href="https://github.com/o/r/pull/4"`)
	if i < 0 {
		t.Fatal("the PR link is not on the page")
	}
	tag := body[i:min(i+200, len(body))]
	if !strings.Contains(tag, "noopener") || !strings.Contains(tag, "noreferrer") {
		t.Errorf("an outbound link is missing noopener/noreferrer: %s", tag)
	}
}

// Most sessions were captured by a transcript walk and have neither. An empty
// panel on every one of them reads as a broken feature rather than an absent
// one, which is the whole reason the template guards on length.
func TestASessionWithNoArtifactsRendersNoPanels(t *testing.T) {
	f := newFake()
	f.seedSession("s1", "dev@example.com")
	srv := newServer(t, f, Viewer{Email: "dev@example.com"})

	rec := get(t, srv, "/sessions/s1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, unwanted := range []string{"Files changed", `id="links-h"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("an empty session rendered %q", unwanted)
		}
	}
}

// The transcript is the page. A store that cannot answer the supplementary
// question must not cost somebody the session they came to read.
func TestAFailingArtifactPanelDoesNotBreakTheSessionPage(t *testing.T) {
	f := artifactFixture(t)
	f.artifactErr = errors.New("artifacts table is on fire")
	srv := newServer(t, f, Viewer{Email: "dev@example.com"})

	rec := get(t, srv, "/sessions/s1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the transcript to render anyway", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "Files changed") {
		t.Error("the panel rendered despite the store failing")
	}
}

// The artifact page defaults to the newest version, because the question people
// arrive with is what the file looks like now.
func TestTheArtifactPageShowsTheNewestVersionByDefault(t *testing.T) {
	f := artifactFixture(t)
	srv := newServer(t, f, Viewer{Email: "dev@example.com"})

	rec := get(t, srv, "/artifacts/7")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "package app // newest") {
		t.Error("the newest version's contents are not shown")
	}
	if strings.Contains(body, "package app // older") {
		t.Error("an unselected version's contents were rendered")
	}
	// Both versions must be listed even though one is displayed.
	if !strings.Contains(body, "v2") || !strings.Contains(body, "v1") {
		t.Error("the version history is not listed")
	}
}

// Past versions are the other half of the ask: history you can actually read.
func TestAPastVersionCanBeSelected(t *testing.T) {
	f := artifactFixture(t)
	srv := newServer(t, f, Viewer{Email: "dev@example.com"})

	rec := get(t, srv, "/artifacts/7?v=e1")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "package app // older") {
		t.Error("the requested older version was not rendered")
	}
	if strings.Contains(body, "package app // newest") {
		t.Error("the newest version was rendered instead of the one asked for")
	}
}

// The version id in the query is a detail of this page, not something a person
// typed, so an unknown one falls back rather than 404ing.
func TestAnUnknownVersionFallsBackToTheNewest(t *testing.T) {
	f := artifactFixture(t)
	srv := newServer(t, f, Viewer{Email: "dev@example.com"})

	rec := get(t, srv, "/artifacts/7?v=nope")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "package app // newest") {
		t.Error("an unknown version did not fall back to the newest")
	}
}

// File contents are attacker-controlled as far as this page is concerned: they
// are whatever was in somebody's working tree. Rendering them unescaped would
// make every stored file a stored XSS.
func TestFileContentsAreEscaped(t *testing.T) {
	f := artifactFixture(t)
	f.contents["e2"] = `<script>alert(1)</script>`
	srv := newServer(t, f, Viewer{Email: "dev@example.com"})

	rec := get(t, srv, "/artifacts/7")
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("file contents were rendered as live markup")
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Error("the escaped contents are not present at all")
	}
}

// An artifact nobody may read is indistinguishable from one that is not there,
// the same rule the rest of the dashboard follows.
func TestAnUnknownArtifactIsNotFound(t *testing.T) {
	f := artifactFixture(t)
	srv := newServer(t, f, Viewer{Email: "dev@example.com"})

	for _, target := range []string{"/artifacts/999", "/artifacts/abc", "/artifacts/-1"} {
		rec := get(t, srv, target)
		if rec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, rec.Code)
		}
	}
}

// A file too large to embed is truncated and says so. Silently showing the first
// part of a file produces confident wrong conclusions about the rest.
func TestAnOversizeFileIsTruncatedAndSaysSo(t *testing.T) {
	f := artifactFixture(t)
	f.contents["e2"] = strings.Repeat("x", maxRendered+1000)
	srv := newServer(t, f, Viewer{Email: "dev@example.com"})

	rec := get(t, srv, "/artifacts/7")
	body := rec.Body.String()
	if !strings.Contains(body, "Showing the first") {
		t.Error("an oversize file was rendered without saying it was truncated")
	}
}

// The artifact page keeps the dashboard's strict policy: nothing on it needs
// script, and it renders file contents from somebody's working tree.
func TestTheArtifactPageBansScript(t *testing.T) {
	f := artifactFixture(t)
	srv := newServer(t, f, Viewer{Email: "dev@example.com"})

	rec := get(t, srv, "/artifacts/7")
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'none'") {
		t.Errorf("artifact page CSP does not ban script: %q", csp)
	}
}
