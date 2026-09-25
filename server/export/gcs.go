package export

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// GCS writes objects to one bucket through the JSON API's media upload,
// streaming the body: the writer the caller fills is one end of a pipe and
// the request body is the other, so a partition is never held in memory.
//
// A single media upload rather than a resumable one, on purpose. Resumable
// uploads survive a dropped connection but need the chunk to be
// re-sendable, which means buffering it; the failure this job sees is rare
// and the recovery is to repeat the COPY (exportPartition tries twice), so
// the simpler upload is the honest one. The object appears only when the
// upload completes, so a reader can never see a partial partition.
type GCS struct {
	Bucket string
	Token  TokenSource
	// HTTP is the client; nil takes http.DefaultClient. No client timeout,
	// because a large partition legitimately uploads for minutes; the
	// request carries the run's context and dies with it.
	HTTP *http.Client
	// Endpoint is the API base, for the tests; empty is the real one.
	Endpoint string
}

const gcsEndpoint = "https://storage.googleapis.com"

// Put streams one object. The bytes reported are the compressed bytes sent.
func (g *GCS) Put(ctx context.Context, object string, write func(w io.Writer) error) (int64, error) {
	tok, err := g.Token(ctx)
	if err != nil {
		return 0, fmt.Errorf("export: bucket token: %w", err)
	}
	base := g.Endpoint
	if base == "" {
		base = gcsEndpoint
	}
	api := fmt.Sprintf("%s/upload/storage/v1/b/%s/o?uploadType=media&name=%s",
		base, url.PathEscape(g.Bucket), url.QueryEscape(object))

	pr, pw := io.Pipe()
	counter := &countingWriter{w: pw}
	// The producer runs beside the request; its error, not the transport's,
	// is the one worth reporting when it fails, because the transport only
	// ever sees "the body ended early".
	produced := make(chan error, 1)
	go func() {
		err := write(counter)
		pw.CloseWithError(err)
		produced <- err
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, api, pr)
	if err != nil {
		pr.CloseWithError(err)
		<-produced
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/gzip")
	client := g.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	// Whatever the transport said, the producer is drained first: a body
	// error surfaces as its own message, and the goroutine never leaks.
	pr.CloseWithError(err)
	perr := <-produced
	if perr != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return 0, perr
	}
	if err != nil {
		return 0, fmt.Errorf("export: upload %s: %w", object, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("export: upload %s: %s", object, apiError(resp))
	}
	return counter.n, nil
}

// countingWriter counts what passed through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// apiError renders a Google API error response as one line: the status and
// the start of the body, which carries the JSON error message. Bounded so a
// misrouted request cannot put a page into the log.
func apiError(resp *http.Response) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	text := strings.Join(strings.Fields(string(body)), " ")
	if text == "" {
		return fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return fmt.Sprintf("HTTP %d: %s", resp.StatusCode, text)
}
