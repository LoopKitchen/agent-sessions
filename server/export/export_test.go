package export

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// The fakes. A run is a plan over what the source reports, a stream of
// uploads and a set of loads; every property worth pinning (which
// partitions, in what order, whether the watermark moved) is observable on
// these three without a database, a bucket or a BigQuery project, and none
// of them opens a socket.

type fakeSource struct {
	mu        sync.Mutex
	now       time.Time
	marks     Watermarks
	sessions  []TouchedSession
	eventDays []Day
	health    []Day
	// rows per (table, day) partition and per bundle, as the lines written
	copies  map[string][]string
	bundles map[string][]string
	// healthAbsent makes Plan report that health_hourly does not exist.
	healthAbsent bool
	// planErr fails the planning read, the way a statement timeout would.
	planErr error
	// what the run read and wrote
	touchedLimit int
	eventLimit   int
	healthSince  time.Time
	advanced     *Watermarks
	copyErr      map[string]error
	copyCalls    map[string]int
}

func (f *fakeSource) Now(context.Context) (time.Time, error) { return f.now, nil }
func (f *fakeSource) Watermarks(context.Context) (Watermarks, error) {
	return f.marks, nil
}
func (f *fakeSource) Plan(_ context.Context, req PlanRequest) (PlanReads, error) {
	if f.planErr != nil {
		return PlanReads{}, f.planErr
	}
	f.touchedLimit = req.SessionsLimit
	f.eventLimit = req.EventsLimit
	f.healthSince = req.HealthSince
	var out PlanReads
	for _, s := range f.sessions {
		if s.UpdatedAt.After(req.SessionsSince) {
			out.Sessions = append(out.Sessions, s)
		}
	}
	if len(out.Sessions) > req.SessionsLimit {
		out.Sessions = out.Sessions[:req.SessionsLimit]
	}
	for _, d := range f.eventDays {
		if d.Next().Start().Add(-time.Microsecond).After(req.EventsSince) {
			out.EventDays = append(out.EventDays, d)
		}
	}
	if len(out.EventDays) > req.EventsLimit {
		out.EventDays = out.EventDays[:req.EventsLimit]
	}
	if !f.healthAbsent {
		out.HealthTableExists = true
		out.HealthDays = f.health
	}
	return out, nil
}
func (f *fakeSource) CopyPartition(_ context.Context, table Table, day Day, w io.Writer) (int64, error) {
	key := string(table) + "/" + string(day)
	f.mu.Lock()
	if f.copyCalls == nil {
		f.copyCalls = map[string]int{}
	}
	f.copyCalls[key]++
	err := f.copyErr[key]
	lines := f.copies[key]
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	for _, l := range lines {
		if _, err := io.WriteString(w, l+"\n"); err != nil {
			return 0, err
		}
	}
	return int64(len(lines)), nil
}
func (f *fakeSource) CopyBundle(_ context.Context, sid string, w io.Writer) (int64, error) {
	f.mu.Lock()
	lines := f.bundles[sid]
	err := f.copyErr["bundle/"+sid]
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	for _, l := range lines {
		if _, err := io.WriteString(w, l+"\n"); err != nil {
			return 0, err
		}
	}
	return int64(len(lines)), nil
}
func (f *fakeSource) AdvanceWatermarks(_ context.Context, w Watermarks) error {
	f.advanced = &w
	return nil
}

type fakeObjects struct {
	mu      sync.Mutex
	objects map[string][]byte
	order   []string
	failOn  string
	fails   int
}

