package app

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testDownloads(t *testing.T, rt http.RoundTripper) *Downloads {
	t.Helper()
	d, err := NewDownloads(DownloadOptions{
		Bucket:      "releases",
		PublicURL:   "https://sessions.example.com",
		TokenSource: func() (string, time.Time, error) { return "tok", time.Now().Add(time.Hour), nil },
		HTTPClient:  &http.Client{Transport: rt},
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewDownloads: %v", err)
	}
	return d
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func okObject(body string) roundTripFunc {
	return func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     http.Header{},
		}, nil
	}
}

// The asset name is interpolated into a Cloud Storage object path, so the
// property that matters is not "does it look reasonable" but "can any name a
// caller supplies reach an object we did not publish". The allowlist is what
// answers that, and this is the test that stops somebody replacing it with a
// pattern later.
func TestOnlyPublishedArtifactsAreReachable(t *testing.T) {
	t.Parallel()
	var asked string
	d := testDownloads(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		asked = r.URL.String()
		return okObject("binary")(r)
	}))
	mux := http.NewServeMux()
	d.Register(mux)

	for _, tc := range []struct {
		name, target string
		want         int
	}{
		{"a published binary", "/dl/latest/loop-sessions_darwin_arm64", http.StatusOK},
		{"the checksum manifest", "/dl/latest/SHA256SUMS", http.StatusOK},
		{"the installer", "/dl/latest/install.sh", http.StatusOK},
		{"an unpublished channel", "/dl/secret/SHA256SUMS", http.StatusNotFound},
		{"an asset nobody published", "/dl/latest/id_rsa", http.StatusNotFound},
		// Traversal is spelled several ways; none of them are on the list, so
		// none of them reach the bucket. The allowlist makes this boring, which
		// is the point.
		{"traversal in the asset", "/dl/latest/..%2f..%2fsecrets", http.StatusNotFound},
		{"traversal in the channel", "/dl/..%2flatest/SHA256SUMS", http.StatusNotFound},
		{"an empty asset", "/dl/latest/", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.target, nil))
			if rec.Code != tc.want {
				t.Errorf("GET %s = %d, want %d", tc.target, rec.Code, tc.want)
			}
		})
	}
	if asked != "" && !strings.Contains(asked, "%2F") {
		t.Errorf("the object name reached Cloud Storage unescaped: %s", asked)
	}
}

// A release artifact must never be handed to a shared cache. "latest" that a
// proxy holds is a fleet installing yesterday's agent with nothing to show that
// it happened.
func TestReleasesAreNeverCached(t *testing.T) {
	t.Parallel()
	d := testDownloads(t, okObject("binary"))
	mux := http.NewServeMux()
	d.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dl/latest/SHA256SUMS", nil))
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// A missing artifact is our failure, not the caller's, and it must not be
// reported as if the bucket had answered.
func TestAMissingArtifactIsNotReportedAsSuccess(t *testing.T) {
	t.Parallel()
	d := testDownloads(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader("no such object")),
			Header:     http.Header{},
		}, nil
	}))
	mux := http.NewServeMux()
	d.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dl/latest/loop-sessions_linux_amd64", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "no such object") {
		t.Error("the bucket's own error text reached the caller")
	}
}

// The install page has to print an absolute command. A page that says "curl
// /install.sh" tells somebody standing at a new laptop nothing at all.
func TestTheInstallPagePrintsACommandSomebodyCanPaste(t *testing.T) {
	t.Parallel()
	d := testDownloads(t, okObject(""))
	mux := http.NewServeMux()
	d.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, InstallPagePath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	want := "curl -fsSL https://sessions.example.com/install.sh | sh"
	if !strings.Contains(body, want) {
		t.Errorf("the page does not contain the one-liner %q", want)
	}
	// Every offered platform must resolve to a route that exists, or the page
	// sends people to a 404 with no way to tell it apart from a broken release.
	for _, p := range installPlatforms {
		if !strings.Contains(body, "/dl/latest/"+p.Asset) {
			t.Errorf("the page does not offer %s", p.Asset)
		}
		if _, ok := downloadableAssets[p.Asset]; !ok {
			t.Errorf("the page offers %s, which the download route will refuse", p.Asset)
		}
	}
	// It ships no script, so it keeps the strict policy rather than the relaxed
	// one the sign-in page needs. It is also outside the dashboard's 2026-08-13
	// split: this page sets its own header in installpage.go and does not pass
	// through web.Secure, so the filter pages moving to script-src 'self' must
	// not carry it along. A download surface that started allowing script would
	// be allowing it on the page that hands people a binary to run.
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'none'") {
		t.Errorf("install page CSP does not ban script: %s", csp)
	}
}

// Downloads must not be mountable without somewhere to read from; a route that
// answers 503 for every request is worse than a route that does not exist.
func TestDownloadsRefuseToBuildWithoutABucket(t *testing.T) {
	t.Parallel()
	if _, err := NewDownloads(DownloadOptions{PublicURL: "https://x.example"}); err == nil {
		t.Fatal("NewDownloads accepted an empty bucket")
	}
}
