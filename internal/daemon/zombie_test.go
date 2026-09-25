package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The harness said goodbye while the owner pid lives on (an IDE hosting many
// sessions): the SessionEnd marker ends the daemon within a poll, and the
// recovery pass runs as it does when the owner dies.
func TestSessionEndMarkerFinalizesTheDaemonWhileTheOwnerLives(t *testing.T) {
	dir := t.TempDir()
	d, err := New(Options{
		Dir: dir, SessionID: "s-ide", OwnerPID: 999, Build: "cur",
		PollInterval:  5 * time.Millisecond,
		FlushInterval: time.Hour,
		Alive:         func(pid int) bool { return true },
		Recover:       func(State) int { return 3 },
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(t.Context()) }()
	time.Sleep(20 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("the daemon left before the session ended: %v", err)
	default:
	}
	if err := MarkEnded(dir, "s-ide", time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the daemon did not finalize on the marker")
	}
	st, err := LoadState(dir, "s-ide")
	if err != nil {
		t.Fatal(err)
	}
	if !st.Finalized || st.EndReason != "session_ended" || st.Recovered != 3 || st.Build != "cur" {
		t.Errorf("state = %+v, want finalized session_ended with the recovery pass run and the build recorded", st)
	}
}

// A resumed session reuses its id: the hook clears an earlier end before it
// starts the daemon, and only an end reported after that counts.
func TestClearEndedForgetsAnEarlierEndAndALaterOneCounts(t *testing.T) {
	dir := t.TempDir()
	if err := MarkEnded(dir, "s-resume", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	ClearEnded(dir, "s-resume")
	d, err := New(Options{
		Dir: dir, SessionID: "s-resume", OwnerPID: 999,
		PollInterval:  5 * time.Millisecond,
		FlushInterval: time.Hour,
		Alive:         func(int) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.Run(t.Context()) }()
	select {
	case err := <-done:
		t.Fatalf("the daemon finalized on a marker from before it started: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := MarkEnded(dir, "s-resume", time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the daemon did not finalize on the new marker")
	}
}

func writeStateFile(t *testing.T, dir string, st State) {
	t.Helper()
	b, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(dir, st.SessionID), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// Liveness is the session lock, or a fresh heartbeat on a live pid. A live
// daemon of another build is signalled and its session finalized; a live one
// of this build is left alone; a dead daemon under a live owner is finalized
// as it stands (daemon_stale for another build, daemon_died for this one); a
// recycled pid (stale heartbeat, no lock) is never signalled; an abandoned
// session (both pids dead) and self are left alone; a kill that fails leaves
// the state untouched.
func TestStopStaleFinalizesSessionsOfOtherBuildsAndDeadDaemons(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 17, 14, 0, 0, 0, time.UTC)
	started := now.Add(-40 * 24 * time.Hour)
	stale := now.Add(-7 * 24 * time.Hour)
	writeStateFile(t, dir, State{SessionID: "unrecorded", OwnerPID: 1, DaemonPID: 4242, StartedAt: started, Heartbeat: now})
	writeStateFile(t, dir, State{SessionID: "august", OwnerPID: 1, DaemonPID: 4343, Build: "23713ea", StartedAt: started, Heartbeat: now})
	writeStateFile(t, dir, State{SessionID: "woken", OwnerPID: 1, DaemonPID: 4444, Build: "23713ea", StartedAt: started, Heartbeat: stale}) // lock held, heartbeat stale: live
	writeStateFile(t, dir, State{SessionID: "current", OwnerPID: 1, DaemonPID: 4545, Build: "cur", StartedAt: started, Heartbeat: now})
	writeStateFile(t, dir, State{SessionID: "current-dead", OwnerPID: 1, DaemonPID: 4646, Build: "cur", StartedAt: started, Heartbeat: stale})
	writeStateFile(t, dir, State{SessionID: "done", OwnerPID: 1, DaemonPID: 4747, StartedAt: started, Finalized: true, EndedAt: now})
	writeStateFile(t, dir, State{SessionID: "dead", OwnerPID: 1, DaemonPID: 4848, StartedAt: started, Heartbeat: now})
	writeStateFile(t, dir, State{SessionID: "abandoned", OwnerPID: 2, DaemonPID: 4949, StartedAt: started, Heartbeat: now})
	writeStateFile(t, dir, State{SessionID: "recycled", OwnerPID: 1, DaemonPID: 5050, StartedAt: started, Heartbeat: stale})
	writeStateFile(t, dir, State{SessionID: "me", OwnerPID: 1, DaemonPID: 7000, StartedAt: started, Heartbeat: now})
	deadPids := map[int]bool{4646: true, 4848: true, 4949: true, 2: true}
	alive := func(pid int) bool { return !deadPids[pid] }
	held := func(id string) bool { return id == "woken" || id == "current" || id == "me" }
	var killed []int
	got := map[string]Stale{}
	stale2, err := StopStale(dir, "cur", 7000, "", alive, held, func(pid int) error { killed = append(killed, pid); return nil }, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stale2 {
		got[s.SessionID] = s
	}
	wantKilled := map[int]bool{4242: true, 4343: true, 4444: true}
	if len(killed) != 3 {
		t.Errorf("killed %v, want exactly the three live daemons of other builds", killed)
	}
	for _, pid := range killed {
		if !wantKilled[pid] {
			t.Errorf("killed %d, which is not a live daemon of another build", pid)
		}
	}
	for id, want := range map[string]string{"unrecorded": "daemon_stale", "august": "daemon_stale", "woken": "daemon_stale", "current-dead": "daemon_died", "dead": "daemon_stale", "recycled": "daemon_stale"} {
		st, _ := LoadState(dir, id)
		if !st.Finalized || st.EndReason != want || !st.EndedAt.Equal(now) {
			t.Errorf("session %s = finalized %v reason %q, want %s at now", id, st.Finalized, st.EndReason, want)
		}
		if _, ok := got[id]; !ok {
			t.Errorf("session %s not reported", id)
		}
	}
	if got["recycled"].Killed || got["dead"].Killed || got["current-dead"].Killed || !got["woken"].Killed {
		t.Errorf("killed flags wrong: %+v", got)
	}
	for _, id := range []string{"current", "me", "abandoned", "done"} {
		if st, _ := LoadState(dir, id); st.Finalized != (id == "done") {
			t.Errorf("session %s finalized=%v, want untouched", id, st.Finalized)
		}
	}
	writeStateFile(t, dir, State{SessionID: "guarded", OwnerPID: 1, DaemonPID: 5151, StartedAt: started, Heartbeat: now})
	if _, err := StopStale(dir, "cur", 7000, "", alive, held, func(int) error { return os.ErrPermission }, now); err != nil {
		t.Fatal(err)
	}
	if st, _ := LoadState(dir, "guarded"); st.Finalized {
		t.Error("a session whose daemon could not be signalled was finalized")
	}
}

// The default liveness probe is the session lock itself.
func TestLockHeldSeesARealClaim(t *testing.T) {
	dir := t.TempDir()
	if lockHeld(dir, "s-lock") {
		t.Fatal("a free lock reads as held")
	}
	claim, err := ClaimSession(dir, "s-lock")
	if err != nil {
		t.Fatal(err)
	}
	if !lockHeld(dir, "s-lock") {
		t.Error("a held lock reads as free")
	}
	_ = claim.Close()
	if lockHeld(dir, "s-lock") {
		t.Error("a released lock reads as held")
	}
}

// blockingFlusher holds RunOnce until released, the shape of an upload in
// flight when a reaper's signal lands.
type blockingFlusher struct {
	entered chan struct{}
	release chan struct{}
}

func (f *blockingFlusher) RunOnce(ctx context.Context) (int, error) {
	// The daemon may run this again after the cancel (a pending flush tick,
	// the shutdown drain); only the first entry is announced, and a later
	// one must not block the daemon's exit.
	select {
	case f.entered <- struct{}{}:
	default:
	}
	select {
	case <-f.release:
	case <-ctx.Done():
	}
	return 0, nil
}

// A reaper finalizes the state on disk while the daemon is mid-upload; the
// daemon's own heartbeat after the cancel must not write the live state
// back over it.
func TestADyingDaemonsHeartbeatNeverOverwritesAFinalizedState(t *testing.T) {
	dir := t.TempDir()
	f := &blockingFlusher{entered: make(chan struct{}, 1), release: make(chan struct{})}
	d, err := New(Options{
		Dir: dir, SessionID: "s-reaped", OwnerPID: 999, Build: "old",
		PollInterval:  time.Hour,
		FlushInterval: 5 * time.Millisecond,
		Flush:         f,
		Alive:         func(int) bool { return true },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	<-f.entered
	cancel()
	st, _ := LoadState(dir, "s-reaped")
	st.Finalized, st.EndReason, st.EndedAt = true, "daemon_stale", time.Now()
	if !writeState(dir, statePath(dir, "s-reaped"), st) {
		t.Fatal("could not write the finalized state")
	}
	close(f.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon did not stop")
	}
	if on, _ := LoadState(dir, "s-reaped"); !on.Finalized || on.EndReason != "daemon_stale" {
		t.Errorf("the dying daemon wrote its live state back over the reaper's: %+v", on)
	}
}

// The ended marker leaves with its state file, and an orphan marker (a
// session that never had a daemon) leaves by age.
func TestSweepRemovesEndedMarkers(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	writeStateFile(t, dir, State{SessionID: "gone", OwnerPID: 1, DaemonPID: 1, Finalized: true, EndedAt: now.Add(-48 * time.Hour)})
	if err := MarkEnded(dir, "gone", now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := MarkEnded(dir, "orphan", now.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "orphan.json.ended"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := MarkEnded(dir, "fresh", now); err != nil {
		t.Fatal(err)
	}
	if n, err := Sweep(dir, 24*time.Hour, now); err != nil || n != 1 {
		t.Fatalf("swept %d, err %v", n, err)
	}
	for _, name := range []string{"gone.json.ended", "orphan.json.ended"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived the sweep", name)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "fresh.json.ended")); err != nil {
		t.Error("a fresh marker was swept")
	}
}

// The heartbeat alone is guarded: after a reaper finalized the state on
// disk, a beat leaves it; a new daemon for the same pid (an image exec'd in
// place) and a finalize still write.
func TestABeatLeavesAFinalizedStateButANewDaemonWrites(t *testing.T) {
	dir := t.TempDir()
	d, err := New(Options{Dir: dir, SessionID: "s-beat", OwnerPID: 999, Build: "old", Alive: func(int) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := LoadState(dir, "s-beat")
	st.Finalized, st.EndReason, st.EndedAt = true, "daemon_stale", time.Now()
	if !writeState(dir, statePath(dir, "s-beat"), st) {
		t.Fatal("could not write the finalized state")
	}
	d.beat()
	if on, _ := LoadState(dir, "s-beat"); !on.Finalized || on.EndReason != "daemon_stale" {
		t.Errorf("a heartbeat wrote over the reaper's finalize: %+v", on)
	}
	// The same pid starts a new daemon (an exec in place): its first save
	// must land, or the session reads as finalized while a daemon runs.
	d2, err := New(Options{Dir: dir, SessionID: "s-beat", OwnerPID: 999, Build: "new", Alive: func(int) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if on, _ := LoadState(dir, "s-beat"); on.Finalized || on.Build != "new" {
		t.Errorf("a new daemon's first save was refused: %+v", on)
	}
	if err := d2.Finalize("owner_exited"); err != nil {
		t.Fatal(err)
	}
	if on, _ := LoadState(dir, "s-beat"); !on.Finalized || on.EndReason != "owner_exited" {
		t.Errorf("a finalize did not land: %+v", on)
	}
}

// A daemon starting for a session holds that session's lock before New
// rewrites the state, which still names its dead predecessor; the reaper it
// runs must not take its own lock as proof that the predecessor lives.
func TestStopStaleSkipsTheCallersOwnSession(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 17, 14, 0, 0, 0, time.UTC)
	writeStateFile(t, dir, State{SessionID: "mine", OwnerPID: 1, DaemonPID: 4242, Build: "old", StartedAt: now.Add(-time.Hour), Heartbeat: now.Add(-time.Hour)})
	claim, err := ClaimSession(dir, "mine")
	if err != nil {
		t.Fatal(err)
	}
	defer claim.Close()
	var killed []int
	alive := func(int) bool { return true }
	if _, err := StopStale(dir, "cur", 7000, "", alive, nil, func(pid int) error { killed = append(killed, pid); return nil }, now); err != nil {
		t.Fatal(err)
	}
	if len(killed) != 1 {
		t.Fatalf("without the exclusion the caller's own lock vouched for pid 4242: killed %v (this is the hole the exclusion closes)", killed)
	}
	writeStateFile(t, dir, State{SessionID: "mine", OwnerPID: 1, DaemonPID: 4242, Build: "old", StartedAt: now.Add(-time.Hour), Heartbeat: now.Add(-time.Hour)})
	killed = nil
	if _, err := StopStale(dir, "cur", 7000, "mine", alive, nil, func(pid int) error { killed = append(killed, pid); return nil }, now); err != nil {
		t.Fatal(err)
	}
	if len(killed) != 0 {
		t.Errorf("the reaper signalled its own session's predecessor pid: %v", killed)
	}
	if st, _ := LoadState(dir, "mine"); st.Finalized {
		t.Error("the caller's own session state was finalized")
	}
}