func (o *fakeObjects) Put(_ context.Context, object string, write func(w io.Writer) error) (int64, error) {
	var buf bytes.Buffer
	if err := write(&buf); err != nil {
		return 0, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.failOn != "" && strings.HasPrefix(object, o.failOn) && o.fails > 0 {
		o.fails--
		return 0, errors.New("upload dropped")
	}
	if o.objects == nil {
		o.objects = map[string][]byte{}
	}
	o.objects[object] = buf.Bytes()
	o.order = append(o.order, object)
	return int64(buf.Len()), nil
}

// lines decompresses one object back into its lines.
func (o *fakeObjects) lines(t *testing.T, object string) []string {
	t.Helper()
	o.mu.Lock()
	raw, ok := o.objects[object]
	o.mu.Unlock()
	if !ok {
		t.Fatalf("object %s was not written", object)
	}
	gz, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s is not gzip: %v", object, err)
	}
	body, err := io.ReadAll(gz)
	if err != nil {
		t.Fatalf("%s: %v", object, err)
	}
	text := strings.TrimSuffix(string(body), "\n")
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

type fakeLoader struct {
	mu      sync.Mutex
	started []LoadJob
	waited  []LoadJob
	failIDs map[string]error
}

func (l *fakeLoader) Start(_ context.Context, job LoadJob) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.started = append(l.started, job)
	return nil
}
func (l *fakeLoader) Wait(_ context.Context, job LoadJob) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.waited = append(l.waited, job)
	return l.failIDs[job.ID]
}

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func sess(id, email, started, updated string) TouchedSession {
	return TouchedSession{SessionID: id, Email: email, StartedAt: at(started), UpdatedAt: at(updated)}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestPartitionSelectionIsTheSessionsStartDay is the guard on partition
// selection contract F(b) asks for: a run rewrites, for the session-keyed
// tables, exactly the start days of the sessions whose row changed since
// the watermark, whole, one file per table per day; and for events exactly
// the ingested days since the watermark. A session touched at 03:00 on the
// 12th whose start day is the 10th brings the 10th's partition back in full
// (ops review F8: with per-row selection the unchanged turns of the 10th
// would have been truncated away).
func TestPartitionSelectionIsTheSessionsStartDay(t *testing.T) {
	src := &fakeSource{
		now: at("2026-09-12T04:00:00Z"),
		marks: Watermarks{
			Sessions: at("2026-09-12T02:00:00Z"),
			Events:   at("2026-09-12T02:00:00Z"),
			Health:   at("2026-09-12T02:00:00Z"),
		},
		sessions: []TouchedSession{
			// changed before the watermark: not this run's business
			sess("old", "a@example.org", "2026-09-01T10:00:00Z", "2026-09-11T10:00:00Z"),
			// re-folded long after it started
			sess("s10", "a@example.org", "2026-09-10T23:50:00Z", "2026-09-12T03:00:00Z"),
			// two sessions on the same day: one partition
			sess("s11a", "b@example.com", "2026-09-11T01:00:00Z", "2026-09-12T03:10:00Z"),
			sess("s11b", "b@example.com", "2026-09-11T22:00:00Z", "2026-09-12T03:20:00Z"),
		},
		eventDays: []Day{"2026-09-12"},
		health:    []Day{"2026-09-11", "2026-09-12"},
		copies: map[string][]string{
			"turns/2026-09-10":         {`{"session_id":"s10","turn_index":0}`, `{"session_id":"s10","turn_index":1}`, `{"session_id":"unchanged-same-day","turn_index":0}`},
			"sessions/2026-09-10":      {`{"session_id":"s10"}`, `{"session_id":"unchanged-same-day"}`},
			"messages/2026-09-10":      {`{"event_id":"m1"}`},
			"turns/2026-09-11":         {`{"session_id":"s11a","turn_index":0}`},
			"sessions/2026-09-11":      {`{"session_id":"s11a"}`, `{"session_id":"s11b"}`},
			"messages/2026-09-11":      {},
			"events/2026-09-12":        {`{"id":"e1"}`, `{"id":"e2"}`},
			"health_hourly/2026-09-11": {`{"hour":"2026-09-11T23:00:00+00:00"}`},
			"health_hourly/2026-09-12": {`{"hour":"2026-09-12T03:00:00+00:00"}`},
		},
		bundles: map[string][]string{
			"s10":  {`{"record":"session","session_id":"s10"}`, `{"record":"turn"}`},
			"s11a": {`{"record":"session","session_id":"s11a"}`},
			"s11b": {`{"record":"session","session_id":"s11b"}`},
		},
	}
	objs := &fakeObjects{}
	loader := &fakeLoader{}
	res, err := Run(context.Background(), src, objs, loader, Options{RunID: "r1", Margin: 5 * time.Minute}, quietLog())
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// The partitions, as the loads BigQuery was asked for.
	var got []string
	for _, j := range loader.started {
		got = append(got, fmt.Sprintf("%s$%s<-%s", j.Table, j.Day.Decorator(), j.Object))
	}
	want := []string{
		"turns$20260910<-turns/dt=2026-09-10/part-0000.jsonl.gz",
		"sessions$20260910<-sessions/dt=2026-09-10/part-0000.jsonl.gz",
		"messages$20260910<-messages/dt=2026-09-10/part-0000.jsonl.gz",
		"turns$20260911<-turns/dt=2026-09-11/part-0000.jsonl.gz",
		"sessions$20260911<-sessions/dt=2026-09-11/part-0000.jsonl.gz",
		"messages$20260911<-messages/dt=2026-09-11/part-0000.jsonl.gz",
		"events$20260912<-events/dt=2026-09-12/part-0000.jsonl.gz",
		"health_hourly$20260911<-health_hourly/dt=2026-09-11/part-0000.jsonl.gz",
		"health_hourly$20260912<-health_hourly/dt=2026-09-12/part-0000.jsonl.gz",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("loads:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(loader.waited) != len(loader.started) {
		t.Errorf("waited on %d loads, started %d", len(loader.waited), len(loader.started))
	}
	// The whole partition, not the changed rows: the unchanged session of
	// the 10th is in the file that will WRITE_TRUNCATE the 10th.
	if lines := objs.lines(t, "turns/dt=2026-09-10/part-0000.jsonl.gz"); len(lines) != 3 {
		t.Errorf("turns 2026-09-10 has %d lines, want the whole partition (3)", len(lines))
	}
	// The old session's day was not touched.
	for _, o := range objs.order {
		if strings.Contains(o, "2026-09-01") {
			t.Errorf("a partition of 2026-09-01 was written: %s", o)
		}
	}
	// Bundles, per touched session, under the owner's address.
	for _, want := range []string{"bundles/a@example.org/s10.jsonl.gz", "bundles/b@example.com/s11a.jsonl.gz", "bundles/b@example.com/s11b.jsonl.gz"} {
		if _, ok := objs.objects[want]; !ok {
			t.Errorf("bundle %s was not written", want)
		}
	}
	if _, ok := objs.objects["bundles/a@example.org/old.jsonl.gz"]; ok {
		t.Error("the untouched session's bundle was rewritten")
	}
	// The health stream re-reads a trailing day.
	if want := at("2026-09-11T02:00:00Z"); !src.healthSince.Equal(want) {
		t.Errorf("health days read since %v, want the watermark less a day (%v)", src.healthSince, want)
	}
	// The run asked for one row more than it takes, to see a cut.
	if src.touchedLimit != DefaultMaxSessions+1 || src.eventLimit != DefaultMaxEventDays+1 {
		t.Errorf("limits asked: sessions %d, event days %d; want caps + 1", src.touchedLimit, src.eventLimit)
	}
	// Everything succeeded: the watermarks moved to the clock floor.
	if src.advanced == nil {
		t.Fatal("watermarks were not advanced")
	}
	floor := at("2026-09-12T03:55:00Z")
	if !src.advanced.Sessions.Equal(floor) || !src.advanced.Events.Equal(floor) || !src.advanced.Health.Equal(floor) {
		t.Errorf("advanced to %+v, want every stream at the floor %v", *src.advanced, floor)
	}
	// 13 partition lines (3+2+1, 1+2+0, 2, 1+1) and 4 bundle lines.
	if res.Status != "ok" || res.Partitions != 9 || res.Bundles != 3 || res.Rows != 13+4 || res.Capped {
		t.Errorf("result %+v", res)
	}
	if want := 5 * time.Minute; res.LagSeconds != want.Seconds() {
		t.Errorf("lag_seconds = %v, want the margin (%v)", res.LagSeconds, want.Seconds())
	}
}

// TestWatermarkDoesNotAdvanceWhenALoadFails is the second guard contract
// F(b) names. A load that BigQuery rejects leaves every watermark where it
// was, the run reports failed with the count, and the failure is on its
// own log line for the metric; the next run repeats the partition.
func TestWatermarkDoesNotAdvanceWhenALoadFails(t *testing.T) {
	src := &fakeSource{
		now:   at("2026-09-12T04:00:00Z"),
		marks: Watermarks{Sessions: at("2026-09-12T02:00:00Z"), Events: at("2026-09-12T02:00:00Z"), Health: at("2026-09-12T02:00:00Z")},
		sessions: []TouchedSession{
			sess("s1", "a@example.org", "2026-09-11T01:00:00Z", "2026-09-12T03:00:00Z"),
		},
		eventDays: []Day{"2026-09-12"},
		copies:    map[string][]string{"turns/2026-09-11": {`{}`}, "sessions/2026-09-11": {`{}`}, "messages/2026-09-11": {`{}`}, "events/2026-09-12": {`{}`}},
		bundles:   map[string][]string{"s1": {`{"record":"session"}`}},
	}
	objs := &fakeObjects{}
	loader := &fakeLoader{failIDs: map[string]error{
		jobID("r2", TableEvents, "2026-09-12"): errors.New("Provided Schema does not match Table"),
	}}
	var logged bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logged, nil))
	res, err := Run(context.Background(), src, objs, loader, Options{RunID: "r2"}, log)
	if err == nil {
		t.Fatal("run succeeded with a failed load")
	}
	if src.advanced != nil {
		t.Errorf("watermarks advanced to %+v after a failed load", *src.advanced)
	}
	if res.Status != "failed" || res.FailedLoads != 1 {
		t.Errorf("result %+v, want status failed with one failed load", res)
	}
	if !res.Watermarks.Sessions.Equal(at("2026-09-12T02:00:00Z")) {
		t.Errorf("result reports watermark %v, want the old one", res.Watermarks.Sessions)
	}
	// Lag is measured against the old watermark, which is what an operator
	// watching the metric needs to see growing.
	if want := (2 * time.Hour).Seconds(); res.LagSeconds != want {
		t.Errorf("lag_seconds = %v, want %v against the old watermark", res.LagSeconds, want)
	}
	line := logged.String()
	if !strings.Contains(line, `"msg":"export load failed"`) || !strings.Contains(line, `"table":"events"`) || !strings.Contains(line, `"day":"2026-09-12"`) || !strings.Contains(line, "Provided Schema") {
		t.Errorf("the failure line is missing or incomplete:\n%s", line)
	}
	// Every load was still waited on: one failure does not hide another.
	if len(loader.waited) != 4 {
		t.Errorf("waited on %d loads, want all 4", len(loader.waited))
	}
}

