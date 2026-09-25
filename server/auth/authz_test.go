package auth

import (
	"testing"
	"time"
)

var authzNow = time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)

func TestCanRead(t *testing.T) {
	past := authzNow.Add(-time.Hour)
	future := authzNow.Add(time.Hour)

	toMe := Grant{ID: "g-me", Grantee: "viewer@example.com"}
	toAnyone := Grant{ID: "g-any"}
	toSomeoneElse := Grant{ID: "g-other", Grantee: "third@example.com"}

	cases := []struct {
		name    string
		viewer  Viewer
		owner   string
		grants  []Grant
		allowed bool
		via     Via
		audit   bool
		shareID string
	}{
		{
			name:    "member reading their own session",
			viewer:  Viewer{Email: "viewer@example.com", Role: RoleMember},
			owner:   "viewer@example.com",
			allowed: true, via: ViaOwn,
		},
		{
			name:    "admin reading their own session is own, not admin",
			viewer:  Viewer{Email: "boss@example.com", Role: RoleAdmin},
			owner:   "boss@example.com",
			allowed: true, via: ViaOwn,
		},
		{
			name:    "admin reading a colleague is audited",
			viewer:  Viewer{Email: "boss@example.com", Role: RoleAdmin},
			owner:   "viewer@example.com",
			allowed: true, via: ViaAdmin, audit: true,
		},
		{
			name:   "member reading a colleague with no share",
			viewer: Viewer{Email: "viewer@example.com", Role: RoleMember},
			owner:  "someone@example.com",
		},
		{
			name:    "member with a share naming them",
			viewer:  Viewer{Email: "viewer@example.com", Role: RoleMember},
			owner:   "someone@example.com",
			grants:  []Grant{toMe},
			allowed: true, via: ViaShare, audit: true, shareID: "g-me",
		},
		{
			name:    "member with an open share",
			viewer:  Viewer{Email: "viewer@example.com", Role: RoleMember},
			owner:   "someone@example.com",
			grants:  []Grant{toAnyone},
			allowed: true, via: ViaShare, audit: true, shareID: "g-any",
		},
		{
			name:   "share naming somebody else",
			viewer: Viewer{Email: "viewer@example.com", Role: RoleMember},
			owner:  "someone@example.com",
			grants: []Grant{toSomeoneElse},
		},
		{
			name:   "expired share",
			viewer: Viewer{Email: "viewer@example.com", Role: RoleMember},
			owner:  "someone@example.com",
			grants: []Grant{{ID: "g", Grantee: "viewer@example.com", ExpiresAt: past}},
		},
		{
			name:   "share expiring exactly now is already expired",
			viewer: Viewer{Email: "viewer@example.com", Role: RoleMember},
			owner:  "someone@example.com",
			grants: []Grant{{ID: "g", Grantee: "viewer@example.com", ExpiresAt: authzNow}},
		},
		{
			name:    "share expiring later still works",
			viewer:  Viewer{Email: "viewer@example.com", Role: RoleMember},
			owner:   "someone@example.com",
			grants:  []Grant{{ID: "g", Grantee: "viewer@example.com", ExpiresAt: future}},
			allowed: true, via: ViaShare, audit: true, shareID: "g",
		},
		{
			name:   "revoked share",
			viewer: Viewer{Email: "viewer@example.com", Role: RoleMember},
			owner:  "someone@example.com",
			grants: []Grant{{ID: "g", Grantee: "viewer@example.com", RevokedAt: past}},
		},
		{
			name:   "revoked open share",
			viewer: Viewer{Email: "viewer@example.com", Role: RoleMember},
			owner:  "someone@example.com",
			grants: []Grant{{ID: "g", RevokedAt: past}},
		},
		{
			name:    "a revocation scheduled in the future has not happened yet",
			viewer:  Viewer{Email: "viewer@example.com", Role: RoleMember},
			owner:   "someone@example.com",
			grants:  []Grant{{ID: "g", Grantee: "viewer@example.com", RevokedAt: future}},
			allowed: true, via: ViaShare, audit: true, shareID: "g",
		},
		{
			name:    "the first active matching share wins",
			viewer:  Viewer{Email: "viewer@example.com", Role: RoleMember},
			owner:   "someone@example.com",
			grants:  []Grant{{ID: "dead", Grantee: "viewer@example.com", RevokedAt: past}, toMe},
			allowed: true, via: ViaShare, audit: true, shareID: "g-me",
		},
		{
			name:    "ownership is case insensitive",
			viewer:  Viewer{Email: "Viewer@Example.com", Role: RoleMember},
			owner:   "viewer@example.com",
			allowed: true, via: ViaOwn,
		},
		{
			name:    "a grantee is matched case insensitively too",
			viewer:  Viewer{Email: "VIEWER@example.com", Role: RoleMember},
			owner:   "someone@example.com",
			grants:  []Grant{toMe},
			allowed: true, via: ViaShare, audit: true, shareID: "g-me",
		},
		{
			name:   "an unauthenticated caller never matches an open share",
			viewer: Viewer{Email: "", Role: RoleMember},
			owner:  "someone@example.com",
			grants: []Grant{toAnyone},
		},
		{
			name:   "an unauthenticated caller never matches an empty owner",
			viewer: Viewer{Email: "", Role: RoleMember},
			owner:  "",
		},
		{
			name:   "an unauthenticated caller claiming admin is still nobody",
			viewer: Viewer{Email: "  ", Role: RoleAdmin},
			owner:  "someone@example.com",
		},
		{
			name:   "an unrecognised role is not an admin",
			viewer: Viewer{Email: "viewer@example.com", Role: Role("owner")},
			owner:  "someone@example.com",
		},
		{
			name:   "a session with no owner is readable by nobody but an admin",
			viewer: Viewer{Email: "viewer@example.com", Role: RoleMember},
			owner:  "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CanRead(tc.viewer, tc.owner, tc.grants, authzNow)
			if got.Allowed != tc.allowed {
				t.Fatalf("Allowed = %v, want %v (%+v)", got.Allowed, tc.allowed, got)
			}
			if got.Via != tc.via {
				t.Errorf("Via = %q, want %q", got.Via, tc.via)
			}
			if got.Audit != tc.audit {
				t.Errorf("Audit = %v, want %v", got.Audit, tc.audit)
			}
			if got.ShareID != tc.shareID {
				t.Errorf("ShareID = %q, want %q", got.ShareID, tc.shareID)
			}
			// The contract ties auditing to the rule, not to the caller: any
			// allow that is not "own" has to leave a trail.
			if got.Allowed && got.Audit != (got.Via != ViaOwn) {
				t.Errorf("audit flag disagrees with via=%q", got.Via)
			}
			if !got.Allowed && (got.Via != "" || got.ShareID != "") {
				t.Errorf("denial carried a rule: %+v", got)
			}
		})
	}
}

