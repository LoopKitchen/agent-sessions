//go:build integration

package store

// The half of enrolment only a real Postgres can answer.
//
// store_test.go pins which statements are issued and with what arguments, which
// needs no server. What needs one is every claim this code makes about the
// database: that a laptop naming its own row lands back in that row rather than
// beside it, that the row it abandoned when its config was deleted is retired
// rather than left counting against coverage forever, that neither of those
// paths can reach across to another person, and that a device somebody revoked
// deliberately stays revoked when the same machine enrols again. Each of those
// is a property of the SQL, and the SQL is what a unit test cannot execute.
//
//	createdb loop_sessions_test
//	LOOP_SESSIONS_TEST_DSN=postgres:///loop_sessions_test go test -tags integration ./server/store/...

import (
	"context"
	"errors"
	"testing"
	"time"
)

// enrolledRow is what these tests read back: identity, and whether the row is
// still part of the fleet.
type enrolledRow struct {
	ID        string
	Email     string
	Hostname  string
	Revoked   bool
	TokenLive int
}

func readDevice(t *testing.T, id string) enrolledRow {
	t.Helper()
	var d enrolledRow
	err := pool.QueryRow(context.Background(), `
		SELECT d.id::text, d.email, COALESCE(d.hostname,''), d.revoked_at IS NOT NULL,
		       (SELECT count(*) FROM device_tokens t WHERE t.device_id = d.id AND t.revoked_at IS NULL)
		FROM devices d WHERE d.id = $1::uuid`, id).Scan(&d.ID, &d.Email, &d.Hostname, &d.Revoked, &d.TokenLive)
	if err != nil {
		t.Fatalf("read device %s: %v", id, err)
	}
	return d
}