// TestACappedRunAdvancesToWhereItStopped: the first run over the corpus
// cannot finish in the job's budget, so a run takes at most the cap and
// records a watermark from which the next run continues, never one past
// work it did not do.
func TestACappedRunAdvancesToWhereItStopped(t *testing.T) {
	src := &fakeSource{
		now: at("2026-09-12T04:00:00Z"),
		sessions: []TouchedSession{
			sess("a", "x@example.org", "2026-08-01T00:00:00Z", "2026-09-01T00:00:00Z"),
			sess("b", "x@example.org", "2026-08-02T00:00:00Z", "2026-09-02T00:00:00Z"),
			sess("c", "x@example.org", "2026-08-03T00:00:00Z", "2026-09-02T00:00:00Z"),
			sess("d", "x@example.org", "2026-08-04T00:00:00Z", "2026-09-03T00:00:00Z"),
		},
		eventDays: []Day{"2026-08-01", "2026-08-02", "2026-08-03"},
		copies:    map[string][]string{},
		bundles:   map[string][]string{},
	}
	objs := &fakeObjects{}
	loader := &fakeLoader{}
	res, err := Run(context.Background(), src, objs, loader, Options{RunID: "r3", MaxSessions: 3, MaxEventDays: 2}, quietLog())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Capped || res.Bootstrap == false {
		t.Errorf("result %+v, want capped and bootstrap", res)
	}
	// Sessions: the cut is at d's updated_at (the 4th row); b and c share
	// the largest updated_at taken, and both are inside.
	var days []string
	for _, j := range loader.started {
		if j.Table == TableSessions {
			days = append(days, string(j.Day))
		}
	}
	if strings.Join(days, ",") != "2026-08-01,2026-08-02,2026-08-03" {
		t.Errorf("session partitions %v, want a, b and c's days", days)
	}
	if src.advanced == nil {
		t.Fatal("watermarks were not advanced")
	}
	if want := at("2026-09-02T00:00:00Z"); !src.advanced.Sessions.Equal(want) {
		t.Errorf("sessions watermark %v, want the largest updated_at taken (%v)", src.advanced.Sessions, want)
	}
	// Events: two of three days, and the watermark is the last microsecond
	// of the second, so the third day's first row is "after" it.
	var eventDays []string
	for _, j := range loader.started {
		if j.Table == TableEvents {
			eventDays = append(eventDays, string(j.Day))
		}
	}
	if strings.Join(eventDays, ",") != "2026-08-01,2026-08-02" {
		t.Errorf("event partitions %v", eventDays)
	}
	if want := at("2026-08-02T23:59:59.999999Z"); !src.advanced.Events.Equal(want) {
		t.Errorf("events watermark %v, want %v", src.advanced.Events, want)
	}
	// The next run, from those watermarks, takes what was left.
	src.marks = *src.advanced
	src.advanced = nil
	loader.started = nil
	if _, err := Run(context.Background(), src, objs, loader, Options{RunID: "r4", MaxSessions: 3, MaxEventDays: 2}, quietLog()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	var second []string
	for _, j := range loader.started {
		second = append(second, fmt.Sprintf("%s/%s", j.Table, j.Day))
	}
	if strings.Join(second, ",") != "turns/2026-08-04,sessions/2026-08-04,messages/2026-08-04,events/2026-08-03" {
		t.Errorf("second run took %v", second)
	}
}

// TestPlanSessionsHandlesTheCutAndTheTie pins planSessions on its own: a
// complete set goes to the floor and never behind the old watermark; a cut
// set stops strictly before the (limit+1)-th row's updated_at; a set that
// is one updated_at all the way through takes the cap and steps past it,
// reporting what it left.
func TestPlanSessionsHandlesTheCutAndTheTie(t *testing.T) {
	old := at("2026-09-01T00:00:00Z")
	floor := at("2026-09-12T00:00:00Z")
	rows := []TouchedSession{
		sess("a", "", "2026-08-01T00:00:00Z", "2026-09-02T00:00:00Z"),
		sess("b", "", "2026-08-01T00:00:00Z", "2026-09-03T00:00:00Z"),
	}
	p := planSessions(rows, 5, 0, old, floor)
	if p.capped || len(p.included) != 2 || !p.watermark.Equal(floor) || len(p.days) != 1 {
		t.Errorf("complete set: %+v", p)
	}
	if p := planSessions(nil, 5, 0, floor.Add(time.Hour), floor); !p.watermark.Equal(floor.Add(time.Hour)) {
		t.Errorf("an empty set moved the watermark backwards to %v", p.watermark)
	}
	tie := []TouchedSession{
		sess("a", "", "2026-08-01T00:00:00Z", "2026-09-02T00:00:00Z"),
		sess("b", "", "2026-08-02T00:00:00Z", "2026-09-02T00:00:00Z"),
		sess("c", "", "2026-08-03T00:00:00Z", "2026-09-02T00:00:00Z"),
	}
	p = planSessions(tie, 2, 0, old, floor)
	if !p.capped || p.tieSplit != 1 || len(p.included) != 2 || !p.watermark.Equal(at("2026-09-02T00:00:00Z")) {
		t.Errorf("tie: %+v", p)
	}

	// The day cap: five sessions on three days, cap two days, so the cut
	// falls before the first session of the third day and the watermark is
	// the largest updated_at taken.
	spread := []TouchedSession{
		sess("a", "", "2026-08-01T00:00:00Z", "2026-09-01T00:00:00Z"),
		sess("b", "", "2026-08-01T12:00:00Z", "2026-09-02T00:00:00Z"),
		sess("c", "", "2026-08-02T00:00:00Z", "2026-09-03T00:00:00Z"),
		sess("d", "", "2026-08-03T00:00:00Z", "2026-09-04T00:00:00Z"),
		sess("e", "", "2026-08-02T06:00:00Z", "2026-09-05T00:00:00Z"),
	}
	p = planSessions(spread, 100, 2, old, floor)
	if !p.capped || len(p.included) != 3 || len(p.days) != 2 || p.days[0] != "2026-08-01" || p.days[1] != "2026-08-02" || !p.watermark.Equal(at("2026-09-03T00:00:00Z")) {
		t.Errorf("day cap: %+v", p)
	}
	// A cap the set fits inside is not a cut.
	if p = planSessions(spread, 100, 3, old, floor); p.capped || len(p.included) != 5 || len(p.days) != 3 {
		t.Errorf("day cap not reached: %+v", p)
	}

	// The floor rule on a cut set (review-1 M1): rows stamped inside the
	// margin are not taken and the watermark never lands past the floor, so
	// a transaction that began before the run read the clock cannot commit
	// an updated_at the watermark has already passed.
	inside := []TouchedSession{
		sess("a", "", "2026-08-01T00:00:00Z", "2026-09-11T23:00:00Z"),
		sess("b", "", "2026-08-02T00:00:00Z", "2026-09-11T23:58:00Z"),
		sess("c", "", "2026-08-03T00:00:00Z", "2026-09-12T00:01:00Z"), // inside the margin
		sess("d", "", "2026-08-04T00:00:00Z", "2026-09-12T00:02:00Z"),
	}
	p = planSessions(inside, 3, 0, old, floor)
	if !p.capped || len(p.included) != 2 || p.included[1].SessionID != "b" || !p.watermark.Equal(at("2026-09-11T23:58:00Z")) {
		t.Errorf("cut inside the margin: %+v", p)
	}
	if p.watermark.After(floor) {
		t.Errorf("a cut set's watermark %v is past the floor %v", p.watermark, floor)
	}
	// Every row up to the cut inside the margin: nothing this hour, the old
	// position stands, and the run is still reported as cut.
	p = planSessions(inside[2:], 1, 0, old, floor)
	if !p.capped || len(p.included) != 0 || p.tieSplit != 0 || !p.watermark.Equal(old) || len(p.days) != 0 {
		t.Errorf("all rows inside the margin: %+v", p)
	}
	// A tie inside the margin waits too; the tie fallback is for a tie the
	// floor has passed.
	tieInside := []TouchedSession{
		sess("a", "", "2026-08-01T00:00:00Z", "2026-09-12T00:01:00Z"),
		sess("b", "", "2026-08-02T00:00:00Z", "2026-09-12T00:01:00Z"),
		sess("c", "", "2026-08-03T00:00:00Z", "2026-09-12T00:01:00Z"),
	}
	if p = planSessions(tieInside, 2, 0, old, floor); len(p.included) != 0 || p.tieSplit != 0 || !p.watermark.Equal(old) {
		t.Errorf("tie inside the margin: %+v", p)
	}
}

// TestADroppedUploadIsRetriedFromTheCopy: one lost upload costs a second
// COPY of that partition, not the run.
func TestADroppedUploadIsRetriedFromTheCopy(t *testing.T) {
	src := &fakeSource{
		now:       at("2026-09-12T04:00:00Z"),
		eventDays: []Day{"2026-09-12"},
		copies:    map[string][]string{"events/2026-09-12": {`{"id":"e1"}`}},
	}
	objs := &fakeObjects{failOn: "events/", fails: 1}
	loader := &fakeLoader{}
	if _, err := Run(context.Background(), src, objs, loader, Options{RunID: "r5"}, quietLog()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if src.copyCalls["events/2026-09-12"] != 2 {
		t.Errorf("COPY ran %d times, want 2", src.copyCalls["events/2026-09-12"])
	}
	objs = &fakeObjects{failOn: "events/", fails: 2}
	if _, err := Run(context.Background(), src, objs, &fakeLoader{}, Options{RunID: "r6"}, quietLog()); err == nil {
		t.Error("a partition that failed twice did not fail the run")
	}
}

// TestABundleThatCannotBeWrittenFailsTheRun: the session is inside the
// watermark the run would record, so its bundle cannot be left missing.
func TestABundleThatCannotBeWrittenFailsTheRun(t *testing.T) {
	src := &fakeSource{
		now:      at("2026-09-12T04:00:00Z"),
		sessions: []TouchedSession{sess("s1", "a@example.org", "2026-09-11T01:00:00Z", "2026-09-12T03:00:00Z")},
		copies:   map[string][]string{},
		bundles:  map[string][]string{},
		copyErr:  map[string]error{"bundle/s1": errors.New("statement timeout")},
	}
	if _, err := Run(context.Background(), src, &fakeObjects{}, &fakeLoader{}, Options{RunID: "r7"}, quietLog()); err == nil || !strings.Contains(err.Error(), "bundle s1") {
		t.Errorf("err = %v, want the bundle failure", err)
	}
	if src.advanced != nil {
		t.Error("watermarks advanced past a session whose bundle is missing")
	}
}

// TestTheRunLineCarriesTheFieldsTheMetricsRead pins the "export run" line:
// export_lag_seconds extracts lag_seconds, the dashboards read partitions,
// rows, bytes, seconds and status.
func TestTheRunLineCarriesTheFieldsTheMetricsRead(t *testing.T) {
	var logged bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&logged, nil))
	LogRun(log, Result{Partitions: 3, Rows: 10, Bytes: 100, Seconds: 1.5, LagSeconds: 300, Status: "ok"}, nil)
	for _, want := range []string{`"msg":"export run"`, `"partitions":3`, `"rows":10`, `"bytes":100`, `"seconds":1.5`, `"lag_seconds":300`, `"status":"ok"`, `"level":"INFO"`} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("run line lacks %s:\n%s", want, logged.String())
		}
	}
	logged.Reset()
	LogRun(log, Result{Status: "failed", FailedLoads: 2}, errors.New("2 of 5 load jobs failed"))
	for _, want := range []string{`"level":"ERROR"`, `"status":"failed"`, `"failed_loads":2`, `"error":"2 of 5 load jobs failed"`} {
		if !strings.Contains(logged.String(), want) {
			t.Errorf("failed run line lacks %s:\n%s", want, logged.String())
		}
	}
}

