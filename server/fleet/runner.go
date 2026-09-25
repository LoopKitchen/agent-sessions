package fleet

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// ErrManifestMissing is what a ManifestSource returns when nothing has been
// published yet: an ordinary state before the first release, reported once
// as a WARNING and never as version lag.
var ErrManifestMissing = errors.New("fleet: release manifest missing")

// ManifestSource reads the published release manifest.
type ManifestSource interface {
	Latest(ctx context.Context) (Manifest, error)
}

// Hour is one machine's hour in the health ledger, as the runner reads it
// before and after its rollup to find what was dropped since the last tick.
type Hour struct {
	Email    string
	DeviceID string
	Hour     time.Time
	Dropped  map[string]int64
}

// Store is what the runner reads and writes. Every method is one bounded
// query; the composition root adapts the real store to it.
type Store interface {
	// Lock takes the evaluator's single-flight lock and returns the release,
	// or held false when another instance has it.
	Lock(ctx context.Context) (release func(), held bool, err error)
	// TickDue records a tick and reports whether it should run: false when
	// the newest tick on record, from any instance, is younger than minAge,
	// in which case the interval is that tick's. Called under Lock. The
	// moment stamped and the age measured are both the store's, never this
	// instance's: one wrong clock would otherwise set the cadence for every
	// other instance, so the runner has no clock to pass here.
	TickDue(ctx context.Context, minAge time.Duration) (bool, error)
	People(ctx context.Context) ([]Person, error)
	Devices(ctx context.Context) ([]Device, error)
	Reports(ctx context.Context) ([]Report, error)
	// RecentBuilds reads every build each machine reported in a report that
	// arrived at or after since, with the last moment each was seen.
	RecentBuilds(ctx context.Context, since time.Time) ([]BuildSighting, error)
	// Hours reads the ledger rows at or after since; Rollup recomputes the
	// ledger for [from, to) from the raw reports, or reports deferred when
	// the sweep holds the ledger's lock, in which case the deltas wait for
	// the next tick.
	Hours(ctx context.Context, since time.Time) ([]Hour, error)
	Rollup(ctx context.Context, from, to time.Time) (deferred bool, err error)
	Mutes(ctx context.Context) ([]Mute, error)
	EmptyStarts(ctx context.Context, since time.Time) ([]EmptyStart, error)
	MissingAnswers(ctx context.Context, since time.Time) ([]AnswerCohort, error)
	// SweepHealth rolls the raw history up and deletes what is past its
	// window; the runner calls it once an hour so a deployment with
	// retention switched off still bounds the raw table.
	SweepHealth(ctx context.Context, now time.Time) (slog.Value, error)
}

const (
	// Interval is the evaluator's cadence, the client's report period: a
	// report lands every five minutes per machine, and evaluating more often
	// than that reads the same rows twice.
	Interval = 5 * time.Minute
	// SweepEvery is how often the tick also sweeps the raw health table.
	SweepEvery = time.Hour
	// rollupWindow is how far back each tick recomputes the ledger: the
	// current hour and the one before, so a report arriving just after the
	// hour still lands in the hour it belongs to.
	rollupWindow = 2 * time.Hour
	// TickSlack is how much younger than the interval another instance's
	// tick may be and still count as this interval's. The advisory lock only
	// stops ticks that overlap; Cloud Run runs several instances whose
	// timers fire at their own moments, so without a record of the last
	// tick each instance logged its own set of fleet lines per interval and
	// the line-count metrics read as many fleets as instances. Thirty
	// seconds covers the drift between the timers, which is all there is to
	// cover now the gate itself is measured by the store's clock.
	TickSlack = 30 * time.Second
	// ManifestCacheFor is how long a page load may read the last manifest
	// fetched instead of the bucket: one tick interval, so the page is never
	// staler than the alerts and the bucket is read a dozen times an hour
	// however many people have the fleet page open.
	ManifestCacheFor = 5 * time.Minute
)

// manifestCopy is the last manifest read, its error, and when.
type manifestCopy struct {
	m   Manifest
	err error
	at  time.Time
}

