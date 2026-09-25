package export

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The two REST clients against a local server: the request shapes are the
// whole of what can go wrong with them, and a shape wrong here is a run
// that fails on its first real partition.

func TestGCSPutStreamsAMediaUploadAndReportsTheProducersError(t *testing.T) {
	var (
		gotPath, gotQuery, gotAuth, gotType string
		gotBody                             []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		gotAuth, gotType = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"name":"turns/dt=2026-09-10/part-0000.jsonl.gz"}`)
	}))
	defer srv.Close()
	g := &GCS{Bucket: "loop-sessions-analytics", Token: StaticToken("tok"), Endpoint: srv.URL}
	n, err := g.Put(context.Background(), "turns/dt=2026-09-10/part-0000.jsonl.gz", func(w io.Writer) error {
		gz := gzip.NewWriter(w)
		io.WriteString(gz, `{"a":1}`+"\n")
		return gz.Close()
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if gotPath != "/upload/storage/v1/b/loop-sessions-analytics/o" || gotQuery != "uploadType=media&name=turns%2Fdt%3D2026-09-10%2Fpart-0000.jsonl.gz" {
		t.Errorf("request %s?%s", gotPath, gotQuery)
	}
	if gotAuth != "Bearer tok" || gotType != "application/gzip" {
		t.Errorf("headers %q %q", gotAuth, gotType)
	}
	if int64(len(gotBody)) != n {
		t.Errorf("reported %d bytes, server received %d", n, len(gotBody))
	}
	gz, _ := gzip.NewReader(strings.NewReader(string(gotBody)))
	body, _ := io.ReadAll(gz)
	if string(body) != `{"a":1}`+"\n" {
		t.Errorf("body %q", body)
	}

	// The producer's error is the one reported, not the transport's view
	// of a body that ended early.
	_, err = g.Put(context.Background(), "x", func(w io.Writer) error {
		io.WriteString(w, "partial")
		return errors.New("statement timeout")
	})
	if err == nil || !strings.Contains(err.Error(), "statement timeout") {
		t.Errorf("err = %v, want the producer's", err)
	}

	// A non-200 is an error that carries the API's message.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"Access denied"}}`, http.StatusForbidden)
	}))
	defer bad.Close()
	g.Endpoint = bad.URL
	_, err = g.Put(context.Background(), "x", func(w io.Writer) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "Access denied") {
		t.Errorf("err = %v", err)
	}
}