// TestDefaultsAreTheDocumentedOnes pins the numbers the deploy files and
// the README quote.
func TestDefaultsAreTheDocumentedOnes(t *testing.T) {
	if DefaultMaxSessions != 2000 || DefaultMaxSessionDays != 40 || DefaultMaxEventDays != 6 || DefaultEventCap != 16*1024 || DefaultBundleWorkers != 2 {
		t.Errorf("defaults changed: sessions %d, session days %d, event days %d, cap %d, workers %d", DefaultMaxSessions, DefaultMaxSessionDays, DefaultMaxEventDays, DefaultEventCap, DefaultBundleWorkers)
	}
	if RunBudget >= 30*time.Minute {
		t.Errorf("RunBudget %v is not below the job's 30-minute task timeout", RunBudget)
	}
	if poolMaxConns != 2 || poolMinConns != 0 {
		t.Errorf("job pool is %d/%d, want 2/0 (the connection budget reserves two)", poolMaxConns, poolMinConns)
	}
	if d := Day("2026-09-10"); d.Decorator() != "20260910" || d.Next() != "2026-09-11" || !d.Valid() || Day("2026-13-01").Valid() {
		t.Error("Day helpers")
	}
	if DayOf(at("2026-09-10T23:30:00-05:00")) != "2026-09-11" {
		t.Error("DayOf is not UTC")
	}
}

