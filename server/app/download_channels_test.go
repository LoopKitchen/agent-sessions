package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTheCanaryChannelAndTheReleaseManifestAreReachable extends the allowlist
// test for the two entries the rollout design adds: a "canary" channel CI
// publishes on every merge, and the latest.json manifest the fleet evaluator
// and the agent's upgrade check read. Both go through the same proxied route
// as the binaries, so the fleet needs no bucket credential for either.
//
// The refusals are listed alongside, because an allowlist is defined as much
// by what it leaves out: a third channel is still a 404, and so is a manifest
// under a name nobody publishes.
func TestTheCanaryChannelAndTheReleaseManifestAreReachable(t *testing.T) {
	t.Parallel()
	var asked []string
	d := testDownloads(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		asked = append(asked, r.URL.String())
		return okObject(`{"version":"v1"}`)(r)
	}))
	mux := http.NewServeMux()
	d.Register(mux)

	for _, tc := range []struct {
		name, target string
		want         int
		contentType  string
	}{
		{"the manifest on latest", "/dl/latest/latest.json", http.StatusOK, "application/json; charset=utf-8"},
		{"the manifest on canary", "/dl/canary/latest.json", http.StatusOK, "application/json; charset=utf-8"},
		{"a binary on canary", "/dl/canary/loop-sessions_darwin_arm64", http.StatusOK, "application/octet-stream"},
		{"the checksum manifest on canary", "/dl/canary/SHA256SUMS", http.StatusOK, "text/plain; charset=utf-8"},
		{"a third channel", "/dl/stable/latest.json", http.StatusNotFound, ""},
		{"a manifest under another name", "/dl/latest/canary.json", http.StatusNotFound, ""},
		{"a channel spelled with a capital", "/dl/Canary/SHA256SUMS", http.StatusNotFound, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.target, nil))
			if rec.Code != tc.want {
				t.Errorf("GET %s = %d, want %d", tc.target, rec.Code, tc.want)
			}
			if tc.contentType != "" {
				if got := rec.Header().Get("Content-Type"); got != tc.contentType {
					t.Errorf("GET %s Content-Type = %q, want %q", tc.target, got, tc.contentType)
				}
			}
		})
	}
	for _, url := range asked {
		if !strings.Contains(url, "canary%2F") && !strings.Contains(url, "latest%2F") {
			t.Errorf("an object outside the two channels reached Cloud Storage: %s", url)
		}
	}
}

// TestTheChannelAllowlistIsExactlyLatestAndCanary pins the set itself, so a
// third channel cannot be added by accident and neither can be dropped: the
// installer follows latest and the rollout runs on canary.
func TestTheChannelAllowlistIsExactlyLatestAndCanary(t *testing.T) {
	t.Parallel()
	want := map[string]bool{"latest": true, "canary": true}
	if len(downloadableChannels) != len(want) {
		t.Fatalf("downloadableChannels = %v, want %v", downloadableChannels, want)
	}
	for ch := range want {
		if !downloadableChannels[ch] {
			t.Errorf("channel %q is not served", ch)
		}
	}
	if _, ok := downloadableAssets["latest.json"]; !ok {
		t.Error("latest.json is not on the asset allowlist; the upgrade check has nothing to read")
	}
}