func TestBigQueryStartsALoadIntoThePartitionAndWaitsForIt(t *testing.T) {
	var inserted map[string]any
	var gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/bigquery/v2/projects/example-project-12345/jobs":
			json.NewDecoder(r.Body).Decode(&inserted)
			io.WriteString(w, `{"jobReference":{"jobId":"j"}}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/bigquery/v2/projects/example-project-12345/jobs/"):
			if r.URL.Query().Get("location") != "asia-south1" {
				t.Errorf("jobs.get without the location: %s", r.URL.RawQuery)
			}
			n := atomic.AddInt32(&gets, 1)
			if n < 3 {
				io.WriteString(w, `{"status":{"state":"RUNNING"}}`)
				return
			}
			if strings.HasSuffix(r.URL.Path, "_events_20260912") {
				io.WriteString(w, `{"status":{"state":"DONE","errorResult":{"reason":"invalid","message":"Provided Schema does not match Table","location":"gs://x"}}}`)
				return
			}
			io.WriteString(w, `{"status":{"state":"DONE"}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	b := &BigQuery{Project: "example-project-12345", Dataset: "loop_sessions", Location: "asia-south1", SourceBucket: "loop-sessions-analytics",
		Token: StaticToken("tok"), Endpoint: srv.URL, Poll: time.Millisecond}
	job := LoadJob{ID: jobID("r1", TableTurns, "2026-09-10"), Table: TableTurns, Day: "2026-09-10", Object: partitionObject(TableTurns, "2026-09-10")}
	if err := b.Start(context.Background(), job); err != nil {
		t.Fatalf("start: %v", err)
	}
	ref := inserted["jobReference"].(map[string]any)
	if ref["jobId"] != "loop_sessions_export_r1_turns_20260910" || ref["location"] != "asia-south1" || ref["projectId"] != "example-project-12345" {
		t.Errorf("jobReference %v", ref)
	}
	load := inserted["configuration"].(map[string]any)["load"].(map[string]any)
	dest := load["destinationTable"].(map[string]any)
	if dest["tableId"] != "turns$20260910" || dest["datasetId"] != "loop_sessions" {
		t.Errorf("destination %v", dest)
	}
	if uris := load["sourceUris"].([]any); len(uris) != 1 || uris[0] != "gs://loop-sessions-analytics/turns/dt=2026-09-10/part-0000.jsonl.gz" {
		t.Errorf("sourceUris %v", uris)
	}
	if load["writeDisposition"] != "WRITE_TRUNCATE" || load["sourceFormat"] != "NEWLINE_DELIMITED_JSON" || load["ignoreUnknownValues"] != false {
		t.Errorf("load %v", load)
	}
	if err := b.Wait(context.Background(), job); err != nil {
		t.Errorf("wait: %v", err)
	}
	if gets < 3 {
		t.Errorf("polled %d times, want until DONE", gets)
	}
	failed := LoadJob{ID: jobID("r1", TableEvents, "2026-09-12"), Table: TableEvents, Day: "2026-09-12"}
	err := b.Wait(context.Background(), failed)
	if err == nil || !strings.Contains(err.Error(), "Provided Schema does not match Table") {
		t.Errorf("failed load: %v", err)
	}
}

// TestBigQueryRetriesOnceOnATransientAnswer (review-1 M4): a 503 or a 429
// while polling a load that is succeeding, or on the insert, is retried
// once after the backoff rather than reported as the load having failed;
// a second such answer is the error, and a 4xx is never retried.
func TestBigQueryRetriesOnceOnATransientAnswer(t *testing.T) {
	var gets, posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if atomic.AddInt32(&posts, 1) == 1 {
				http.Error(w, `{"error":{"message":"backendError"}}`, http.StatusInternalServerError)
				return
			}
			io.WriteString(w, `{"jobReference":{"jobId":"j"}}`)
		case http.MethodGet:
			switch atomic.AddInt32(&gets, 1) {
			case 1:
				http.Error(w, `{"error":{"message":"rateLimitExceeded"}}`, http.StatusTooManyRequests)
			case 2:
				io.WriteString(w, `{"status":{"state":"DONE"}}`)
			default:
				// Every later poll: two transient answers in a row.
				http.Error(w, `{"error":{"message":"backendError"}}`, http.StatusServiceUnavailable)
			}
		}
	}))
	defer srv.Close()
	b := &BigQuery{Project: "p", Dataset: "d", Location: "l", Token: StaticToken("t"), Endpoint: srv.URL, Poll: time.Millisecond, Backoff: time.Millisecond}
	job := LoadJob{ID: "j", Table: TableTurns, Day: "2026-09-10"}
	if err := b.Start(context.Background(), job); err != nil {
		t.Errorf("a 500 on the insert was not retried: %v", err)
	}
	if posts != 2 {
		t.Errorf("insert was attempted %d times, want 2", posts)
	}
	if err := b.Wait(context.Background(), job); err != nil {
		t.Errorf("a 429 while polling was not retried: %v", err)
	}
	if gets != 2 {
		t.Errorf("polled %d times, want the 429 and the DONE", gets)
	}
	err := b.Wait(context.Background(), job)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("two 503s in a row: err = %v, want the second one reported", err)
	}
	if gets != 4 {
		t.Errorf("polled %d times in total, want exactly one retry of the 503", gets)
	}

	// A 4xx is the API's answer, not a transient: no retry.
	var denied int32
	forbidden := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&denied, 1)
		http.Error(w, `{"error":{"message":"Access Denied"}}`, http.StatusForbidden)
	}))
	defer forbidden.Close()
	b.Endpoint = forbidden.URL
	if err := b.Start(context.Background(), job); err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("err = %v, want the 403", err)
	}
	if denied != 1 {
		t.Errorf("a 403 was attempted %d times, want 1", denied)
	}
}

func TestBigQueryTreatsAnExistingJobAsStarted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"Already Exists: Job"}}`, http.StatusConflict)
	}))
	defer srv.Close()
	b := &BigQuery{Project: "p", Dataset: "d", Location: "l", Token: StaticToken("t"), Endpoint: srv.URL}
	if err := b.Start(context.Background(), LoadJob{ID: "j", Table: TableTurns, Day: "2026-09-10"}); err != nil {
		t.Errorf("a 409 on insert is the job this run already started, got %v", err)
	}
}

func TestCachedTokenRefreshesBeforeExpiry(t *testing.T) {
	var fetches int
	src := cachedToken(func(context.Context) (string, time.Time, error) {
		fetches++
		if fetches == 1 {
			// Expires inside the refresh margin: used once, then refreshed.
			return "first", time.Now().Add(time.Minute), nil
		}
		return "second", time.Now().Add(time.Hour), nil
	})
	ctx := context.Background()
	tok, _ := src(ctx)
	tok2, _ := src(ctx)
	tok3, _ := src(ctx)
	if tok != "first" || tok2 != "second" || tok3 != "second" || fetches != 2 {
		t.Errorf("tokens %s %s %s, fetches %d", tok, tok2, tok3, fetches)
	}
}
