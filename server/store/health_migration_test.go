package store

import (
	"strings"
	"testing"
)

// TestHealthRollupMigrationIsBoundedAndAdditive pins 0020 to what the contract
// section 3 names for PR E and to the migration rules: four tables and one
// index, a bounded seed that reads health_reports only, no DROP of anything,
// and a stated rollback floor. Judged on the statements rather than the prose.
func TestHealthRollupMigrationIsBoundedAndAdditive(t *testing.T) {
	body, err := migrations.ReadFile("migrations/0020_health_rollups.sql")
	if err != nil {
		t.Fatalf("read 0020: %v", err)
	}
	sql := string(body)
	upper := strings.ToUpper(sql)
	for _, forbidden := range []string{"DROP ", "FROM EVENTS", "FROM MESSAGES", "FROM SESSIONS", "UPDATE EVENTS", "UPDATE MESSAGES", "ALTER TABLE"} {
		if strings.Contains(upper, forbidden) {
			t.Errorf("0020 contains %s", forbidden)
		}
	}
	if !strings.Contains(sql, "Rollback floor") {
		t.Error("0020 does not state its rollback floor")
	}
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS health_latest",
		"UNIQUE NULLS NOT DISTINCT (email, device_id)",
		"CREATE TABLE IF NOT EXISTS health_hourly",
		"UNIQUE NULLS NOT DISTINCT (email, device_id, hour)",
		"CREATE INDEX IF NOT EXISTS health_reports_received_at_idx ON health_reports (received_at)",
		"CREATE TABLE IF NOT EXISTS fleet_mutes",
		"PRIMARY KEY (email, kind)",
		"CREATE TABLE IF NOT EXISTS fleet_ticks",
		"last_tick_at TIMESTAMPTZ NOT NULL",
		"until      TIMESTAMPTZ NOT NULL",
		"ON CONFLICT (email, device_id) DO NOTHING",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("0020 lacks %q", want)
		}
	}
	for _, col := range []string{"reports", "worst", "conditions", "drops", "quarantined", "parked", "capture_blocked_secs", "empty_starts"} {
		if !strings.Contains(sql, "\n  "+col) {
			t.Errorf("health_hourly lacks the %s column", col)
		}
	}
	// A version 1 report carries neither parked nor empty starts; the hour
	// must be able to say unknown, so neither column may be NOT NULL.
	for _, col := range []string{"parked               BIGINT,", "empty_starts         INT,"} {
		if !strings.Contains(sql, "\n  "+col) {
			t.Errorf("health_hourly.%s is not nullable", strings.Fields(col)[0])
		}
	}
	// The seed keeps the newest report by the report's own clock, the rule
	// the upsert in PutHealthReport keeps.
	if !strings.Contains(sql, "ORDER BY email, device_id, emitted_at DESC") {
		t.Error("the health_latest seed does not order by emitted_at")
	}
	// The one index is on the raw table's arrival stamp, which is what the
	// seven-day sweep deletes by; nothing else in this file may build one.
	if got := strings.Count(upper, "CREATE INDEX"); got != 1 {
		t.Errorf("0020 builds %d indexes, want exactly the received_at one", got)
	}
	// The seed is the only statement that reads a table, and it reads the raw
	// health table through the (email, device_id, emitted_at) idempotency
	// index.
	if got := strings.Count(upper, "FROM HEALTH_REPORTS"); got != 1 {
		t.Errorf("0020 reads health_reports %d times, want once for the seed", got)
	}
}
