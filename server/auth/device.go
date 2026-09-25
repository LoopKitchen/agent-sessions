package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// TokenPrefix marks a device credential as ours.
//
// It buys two things for four characters. Secret scanners, including the one
// GitHub runs over every push, match on a distinctive prefix, so a token pasted
// into a repository or a support ticket can be recognised and revoked instead of
// sitting there looking like base64. And a malformed credential can be rejected
// before it reaches the database, which keeps a stream of guesses from becoming
// a stream of index lookups.
const TokenPrefix = "lsd_"

// tokenBytes is the entropy behind the prefix. 256 bits is far past guessable
// and still short enough to fit on one line of a terminal, which matters
// because a person copies this once during enrollment.
const tokenBytes = 32

// Device credential failures. Verify returns these separately so the server can
// record which condition fired, but every one of them is a 401 to the caller
// with no detail: the agent cannot act differently on any of them, and an
// attacker learning that a token was real but revoked has learned something.
var (
	ErrDeviceTokenUnknown   = errors.New("auth: device token is not recognised")
	ErrDeviceTokenMalformed = errors.New("auth: device token is not in the expected format")
	ErrDeviceTokenRevoked   = errors.New("auth: device token has been revoked")
	ErrDeviceTokenExpired   = errors.New("auth: device token has expired")
	ErrDeviceRevoked        = errors.New("auth: device has been revoked")
	ErrPrincipalDisabled    = errors.New("auth: principal is disabled")
)

// TokenRecord is a row to be written to device_tokens.
type TokenRecord struct {
	ID       string
	DeviceID string
	// Email is not a column on device_tokens; it is carried so an
	// implementation can validate the device belongs to the principal it was
	// issued for before writing, and is otherwise ignored.
	Email     string
	TokenHash []byte
	IssuedAt  time.Time
	// ExpiresAt zero means the credential does not expire on its own. That is
	// the normal case: a laptop that has been offline for two months must still
	// be able to deliver what it captured, and an expiry that silently strands
	// data is worse than one revocation an admin performs deliberately.
	ExpiresAt time.Time
}

// TokenRow is the joined view Verify needs: the credential, the device it
// belongs to and the principal who owns it.
//
// The join is the store's job rather than three round trips here because the
// three facts have to be consistent with each other. Reading them separately
// leaves a window in which a token verifies against a device that was revoked
// between the two queries, and that window is exactly the moment somebody is
// being offboarded.
type TokenRow struct {
	TokenID  string
	DeviceID string
	Email    string
	Role     Role
	// TokenHash is the hash as stored, re-checked here in constant time.
	TokenHash []byte

	ExpiresAt  time.Time
	RevokedAt  time.Time
	LastUsedAt time.Time

	DeviceRevokedAt     time.Time
	PrincipalDisabledAt time.Time
}

// DeviceStore is the persistence this package needs, declared here rather than
// imported from the storage package so that auth can be built and tested on its
// own against a fake.
//
// Implementations must return an error wrapping ErrDeviceTokenUnknown when no row
// matches, and must never return a partially populated TokenRow with a nil
// error: an empty Email would authenticate a request as nobody.
type DeviceStore interface {
	InsertDeviceToken(ctx context.Context, rec TokenRecord) error
	// DeviceTokenByHash looks a credential up by the sha256 of the presented
	// token, joined to its device and principal.
	//
	// The schema's uniqueness on token_hash is partial — it excludes revoked
	// rows — so more than one row can carry the same hash once revocations
	// accumulate. An implementation must therefore order deterministically and
	// prefer the live row, or a re-issued credential will intermittently
	// resolve to its own revoked predecessor.
	DeviceTokenByHash(ctx context.Context, hash []byte) (TokenRow, error)
	RevokeDeviceToken(ctx context.Context, tokenID string, at time.Time) error
	// TouchDeviceToken records last_used_at. It is called on a throttle, so it
	// may be a plain UPDATE without any contention concern.
	TouchDeviceToken(ctx context.Context, tokenID string, at time.Time) error
}

// DeviceOptions configure Devices.
type DeviceOptions struct {
	Store DeviceStore
	// Lifetime, when set, expires issued credentials. Zero means they live
	// until revoked. See TokenRecord.ExpiresAt.
	Lifetime time.Duration
	// TouchInterval is how stale last_used_at may get before Verify writes it
	// again. Zero takes defaultTouchInterval.
	TouchInterval time.Duration
	// Now is injectable for tests.
	Now func() time.Time
}