// Runner evaluates the fleet on a cadence and logs what it finds. One
// goroutine per instance; the lock makes one instance's tick the fleet's.
type Runner struct {
	Store       Store
	Manifest    ManifestSource
	ServerBuild string
	Log         *slog.Logger
	Now         func() time.Time
	// Interval and SweepEvery default to the constants; fields so a test can
	// drive the loop in milliseconds.
	Interval   time.Duration
	SweepEvery time.Duration
	// AfterTick runs once per tick this instance performed, under the
	// same lock and after the fleet lines, for the summaries that ride the
	// fleet cadence but are not the fleet's (the skill platform summary,
	// server/store/skills.go). Under the lock, so several instances still
	// log one set of lines per interval. Nil is nothing extra.
	AfterTick func(ctx context.Context, now time.Time)

	lastSweep      time.Time
	warnedManifest bool

	// mu guards manifest, which page loads and the tick share.
	mu       sync.Mutex
	manifest manifestCopy
}

// interval is the tick cadence in force.
func (r *Runner) interval() time.Duration {
	if r.Interval > 0 {
		return r.Interval
	}
	return Interval
}

// latestManifest returns the release manifest: read from the bucket when
// refresh is set (a tick always judges against the live manifest) or when
// the last copy is older than ManifestCacheFor, and the copy otherwise. A
// missing manifest is cached like any other answer; it is the ordinary
// state before the first publish, not a failure to retry every page load.
func (r *Runner) latestManifest(ctx context.Context, now time.Time, refresh bool) (Manifest, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.manifest
	if !refresh && !c.at.IsZero() && !now.Before(c.at) && now.Sub(c.at) < ManifestCacheFor {
		return c.m, c.err
	}
	m, err := r.Manifest.Latest(ctx)
	r.manifest = manifestCopy{m: m, err: err, at: now}
	return m, err
}

// Tick runs one evaluation: takes the lock, checks that no other instance
// ticked this interval, rolls the recent ledger up, reads every input,
// evaluates, logs, releases the lock, and once an hour sweeps. It reports
// the evaluation, whether this instance did the work (false when another
// held the lock or ticked this interval), and the first error.
func (r *Runner) Tick(ctx context.Context) (Evaluation, bool, error) {
	now := r.now()
	release, held, err := r.Store.Lock(ctx)
	if err != nil {
		return Evaluation{}, false, err
	}
	if !held {
		return Evaluation{}, false, nil
	}
	// The lock covers the rollup, the reads and the lines: one instance's
	// tick is the fleet's. It is let go before the sweep, which is minutes
	// of batches under the retention lock of its own, so another instance's
	// tick is never a no-op for the length of a sweep.
	released := false
	unlock := func() {
		if !released {
			released = true
			release()
		}
	}
	defer unlock()

	// One set of lines per interval across instances: the tick on record
	// is the fleet's, and one younger than the interval (less the slack) is
	// another instance's, whose interval this is. The age is the store's to
	// judge; now below dates this evaluation, not the gate.
	due, err := r.Store.TickDue(ctx, r.interval()-TickSlack)
	if err != nil {
		return Evaluation{}, false, err
	}
	if !due {
		return Evaluation{}, false, nil
	}

	// The drop deltas: the ledger before and after this tick's rollup of the
	// recent hours. The ledger is the persisted state, so two instances or
	// a restart never count a drop twice and never miss one. A rollup the
	// sweep's lock defers leaves the ledger as it was, and the deltas are
	// the next tick's.
	since := now.Add(-rollupWindow).Truncate(time.Hour)
	before, err := r.Store.Hours(ctx, since)
	if err != nil {
		return Evaluation{}, true, err
	}
	deferred, err := r.Store.Rollup(ctx, since, now.Add(time.Hour))
	if err != nil {
		return Evaluation{}, true, err
	}
	if deferred {
		r.log().InfoContext(ctx, "health rollup deferred", "from", since, "to", now.Add(time.Hour))
	}
	after, err := r.Store.Hours(ctx, since)
	if err != nil {
		return Evaluation{}, true, err
	}

	in := Inputs{Now: now, ServerBuild: r.ServerBuild, Drops: dropDeltas(before, after)}
	if r.Manifest != nil {
		m, err := r.latestManifest(ctx, now, true)
		switch {
		case err == nil:
			in.Manifest = &m
			r.warnedManifest = false
		case errors.Is(err, ErrManifestMissing):
			// Said once, not every five minutes: the state is ordinary until
			// the first publish and the summary line carries manifest_present.
			if !r.warnedManifest {
				r.log().WarnContext(ctx, "release manifest missing", "channel", "latest")
				r.warnedManifest = true
			}
		default:
			r.log().WarnContext(ctx, "release manifest unreadable", "err", err)
		}
	}
	if err := r.readInputs(ctx, &in); err != nil {
		return Evaluation{}, true, err
	}

	ev := Evaluate(in)
	// The manifest warning is the runner's (once), not the evaluation's
	// (every tick), so it does not repeat here.
	Emit(ctx, r.log(), ev)
	if r.AfterTick != nil {
		r.AfterTick(ctx, now)
	}
	unlock()

	every := r.SweepEvery
	if every <= 0 {
		every = SweepEvery
	}
	if r.lastSweep.IsZero() || now.Sub(r.lastSweep) >= every {
		sweep, err := r.Store.SweepHealth(ctx, now)
		if err != nil {
			r.log().ErrorContext(ctx, "health sweep failed", "err", err)
		} else {
			r.log().InfoContext(ctx, "health sweep", "sweep", sweep)
		}
		r.lastSweep = now
	}
	return ev, true, nil
}

