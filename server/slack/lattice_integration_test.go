//go:build integration

package slack

import (
	"context"
	"testing"
	"time"
)

// The digest and the live thread restate the store's default view: a session
// in which nothing happened and a harness helper run are never due a message,
// unless the device reported capture loss around them, in which case the row
// is whatever its type says it is.
func TestHiddenSessionTypesAreNeverDueAMessage(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		typ, head string
		wantDue   bool
	}{
		{"a person's session", "user", "complete", true},
		{"an automation run", "automation", "complete", true},
		{"a session in which nothing happened", "empty", "unknown", false},
		{"a harness helper run", "internal", "complete", false},
		{"an empty row whose device lost captures", "empty", "capture_loss", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			freshDB(t)
			addSession(t, sessionRow{id: "s", email: "ana@example.org", ended: true})
			if _, err := pool.Exec(ctx,
				`UPDATE sessions SET session_type = $1, head_state = $2, mirror_request = 'on' WHERE session_id = 's'`,
				tc.typ, tc.head); err != nil {
				t.Fatal(err)
			}
			if err := savePrefs(ctx, pool, Prefs{Email: "ana@example.org", Mode: ModeChannel, Channel: "C1"},
				time.Now().Add(-72*time.Hour)); err != nil {
				t.Fatalf("savePrefs: %v", err)
			}

			due := false
			for _, id := range ids(mustEligible(t, wideOpen())) {
				if id == "s" {
					due = true
				}
			}
			if due != tc.wantDue {
				t.Errorf("digest due = %v, want %v", due, tc.wantDue)
			}

			live, err := liveEligible(ctx, pool, time.Now().Add(-24*time.Hour), 10)
			if err != nil {
				t.Fatal(err)
			}
			liveDue := false
			for _, c := range live {
				if c.SessionID == "s" {
					liveDue = true
				}
			}
			if liveDue != tc.wantDue {
				t.Errorf("live eligible = %v, want %v", liveDue, tc.wantDue)
			}
		})
	}
}