// TestAnEarlyFailureReportsTheLagAgainstTheOldWatermark (review-1 I1): a
// run that dies in its planning read, or in a partition COPY, has left the
// old positions in force, and its run line has to say how old they are. A
// zero here reads as "caught up" to the lag alert, which is the opposite of
// the truth for a job that fails every hour.
func TestAnEarlyFailureReportsTheLagAgainstTheOldWatermark(t *testing.T) {
	marks := Watermarks{Sessions: at("2026-09-12T02:00:00Z"), Events: at("2026-09-12T02:00:00Z"), Health: at("2026-09-12T02:00:00Z")}
	want := (2 * time.Hour).Seconds()

	planning := &fakeSource{now: at("2026-09-12T04:00:00Z"), marks: marks, planErr: errors.New("canceling statement due to statement timeout")}
	res, err := Run(context.Background(), planning, &fakeObjects{}, &fakeLoader{}, Options{RunID: "r8"}, quietLog())
	if err == nil || !strings.Contains(err.Error(), "plan") {
		t.Fatalf("err = %v, want the planning failure", err)
	}
	if res.Status != "failed" || res.LagSeconds != want {
		t.Errorf("planning failure: status %q lag_seconds %v, want failed and %v", res.Status, res.LagSeconds, want)
	}

	copying := &fakeSource{
		now: at("2026-09-12T04:00:00Z"), marks: marks,
		eventDays: []Day{"2026-09-12"},
		copies:    map[string][]string{},
		copyErr:   map[string]error{"events/2026-09-12": errors.New("canceling statement due to statement timeout")},
	}
	res, err = Run(context.Background(), copying, &fakeObjects{}, &fakeLoader{}, Options{RunID: "r9"}, quietLog())
	if err == nil {
		t.Fatal("a failed COPY did not fail the run")
	}
	if res.LagSeconds != want || copying.advanced != nil {
		t.Errorf("COPY failure: lag_seconds %v (want %v), advanced %v", res.LagSeconds, want, copying.advanced)
	}
	// And the line carries it, so the metric sees it.
	var logged bytes.Buffer
	LogRun(slog.New(slog.NewJSONHandler(&logged, nil)), res, err)
	if !strings.Contains(logged.String(), `"lag_seconds":7200`) || !strings.Contains(logged.String(), `"status":"failed"`) {
		t.Errorf("run line: %s", logged.String())
	}
	// A bootstrap failure has no watermark to measure against; zero, with
	// bootstrap and status saying why, which is what the status alert reads.
	res, _ = Run(context.Background(), &fakeSource{now: at("2026-09-12T04:00:00Z"), planErr: errors.New("timeout")}, &fakeObjects{}, &fakeLoader{}, Options{RunID: "r10"}, quietLog())
	if !res.Bootstrap || res.LagSeconds != 0 || res.Status != "failed" {
		t.Errorf("bootstrap failure: %+v", res)
	}
}

