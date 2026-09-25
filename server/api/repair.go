package api

// GET /v1/repair: the sessions a laptop should re-walk from the transcripts
// still on it.
//
// Two things the corpus is missing can only come from the laptop: the final
// answers of turns the old Stop handler captured without them, and the token
// counts of Codex sessions stored before the walker read them. The server
// knows which sessions those are (the derive layer's turns say so) and the
// laptop knows where the files are, so the server names the sessions and the
// daemon walks exactly those, once a day. The route is device-authenticated:
// the list is per machine, and a person's cookie is not a machine.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DeviceIdentity is the machine behind a device bearer token.
type DeviceIdentity struct {
	DeviceID string
	Email    string
}

// DeviceAuthenticator verifies the credential an enrolled laptop presents.
// Implementations return ErrNoIdentity for a missing, malformed, revoked or
// unknown credential and a real error for an infrastructure failure, for the
// reason Authenticator does: the first is the caller's, the second is ours.
type DeviceAuthenticator interface {
	VerifyDevice(ctx context.Context, token string) (DeviceIdentity, error)
}

// RepairEntry is one session the calling device should re-walk. The shape is
// the client's (cmd/loop-sessions/repair.go): it skips an entry whose
// transcript_path is empty, which is why the hint says so when the server
// could not place the file.
type RepairEntry struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	// Reason is missing_answers or codex_tokens.
	Reason string `json:"reason"`
	Source string `json:"source,omitempty"`
	// Hint is for an operator reading the list: what the walk recovers, or
	// why the path could not be built.
	Hint string `json:"hint,omitempty"`
}

// RepairStore lists what a device should re-walk, newest first, at most
// limit entries, as of now.
type RepairStore interface {
	RepairList(ctx context.Context, deviceID string, now time.Time, limit int) ([]RepairEntry, error)
}

const (
	// repairLimit bounds one answer. The client walks the list back to back
	// with pacing; two hundred sessions is a long night on a laptop already.
	repairLimit = 200
	// repairEvery is how often one device may ask. The list changes when the
	// derive runner runs, which is nightly, and the client asks daily; the
	// hour is the guard against a daemon in a restart loop.
	repairEvery = time.Hour
)

// repairLimiter is the per-device rate limit, per instance. It is in memory
// rather than in the database because the question it answers is "did this
// instance just answer this device", and a second instance answering the
// same device once in the same hour costs one bounded query.
type repairLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// reserve claims the device's slot for the hour, or reports how long until
// it may ask again. The slot is taken before the work rather than after the
// answer: twenty requests that arrive together would otherwise all pass the
// check before the first answer landed, and all be served and all run the
// store query. The second of them now finds the reservation and is limited.
// A reservation whose request does not end in an answer is given back by
// refund, so a failure on our side (a store the client could not have
// caused) does not cost the laptop its daily walk.
func (l *repairLimiter) reserve(deviceID string, now time.Time) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if at, ok := l.last[deviceID]; ok && now.Sub(at) < repairEvery {
		return false, repairEvery - now.Sub(at)
	}
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	// Entries older than the window are dropped so the map is bounded by the
	// fleet that asked in the last hour, not by every device that ever did.
	for id, at := range l.last {
		if now.Sub(at) >= repairEvery {
			delete(l.last, id)
		}
	}
	l.last[deviceID] = now
	return true, 0
}

// refund gives a reservation back: the request it was made for did not end
// in an answer (a 200 or a 304), so the device's hour has not been spent.
// Only the reservation stamped at now is dropped, never a later answer's.
func (l *repairLimiter) refund(deviceID string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if at, ok := l.last[deviceID]; ok && at.Equal(now) {
		delete(l.last, deviceID)
	}
}

type repairBody struct {
	Sessions []RepairEntry `json:"sessions"`
}

// handleRepair answers the device's list with an ETag, so a client that
// already holds the list can be told nothing changed without the body.
func (h *Handler) handleRepair(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "unauthenticated", "a device bearer token is required")
		return
	}
	id, err := h.devices.VerifyDevice(r.Context(), token)
	if err != nil {
		if errors.Is(err, ErrNoIdentity) {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "the device credential was not accepted")
			return
		}
		h.fail(r, "verify device", err)
		writeInternal(w)
		return
	}
	now := h.now()
	ok, wait := h.repairs.reserve(id.DeviceID, now)
	if !ok {
		setResponseHeaders(w)
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "rate_limited", "the repair list is served once an hour per device")
		return
	}
	// The hour is spent by an answer (a 200 or a 304) and by nothing else:
	// every other way out of this handler gives the reservation back.
	answered := false
	defer func() {
		if !answered {
			h.repairs.refund(id.DeviceID, now)
		}
	}()
	list, err := h.repair.RepairList(r.Context(), id.DeviceID, now, repairLimit)
	if err != nil {
		h.fail(r, "repair list", err)
		writeInternal(w)
		return
	}
	if list == nil {
		list = []RepairEntry{}
	}
	body, err := json.Marshal(repairBody{Sessions: list})
	if err != nil {
		h.fail(r, "repair list", err)
		writeInternal(w)
		return
	}
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:16]) + `"`
	answered = true
	setResponseHeaders(w)
	w.Header().Set("ETag", etag)
	if strings.TrimSpace(r.Header.Get("If-None-Match")) == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// bearerToken reads the Authorization header's bearer credential.
func bearerToken(r *http.Request) string {
	scheme, token, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}