func countDevices(t *testing.T, email string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM devices WHERE email = $1`, email).Scan(&n); err != nil {
		t.Fatalf("count devices: %v", err)
	}
	return n
}

// enrol is one run of the installer on a machine: a hostname, and whatever id
// that machine still has on disk.
func enrol(t *testing.T, s *Store, email, hostname, claim string, hash []byte) Device {
	t.Helper()
	dev, err := s.EnrollDevice(context.Background(),
		Device{ID: claim, Email: email, Hostname: hostname, OS: "darwin", Arch: "arm64"},
		hash, time.Time{})
	if err != nil {
		t.Fatalf("enrol %s on %s: %v", email, hostname, err)
	}
	return dev
}

// Property: a laptop that still knows its id keeps its row. This is the whole
// point of sending it — the row is the unit admin/fleet.go counts, so a second
// row for one machine is a permanent hole in coverage that nothing can close.
func TestIntegrationEnrolmentReusesTheRowTheLaptopNames(t *testing.T) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustPrincipal(t, s, "me@example.com", RoleMember)

	first := enrol(t, s, "me@example.com", "mymac", "", []byte("hash-one"))
	second := enrol(t, s, "me@example.com", "mymac", first.ID, []byte("hash-two"))

	if second.ID != first.ID {
		t.Fatalf("re-enrolment minted %s, want the row the laptop named (%s)", second.ID, first.ID)
	}
	if n := countDevices(t, "me@example.com"); n != 1 {
		t.Errorf("one laptop left %d device rows", n)
	}
	if row := readDevice(t, first.ID); row.Revoked {
		t.Error("the machine revoked its own row on the way back in")
	}
	// The new credential works, which is what the person came for.
	id, err := s.AuthenticateDevice(ctx, []byte("hash-two"))
	if err != nil {
		t.Fatalf("the credential minted by the second enrolment does not authenticate: %v", err)
	}
	if id.DeviceID != first.ID {
		t.Errorf("the new credential belongs to %s, want %s", id.DeviceID, first.ID)
	}
	// Enrolment updates the row it reuses rather than only its timestamps.
	if _, err := s.EnrollDevice(ctx, Device{
		ID: first.ID, Email: "me@example.com", Hostname: "mymac-renamed",
	}, []byte("hash-three"), time.Time{}); err != nil {
		t.Fatalf("third enrolment: %v", err)
	}
	if got := readDevice(t, first.ID).Hostname; got != "mymac-renamed" {
		t.Errorf("hostname = %q, want the one the machine last reported", got)
	}
}

// Property: a machine whose config was deleted has nothing to name, and the row
// it left behind is retired rather than left to count as a laptop that enrolled
// and never reported. This is the case that produced the phantoms in production,
// because a purged config is indistinguishable from a first install.
func TestIntegrationAPurgedLaptopSupersedesItsOwnPriorRow(t *testing.T) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustPrincipal(t, s, "me@example.com", RoleMember)

	before := enrol(t, s, "me@example.com", "mymac", "", []byte("hash-before"))
	after := enrol(t, s, "me@example.com", "mymac", "", []byte("hash-after"))

	if after.ID == before.ID {
		t.Fatal("a laptop that named no row was given the old one; the id is proof and a hostname is not")
	}
	old := readDevice(t, before.ID)
	if !old.Revoked {
		t.Error("the abandoned row is still live, so coverage counts a machine that will never report again")
	}
	if old.TokenLive != 0 {
		t.Errorf("the abandoned row keeps %d live credential(s)", old.TokenLive)
	}
	if _, err := s.AuthenticateDevice(ctx, []byte("hash-before")); !errors.Is(err, ErrNotFound) {
		t.Errorf("the superseded credential still authenticates: %v", err)
	}
	if row := readDevice(t, after.ID); row.Revoked {
		t.Error("the machine that just enrolled is revoked")
	}
	if _, err := s.AuthenticateDevice(ctx, []byte("hash-after")); err != nil {
		t.Errorf("the machine that just enrolled cannot authenticate: %v", err)
	}
	// Revoked rather than deleted: a revocation is reversible by a human with
	// one UPDATE, and the row is the only record that this laptop ever existed.
	if n := countDevices(t, "me@example.com"); n != 2 {
		t.Errorf("%d rows, want the superseded one kept alongside the new one", n)
	}
}

// Property: nothing about one person's enrolment reaches another person's rows —
// not the id it claims, and not the hostname it happens to share. Two people
// with laptops called the same thing is ordinary, and one of them reinstalling
// must not take the other off the fleet.
func TestIntegrationEnrolmentNeverTouchesAnotherPersonsDevices(t *testing.T) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustPrincipal(t, s, "her@example.com", RoleMember)
	mustPrincipal(t, s, "him@example.com", RoleMember)

	hers := enrol(t, s, "her@example.com", "macbook-pro", "", []byte("hash-hers"))

	// He claims her row by id, and enrols on a machine named the same as hers.
	his := enrol(t, s, "him@example.com", "macbook-pro", hers.ID, []byte("hash-his"))

	if his.ID == hers.ID {
		t.Fatal("one person's enrolment was written into another person's device row")
	}
	row := readDevice(t, hers.ID)
	if row.Revoked {
		t.Error("her laptop was revoked by his enrolment")
	}
	if row.Email != "her@example.com" {
		t.Errorf("her row now belongs to %q", row.Email)
	}
	if row.TokenLive != 1 {
		t.Errorf("her credential count is %d, want the one she enrolled with", row.TokenLive)
	}
	id, err := s.AuthenticateDevice(ctx, []byte("hash-hers"))
	if err != nil {
		t.Fatalf("her laptop stopped authenticating after his enrolment: %v", err)
	}
	if id.Email != "her@example.com" {
		t.Errorf("her credential now resolves to %q", id.Email)
	}
}

// Property: revocation is final. Re-running the installer on a laptop that was
// deliberately taken off the fleet enrols a new device rather than reviving the
// old one, so "revoke this stolen machine" cannot be undone by whoever holds it.
func TestIntegrationARevokedDeviceIsNeverResurrectedByReEnrolling(t *testing.T) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustPrincipal(t, s, "me@example.com", RoleMember)

	stolen := enrol(t, s, "me@example.com", "mymac", "", []byte("hash-stolen"))
	if err := s.RevokeDevice(ctx, Viewer{Email: "me@example.com"}, stolen.ID); err != nil {
		t.Fatalf("RevokeDevice: %v", err)
	}

	back := enrol(t, s, "me@example.com", "mymac", stolen.ID, []byte("hash-back"))
	if back.ID == stolen.ID {
		t.Fatal("re-enrolling revived the revoked device")
	}
	row := readDevice(t, stolen.ID)
	if !row.Revoked {
		t.Error("the revoked row is live again")
	}
	if row.TokenLive != 0 {
		t.Errorf("the revoked row has %d live credential(s) again", row.TokenLive)
	}
	if _, err := s.AuthenticateDevice(ctx, []byte("hash-stolen")); !errors.Is(err, ErrNotFound) {
		t.Errorf("the revoked credential authenticates again: %v", err)
	}
}

// Property: the guess is only made when there is nothing better. A person's
// second machine can carry the same hostname as their first, so a laptop that
// has identified itself exactly must not drag the other one off the fleet.
func TestIntegrationAnExactClaimSupersedesNoOtherMachine(t *testing.T) {
	s := newStore(t, nil)
	mustPrincipal(t, s, "me@example.com", RoleMember)

	desk := enrol(t, s, "me@example.com", "macbook-pro", "", []byte("hash-desk"))
	// The second machine, named the same, has no id of its own yet — so it does
	// supersede the first. That is the trade this design accepts.
	travel := enrol(t, s, "me@example.com", "macbook-pro", "", []byte("hash-travel"))
	if !readDevice(t, desk.ID).Revoked {
		t.Fatal("the fixture is wrong: a claimless enrolment should have superseded the first row")
	}

	// From here the travel machine knows its id, so re-enrolling it touches
	// nothing else, and the desk machine can come back the same way.
	if got := enrol(t, s, "me@example.com", "macbook-pro", travel.ID, []byte("hash-travel-2")); got.ID != travel.ID {
		t.Fatalf("re-enrolment minted %s, want %s", got.ID, travel.ID)
	}
	if readDevice(t, travel.ID).Revoked {
		t.Error("a machine that named its own row revoked itself")
	}
}

// Property: an enrolment that cannot proceed leaves nothing behind. The
// supersession runs before the roster check the insert performs, so a disabled
// person's laptop must not take their other machines off the fleet on its way to
// being refused.
func TestIntegrationARefusedEnrolmentSupersedesNothing(t *testing.T) {
	s := newStore(t, nil)
	ctx := context.Background()
	mustPrincipal(t, s, "leaver@example.com", RoleMember)

	had := enrol(t, s, "leaver@example.com", "mymac", "", []byte("hash-had"))
	if _, err := pool.Exec(ctx,
		`UPDATE principals SET disabled_at = now() WHERE email = 'leaver@example.com'`); err != nil {
		t.Fatalf("disable: %v", err)
	}

	if _, err := s.EnrollDevice(ctx, Device{Email: "leaver@example.com", Hostname: "mymac"},
		[]byte("hash-after"), time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a disabled principal enrolled a device: %v", err)
	}
	if readDevice(t, had.ID).Revoked {
		t.Error("a refused enrolment revoked a device on its way out")
	}
	if n := countDevices(t, "leaver@example.com"); n != 1 {
		t.Errorf("%d device rows after a refused enrolment, want the original only", n)
	}
}
