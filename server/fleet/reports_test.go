package fleet

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestVersionTwoReportsOmitZeroValuedFieldsAndReadAsZero is the adversarial
// pass's F1. The 788dcb3 client serialises its health struct with omitempty,
// so a machine with nothing parked and no empty starts sends neither key
// (production, 2026-09-13: nineteen of nineteen version 2 reports lacked
// both). Absence on a version 2 report therefore means zero; only a version
// 1 report, which has no such fields, reads them as unknown.
func TestVersionTwoReportsOmitZeroValuedFieldsAndReadAsZero(t *testing.T) {
	v2 := json.RawMessage(`{"schema_version":2,"agent_version":"788dcb3","hostname":"m","spool":{"pending":1,"quarantine":0}}`)
	r, err := DecodeReport("a@example.com", "d1", now, now, "info", "788dcb3", v2)
	if err != nil {
		t.Fatal(err)
	}
	if r.Parked == nil || *r.Parked != 0 {
		t.Errorf("a version 2 report without spool.parked decoded as parked %v, want zero", r.Parked)
	}
	if r.EmptyStarts24h == nil || *r.EmptyStarts24h != 0 {
		t.Errorf("a version 2 report without empty_starts_24h decoded as %v, want zero", r.EmptyStarts24h)
	}
	if r.Build() != "788dcb3" {
		t.Errorf("a version 2 report without agent_commit reads build %q, want agent_version", r.Build())
	}

	v1 := json.RawMessage(`{"schema_version":1,"agent_version":"23713ea","spool":{"pending":1}}`)
	old, err := DecodeReport("b@example.com", "d2", now, now, "info", "23713ea", v1)
	if err != nil {
		t.Fatal(err)
	}
	if old.Parked != nil || old.EmptyStarts24h != nil {
		t.Errorf("a version 1 report decoded as parked %v empty starts %v, want unknown", old.Parked, old.EmptyStarts24h)
	}

	// Through the evaluation: the page prints the version 2 machine's zero
	// and n/a for the version 1 machine only.
	ev := Evaluate(Inputs{Now: now,
		Devices: []Device{{ID: "d1", Email: "a@example.com"}, {ID: "d2", Email: "b@example.com"}},
		Reports: []Report{r, old},
	})
	for _, m := range ev.Machines {
		switch m.DeviceID {
		case "d1":
			if m.EmptyStarts24h == nil || *m.EmptyStarts24h != 0 || m.Parked == nil || *m.Parked != 0 {
				t.Errorf("the version 2 machine reads empty starts %v parked %v, want zero and zero", m.EmptyStarts24h, m.Parked)
			}
		case "d2":
			if m.EmptyStarts24h != nil || m.Parked != nil {
				t.Errorf("the version 1 machine reads empty starts %v parked %v, want unknown", m.EmptyStarts24h, m.Parked)
			}
		}
	}
	if ev.Summary.ParkedDevices != 0 {
		t.Errorf("parked devices = %d, want none: zero parked is not a parked machine", ev.Summary.ParkedDevices)
	}
}

// TestAShortBuildIsNeverCurrentByPrefix is F6: the prefix comparison is for
// a seven-digit client sha against the manifest's full commit, and only
// that. A build of "7" is not on the published commit because it starts the
// same way.
func TestAShortBuildIsNeverCurrentByPrefix(t *testing.T) {
	m := &Manifest{Commit: "788dcb3a1b2c3d4e5f60718293a4b5c6d7e8f901", BuildDate: now.Add(-48 * time.Hour)}
	for _, b := range []string{"7", "78", "788dcb", "788DCB"} {
		if sameBuild(b, m.Commit) || sameBuild(m.Commit, b) {
			t.Errorf("build %q compared equal to the manifest commit by prefix", b)
		}
		state, _, _, _ := judgeBuild(b, []BuildSighting{{Email: "a@example.com", DeviceID: "d1", Build: b, LastSeen: now}}, m, now)
		if state == VersionCurrent {
			t.Errorf("a machine whose only sighting is build %q was judged current", b)
		}
	}
	for _, b := range []string{"788dcb3", "788DCB3", m.Commit} {
		if !sameBuild(b, m.Commit) || !sameBuild(m.Commit, b) {
			t.Errorf("build %q did not compare equal to the manifest commit", b)
		}
	}
}

// TestAFleetWideCTAHasNoPersonAndMutesUnderTheSentinel is F7. The
// missing-answer row is the fleet's, not a person's: its action names no
// one to ask, and it is muted under the sentinel person "*", which also
// silences a kind for everybody when an operator wants that.
func TestAFleetWideCTAHasNoPersonAndMutesUnderTheSentinel(t *testing.T) {
	day := now.Truncate(24 * time.Hour)
	in := Inputs{Now: now, Answers: []AnswerCohort{{Day: day, AgentVersion: "23713ea", Entrypoint: "cli", Turns: 100, Answered: 50}}}
	ev := Evaluate(in)
	var row *CTA
	for i := range ev.CTAs {
		if ev.CTAs[i].Kind == "missing_answer" {
			row = &ev.CTAs[i]
		}
	}
	if row == nil {
		t.Fatalf("no missing_answer CTA at a 50%% rate: %+v", ev.CTAs)
	}
	if row.Email != "" || strings.Contains(row.Action, "Ask ") || row.Command != "loop-sessions daemon --upgrade-now" {
		t.Errorf("fleet-wide row = email %q action %q command %q; want no person and no ask", row.Email, row.Action, row.Command)
	}

	in.Mutes = []Mute{{Email: "*", Kind: "missing_answer", Until: now.Add(time.Hour), Note: "fleet still converging"}}
	ev = Evaluate(in)
	for _, c := range ev.CTAs {
		if c.Kind == "missing_answer" {
			t.Errorf("the sentinel mute did not hold back the fleet-wide row: %+v", c)
		}
	}
	found := false
	for _, c := range ev.Muted {
		if c.Kind == "missing_answer" && c.Muted != nil && c.Muted.Email == "*" {
			found = true
		}
	}
	if !found {
		t.Errorf("the fleet-wide row is not listed under muted with the sentinel: %+v", ev.Muted)
	}

	// The sentinel is a wildcard: "*" on silent quiets every person's
	// silent line and the gauge, and lists each machine under muted.
	quiet := now.Add(-30 * time.Hour)
	wide := Inputs{Now: now,
		Devices: []Device{{ID: "d1", Email: "a@example.com"}, {ID: "d2", Email: "b@example.com"}},
		Reports: []Report{v1Report("a@example.com", "d1", "23713ea", quiet, 0), v1Report("b@example.com", "d2", "23713ea", quiet, 0)},
		Mutes:   []Mute{{Email: "*", Kind: "silent", Until: now.Add(time.Hour)}},
	}
	ev = Evaluate(wide)
	if ev.Summary.Silent != 0 || len(ev.Silent) != 0 {
		t.Errorf("a wildcard silent mute left silent=%d lines=%d", ev.Summary.Silent, len(ev.Silent))
	}
	if len(ev.Muted) != 2 || len(ev.CTAs) != 0 {
		t.Errorf("muted %d ctas %d, want both silent machines under muted", len(ev.Muted), len(ev.CTAs))
	}
}
