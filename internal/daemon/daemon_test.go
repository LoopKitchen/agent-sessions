package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

type stubFlush struct {
	calls     atomic.Int32
	remaining atomic.Int32
	err       error
}

func (s *stubFlush) RunOnce(context.Context) (int, error) {
	s.calls.Add(1)
	// Report progress only while there is simulated backlog, so the shutdown
	// drain loop terminates the way it would against a real spool.
	if s.remaining.Load() > 0 {
		s.remaining.Add(-1)
		return 1, s.err
	}
	return 0, s.err
}

// aliveSet builds a liveness function over a fixed set of pids, so tests can
// kill a process by removing it from a map rather than by spawning real ones.
func aliveSet(pids ...int) func(int) bool {
	m := map[int]bool{}
	for _, p := range pids {
		m[p] = true
	}
	return func(p int) bool { return m[p] }
}

func fixedNow(t time.Time) func() time.Time { return func() time.Time { return t } }

func newDaemon(t *testing.T, o Options) *Daemon {
	t.Helper()
	if o.Dir == "" {
		o.Dir = t.TempDir()
	}
	if o.SessionID == "" {
		o.SessionID = "s1"
	}
	if o.OwnerPID == 0 {
		o.OwnerPID = 4242
	}
	if o.Alive == nil {
		o.Alive = aliveSet(o.OwnerPID)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	d, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestNewClaimsSessionOnDisk(t *testing.T) {
	dir := t.TempDir()
	d := newDaemon(t, Options{Dir: dir, SessionID: "abc", OwnerPID: 111, Cwd: "/repo"})
	b, err := os.ReadFile(filepath.Join(dir, "abc.json"))
	if err != nil {
		t.Fatalf("state file not written: %v", err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if st.SessionID != "abc" || st.OwnerPID != 111 || st.Cwd != "/repo" {
		t.Fatalf("state wrong: %+v", st)
	}
	if st.DaemonPID != os.Getpid() {
		t.Fatalf("daemon pid = %d, want %d", st.DaemonPID, os.Getpid())
	}
	if st.Finalized {
		t.Fatal("a new session must not start finalized")
	}
	_ = d
}

func TestRequiresDirAndSessionID(t *testing.T) {
	if _, err := New(Options{SessionID: "s"}); err == nil {
		t.Fatal("expected error without Dir")
	}
	if _, err := New(Options{Dir: t.TempDir()}); err == nil {
		t.Fatal("expected error without SessionID")
	}
}

// The reason this package exists: a SIGKILLed harness fires no SessionEnd, so
// the daemon must notice the process is gone on its own and close the session.
func TestOwnerDeathIsDetectedAndSessionFinalized(t *testing.T) {
	dir := t.TempDir()
	ownerLive := atomic.Bool{}
	ownerLive.Store(true)
	fl := &stubFlush{}

	d := newDaemon(t, Options{
		Dir: dir, SessionID: "s1", OwnerPID: 999,
		Flush:         fl,
		PollInterval:  5 * time.Millisecond,
		FlushInterval: time.Hour, // isolate the poll path
		Alive:         func(pid int) bool { return pid == 999 && ownerLive.Load() },
	})

	done := make(chan error, 1)
	go func() { done <- d.Run(context.Background()) }()

	time.Sleep(20 * time.Millisecond)
	ownerLive.Store(false) // simulates kill -9

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil after owner exit", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not notice the owner had died")
	}

	if !d.State().Finalized {
		t.Fatal("session not finalized after owner death")
	}
	if d.State().EndReason != "owner_exited" {
		t.Fatalf("end reason = %q", d.State().EndReason)
	}
	// The final flush is the point of outliving the harness.
	if fl.calls.Load() == 0 {
		t.Fatal("no final flush on owner death")
	}
}

// Regression: the shutdown flush must drain the whole backlog, not one batch.
//
// The daemon is the last thing running for this session. Anything left queued
// when it exits waits for a future session that may not happen for days, by
// which point the harness's own 30-day cleanup can have deleted the source
// transcript — making the loss permanent. An earlier version called the flush
// exactly once here and would have stranded everything past the first batch.
func TestShutdownDrainsWholeBacklogNotOneBatch(t *testing.T) {
	fl := &stubFlush{}
	fl.remaining.Store(7) // seven batches' worth of backlog

	d := newDaemon(t, Options{
		Flush:         fl,
		PollInterval:  5 * time.Millisecond,
		FlushInterval: time.Hour,
		Alive:         func(int) bool { return false }, // owner already gone
	})
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Seven productive cycles plus the one that reports no progress and stops.
	if got := fl.calls.Load(); got < 8 {
		t.Fatalf("flushed %d times, want at least 8 to clear a 7-batch backlog", got)
	}
	if fl.remaining.Load() != 0 {
		t.Fatalf("backlog not drained: %d remaining", fl.remaining.Load())
	}
}

// The drain loop terminates on lack of progress, not on an empty spool. A
// server that answers 200 without accepting or rejecting an item leaves it
// pending forever, and looping until empty would spin until the grace period.
func TestShutdownDrainStopsWhenNoProgressIsMade(t *testing.T) {
	fl := &stubFlush{} // remaining stays 0, so every cycle reports sent=0
	d := newDaemon(t, Options{
		Flush:         fl,
		PollInterval:  5 * time.Millisecond,
		FlushInterval: time.Hour,
		ShutdownGrace: 2 * time.Second,
		Alive:         func(int) bool { return false },
	})
	start := time.Now()
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("drain spun for %v; it should stop on the first cycle with no progress", elapsed)
	}
	if got := fl.calls.Load(); got > 2 {
		t.Fatalf("flushed %d times with no progress available, want 1", got)
	}
}

// A daemon must not linger forever trying to deliver while offline.
func TestShutdownDrainIsBoundedByGrace(t *testing.T) {
	fl := &stubFlush{}
	fl.remaining.Store(1 << 20) // effectively endless backlog
	d := newDaemon(t, Options{
		Flush:         fl,
		PollInterval:  5 * time.Millisecond,
		FlushInterval: time.Hour,
		ShutdownGrace: 50 * time.Millisecond,
		Alive:         func(int) bool { return false },
	})
	start := time.Now()
	_ = d.Run(context.Background())
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("shutdown took %v; the grace period should bound it", elapsed)
	}
	if fl.remaining.Load() == 0 {
		t.Fatal("test fixture drained fully; it should have been cut short")
	}
}