// readInputs fills the inputs every evaluation reads from the store; the
// tick and the page share it so the two never disagree on what a machine is.
func (r *Runner) readInputs(ctx context.Context, in *Inputs) error {
	var err error
	if in.People, err = r.Store.People(ctx); err != nil {
		return err
	}
	if in.Devices, err = r.Store.Devices(ctx); err != nil {
		return err
	}
	if in.Reports, err = r.Store.Reports(ctx); err != nil {
		return err
	}
	if in.Builds, err = r.Store.RecentBuilds(ctx, in.Now.Add(-BuildWindow)); err != nil {
		return err
	}
	if in.Mutes, err = r.Store.Mutes(ctx); err != nil {
		return err
	}
	if in.EmptyStarts, err = r.Store.EmptyStarts(ctx, in.Now.Add(-24*time.Hour)); err != nil {
		return err
	}
	if in.Answers, err = r.Store.MissingAnswers(ctx, in.Now.Add(-48*time.Hour).Truncate(24*time.Hour)); err != nil {
		return err
	}
	return nil
}

// View evaluates the fleet for the admin page: the same inputs and the same
// judgement as a tick, without the lock, the rollup, the log lines or the
// sweep, so reading the page changes nothing and says what the alerts say.
// Drops are the last day's ledger totals rather than a delta, because a
// page has no previous tick to measure from; the manifest is the last copy
// fetched within ManifestCacheFor, and its being missing is left to the
// summary's manifest_present, never warned about here.
func (r *Runner) View(ctx context.Context) (Evaluation, error) {
	now := r.now()
	in := Inputs{Now: now, ServerBuild: r.ServerBuild}
	hours, err := r.Store.Hours(ctx, now.Add(-24*time.Hour).Truncate(time.Hour))
	if err != nil {
		return Evaluation{}, err
	}
	in.Drops = dropDeltas(nil, hours)
	if r.Manifest != nil {
		if m, err := r.latestManifest(ctx, now, false); err == nil {
			in.Manifest = &m
		} else if !errors.Is(err, ErrManifestMissing) {
			return Evaluation{}, err
		}
	}
	if err := r.readInputs(ctx, &in); err != nil {
		return Evaluation{}, err
	}
	return Evaluate(in), nil
}