// TestTheHealthWatermarkWaitsForTheTable (review-1 I2): until PR E's
// health_hourly exists, the health watermark does not move, so the rollups
// PR E writes for the retained week are exported when they appear rather
// than skipped as older than a position they were never exported from. Once
// the table exists the stream advances like the others.
func TestTheHealthWatermarkWaitsForTheTable(t *testing.T) {
	src := &fakeSource{
		now:          at("2026-09-12T04:00:00Z"),
		marks:        Watermarks{Sessions: at("2026-09-12T02:00:00Z"), Events: at("2026-09-12T02:00:00Z")},
		eventDays:    []Day{"2026-09-12"},
		copies:       map[string][]string{"events/2026-09-12": {`{}`}},
		healthAbsent: true,
	}
	res, err := Run(context.Background(), src, &fakeObjects{}, &fakeLoader{}, Options{RunID: "r11"}, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if src.advanced == nil {
		t.Fatal("watermarks were not advanced")
	}
	if !src.advanced.Health.IsZero() || !src.advanced.Events.Equal(at("2026-09-12T03:55:00Z")) {
		t.Errorf("advanced to %+v, want health untouched (zero) and events at the floor", *src.advanced)
	}
	// The lag ignores the stream with no position rather than reading it as
	// two thousand years behind.
	if res.LagSeconds != (5 * time.Minute).Seconds() {
		t.Errorf("lag_seconds = %v, want the margin", res.LagSeconds)
	}
	// An older health position is kept as it is while the table is absent.
	src.marks.Health = at("2026-09-01T00:00:00Z")
	src.advanced = nil
	if _, err := Run(context.Background(), src, &fakeObjects{}, &fakeLoader{}, Options{RunID: "r12"}, quietLog()); err != nil {
		t.Fatal(err)
	}
	if !src.advanced.Health.Equal(at("2026-09-01T00:00:00Z")) {
		t.Errorf("health watermark moved to %v while the table is absent", src.advanced.Health)
	}
	// The table exists, with no rows yet: the stream advances to the floor
	// like the others.
	src.healthAbsent = false
	src.advanced = nil
	if _, err := Run(context.Background(), src, &fakeObjects{}, &fakeLoader{}, Options{RunID: "r13"}, quietLog()); err != nil {
		t.Fatal(err)
	}
	if !src.advanced.Health.Equal(at("2026-09-12T03:55:00Z")) {
		t.Errorf("health watermark %v once the table exists, want the floor", src.advanced.Health)
	}
}