func TestCanReadExhaustive(t *testing.T) {
	// Every combination of the axes the rule reads, checked against an
	// independently written expectation rather than the implementation.
	roles := []Role{RoleAdmin, RoleMember, Role("")}
	viewers := []string{"viewer@example.com", ""}
	owners := []string{"viewer@example.com", "other@example.com", ""}
	grantSets := map[string][]Grant{
		"none":    nil,
		"mine":    {{ID: "g", Grantee: "viewer@example.com"}},
		"open":    {{ID: "g"}},
		"others":  {{ID: "g", Grantee: "third@example.com"}},
		"expired": {{ID: "g", Grantee: "viewer@example.com", ExpiresAt: authzNow.Add(-time.Second)}},
		"revoked": {{ID: "g", Grantee: "viewer@example.com", RevokedAt: authzNow.Add(-time.Second)}},
	}

	for _, role := range roles {
		for _, viewer := range viewers {
			for _, owner := range owners {
				for label, grants := range grantSets {
					v := Viewer{Email: viewer, Role: role}
					got := CanRead(v, owner, grants, authzNow)

					wantAllowed, wantVia := false, Via("")
					switch {
					case viewer == "":
						// nobody
					case SameEmail(viewer, owner):
						wantAllowed, wantVia = true, ViaOwn
					case role == RoleAdmin:
						wantAllowed, wantVia = true, ViaAdmin
					case label == "mine" || label == "open":
						wantAllowed, wantVia = true, ViaShare
					}

					if got.Allowed != wantAllowed || got.Via != wantVia {
						t.Errorf("role=%q viewer=%q owner=%q grants=%s: got %+v, want allowed=%v via=%q",
							role, viewer, owner, label, got, wantAllowed, wantVia)
					}
				}
			}
		}
	}
}

func TestCanShare(t *testing.T) {
	owner := "owner@example.com"
	cases := []struct {
		name string
		v    Viewer
		want bool
	}{
		{"the owner", Viewer{Email: owner, Role: RoleMember}, true},
		{"an admin", Viewer{Email: "boss@example.com", Role: RoleAdmin}, true},
		{"a member who was shared with cannot reshare", Viewer{Email: "guest@example.com", Role: RoleMember}, false},
		{"nobody", Viewer{Email: "", Role: RoleAdmin}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanShare(tc.v, owner); got != tc.want {
				t.Errorf("CanShare = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGrantActive(t *testing.T) {
	if (Grant{}).Active(authzNow) != true {
		t.Error("a grant with no expiry and no revocation should be active")
	}
	if (Grant{ExpiresAt: authzNow.Add(time.Hour)}).Active(authzNow) != true {
		t.Error("a grant expiring later should be active")
	}
	if (Grant{RevokedAt: authzNow}).Active(authzNow) != false {
		t.Error("a grant revoked exactly now should not be active")
	}
}

func TestRoleParsing(t *testing.T) {
	for _, s := range []string{"admin", "ADMIN", " member "} {
		if _, err := ParseRole(s); err != nil {
			t.Errorf("ParseRole(%q): %v", s, err)
		}
	}
	for _, s := range []string{"", "owner", "administrator", "root"} {
		if r, err := ParseRole(s); err == nil {
			t.Errorf("ParseRole(%q) = %q, want an error", s, r)
		}
	}
}

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"  Dev@Example.com ": "dev@example.com",
		"dev@example.com":    "dev@example.com",
		"":                   "",
		"   ":                "",
		// Dots and plus tags are Gmail delivery behaviour, not identity: two
		// distinct Workspace accounts must not fold into one principal.
		"first.last+ci@example.com": "first.last+ci@example.com",
	}
	for in, want := range cases {
		if got := Normalize(in); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
	if SameEmail("", "") {
		t.Error("two empty addresses must not be the same person")
	}
}
