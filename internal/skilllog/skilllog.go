// Package skilllog holds the skill-usage log messages, and nothing else.
//
// Two packages emit the refusal line: server/skillusage, which owns
// POST /v1/skill-invocations, and server/api, whose catalog PUT answers a
// refused credential the same way (design 7.1; ruled 2026-09-22 from LS-3
// review-2). The filter in examples/deploy-gcp/monitoring/metrics reads the one
// literal, so a second declaration would let a rename in one package leave
// the other's refusals under no metric with nothing failing to say so.
//
// It lives here rather than in either emitter because server/api reaches
// the store only through the interfaces in server/api/ports.go, and
// importing server/skillusage for a string would pull the store, the derive
// package and six internal packages behind that boundary (LS-3 review-3
// finding 3). This package imports nothing of ours, so both sides can
// depend on it without depending on each other. Keep it that way: strings
// only, no types, no imports.
package skilllog

const (
	// Accepted is the line a stored invocation writes.
	Accepted = "skill invocation accepted"
	// Rejected is the line every 4xx on POST /v1/skill-invocations writes,
	// and every 401 or 403 on PUT /v1/skill-catalog/{source_repo}.
	Rejected = "skill invocation rejected"
	// StoreFailed is the line a 503 from the store writes.
	StoreFailed = "skill invocation store failed"
	// ReconcilerRun is the line POST /v1/skill-invocations/reconciler-runs
	// writes for every run, recorded or soft-revoked.
	ReconcilerRun = "skill reconciler run"
	// CatalogPublished is the line PUT /v1/skill-catalog/{source_repo}
	// writes for every accepted publish, including a soft-revoked one.
	CatalogPublished = "skill catalog published"
)
