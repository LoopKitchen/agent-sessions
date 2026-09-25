package store

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestTheTickRollupTakesTheRetentionLock is the adversarial pass's F13. The
// tick's rollup and the sweep's batches both upsert health_hourly over the
// same hours, and two ON CONFLICT DO UPDATE statements that touch the same
// keys in different orders can deadlock. The sweep's batches take the
// retention lock; the tick's rollup must take the same one before it
// writes, so the two never run at once.
func TestTheTickRollupTakesTheRetentionLock(t *testing.T) {
	db := &fakeDB{stubs: []*stub{{match: "pg_try_advisory_xact_lock", rows: [][]any{{true}}}}}
	s := NewWithDB(db, nil)
	if _, locked, err := s.RollupHealth(context.Background(), retNow.Add(-2*time.Hour), retNow); err != nil || !locked {
		t.Fatalf("RollupHealth: locked=%v err=%v", locked, err)
	}
	lock, insert := -1, -1
	for i, c := range db.calls {
		switch {
		case strings.Contains(c.sql, "pg_try_advisory_xact_lock"):
			if lock < 0 {
				lock = i
			}
			if len(c.args) != 1 || c.args[0] != retentionLockKey {
				t.Errorf("the rollup took lock %v, want the retention lock %d", c.args, retentionLockKey)
			}
		case strings.Contains(c.sql, "INSERT INTO health_hourly"):
			insert = i
		}
	}
	if insert < 0 {
		t.Fatal("the rollup wrote nothing")
	}
	if lock < 0 || lock > insert {
		t.Errorf("the tick's rollup wrote health_hourly without the retention lock (lock at %d, insert at %d)", lock, insert)
	}
}

// TestTheTickRollupStandsDownWhileASweepBatchHoldsTheLock: a sweep batch in
// flight means the tick writes nothing this interval; its deltas are the
// next tick's, and nothing is an error.
func TestTheTickRollupStandsDownWhileASweepBatchHoldsTheLock(t *testing.T) {
	db := &fakeDB{stubs: []*stub{{match: "pg_try_advisory_xact_lock", rows: [][]any{{false}}}}}
	rolled, locked, err := NewWithDB(db, nil).RollupHealth(context.Background(), retNow.Add(-2*time.Hour), retNow)
	if err != nil || locked || rolled != 0 {
		t.Errorf("rollup under a held lock = rolled %d locked %v err %v, want nothing done and no error", rolled, locked, err)
	}
	for _, c := range db.calls {
		if strings.Contains(c.sql, "INSERT INTO health_hourly") {
			t.Errorf("wrote without the lock: %s", c.sql)
		}
	}
}