func TestFinalFlushRunsEvenWhenParentContextIsCancelled(t *testing.T) {
	// A cancelled parent context must not prevent the last delivery attempt,
	// or a shutdown would strand whatever was captured last.
	dir := t.TempDir()
	fl := &stubFlush{}
	d := newDaemon(t, Options{
		Dir: dir, Flush: fl,
		PollInterval:  5 * time.Millisecond,
		FlushInterval: time.Hour,
		Alive:         func(int) bool { return false }, // owner already gone
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = d.Run(ctx)
	if fl.calls.Load() == 0 {
		t.Fatal("expected a flush attempt despite the cancelled context")
	}
}

// Cancellation is a shutdown, not an end of session. Finalising here would
// wrongly mark a still-running session as over.
func TestCancellationDoesNotFinalizeSession(t *testing.T) {
	d := newDaemon(t, Options{
		Flush:         &stubFlush{},
		PollInterval:  time.Hour,
		FlushInterval: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := d.Run(ctx)
	if err == nil {
		t.Fatal("expected the context error")
	}
	if d.State().Finalized {
		t.Fatal("shutdown must not finalize a session whose harness may still run")
	}
}

func TestFlushHappensOnCadenceWhileSessionRuns(t *testing.T) {
	fl := &stubFlush{}
	d := newDaemon(t, Options{
		Flush:         fl,
		FlushInterval: 5 * time.Millisecond,
		PollInterval:  time.Hour,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_ = d.Run(ctx)
	if fl.calls.Load() < 2 {
		t.Fatalf("flushed %d times, expected repeated flushes", fl.calls.Load())
	}
}

// A laptop is offline much of the time; a failing flush is normal and must not
// stop the daemon.
func TestFlushErrorsDoNotStopTheDaemon(t *testing.T) {
	fl := &stubFlush{err: context.DeadlineExceeded}
	d := newDaemon(t, Options{
		Flush:         fl,
		FlushInterval: 5 * time.Millisecond,
		PollInterval:  time.Hour,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_ = d.Run(ctx)
	if fl.calls.Load() < 2 {
		t.Fatal("daemon stopped flushing after an error")
	}
}

func TestHeartbeatIsRefreshedWhileRunning(t *testing.T) {
	dir := t.TempDir()
	d := newDaemon(t, Options{
		Dir: dir, Flush: &stubFlush{},
		PollInterval:  5 * time.Millisecond,
		FlushInterval: time.Hour,
	})
	first := d.State().Heartbeat
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_ = d.Run(ctx)
	if !d.State().Heartbeat.After(first) {
		t.Fatal("heartbeat was not refreshed")
	}
}

func TestAlreadyRunningDetectsLiveDaemon(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	_ = newDaemon(t, Options{Dir: dir, SessionID: "s1", Now: fixedNow(now)})

	if !AlreadyRunning(dir, "s1", aliveSet(os.Getpid()), now, time.Minute) {
		t.Fatal("a live daemon should be detected so a second one does not start")
	}
}

func TestAlreadyRunningIgnoresDeadDaemon(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	_ = newDaemon(t, Options{Dir: dir, SessionID: "s1", Now: fixedNow(now)})

	if AlreadyRunning(dir, "s1", func(int) bool { return false }, now, time.Minute) {
		t.Fatal("a dead daemon must not block a new one from starting")
	}
}

// Pids are reused. A live pid with a stale heartbeat is almost certainly a
// different process, and assuming otherwise would leave a session uncovered.
func TestAlreadyRunningTreatsStaleHeartbeatAsDead(t *testing.T) {
	dir := t.TempDir()
	start := time.Now()
	_ = newDaemon(t, Options{Dir: dir, SessionID: "s1", Now: fixedNow(start)})

	later := start.Add(10 * time.Minute)
	if AlreadyRunning(dir, "s1", aliveSet(os.Getpid()), later, time.Minute) {
		t.Fatal("stale heartbeat with a reused pid must not count as running")
	}
}

func TestAlreadyRunningIgnoresFinalizedSession(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	d := newDaemon(t, Options{Dir: dir, SessionID: "s1", Now: fixedNow(now)})
	if err := d.Finalize("done"); err != nil {
		t.Fatal(err)
	}
	if AlreadyRunning(dir, "s1", aliveSet(os.Getpid()), now, time.Minute) {
		t.Fatal("a finalized session is not running")
	}
}

// Recovery is event-driven: the next SessionStart closes what the last crash
// left open, instead of a timer polling forever.
func TestReconcileClosesSessionsAbandonedByADeadDaemon(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	_ = newDaemon(t, Options{Dir: dir, SessionID: "crashed", OwnerPID: 777, Now: fixedNow(now)})

	// Both the daemon and the harness are gone: a hard crash.
	got, err := Reconcile(dir, func(int) bool { return false }, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Reason != "daemon_died" {
		t.Fatalf("reconcile = %+v", got)
	}
	st, err := load(filepath.Join(dir, "crashed.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Finalized || st.EndReason != "daemon_died" {
		t.Fatalf("session not closed on disk: %+v", st)
	}
}

func TestReconcileLeavesLiveSessionsAlone(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	_ = newDaemon(t, Options{Dir: dir, SessionID: "live", Now: fixedNow(now)})

	got, err := Reconcile(dir, aliveSet(os.Getpid()), now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("a live session was reconciled: %+v", got)
	}
	st, _ := load(filepath.Join(dir, "live.json"))
	if st.Finalized {
		t.Fatal("live session was wrongly finalized")
	}
}

// If the harness outlived its daemon, the session is still running. Reporting
// it is useful; closing it would be a lie.
func TestReconcileReportsButDoesNotCloseWhenOwnerStillAlive(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	_ = newDaemon(t, Options{Dir: dir, SessionID: "orphan", OwnerPID: 555, Now: fixedNow(now)})

	alive := func(pid int) bool { return pid == 555 } // owner up, daemon gone
	got, err := Reconcile(dir, alive, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Reason != "daemon_died_owner_alive" {
		t.Fatalf("reconcile = %+v", got)
	}
	st, _ := load(filepath.Join(dir, "orphan.json"))
	if st.Finalized {
		t.Fatal("a session whose harness is still running must not be closed")
	}
}

func TestReconcileSkipsAlreadyFinalized(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	d := newDaemon(t, Options{Dir: dir, SessionID: "done", Now: fixedNow(now)})
	_ = d.Finalize("owner_exited")

	got, err := Reconcile(dir, func(int) bool { return false }, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("finalized session reconciled again: %+v", got)
	}
}

func TestReconcileOnMissingDirIsNotAnError(t *testing.T) {
	got, err := Reconcile(filepath.Join(t.TempDir(), "never-existed"), nil, time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("a machine that has never run a session is not an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatal("expected nothing to reconcile")
	}
}

func TestReconcileIgnoresUnreadableState(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "junk.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Reconcile(dir, func(int) bool { return false }, time.Now(), time.Minute)
	if err != nil {
		t.Fatalf("a corrupt state file must not abort reconciliation: %v", err)
	}
	if len(got) != 0 {
		t.Fatal("corrupt file should be left for a human, not acted on")
	}
}

func TestSweepRemovesOldFinalizedStateOnly(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()

	old := newDaemon(t, Options{Dir: dir, SessionID: "old", Now: fixedNow(now.Add(-48 * time.Hour))})
	_ = old.Finalize("owner_exited")
	recent := newDaemon(t, Options{Dir: dir, SessionID: "recent", Now: fixedNow(now)})
	_ = recent.Finalize("owner_exited")
	_ = newDaemon(t, Options{Dir: dir, SessionID: "open", Now: fixedNow(now.Add(-48 * time.Hour))})

	n, err := Sweep(dir, 24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("swept %d, want only the old finalized one", n)
	}
	if _, err := os.Stat(filepath.Join(dir, "open.json")); err != nil {
		t.Fatal("an unfinalized session must never be swept; it is the reconciler's input")
	}
	if _, err := os.Stat(filepath.Join(dir, "recent.json")); err != nil {
		t.Fatal("a recently finalized session was swept too early")
	}
}

func TestStatePathSanitizesHostileSessionIDs(t *testing.T) {
	dir := t.TempDir()
	d := newDaemon(t, Options{Dir: dir, SessionID: "../../etc/passwd"})
	if got := filepath.Dir(d.path); got != dir {
		t.Fatalf("state escaped its directory: %s", d.path)
	}
}

func TestProcessAliveAgreesWithReality(t *testing.T) {
	if !processAlive(os.Getpid()) {
		t.Fatal("this process should report as alive")
	}
	if processAlive(0) || processAlive(-1) {
		t.Fatal("invalid pids must not report as alive")
	}
	// A pid this high is not in use on any normal system.
	if processAlive(4194303) {
		t.Skip("pid space unexpectedly occupied; skipping negative case")
	}
}
