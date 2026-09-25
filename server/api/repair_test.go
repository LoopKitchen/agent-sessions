package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeDevices struct {
	tokens map[string]DeviceIdentity
	err    error
}

func (d *fakeDevices) VerifyDevice(_ context.Context, token string) (DeviceIdentity, error) {
	if d.err != nil {
		return DeviceIdentity{}, d.err
	}
	id, ok := d.tokens[token]
	if !ok {
		return DeviceIdentity{}, ErrNoIdentity
	}
	return id, nil
}

type fakeRepair struct {
	lists map[string][]RepairEntry
	err   error
	calls int
	last  struct {
		device string
		now    time.Time
		limit  int
	}
}

func (f *fakeRepair) RepairList(_ context.Context, deviceID string, now time.Time, limit int) ([]RepairEntry, error) {
	f.calls++
	f.last.device, f.last.now, f.last.limit = deviceID, now, limit
	if f.err != nil {
		return nil, f.err
	}
	return f.lists[deviceID], nil
}

func newRepairHandler(t *testing.T, devices *fakeDevices, repair *fakeRepair, now *time.Time) *Handler {
	t.Helper()
	h, err := New(Options{
		Store:   newFakeStore(),
		Auth:    &fakeAuth{email: "x@example.com"},
		Devices: devices,
		Repair:  repair,
		Now:     func() time.Time { return *now },
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func repairGet(h *Handler, token, etag string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "/v1/repair", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if etag != "" {
		r.Header.Set("If-None-Match", etag)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestRepairListIsDeviceAuthenticatedAndShapedForTheClient: a cookie is not a
// machine, an unknown bearer is refused as unauthenticated, and an accepted
// one gets the client's {"sessions": [...]} shape for its own device only.
func TestRepairListIsDeviceAuthenticatedAndShapedForTheClient(t *testing.T) {
	now := testNow
	devices := &fakeDevices{tokens: map[string]DeviceIdentity{
		"tok-a": {DeviceID: "dev-a", Email: "ana@example.com"},
	}}
	repair := &fakeRepair{lists: map[string][]RepairEntry{
		"dev-a": {{SessionID: "s1", TranscriptPath: "/home/ana/.claude/projects/-home-ana-work/s1.jsonl", Reason: "missing_answers", Source: "claude_code", Hint: "2 answerless turns"}},
		"dev-b": {{SessionID: "s9", Reason: "codex_tokens", Source: "codex"}},
	}}
	h := newRepairHandler(t, devices, repair, &now)

	if w := repairGet(h, "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no bearer: %d, want 401", w.Code)
	}
	if w := repairGet(h, "nope", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("unknown bearer: %d, want 401", w.Code)
	}
	if repair.calls != 0 {
		t.Fatal("the store was asked before the device was verified")
	}

	w := repairGet(h, "tok-a", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	if repair.last.device != "dev-a" || repair.last.limit != repairLimit || !repair.last.now.Equal(now) {
		t.Errorf("store asked for %q limit %d at %s", repair.last.device, repair.last.limit, repair.last.now)
	}
	var body struct {
		Sessions []RepairEntry `json:"sessions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if len(body.Sessions) != 1 || body.Sessions[0].SessionID != "s1" || body.Sessions[0].TranscriptPath == "" || body.Sessions[0].Reason != "missing_answers" {
		t.Errorf("body = %+v", body)
	}
	for _, want := range []string{`"session_id"`, `"transcript_path"`, `"reason"`, `"source"`, `"hint"`} {
		if !json.Valid(w.Body.Bytes()) || !contains(w.Body.String(), want) {
			t.Errorf("body lacks %s: %s", want, w.Body.String())
		}
	}
	if w.Header().Get("ETag") == "" || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("headers = %v", w.Header())
	}
	// A device with nothing to repair gets an empty array, never null.
	devices.tokens["tok-c"] = DeviceIdentity{DeviceID: "dev-c"}
	if w := repairGet(h, "tok-c", ""); w.Code != http.StatusOK || w.Body.String() != `{"sessions":[]}` {
		t.Errorf("empty list = %d %s", w.Code, w.Body.String())
	}
}

// TestRepairListIsServedOnceAnHourPerDeviceWithAnETag: the second ask inside
// the hour is 429 with a Retry-After; an hour later it is answered again, and
// a matching If-None-Match is a 304 that still spends the hour.
func TestRepairListIsServedOnceAnHourPerDeviceWithAnETag(t *testing.T) {
	now := testNow
	devices := &fakeDevices{tokens: map[string]DeviceIdentity{"tok-a": {DeviceID: "dev-a"}, "tok-b": {DeviceID: "dev-b"}}}
	repair := &fakeRepair{lists: map[string][]RepairEntry{"dev-a": {{SessionID: "s1", TranscriptPath: "/p", Reason: "missing_answers"}}}}
	h := newRepairHandler(t, devices, repair, &now)

	first := repairGet(h, "tok-a", "")
	if first.Code != http.StatusOK {
		t.Fatalf("first: %d", first.Code)
	}
	etag := first.Header().Get("ETag")
	second := repairGet(h, "tok-a", "")
	if second.Code != http.StatusTooManyRequests || second.Header().Get("Retry-After") == "" {
		t.Fatalf("second ask inside the hour: %d retry-after %q", second.Code, second.Header().Get("Retry-After"))
	}
	if repair.calls != 1 {
		t.Errorf("the store was asked %d times, want once", repair.calls)
	}
	// Another device is not held back by the first.
	if w := repairGet(h, "tok-b", ""); w.Code != http.StatusOK {
		t.Errorf("another device: %d", w.Code)
	}
	now = now.Add(repairEvery + time.Second)
	third := repairGet(h, "tok-a", etag)
	if third.Code != http.StatusNotModified || third.Body.Len() != 0 {
		t.Fatalf("unchanged list with the ETag: %d body %q", third.Code, third.Body.String())
	}
	if repair.calls != 3 {
		t.Errorf("the store was asked %d times, want 3", repair.calls)
	}
	now = now.Add(repairEvery + time.Second)
	repair.lists["dev-a"] = append(repair.lists["dev-a"], RepairEntry{SessionID: "s2", TranscriptPath: "/q", Reason: "codex_tokens"})
	fourth := repairGet(h, "tok-a", etag)
	if fourth.Code != http.StatusOK || fourth.Header().Get("ETag") == etag {
		t.Errorf("a changed list answered %d with the old ETag", fourth.Code)
	}
}

// TestRepairFailuresAreOursNotTheClients: a store or verifier failure is a
// 500 the client retries tomorrow, never a 401 that sends a laptop to
// re-enrol, and it does not spend the device's hour: the next ask, once the
// store is back, is answered.
func TestRepairFailuresAreOursNotTheClients(t *testing.T) {
	now := testNow
	devices := &fakeDevices{tokens: map[string]DeviceIdentity{"tok-a": {DeviceID: "dev-a"}}}
	repair := &fakeRepair{err: errors.New("pool is gone")}
	h := newRepairHandler(t, devices, repair, &now)
	if w := repairGet(h, "tok-a", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("store failure: %d", w.Code)
	}
	repair.err = nil
	if w := repairGet(h, "tok-a", ""); w.Code != http.StatusOK {
		t.Errorf("the ask after a store failure answered %d; the failure was ours and must not cost the device its hour", w.Code)
	}
	if w := repairGet(h, "tok-a", ""); w.Code != http.StatusTooManyRequests {
		t.Errorf("the ask after an answer was %d, want 429: the answer spends the hour", w.Code)
	}
	devices.err = errors.New("roster unreachable")
	if w := repairGet(h, "tok-a", ""); w.Code != http.StatusInternalServerError {
		t.Errorf("verifier failure: %d", w.Code)
	}
}

// TestRepairRouteIsAbsentWithoutItsPorts: a server built without a device
// verifier or a repair store answers the route with the read API's 404.
func TestRepairRouteIsAbsentWithoutItsPorts(t *testing.T) {
	h := newHandler(t, newFakeStore(), &fakeAuth{email: "x@example.com"})
	if w := repairGet(h, "tok", ""); w.Code != http.StatusNotFound {
		t.Errorf("route without ports answered %d", w.Code)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
