//go:build integration

package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestIntegrationRollupReadsAbsentVersionTwoFieldsAsZero is the adversarial
// pass's F1 at the ledger. The 788dcb3 client omits zero-valued fields, so a
// version 2 report with nothing parked and no empty starts carries neither
// key; the hour built from it says zero, and only an hour built from
// version 1 reports says unknown.
func TestIntegrationRollupReadsAbsentVersionTwoFieldsAsZero(t *testing.T) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustPrincipal(t, s, "omit@example.com", RoleMember)
	v2, err := s.EnrollDevice(ctx, Device{Email: "omit@example.com"}, []byte("h-omit-2"), time.Time{})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	v1, err := s.EnrollDevice(ctx, Device{Email: "omit@example.com", Hostname: "old"}, []byte("h-omit-1"), time.Time{})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE health_reports, health_latest, health_hourly`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	at := healthClock.Add(-10 * time.Minute)
	for _, row := range []struct {
		device, report string
	}{
		{v2.ID, `{"schema_version":2,"agent_version":"788dcb3","agent_commit":"788dcb3","spool":{"pending":0,"quarantine":0}}`},
		{v1.ID, `{"schema_version":1,"agent_version":"23713ea","spool":{"pending":0}}`},
	} {
		if _, err := pool.Exec(ctx, `
			INSERT INTO health_reports (email, device_id, emitted_at, worst, report)
			VALUES ('omit@example.com', $1::uuid, $2, 'info', $3::jsonb)`, row.device, at, row.report); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if _, locked, err := s.RollupHealth(ctx, healthClock.Add(-2*time.Hour).Truncate(time.Hour), healthClock.Add(time.Hour)); err != nil || !locked {
		t.Fatalf("RollupHealth: locked=%v err=%v", locked, err)
	}
	hours, err := s.HealthHours(ctx, healthClock.Add(-2*time.Hour))
	if err != nil {
		t.Fatalf("HealthHours: %v", err)
	}
	for _, h := range hours {
		switch h.DeviceID {
		case v2.ID:
			if h.Parked == nil || *h.Parked != 0 || h.EmptyStarts == nil || *h.EmptyStarts != 0 {
				t.Errorf("the version 2 hour reads parked %v empty starts %v, want zero and zero", h.Parked, h.EmptyStarts)
			}
		case v1.ID:
			if h.Parked != nil || h.EmptyStarts != nil {
				t.Errorf("the version 1 hour reads parked %v empty starts %v, want unknown", h.Parked, h.EmptyStarts)
			}
		}
	}
	if len(hours) != 2 {
		t.Errorf("rolled %d hours, want one per machine", len(hours))
	}
}

// TestIntegrationRecomputingAnHourAfterItsPredecessorWasSweptKeepsItsDrops
// is F9. The sweep deletes raw rows past the window; the next rollup over
// the oldest retained hour then found no predecessor for the machine's
// first report there, called it a first report ever, and wrote 0 over the
// rise the ledger already held. The ledger keeps each hour's closing
// counters, and a rollup with no raw predecessor seeds from them.
func TestIntegrationRecomputingAnHourAfterItsPredecessorWasSweptKeepsItsDrops(t *testing.T) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustPrincipal(t, s, "seed@example.com", RoleMember)
	dev, err := s.EnrollDevice(ctx, Device{Email: "seed@example.com"}, []byte("h-seed"), time.Time{})
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE health_reports, health_latest, health_hourly`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	insert := func(at time.Time, dropped int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `
			INSERT INTO health_reports (email, device_id, emitted_at, received_at, worst, report)
			VALUES ('seed@example.com', $1::uuid, $2, $2, 'info', $3::jsonb)`, dev.ID, at,
			fmt.Sprintf(`{"schema_version":1,"agent_version":"23713ea","spool":{"dropped":{"disk_full":%d}}}`, dropped)); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	old := healthClock.Add(-8 * 24 * time.Hour)
	newer := healthClock.Add(-6 * 24 * time.Hour)
	insert(old, 5)
	insert(newer, 9)

	hourOf := func(hour time.Time) *HealthHour {
		t.Helper()
		hours, err := s.HealthHours(ctx, healthClock.Add(-9*24*time.Hour))
		if err != nil {
			t.Fatalf("HealthHours: %v", err)
		}
		for i := range hours {
			if hours[i].Hour.Equal(hour.Truncate(time.Hour)) {
				return &hours[i]
			}
		}
		return nil
	}
	// The first sweep rolls both days up (the newer hour is the rise of
	// four) and then deletes the older raw row, past the window.
	sweep, err := s.SweepHealth(ctx, healthClock)
	if err != nil {
		t.Fatalf("SweepHealth: %v", err)
	}
	if sweep.RawDeleted != 1 {
		t.Fatalf("sweep = %+v, want the one raw row past the window deleted", sweep)
	}
	if row := hourOf(newer); row == nil || row.Drops != 4 {
		t.Fatalf("the newer hour after the first sweep = %+v, want the rise of 4", row)
	}
	if row := hourOf(old); row == nil || row.Drops != 0 {
		t.Fatalf("the first report ever = %+v, want a baseline of 0", row)
	}

	// The predecessor is gone. Recomputing the newer hour, as the next
	// sweep or a tick whose window covers it does, keeps what the ledger
	// knows.
	if _, locked, err := s.RollupHealth(ctx, newer.Truncate(time.Hour), newer.Truncate(time.Hour).Add(time.Hour)); err != nil || !locked {
		t.Fatalf("RollupHealth: locked=%v err=%v", locked, err)
	}
	if row := hourOf(newer); row == nil || row.Drops != 4 {
		t.Errorf("recomputing the hour after its predecessor was swept wrote %+v, want the rise of 4 kept", row)
	}
	if _, err := s.SweepHealth(ctx, healthClock); err != nil {
		t.Fatalf("second SweepHealth: %v", err)
	}
	if row := hourOf(newer); row == nil || row.Drops != 4 {
		t.Errorf("the second sweep wrote %+v, want the rise of 4 kept", row)
	}
	// A later report on the same machine is charged from the ledger's
	// closing counter, not from zero: nine to twelve is three.
	later := newer.Add(2 * time.Hour)
	insert(later, 12)
	if _, _, err := s.RollupHealth(ctx, later.Truncate(time.Hour), later.Truncate(time.Hour).Add(time.Hour)); err != nil {
		t.Fatalf("RollupHealth: %v", err)
	}
	if row := hourOf(later); row == nil || row.Drops != 3 {
		t.Errorf("the hour after a gap = %+v, want the rise of 3 over the ledger's closing counter", row)
	}
}
