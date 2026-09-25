package daemon

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubReport is a Reporter whose behaviour each test dictates.
type stubReport struct {
	calls atomic.Int32
	err   error

	// hold, when non-nil, keeps every report in flight until the test closes
	// it, which is how a server that has stopped answering is simulated without
	// a socket.
	hold chan struct{}
	// inFlight is closed once the first report has entered, so a test can act
	// on a report that is genuinely blocked rather than on a sleep.
	inFlight chan struct{}
	once     sync.Once
}

func (s *stubReport) Report(ctx context.Context) error {
	s.calls.Add(1)
	if s.inFlight != nil {
		s.once.Do(func() { close(s.inFlight) })
	}
	if s.hold != nil {
		select {
		case <-s.hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.err
}

// TestTheFirstHealthReportDoesNotWaitForTheInterval defends the machines a
// cadence alone would never hear from.
//
// Sessions shorter than the report interval are ordinary. If the first sample
// waited a full interval, a laptop whose owner only runs short sessions would
// report nothing, ever, and the fleet view cannot tell that machine apart from
// one this was never installed on.
func TestTheFirstHealthReportDoesNotWaitForTheInterval(t *testing.T) {
	rep := &stubReport{inFlight: make(chan struct{})}
	d := newDaemon(t, Options{
		Flush:          &stubFlush{},
		Report:         rep,
		ReportInterval: time.Hour, // no tick can fire within this test
		FlushInterval:  time.Hour,
		PollInterval:   5 * time.Millisecond,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); _ = d.Run(ctx) }()

	select {
	case <-rep.inFlight:
	case <-time.After(2 * time.Second):
		t.Fatal("no health report was sent in a session shorter than one report interval")
	}
	cancel()
	<-done

	if got := rep.calls.Load(); got != 1 {
		t.Fatalf("sent %d reports, want exactly the one that does not wait for a tick", got)
	}
}

// TestHealthReportsKeepArrivingOnTheInterval defends the other half: a machine
// that reported once at SessionStart and then went quiet for a six-hour session
// is indistinguishable from one that died, because coverage is measured from the
// newest report.
func TestHealthReportsKeepArrivingOnTheInterval(t *testing.T) {
	rep := &stubReport{}
	d := newDaemon(t, Options{
		Flush:          &stubFlush{},
		Report:         rep,
		ReportInterval: 5 * time.Millisecond,
		FlushInterval:  time.Hour,
		PollInterval:   time.Hour,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	_ = d.Run(ctx)

	if got := rep.calls.Load(); got < 3 {
		t.Fatalf("sent %d reports over sixteen intervals; reporting stopped after the first", got)
	}
}

// TestAReportThatFailsLeavesDeliveryUntouched is the property that keeps
// telemetry from breaking the thing it describes.
//
// Health reporting talks to the same server events do, so it fails whenever
// events fail — and additionally whenever only /v1/health is broken. If any of
// those outcomes could stop, slow, or unwind a flush, the fleet's own monitoring
// would be the largest source of data loss on the laptop.
func TestAReportThatFailsLeavesDeliveryUntouched(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"a server answering 500", errors.New("health: answered http 500")},
		{"an unreachable server", errors.New("health: post: dial tcp: connection refused")},
		{"a cancelled report", context.Canceled},
		{"a report that succeeds", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fl := &stubFlush{}
			fl.remaining.Store(4)
			rep := &stubReport{err: tc.err}

			ownerLive := atomic.Bool{}
			ownerLive.Store(true)
			d := newDaemon(t, Options{
				OwnerPID:       999,
				Flush:          fl,
				Report:         rep,
				ReportInterval: 2 * time.Millisecond,
				FlushInterval:  2 * time.Millisecond,
				PollInterval:   5 * time.Millisecond,
				Alive:          func(pid int) bool { return pid == 999 && ownerLive.Load() },
			})

			done := make(chan error, 1)
			go func() { done <- d.Run(context.Background()) }()
			time.Sleep(30 * time.Millisecond)
			ownerLive.Store(false)

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Run returned %v; a health outcome must not end the session badly", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("the daemon never finished; a health report blocked the session loop")
			}

			if fl.remaining.Load() != 0 {
				t.Fatalf("%d batch(es) never left the spool; delivery was disturbed by health reporting",
					fl.remaining.Load())
			}
			if !d.State().Finalized || d.State().EndReason != "owner_exited" {
				t.Fatalf("session state: finalized=%v reason=%q, want a normally closed session",
					d.State().Finalized, d.State().EndReason)
			}
			if rep.calls.Load() == 0 {
				t.Fatal("no report was attempted, so this proves nothing about report failures")
			}
		})
	}
}

// TestAHungHealthReportDoesNotHoldUpDelivery is the strongest form of the same
// rule, and the reason reporting has its own goroutine.
//
// A captive portal answers the TCP handshake and then nothing, so a report hangs
// for the whole client timeout rather than failing fast. Sharing the session
// loop would put that stall in front of every flush and every liveness check:
// twenty seconds during which a laptop uploads nothing and does not notice its
// harness has exited.
func TestAHungHealthReportDoesNotHoldUpDelivery(t *testing.T) {
	dir := t.TempDir()
	fl := &stubFlush{}
	rep := &stubReport{hold: make(chan struct{}), inFlight: make(chan struct{})}

	ownerLive := atomic.Bool{}
	ownerLive.Store(true)
	d := newDaemon(t, Options{
		Dir:            dir,
		SessionID:      "hung",
		OwnerPID:       999,
		Flush:          fl,
		Report:         rep,
		ReportInterval: time.Hour,
		FlushInterval:  5 * time.Millisecond,
		PollInterval:   5 * time.Millisecond,
		Alive:          func(pid int) bool { return pid == 999 && ownerLive.Load() },
	})

	done := make(chan error, 1)
	go func() { done <- d.Run(context.Background()) }()

	select {
	case <-rep.inFlight:
	case <-time.After(2 * time.Second):
		t.Fatal("the first report never started")
	}

	// Flushes continue underneath a report that will never return.
	waitUntil(t, 2*time.Second, "flushes to continue while a report hangs", func() bool {
		return fl.calls.Load() >= 3
	})

	// And the daemon still notices its harness is gone, which is the whole
	// reason it outlives the harness at all. Read from the state file rather
	// than from the live daemon: the file is what any other process sees, and
	// State() is not meant to be read while Run owns it.
	ownerLive.Store(false)
	waitUntil(t, 2*time.Second, "the session to be finalized while a report hangs", func() bool {
		st, err := load(statePath(dir, "hung"))
		return err == nil && st.Finalized
	})

	close(rep.hold)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the daemon did not exit once the report returned")
	}
}

// TestADaemonWithNoReporterBehavesAsItAlwaysDid: reporting is optional, and a
// client that could not build one — no credential, no endpoint — must still
// deliver everything it captured.
func TestADaemonWithNoReporterBehavesAsItAlwaysDid(t *testing.T) {
	fl := &stubFlush{}
	fl.remaining.Store(2)
	d := newDaemon(t, Options{
		Flush:         fl,
		FlushInterval: 2 * time.Millisecond,
		PollInterval:  5 * time.Millisecond,
		Alive:         func(int) bool { return false },
	})
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if fl.remaining.Load() != 0 {
		t.Fatalf("%d batch(es) undelivered with no reporter configured", fl.remaining.Load())
	}
}

// waitUntil polls a condition, so a timing assertion reports what it was waiting
// for rather than only that it gave up.
func waitUntil(t *testing.T, limit time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", limit, what)
}