// An agent uploads every few seconds. Writing last_used_at on every request
// would turn a read-only authentication check into a write on the hottest row
// in the table, for a column whose only consumer is a fleet view that reports
// in minutes.
const defaultTouchInterval = 5 * time.Minute

// Devices issues, verifies and revokes the credentials laptops authenticate
// with.
type Devices struct {
	store DeviceStore
	life  time.Duration
	touch time.Duration
	now   func() time.Time
}

// NewDevices builds a Devices.
func NewDevices(o DeviceOptions) (*Devices, error) {
	if o.Store == nil {
		return nil, errors.New("auth: DeviceStore is required")
	}
	d := &Devices{store: o.Store, life: o.Lifetime, touch: o.TouchInterval, now: o.Now}
	if d.touch <= 0 {
		d.touch = defaultTouchInterval
	}
	if d.now == nil {
		d.now = time.Now
	}
	return d, nil
}

// Issued is the result of minting a credential. Token is the only time the
// secret exists outside the laptop that will hold it; the server keeps a hash
// and cannot reproduce it, so a lost token is re-enrolled rather than recovered.
type Issued struct {
	TokenID   string
	DeviceID  string
	Email     string
	Token     string
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// Issue mints a credential for a device that has just been enrolled.
//
// It does not decide whether this person may enroll. By the time enrollment
// reaches here the handler has verified a Google ID token and confirmed the
// principal exists and is not disabled; putting that check in two places would
// mean two answers to "who is allowed in", and the one nobody remembers to
// update is the one that stays permissive.
//
// Only the sha256 of the token reaches the database. A stolen dump of
// device_tokens therefore contains nothing that can be replayed, which is the
// same reason password hashes exist; unlike a password this value is full-length
// random, so a plain sha256 is enough and a slow KDF would only add latency to
// every upload.
func (d *Devices) Issue(ctx context.Context, email, deviceID string) (Issued, error) {
	email = Normalize(email)
	if email == "" {
		return Issued{}, errors.New("auth: email is required to issue a device token")
	}
	if strings.TrimSpace(deviceID) == "" {
		return Issued{}, errors.New("auth: device id is required to issue a device token")
	}

	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		// Never invent a credential from a weaker source. A degraded random
		// source is a reason to refuse enrollment, not to continue with a token
		// somebody else can predict.
		return Issued{}, fmt.Errorf("auth: no entropy available: %w", err)
	}
	token := TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)

	id, err := NewUUID()
	if err != nil {
		return Issued{}, err
	}

	now := d.now()
	rec := TokenRecord{
		ID:        id,
		DeviceID:  deviceID,
		Email:     email,
		TokenHash: HashToken(token),
		IssuedAt:  now,
	}
	if d.life > 0 {
		rec.ExpiresAt = now.Add(d.life)
	}
	if err := d.store.InsertDeviceToken(ctx, rec); err != nil {
		return Issued{}, err
	}
	return Issued{
		TokenID:   id,
		DeviceID:  deviceID,
		Email:     email,
		Token:     token,
		IssuedAt:  now,
		ExpiresAt: rec.ExpiresAt,
	}, nil
}

// DeviceIdentity is who a verified device credential says is calling.
type DeviceIdentity struct {
	Email    string
	Role     Role
	DeviceID string
	TokenID  string
}

// Viewer converts a device identity into the value the permission rule takes,
// so an upload and a dashboard read are authorised by the same code.
func (i DeviceIdentity) Viewer() Viewer { return Viewer{Email: i.Email, Role: i.Role} }

