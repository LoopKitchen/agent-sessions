package slack

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Groups: named destinations, and the activations that bind sessions to them.

// Group is one named destination.
type Group struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	OwnerEmail  string    `json:"owner_email"`
	Visibility  string    `json:"visibility"`
	Destination string    `json:"destination"`
	Disabled    bool      `json:"disabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// destinationRE mirrors the schema's CHECK so a bad destination is refused
// with a message rather than a constraint violation. C…/G… are channels; U…
// is a person, posted to as the bot's DM with them (chat.postMessage opens
// the conversation from the user id).
var destinationRE = regexp.MustCompile(`^[CGU][A-Z0-9]{6,20}$`)

// ValidDestination reports whether d is 'dm', a channel id or a user id.
func ValidDestination(d string) bool { return d == "dm" || destinationRE.MatchString(d) }

// ErrGroupNotFound distinguishes absence from failure on the CRUD paths.
var ErrGroupNotFound = errors.New("slack: no such group")

// listGroups returns the groups an email may see: their own plus org-visible.
func listGroups(ctx context.Context, db DB, email string) ([]Group, error) {
	rows, err := db.Query(ctx, `
		SELECT id, name, owner_email, visibility, destination, disabled, created_at, updated_at
		  FROM slack_groups
		 WHERE owner_email = $1 OR visibility = 'org'
		 ORDER BY owner_email <> $1, name`, email)
	if err != nil {
		return nil, fmt.Errorf("slack: list groups for %s: %w", email, err)
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.OwnerEmail, &g.Visibility, &g.Destination,
			&g.Disabled, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, fmt.Errorf("slack: read a group: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// saveGroup inserts or updates one group the caller owns.
func saveGroup(ctx context.Context, db DB, g Group, now time.Time) (Group, error) {
	if g.ID == 0 {
		err := db.QueryRow(ctx, `
			INSERT INTO slack_groups (name, owner_email, visibility, destination, disabled, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $6)
			RETURNING id, created_at, updated_at`,
			g.Name, g.OwnerEmail, g.Visibility, g.Destination, g.Disabled, now,
		).Scan(&g.ID, &g.CreatedAt, &g.UpdatedAt)
		if err != nil {
			return g, fmt.Errorf("slack: create group %q: %w", g.Name, err)
		}
		return g, nil
	}
	tag, err := db.Exec(ctx, `
		UPDATE slack_groups
		   SET name = $2, visibility = $3, destination = $4, disabled = $5, updated_at = $6
		 WHERE id = $1 AND owner_email = $7`,
		g.ID, g.Name, g.Visibility, g.Destination, g.Disabled, now, g.OwnerEmail)
	if err != nil {
		return g, fmt.Errorf("slack: update group %d: %w", g.ID, err)
	}
	if tag.RowsAffected() == 0 {
		return g, ErrGroupNotFound
	}
	return g, nil
}

// resolveGroup finds a group by id or by name, visible to email. Name lookup
// prefers the caller's own group over an org one of the same name, which is
// what "mirror to mine" should mean when both exist.
func resolveGroup(ctx context.Context, db DB, email, ref string) (Group, error) {
	var g Group
	var row pgx.Row
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		row = db.QueryRow(ctx, `
			SELECT id, name, owner_email, visibility, destination, disabled, created_at, updated_at
			  FROM slack_groups
			 WHERE id = $1 AND (owner_email = $2 OR visibility = 'org')`, id, email)
	} else {
		row = db.QueryRow(ctx, `
			SELECT id, name, owner_email, visibility, destination, disabled, created_at, updated_at
			  FROM slack_groups
			 WHERE name = $1 AND (owner_email = $2 OR visibility = 'org')
			 ORDER BY owner_email <> $2
			 LIMIT 1`, strings.TrimSpace(ref), email)
	}
	err := row.Scan(&g.ID, &g.Name, &g.OwnerEmail, &g.Visibility, &g.Destination,
		&g.Disabled, &g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, ErrGroupNotFound
	}
	if err != nil {
		return g, fmt.Errorf("slack: resolve group %q: %w", ref, err)
	}
	return g, nil
}

// deleteGroup removes one group the caller owns. The schema does the rest:
// session_mirrors rows cascade away and default_group references null out.
func deleteGroup(ctx context.Context, db DB, email string, id int64) error {
	tag, err := db.Exec(ctx,
		`DELETE FROM slack_groups WHERE id = $1 AND owner_email = $2`, id, email)
	if err != nil {
		return fmt.Errorf("slack: delete group %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrGroupNotFound
	}
	return nil
}

// attachSession binds a session to a group. Idempotent: re-attaching an
// active pair is a no-op, re-attaching a detached one revives it.
func attachSession(ctx context.Context, db DB, sessionID string, groupID int64, by string, now time.Time) error {
	_, err := db.Exec(ctx, `
		INSERT INTO session_mirrors (session_id, group_id, attached_by, attached_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (session_id, group_id) DO UPDATE
		   SET detached_at = NULL, attached_by = $3, attached_at = $4`,
		sessionID, groupID, by, now)
	if err != nil {
		return fmt.Errorf("slack: attach %s to group %d: %w", sessionID, groupID, err)
	}
	return nil
}

// detachSession stops future posts for one pair. The thread stays as it is:
// detaching is "stop narrating", not "unsay".
func detachSession(ctx context.Context, db DB, sessionID string, groupID int64, now time.Time) error {
	_, err := db.Exec(ctx, `
		UPDATE session_mirrors SET detached_at = $3
		 WHERE session_id = $1 AND group_id = $2 AND detached_at IS NULL`,
		sessionID, groupID, now)
	if err != nil {
		return fmt.Errorf("slack: detach %s from group %d: %w", sessionID, groupID, err)
	}
	return nil
}

// groupActivation is one (session, group) pair the live pass advances.
type groupActivation struct {
	liveCandidate
	GroupID     int64
	GroupName   string
	Destination string
	// AttachedAt is the consent moment, and therefore the thread's history
	// horizon: nothing said before it is replayed.
	AttachedAt time.Time
}

// groupEligible reads the active pairs: session fresh and not closed for THIS
// group, group not disabled, owner's live mirroring not killed. The prefs
// join is LEFT — attach is the consent, and a person with no prefs row at all
// may still mirror; the master kill is the one per-person veto.
//
// Freshness is judged by activity, not birth: the fleet turned out to carry
// sessions that run for days across compactions, and one of those, attached
// at noon on day ten, is exactly the session somebody wants narrated. The
// same discovery removed the root-only gate here — a compaction continuation
// carries a parent id, and an explicit attachment names one session; every
// row in session_mirrors IS somebody's deliberate act, so the gate that
// protected the automatic promotion path was silently eating consented
// attachments. Promotion keeps its own root-only rule where it inserts.
func groupEligible(ctx context.Context, db DB, freshAfter time.Time, limit int) ([]groupActivation, error) {
	rows, err := db.Query(ctx, `
		SELECT s.session_id, s.email, coalesce(s.repo, ''), coalesce(s.git_branch, ''),
		       s.started_at, s.updated_at, s.ended,
		       s.user_turns, s.tool_calls, s.errors, s.cost_usd::float8,
		       coalesce(p.slack_user_id, ''),
		       g.id, g.name, g.destination, sm.attached_at,
		       coalesce(root.message_ts, ''), coalesce(root.channel, '')
		  FROM session_mirrors sm
		  JOIN sessions s      ON s.session_id = sm.session_id
		  JOIN slack_groups g  ON g.id = sm.group_id AND NOT g.disabled
		  LEFT JOIN slack_prefs p ON p.email = s.email
		  LEFT JOIN slack_posts root   ON root.key   = 'thread:' || s.session_id || ':g' || g.id
		  LEFT JOIN slack_posts closed ON closed.key = 'thread:' || s.session_id || ':g' || g.id || ':close'
		 WHERE sm.detached_at IS NULL
		   AND coalesce(p.live_disabled, FALSE) = FALSE
		   AND s.updated_at >= $1
		   AND closed.posted_at IS NULL
		 ORDER BY s.updated_at DESC
		 LIMIT $2`, freshAfter, limit)
	if err != nil {
		return nil, fmt.Errorf("slack: select group activations: %w", err)
	}
	defer rows.Close()

	var out []groupActivation
	for rows.Next() {
		var a groupActivation
		if err := rows.Scan(
			&a.SessionID, &a.Email, &a.Repo, &a.Branch,
			&a.StartedAt, &a.UpdatedAt, &a.Ended,
			&a.UserTurns, &a.ToolCalls, &a.Errors, &a.CostUSD,
			&a.SlackUserID,
			&a.GroupID, &a.GroupName, &a.Destination, &a.AttachedAt,
			&a.RootTS, &a.RootChannel,
		); err != nil {
			return nil, fmt.Errorf("slack: read a group activation: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// defaultGroupFor resolves a person's default group id, zero when unset.
func defaultGroupFor(ctx context.Context, db DB, email string) (int64, error) {
	var id *int64
	err := db.QueryRow(ctx,
		`SELECT default_group FROM slack_prefs WHERE email = $1`, email).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) || id == nil {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("slack: read default group for %s: %w", email, err)
	}
	return *id, nil
}
