package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// BootstrapOutcome is what BootstrapAdmins did, or declined to do, for one
// address, so the boot log can say it in as many words.
type BootstrapOutcome struct {
	Email string
	// Created is true when this boot wrote the row.
	Created bool
	// Role and Disabled describe the row that was already there when Created
	// is false. A member row, or a disabled admin, is reported and left
	// exactly as it was: see BootstrapAdmins.
	Role     Role
	Disabled bool
}

// bootstrapAddedBy is the added_by marker on a row this function wrote, so
// the admin page can tell a bootstrapped admin from one a person granted.
const bootstrapAddedBy = "bootstrap"

// BootstrapAdmins makes sure each address in emails has a principals row,
// writing an active admin row for any that has none. It is how a fresh
// deployment gets its first administrator: the addresses come from
// ADMIN_EMAILS (server/app/config.go) and the server applies them once per
// boot, after migrations, so the roster is never empty on the day the
// database is.
//
// It only ever adds. ON CONFLICT DO NOTHING is the whole of the policy: a row
// that exists is left as it is whatever its role or disabled_at, so a person
// demoted or switched off in the admin page stays that way across every
// restart, and removing an address from the variable removes nothing. A
// variable that could re-grant on boot would make "who is an admin" a
// function of the deployment manifest rather than of the roster, and a
// deploy that silently reinstated a role somebody had deliberately removed
// would be indistinguishable from a quiet re-grant. The outcome for an
// existing row is reported rather than acted on, so an operator who set
// ADMIN_EMAILS and sees no change learns why from the log rather than from a
// locked-out colleague.
//
// Each creation is recorded in principal_changes with the address as its own
// actor and a NULL from_role, the shape EnrolSelf and the admin page use for
// a creation, so the trail for a bootstrapped admin reads the same way as
// everybody else's.
//
// Empty input is a no-op that touches nothing: the ordinary boot of a
// deployment that manages its roster in the page issues no statement here.
func (s *Store) BootstrapAdmins(ctx context.Context, emails []string) ([]BootstrapOutcome, error) {
	var out []BootstrapOutcome
	for _, raw := range emails {
		email := strings.ToLower(strings.TrimSpace(raw))
		if email == "" {
			continue
		}
		if at := strings.LastIndex(email, "@"); at <= 0 || at == len(email)-1 {
			return out, fmt.Errorf("store: bootstrap admin %q: not an email address", raw)
		}
		o, err := s.bootstrapAdmin(ctx, email)
		if err != nil {
			return out, err
		}
		out = append(out, o)
	}
	return out, nil
}

func (s *Store) bootstrapAdmin(ctx context.Context, email string) (BootstrapOutcome, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return BootstrapOutcome{}, fmt.Errorf("store: begin admin bootstrap: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var created bool
	if err := tx.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO principals (email, role, added_by)
			VALUES ($1, 'admin', $2)
			ON CONFLICT (email) DO NOTHING
			RETURNING email
		)
		SELECT EXISTS (SELECT 1 FROM ins)`, email, bootstrapAddedBy).Scan(&created); err != nil {
		return BootstrapOutcome{}, fmt.Errorf("store: bootstrap admin %q: %w", email, err)
	}
	o := BootstrapOutcome{Email: email, Created: created}
	if created {
		if _, err := tx.Exec(ctx, `
			INSERT INTO principal_changes (actor, target, from_role, to_role, from_disabled, to_disabled)
			VALUES ($1, $1, NULL, 'admin', false, false)`, email); err != nil {
			return BootstrapOutcome{}, fmt.Errorf("store: record bootstrap of %q: %w", email, err)
		}
	} else {
		// Read back what stood in the way, for the log line. Not finding the
		// row here would mean it vanished between the two statements of one
		// transaction, which is not something to paper over.
		p, found, err := principalLookup(ctx, tx, email)
		if err != nil {
			return BootstrapOutcome{}, fmt.Errorf("store: read existing principal %q: %w", email, err)
		}
		if !found {
			return BootstrapOutcome{}, errors.New("store: bootstrap admin " + email + ": the row neither inserted nor exists")
		}
		o.Role = p.Role
		o.Disabled = p.DisabledAt != nil
	}
	if err := tx.Commit(ctx); err != nil {
		return BootstrapOutcome{}, fmt.Errorf("store: commit admin bootstrap of %q: %w", email, err)
	}
	return o, nil
}
