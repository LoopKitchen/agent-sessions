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

// Recovery runs at the end of a session, before the session is closed: the
// daemon still has the transcript path, and what it recovers must go out with
// the final drain rather than wait for a future session that may never come.
func TestOwnerExitRunsRecoveryBeforeFinalize(t *testing.T) {
	dir := t.TempDir()
	fl := &stubFlush{}
	fl.remaining.Store(2)
	var recovered atomic.Int32
	var finalizedAtRecovery atomic.Bool
	var flushesAtRecovery atomic.Int32

	ownerLive := atomic.Bool{}
	ownerLive.Store(true)
	d := newDaemon(t, Options{
		Dir: dir, SessionID: "ending", OwnerPID: 999, TranscriptPath: "/t/ending.jsonl",
		Flush:         fl,
		PollInterval:  5 * time.Millisecond,
		FlushInterval: time.Hour,
		Alive:         func(pid int) bool { return pid == 999 && ownerLive.Load() },
		Recover: func(st State) int {
			recovered.Add(1)
			finalizedAtRecovery.Store(st.Finalized)
			flushesAtRecovery.Store(fl.calls.Load())
			if st.TranscriptPath != "/t/ending.jsonl" {
				t.Errorf("recovery got state %+v without the transcript path", st)
			}
			return 7
		},
	})

	done := make(chan error, 1)
	go func() { done <- d.Run(context.Background()) }()
	time.Sleep(20 * time.Millisecond)
	ownerLive.Store(false)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("daemon did not exit")
	}

	if recovered.Load() != 1 {
		t.Fatalf("recovery ran %d times, want once", recovered.Load())
	}
	if finalizedAtRecovery.Load() {
		t.Fatal("recovery ran after the session was finalized")
	}
	// The final drain (which reports progress twice here) ran after recovery,
	// so whatever recovery spooled left with it.
	if fl.remaining.Load() != 0 {
		t.Fatal("the final drain did not run")
	}
	if fl.calls.Load() <= flushesAtRecovery.Load() {
		t.Fatalf("no flush after recovery: %d before, %d after", flushesAtRecovery.Load(), fl.calls.Load())
	}
	st, err := load(filepath.Join(dir, "ending.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !st.Finalized || st.Recovered != 7 {
		t.Fatalf("state = %+v, want finalized with 7 recovered", st)
	}
}

// Reconcile is the other end of the same rule: a session a dead daemon left
// open is recovered before it is closed, and the outcome is recorded on it.
func TestReconcileRunsRecoveryForAbandonedSessions(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	// The crashed session's daemon and harness are both gone: pids nothing on
	// this machine holds.
	crashed := State{SessionID: "crashed", OwnerPID: 4194303, DaemonPID: 4194302, StartedAt: now.Add(-time.Hour),
		Heartbeat: now.Add(-time.Hour), TranscriptPath: "/t/c.jsonl"}
	b, err := json.Marshal(crashed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "crashed.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	live := newDaemon(t, Options{Dir: dir, SessionID: "live", Now: fixedNow(now)})

	var seen []string
	got, err := ReconcileWith(dir, func(pid int) bool { return pid == live.state.DaemonPID }, now, time.Minute, func(st State) int {
		seen = append(seen, st.SessionID)
		return 3
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Reason != "daemon_died" {
		t.Fatalf("reconcile = %+v", got)
	}
	if len(seen) != 1 || seen[0] != "crashed" {
		t.Fatalf("recovery ran for %v, want only the crashed session", seen)
	}
	st, _ := load(filepath.Join(dir, "crashed.json"))
	if !st.Finalized || st.Recovered != 3 {
		t.Fatalf("crashed state = %+v", st)
	}
	if st, _ := load(filepath.Join(dir, "live.json")); st.Finalized || st.Recovered != 0 {
		t.Fatalf("a live session was touched: %+v", st)
	}
	// Reconcile without a recovery is unchanged.
	if _, err := Reconcile(dir, func(int) bool { return false }, now, time.Minute); err != nil {
		t.Fatal(err)
	}
}