// Verify authenticates a presented device credential.
//
// The revocation checks are the reason this is a join rather than a token
// lookup. Offboarding somebody means disabling one principals row, and that has
// to stop every machine they ever enrolled in the same instant, including the
// laptop still sitting in a drawer with a valid token on it. Checking only the
// token would leave those machines uploading until somebody remembered to walk
// the device list.
func (d *Devices) Verify(ctx context.Context, presented string) (DeviceIdentity, error) {
	presented = strings.TrimSpace(presented)
	if presented == "" || !strings.HasPrefix(presented, TokenPrefix) {
		return DeviceIdentity{}, ErrDeviceTokenMalformed
	}

	hash := HashToken(presented)
	row, err := d.store.DeviceTokenByHash(ctx, hash)
	if err != nil {
		return DeviceIdentity{}, err
	}

	// The lookup already matched on the hash, so this comparison is not what
	// finds the row. It is here so the decision to trust the row is made in this
	// process, in time that does not vary with how many leading bytes agreed. A
	// store that ever answers with a near miss — a prefix scan, a cache keyed on
	// a truncation, a test double someone writes later — is caught here rather
	// than trusted, and the check costs nothing on a 32-byte value.
	if subtle.ConstantTimeCompare(row.TokenHash, hash) != 1 {
		return DeviceIdentity{}, ErrDeviceTokenUnknown
	}

	now := d.now()
	switch {
	case !row.RevokedAt.IsZero() && !now.Before(row.RevokedAt):
		return DeviceIdentity{}, ErrDeviceTokenRevoked
	case !row.ExpiresAt.IsZero() && !now.Before(row.ExpiresAt):
		return DeviceIdentity{}, ErrDeviceTokenExpired
	case !row.DeviceRevokedAt.IsZero() && !now.Before(row.DeviceRevokedAt):
		return DeviceIdentity{}, ErrDeviceRevoked
	case !row.PrincipalDisabledAt.IsZero() && !now.Before(row.PrincipalDisabledAt):
		return DeviceIdentity{}, ErrPrincipalDisabled
	}

	email := Normalize(row.Email)
	if email == "" {
		// A row with no owner cannot authorise anything: every downstream
		// permission check keys on this address.
		return DeviceIdentity{}, ErrDeviceTokenUnknown
	}
	role := row.Role
	if !role.Valid() {
		// An unreadable role degrades to the least privileged one rather than
		// failing the upload. The agent's own writes do not need a role, and
		// refusing them would lose data over a column this path barely uses.
		role = RoleMember
	}

	if row.LastUsedAt.IsZero() || now.Sub(row.LastUsedAt) >= d.touch {
		// Best effort by design. last_used_at feeds the fleet view; failing an
		// upload because a telemetry column could not be updated would trade
		// real data for a timestamp.
		_ = d.store.TouchDeviceToken(ctx, row.TokenID, now)
	}

	return DeviceIdentity{
		Email:    email,
		Role:     role,
		DeviceID: row.DeviceID,
		TokenID:  row.TokenID,
	}, nil
}

// Revoke marks one credential dead by its id, which is what the device list in
// the dashboard has to hand.
func (d *Devices) Revoke(ctx context.Context, tokenID string) error {
	if strings.TrimSpace(tokenID) == "" {
		return errors.New("auth: token id is required to revoke")
	}
	return d.store.RevokeDeviceToken(ctx, tokenID, d.now())
}

// RevokeByToken revokes the credential a caller holds the secret for. This is
// the path for "this token leaked, kill it", where the person reporting it has
// the token and not its id.
func (d *Devices) RevokeByToken(ctx context.Context, presented string) error {
	presented = strings.TrimSpace(presented)
	if presented == "" {
		return ErrDeviceTokenMalformed
	}
	row, err := d.store.DeviceTokenByHash(ctx, HashToken(presented))
	if err != nil {
		return err
	}
	return d.store.RevokeDeviceToken(ctx, row.TokenID, d.now())
}

// HashToken is the one place a device token becomes a stored value. Both Issue
// and Verify go through it so the two can never disagree about what is hashed,
// which is the bug that turns every credential in the table into a dead one.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// BearerToken extracts the credential from an Authorization header.
//
// The scheme is compared case-insensitively because RFC 7235 says it is
// case-insensitive, and a client that sends "bearer" is not wrong.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const scheme = "bearer "
	if len(h) < len(scheme) || !strings.EqualFold(h[:len(scheme)], scheme) {
		return ""
	}
	return strings.TrimSpace(h[len(scheme):])
}

// NewUUID returns a random RFC 4122 version 4 UUID.
//
// The schema uses UUID primary keys for devices and device_tokens, and this
// module has no dependencies; sixteen random bytes with the version and variant
// bits set is the whole of what a v4 UUID is, so a dependency for it would buy
// nothing.
func NewUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: no entropy available: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