// Run ticks on the interval until ctx ends. The first tick waits one
// interval so a rolling deploy's instances do not all evaluate in the same
// second they boot.
func (r *Runner) Run(ctx context.Context) {
	every := r.interval()
	t := time.NewTimer(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, _, err := r.Tick(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			r.log().ErrorContext(ctx, "fleet evaluation failed", "err", err)
		}
		t.Reset(every)
	}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

// dropDeltas is the rise per (machine, reason) between two reads of the
// ledger. An hour row absent before and present after counts whole, which
// is the first tick after a deploy and once.
func dropDeltas(before, after []Hour) []DropDelta {
	type key struct{ email, device, reason string }
	prev := map[key]int64{}
	for _, h := range before {
		for reason, n := range h.Dropped {
			prev[key{h.Email, h.DeviceID, reason}] += n
		}
	}
	next := map[key]int64{}
	for _, h := range after {
		for reason, n := range h.Dropped {
			next[key{h.Email, h.DeviceID, reason}] += n
		}
	}
	var out []DropDelta
	for k, n := range next {
		if d := n - prev[k]; d > 0 {
			out = append(out, DropDelta{Email: k.email, DeviceID: k.device, Reason: k.reason, Delta: d})
		}
	}
	return out
}

// Emit writes the evaluation as the structured lines the metrics and alert
// policies read (the contract section 6). Field names are the contract:
// a renamed field is a metric that silently stops.
func Emit(ctx context.Context, log *slog.Logger, ev Evaluation) {
	s := ev.Summary
	log.InfoContext(ctx, "fleet summary",
		"enrolled", s.Enrolled, "reporting", s.Reporting, "silent", s.Silent, "never_reported", s.NeverReported,
		"capture_blocked", s.CaptureBlocked, "quarantine_devices", s.QuarantineDevices, "parked_devices", s.ParkedDevices,
		"current", s.Current, "behind", s.Behind, "unmanaged", s.Unmanaged, "version_unknown", s.VersionUnknown,
		"drops", s.Drops, "empties", s.Empties, "sessions", s.Sessions, "empty_rate", s.EmptyRate,
		"answer_turns", s.AnswerTurns, "answered", s.Answered, "missing_answer_rate", s.MissingAnswerRate,
		"published_build", s.PublishedBuild, "server_build", s.ServerBuild, "manifest_present", s.ManifestPresent,
		"ctas", len(ev.CTAs), "muted", len(ev.Muted))
	for _, c := range ev.Conditions {
		log.InfoContext(ctx, "fleet condition",
			"email", c.Email, "device_id", c.DeviceID, "kind", c.Kind, "level", c.Level,
			"since", c.Since, "agent_version", c.AgentVersion, "detail", c.Detail)
	}
	for _, d := range ev.Drops {
		log.WarnContext(ctx, "fleet drops", "email", d.Email, "device_id", d.DeviceID, "reason", d.Reason, "delta", d.Delta)
	}
	for _, sl := range ev.Silent {
		log.WarnContext(ctx, "fleet silent", "email", sl.Email, "device_id", sl.DeviceID, "since", sl.Since, "hours", sl.Hours, "last_worst", sl.LastWorst)
	}
	for _, l := range ev.Lag {
		log.InfoContext(ctx, "fleet version_lag",
			"email", l.Email, "device_id", l.DeviceID, "agent_version", l.AgentVersion,
			"published_build", l.PublishedBuild, "hours_behind", l.HoursBehind)
	}
	for _, e := range ev.EmptyStarts {
		log.WarnContext(ctx, "fleet empty_start",
			"email", e.Email, "device_id", e.DeviceID, "empties", e.Empties, "total", e.Total, "rate", e.Rate,
			"cwds", e.Cwds, "recipe", e.Recipe)
	}
	for _, m := range ev.MissingAnswers {
		log.InfoContext(ctx, "fleet missing_answer",
			"day", m.Day.Format("2006-01-02"), "agent_version", m.AgentVersion, "entrypoint", m.Entrypoint,
			"turns", m.Turns, "answered", m.Answered, "rate", m.Rate)
	}
}
