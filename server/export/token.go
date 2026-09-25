package export

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// TokenSource returns a bearer token for Google APIs.
type TokenSource func(ctx context.Context) (string, error)

const (
	// The metadata server is on the local link; a slow answer means the
	// instance is wrong, not the network.
	metadataTimeout = 5 * time.Second
	// Tokens live an hour; refreshing early keeps a load that starts just
	// before expiry from failing half-way.
	tokenRefreshMargin = 5 * time.Minute
	metadataTokenURL   = "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token"
)

// StaticToken serves one token for the process's life: what a local run
// passes from `gcloud auth print-access-token`.
func StaticToken(tok string) TokenSource {
	return func(context.Context) (string, error) { return tok, nil }
}

// MetadataToken reads the job's own service account token from the
// instance metadata server, which is how Cloud Run hands a workload its
// identity without a key file, and caches it until shortly before expiry.
// Same shape as the server's release-bucket reader (server/app/download.go),
// which this package cannot import without pulling the whole app in.
func MetadataToken() TokenSource {
	return cachedToken(func(ctx context.Context) (string, time.Time, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataTokenURL, nil)
		if err != nil {
			return "", time.Time{}, err
		}
		req.Header.Set("Metadata-Flavor", "Google")
		resp, err := (&http.Client{Timeout: metadataTimeout}).Do(req)
		if err != nil {
			return "", time.Time{}, fmt.Errorf("export: reach the metadata server: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", time.Time{}, fmt.Errorf("export: metadata server answered %d", resp.StatusCode)
		}
		var body struct {
			AccessToken string `json:"access_token"`
			ExpiresIn   int    `json:"expires_in"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&body); err != nil {
			return "", time.Time{}, fmt.Errorf("export: read the metadata token: %w", err)
		}
		if body.AccessToken == "" {
			return "", time.Time{}, errors.New("export: the metadata server returned no access token")
		}
		return body.AccessToken, time.Now().Add(time.Duration(body.ExpiresIn) * time.Second), nil
	})
}

// cachedToken wraps a fetch with an expiry-aware cache.
func cachedToken(fetch func(ctx context.Context) (string, time.Time, error)) TokenSource {
	var (
		mu     sync.Mutex
		tok    string
		expiry time.Time
	)
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if tok != "" && time.Now().Add(tokenRefreshMargin).Before(expiry) {
			return tok, nil
		}
		t, exp, err := fetch(ctx)
		if err != nil {
			return "", err
		}
		tok, expiry = t, exp
		return tok, nil
	}
}
