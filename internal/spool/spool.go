// Package spool is the client's durable outbox.
//
// Everything captured on a laptop lands here first and is deleted only after
// the server has acknowledged it. That ordering is the whole point: a hook has
// at most a few tens of milliseconds before it starts delaying someone's turn,
// and the network is not something you can do in that budget. So the hook
// writes a file and returns, and a separate drain worries about delivery.
//
// The design follows what production agents converged on (Sentry's envelope
// cache, Fluent Bit's chunk storage, the OTel Collector's file_storage
// extension, Vector's disk buffers): a directory of immutable payload files
// published by atomic rename, with no per-record fsync. Notably none of them
// use a database as the payload queue, and neither do we: a spool file is
// already the durable unit, and putting it in SQLite would add a write-amplifying
// dependency that can itself be corrupted.
//
// Delivery is at-least-once by construction. Every item carries a stable
// idempotency key so the server can absorb the duplicates that crash-recovery
// and truncation re-reads inevitably produce. Trying to make the client
// exactly-once instead would mean coordinating a distributed transaction with a
// laptop that can vanish mid-request, which is not a thing that works.
//
// Three directories, three fates. pending/ holds what will be delivered.
// quarantine/ holds what the server has explicitly refused, with the reason in
// the filename, because an item the server said "never" to is evidence and a
// person has to look at it. parked/ holds what the server has neither accepted
// nor refused eight deliveries in a row: that is a server bug or a version
// skew, not a poison payload, so it is retried automatically after an upgrade
// and once a day rather than being written off by a counter.
package spool

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Item is one unit of work: a captured event or a file chunk, plus the metadata
// the drain and the server need to handle it safely.
type Item struct {
	// ID is the idempotency key. The server upserts on it, so a duplicate
	// delivery is a no-op rather than a double-count.
	ID string `json:"id"`
	// Kind routes the item server-side (event, chunk, heartbeat).
	Kind string `json:"kind"`
	// SessionID groups items; empty for machine-level items like heartbeats.
	SessionID string `json:"session_id,omitempty"`
	// Seq orders items within a session. The server sorts on it rather than on
	// arrival, because arrival order is not preserved across retries.
	Seq int64 `json:"seq,omitempty"`
	// EventTime is when the thing happened, which is not when we shipped it.
	// Backfill depends on this separation: a session imported from history must
	// be indistinguishable from one captured live except for IngestTime.
	EventTime time.Time `json:"event_time"`
	// Attempts counts genuine delivery failures attributed to the item. It is
	// reserved for attempt exhaustion; a verdict that names the item in
	// neither list is not a failed attempt and is counted separately.
	Attempts int `json:"attempts,omitempty"`
	// Undecided counts consecutive deliveries after which the server neither
	// accepted nor rejected the item. It resets the moment the item is decided
	// either way, and at the cutoff the item is parked rather than quarantined:
	// the server has not said "never", it has said nothing.
	Undecided int `json:"undecided,omitempty"`
	// Payload is the already-scrubbed body. Nothing unscrubbed is ever written
	// to the spool, so a spool file leaking is not a credential leak.
	Payload json.RawMessage `json:"payload"`
}

// Stats describe the spool for the heartbeat. The server distinguishes a quiet
// user from a broken agent using these, so they ship even when nothing else does.
type Stats struct {
	Pending    int            `json:"pending"`
	Quarantine int            `json:"quarantine"`
	Parked     int            `json:"parked"`
	Bytes      int64          `json:"bytes"`
	OldestAge  time.Duration  `json:"oldest_age_ns"`
	Dropped    map[string]int `json:"dropped,omitempty"`
}

// State is what the spool remembers across processes.
//
// Everything else here is derived by listing a directory, which any process can
// do. These facts cannot be: they are events, not contents. On a laptop the
// spool is written by hook processes that live for milliseconds and read by a
// `status` command that lives barely longer, so a fact held only in memory is
// invisible to the person asking whether the agent works, and that person only
// ever asks when something is already wrong.
//
// Each field exists because without it a real question has no answer. Without
// LastSuccess an empty queue reads identically for "everything shipped" and
// "nothing was ever captured", which is precisely the ambiguity that cost a user
// an evening. Without LastError a stuck queue can be seen but not explained.
// Without a durable Dropped, the one condition that means capture is silently
// off (a full disk) dies with the hook process that observed it.
type State struct {
	// LastSuccess is when the server last accepted an item from this machine.
	LastSuccess time.Time `json:"last_success,omitempty"`
	// LastError is why the last delivery attempt failed, and LastErrorAt is
	// when. Compared against LastSuccess they say whether the failure is the
	// current state of the world or old news.
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitempty"`
	// Dropped counts data the spool discarded, by reason, across every process
	// that has ever written to this directory. It also carries the counters
	// that are not drops but belong beside them in a health report, such as
	// events recovered from a transcript after a hook was abandoned.
	Dropped map[string]int `json:"dropped,omitempty"`
	// PendingBytes is a cached estimate of the bytes queued in pending/, kept
	// so Add does not have to stat every queued file on every hook. It is
	// corrected whenever the directory is measured for real (Stats, and the
	// full scan enforceCap falls back to near the cap), so drift from a lost
	// concurrent update is bounded by the next report.
	PendingBytes int64 `json:"pending_bytes,omitempty"`
	// PendingMeasured says the estimate descends from a real measurement.
	// A state file written by a release that kept no estimate reads as zero
	// bytes queued, and a laptop coming back with a large backlog would then
	// be trusted past its cap until the next report; an unmeasured estimate
	// is instead scanned once, and every adjustment after that inherits the
	// flag.
	PendingMeasured bool `json:"pending_measured,omitempty"`
}

// Delivered reports whether the server has ever accepted anything from this
// machine. It is the question that separates a healthy quiet agent from one that
// has never worked, and it is asked often enough to deserve a name.
func (s State) Delivered() bool { return !s.LastSuccess.IsZero() }

// FailingSince reports the current failure, if the last thing that happened to
// delivery was a failure rather than a success. A recorded error older than the
// last success describes a problem that has since resolved, and reporting it
// would send someone chasing an outage that is over.
func (s State) FailingSince() (string, time.Time, bool) {
	if s.LastError == "" || !s.LastErrorAt.After(s.LastSuccess) {
		return "", time.Time{}, false
	}
	return s.LastError, s.LastErrorAt, true
}

// Options configure a Spool.
type Options struct {
	// Dir is the spool root. Subdirectories are created for pending,
	// quarantined and parked items.
	Dir string
	// MaxBytes caps total pending bytes. On overflow the OLDEST items are
	// dropped, matching Fluent Bit's storage.total_limit_size behavior: when a
	// laptop has been offline for a week, recent context is worth more than a
	// stale backlog, and unbounded growth would eventually fill the disk.
	MaxBytes int64
	// MinFreeRatio overrides the disk guard's ratio. Zero takes the default,
	// and so does the legacy 0.05 that every config written before the guard
	// was rewritten carries explicitly; any other value is honoured. The
	// absolute floor and the cap on the ratio's byte value always apply, so a
	// ratio can make the guard stricter or looser but never let the spool
	// write into the last half gigabyte of a disk.
	MinFreeRatio float64
	// MaxAttempts is the cutoff for genuine delivery failures attributed to an
	// item. Past it the item moves to quarantine rather than retrying forever,
	// so one poison payload cannot block the queue behind it.
	MaxAttempts int
	// MaxUndecided is how many consecutive undecided verdicts an item may
	// collect before it is parked. Default 8.
	MaxUndecided int
	// Now is injectable for tests.
	Now func() time.Time
	// DiskFree is injectable for tests.
	DiskFree func(dir string) (free, total uint64, err error)
}

// Disk guard defaults.
//
// The buffer's own size (MaxBytes) is the primary bound on what the spool can
// cost a machine; the free-space guard only has to keep the spool from being
// the thing that fills the last of a disk. journald's shape is borrowed: a
// percentage of the filesystem, floored and capped in absolute terms. A pure
// ratio had this machine refusing writes below 24.7 GB free on a 494 GB disk
// while its owner's Finder showed tens of gigabytes available, and 17,340
// events were dropped for it.
const (
	// MinFreeBytes is the absolute floor: the headroom a full laptop needs to
	// keep functioning at all, and what a 2 GiB spool can never eat into.
	MinFreeBytes uint64 = 512 << 20
	// MinFreeRatio is the fraction of the filesystem kept free on large disks.
	MinFreeRatio = 0.01
	// MaxRatioBytes caps what the ratio may demand. Past a few hundred
	// gigabytes the percentage stops describing headroom and starts describing
	// a number nobody needs free.
	MaxRatioBytes uint64 = 4 << 30
	// LegacyMinFreeRatio is the ratio the old default wrote into every config
	// file, so it arrives looking explicit. It is treated as unset.
	LegacyMinFreeRatio = 0.05
)

// Guard is the disk-space policy: refuse a write when free space is below
// clamp(MinFreeRatio * total, MinFreeBytes, MaxRatioBytes).
//
// It is one exported decision because three copies of the old ratio existed
// (spool, config, health) and could disagree: `status` would call a machine
// healthy at the exact moment capture was refusing writes. Everything that
// reasons about free space asks this type, so the number the spool refuses at
// and the number health reports are the same number by construction.
type Guard struct {
	MinFreeBytes  uint64
	MinFreeRatio  float64
	MaxRatioBytes uint64
}

// DefaultGuard is the policy a machine with no overrides runs.
func DefaultGuard() Guard { return Guard{}.withDefaults() }

func (g Guard) withDefaults() Guard {
	if g.MinFreeBytes == 0 {
		g.MinFreeBytes = MinFreeBytes
	}
	if g.MinFreeRatio <= 0 || g.MinFreeRatio == LegacyMinFreeRatio {
		g.MinFreeRatio = MinFreeRatio
	}
	if g.MaxRatioBytes == 0 {
		g.MaxRatioBytes = MaxRatioBytes
	}
	return g
}

// Threshold is the number of free bytes below which writes are refused on a
// filesystem of the given total size.
func (g Guard) Threshold(total uint64) uint64 {
	g = g.withDefaults()
	t := uint64(g.MinFreeRatio * float64(total))
	if t > g.MaxRatioBytes {
		t = g.MaxRatioBytes
	}
	if t < g.MinFreeBytes {
		t = g.MinFreeBytes
	}
	return t
}

// Allowed reports whether a write may proceed with free bytes available of
// total, and the threshold the decision was made against.
func (g Guard) Allowed(free, total uint64) (ok bool, threshold uint64) {
	threshold = g.Threshold(total)
	return free >= threshold, threshold
}

// WriteAllowed is the default guard's decision. Health and `status` call this
// so they refuse and report at the same byte the spool does.
func WriteAllowed(free, total uint64) (ok bool, threshold uint64) {
	return DefaultGuard().Allowed(free, total)
}

const (
	defaultMaxBytes     = 2 << 30 // 2 GiB
	defaultMaxAttempts  = 8
	defaultMaxUndecided = 8
	pendingDir          = "pending"
	quarantineDir       = "quarantine"
	parkedDir           = "parked"
	// stateFile holds State. It sits beside the item directories rather than
	// inside any of them, because it describes the spool and must not be
	// mistaken for an item by anything that lists them.
	stateFile = "state.json"
	// successWriteInterval coalesces the durable success timestamp. Ack runs once
	// per delivered item and a backlog flush acks hundreds in a second; the
	// question this timestamp answers ("has anything shipped, and how recently")
	// is asked in minutes, so paying a file write per item would buy nothing.
	successWriteInterval = time.Second
	// maxErrorBytes caps the recorded error. A server answering with a page of
	// HTML must not be able to grow a file the client rewrites on every failure.
	maxErrorBytes = 512
	// tmpSweepAge is how old an unpublished temp file must be before it is
	// treated as abandoned. A hook that was killed between write and rename
	// leaves one behind, and nothing else ever looked at them; a live hook
	// finishes its rename in milliseconds, so minutes is a wide margin.
	tmpSweepAge = 10 * time.Minute
	// capScanRatio is how close to MaxBytes the cached pending total may get
	// before Add measures the directory for real. Below it the cache is trusted
	// and a hook costs no stat per queued file; above it the scan is the price
	// of not evicting on a stale number.
	capScanRatio = 0.9
)

// Spool is safe for concurrent use. Hook processes are short-lived and separate,
// so cross-process safety comes from atomic rename rather than the mutex; the
// mutex only guards in-process bookkeeping.
type Spool struct {
	opts  Options
	guard Guard
	mu    sync.Mutex
	// dropped is this process's own tally, kept alongside the durable one so a
	// spool directory that has become unwritable still reports the drops it
	// observed. A state file we cannot write is itself a symptom, and losing the
	// count of what capture threw away would hide it.
	dropped map[string]int
	// lastSuccessWritten is the coalescing clock for the durable timestamp.
	lastSuccessWritten time.Time
}

// Open prepares a spool directory.
func Open(opts Options) (*Spool, error) {
	if opts.Dir == "" {
		return nil, errors.New("spool: Dir is required")
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultMaxBytes
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = defaultMaxAttempts
	}
	if opts.MaxUndecided <= 0 {
		opts.MaxUndecided = defaultMaxUndecided
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.DiskFree == nil {
		opts.DiskFree = diskFree
	}
	for _, d := range []string{pendingDir, quarantineDir, parkedDir} {
		if err := os.MkdirAll(filepath.Join(opts.Dir, d), 0o700); err != nil {
			return nil, fmt.Errorf("spool: mkdir %s: %w", d, err)
		}
	}
	s := &Spool{opts: opts, guard: Guard{MinFreeRatio: opts.MinFreeRatio}.withDefaults(), dropped: map[string]int{}}
	s.sweepTemp()
	return s, nil
}

// Guard reports the disk policy this spool refuses writes under, so a reporter
// over the same directory judges free space the way this spool does.
func (s *Spool) Guard() Guard { return s.guard }

// NewID mints an idempotency key. Callers that can derive a deterministic key
// (a file byte-range, say) should do that instead so a re-read after a crash
// produces the same key and the server dedups it.
func NewID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

// Add writes an item durably.
//
// The write is temp-file-then-rename so a reader never observes a partial item:
// rename within a directory is atomic on every filesystem we target, whereas a
// direct write can be interrupted mid-flush by the SIGKILL we know we cannot
// hook. We deliberately do not fsync: the cost is real on every turn, and the
// failure it protects against (power loss between write and rename) loses at
// most the last event, which the reconciliation pass recovers from the
// transcript anyway.
func (s *Spool) Add(it Item) error {
	if it.ID == "" {
		it.ID = NewID()
	}
	if it.EventTime.IsZero() {
		it.EventTime = s.opts.Now()
	}
	body, err := json.Marshal(it)
	if err != nil {
		return fmt.Errorf("spool: marshal: %w", err)
	}

	if err := s.guardDisk(); err != nil {
		s.drop("disk_full")
		return err
	}
	if err := s.enforceCap(int64(len(body))); err != nil {
		return err
	}

	dir := filepath.Join(s.opts.Dir, pendingDir)
	// Filenames sort chronologically so the drain delivers in roughly the order
	// things happened, which keeps a session's events adjacent on the wire.
	name := fmt.Sprintf("%013d-%s.json", s.opts.Now().UnixMilli(), it.ID)
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("spool: write: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("spool: publish: %w", err)
	}
	s.adjustPendingBytes(int64(len(body)))
	return nil
}

// Lease returns up to n pending items, oldest first, with the paths needed to
// ack or fail them. Items are not hidden from other readers: a laptop runs one
// drain, and the cost of a lease protocol is not worth paying for a race that
// resolves into a duplicate the server already dedups.
func (s *Spool) Lease(n int) ([]Leased, error) {
	s.sweepTemp()
	names, err := s.pendingNames()
	if err != nil {
		return nil, err
	}
	if n > 0 && len(names) > n {
		names = names[:n]
	}
	out := make([]Leased, 0, len(names))
	for _, name := range names {
		p := filepath.Join(s.opts.Dir, pendingDir, name)
		b, err := os.ReadFile(p)
		if err != nil {
			continue // vanished under us; nothing to do
		}
		var it Item
		if err := json.Unmarshal(b, &it); err != nil {
			// A corrupt item can never succeed, so quarantine it immediately
			// rather than letting it fail MaxAttempts times first.
			_ = s.quarantine(p, "corrupt")
			s.drop("corrupt")
			continue
		}
		out = append(out, Leased{Item: it, path: p})
	}
	return out, nil
}

// Leased is an item checked out for delivery.
type Leased struct {
	Item Item
	path string
}

// Quarantined lists up to n quarantined items, oldest first, as Leased so
// the operator's replay can hand one to the transport and, if the server
// accepts it now, Ack it out of quarantine.
func (s *Spool) Quarantined(n int) ([]Leased, error) {
	dir := filepath.Join(s.opts.Dir, quarantineDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range ents {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if n > 0 && len(names) > n {
		names = names[:n]
	}
	out := make([]Leased, 0, len(names))
	for _, name := range names {
		p := filepath.Join(dir, name)
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var it Item
		if err := json.Unmarshal(b, &it); err != nil {
			continue
		}
		out = append(out, Leased{Item: it, path: p})
	}
	return out, nil
}

// Ack removes a delivered item. This is the only path that deletes pending
// data, so nothing is lost until the server has it.
//
// It is also where "this machine has delivered something" becomes durable, and
// it is here rather than in the uploader on purpose. Ack is reached only after
// the server has accepted an item and is the sole route out of pending, so a
// delivery cannot occur without passing through this line. A recorder the
// uploader had to remember to call could be, and in this codebase repeatedly
// has been, written, tested, and never called.
func (s *Spool) Ack(l Leased) error {
	s.recordSuccess()
	size := fileSize(l.path)
	if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	// PendingBytes counts pending/ alone. An item Quarantined() handed back
	// left that total when it was moved out, so an Ack that subtracted it
	// again would run the cache low until the next full measure.
	if filepath.Dir(l.path) == filepath.Join(s.opts.Dir, pendingDir) {
		s.adjustPendingBytes(-size)
	}
	return nil
}

// State reports what the spool remembers across processes. A spool that has
// never recorded anything returns a zero State and no error: nothing to say is
// not a failure, and callers must be able to tell that apart from a state file
// they could not read, which is an error and is reported as one.
func (s *Spool) State() (State, error) {
	b, err := os.ReadFile(filepath.Join(s.opts.Dir, stateFile))
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("spool: read state: %w", err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return State{}, fmt.Errorf("spool: state is unreadable: %w", err)
	}
	return st, nil
}

// RecordDeliveryFailure notes why the last upload attempt failed, so a queue
// that is not moving can say what is stopping it instead of only how long it is.
//
// The uploader has to call this because only the uploader has the error: a
// transport failure never reaches the items, deliberately, so that a week-long
// outage does not quarantine a whole backlog. Nothing breaks if it is never
// called; the absence of a recorded reason is reported as an absence, never as
// health.
func (s *Spool) RecordDeliveryFailure(err error) {
	if err == nil {
		return
	}
	msg := err.Error()
	if len(msg) > maxErrorBytes {
		msg = msg[:maxErrorBytes] + "..."
	}
	now := s.opts.Now()
	s.mutateState(func(st *State) {
		st.LastError = msg
		st.LastErrorAt = now
	})
}

// recordSuccess durably marks that the server accepted something just now.
func (s *Spool) recordSuccess() {
	now := s.opts.Now()
	s.mu.Lock()
	if !s.lastSuccessWritten.IsZero() && now.Sub(s.lastSuccessWritten) < successWriteInterval {
		s.mu.Unlock()
		return
	}
	s.lastSuccessWritten = now
	s.mu.Unlock()

	s.mutateState(func(st *State) {
		// Monotonic on purpose: two daemons on one machine can be draining the
		// same spool, and an older writer must not walk the timestamp backwards.
		if now.After(st.LastSuccess) {
			st.LastSuccess = now
		}
	})
}

// mutateState applies a change to the durable state, read-modify-write.
//
// Errors are swallowed here and nowhere else. This is bookkeeping about the
// data, not the data: failing a delivery because a note about it could not be
// written would trade something recoverable for something that is not. The loss
// is visible anyway: State() reports what it cannot read, and a state file that
// stops advancing while items keep leaving reads as an unreadable spool rather
// than as health.
//
// Concurrent writers can lose an update, because two processes can read the same
// file before either writes. What is lost is at most one drop count, a
// sub-second-old timestamp or a byte estimate the next measurement corrects; a
// lock file would cost every delivery a syscall to prevent an error nobody can
// observe.
func (s *Spool) mutateState(fn func(*State)) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, err := s.State()
	if err != nil {
		// Unreadable or corrupt: start a fresh record rather than refusing to
		// record anything ever again.
		st = State{}
	}
	fn(&st)
	body, err := json.Marshal(st)
	if err != nil {
		return
	}
	path := filepath.Join(s.opts.Dir, stateFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// Fail records a genuine failed attempt and quarantines the item once it has
// exhausted MaxAttempts, under the reason max_attempts. Quarantine, not
// deletion: an item we could not deliver is evidence, and an operator
// redriving it is a supported action.
//
// Nothing on the delivery path calls it today, and that is deliberate. A
// verdict that names the item goes to Ack or Quarantine, one that omits it
// goes to Undecided, and a transport failure belongs to the batch, not to any
// item in it, so no item is ever charged an attempt. Nine machines'
// quarantines were traced to counting silence as failure (eight deliveries
// the server answered 200 to without naming the item, counted as eight
// failures), which is why the count-based path was cut off rather than made
// reachable again. It stays, with its tests, as the one sanctioned way to
// quarantine by count should a per-item failure that is neither a rejection
// nor silence ever exist; until then max_attempts is a reserved reason.
func (s *Spool) Fail(l Leased) error {
	l.Item.Attempts++
	if l.Item.Attempts >= s.opts.MaxAttempts {
		s.drop("max_attempts")
		return s.moveOut(l.path, quarantineDir, "max_attempts")
	}
	return s.rewrite(l)
}

// Undecided records a delivery after which the server named the item in
// neither list. The item stays pending; at MaxUndecided consecutive such
// deliveries it is parked and the returned flag is true.
//
// Parked is not quarantined. An item the server keeps ignoring is almost
// always waiting on a server fix or a client upgrade, both of which happen
// without anybody looking at this machine, so the item is redriven
// automatically when the binary changes and once a day besides.
func (s *Spool) Undecided(l Leased) (parked bool, err error) {
	l.Item.Undecided++
	if l.Item.Undecided >= s.opts.MaxUndecided {
		s.drop("parked")
		return true, s.moveOut(l.path, parkedDir, "")
	}
	return false, s.rewrite(l)
}

// Quarantine moves an item the server explicitly rejected out of the pending
// set, keeping the server's reason in the filename so `ls quarantine/` answers
// "why" without opening anything. The reason is also the drop counter's key,
// so health can tell a fleet of oversized payloads from a schema break.
func (s *Spool) Quarantine(l Leased, reason string) error {
	tag := reasonTag(reason)
	s.drop("rejected:" + tag)
	return s.moveOut(l.path, quarantineDir, "rejected-"+tag)
}

// rewrite persists an item's counters in place.
func (s *Spool) rewrite(l Leased) error {
	body, err := json.Marshal(l.Item)
	if err != nil {
		return err
	}
	tmp := tempName(l.path)
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

// tempName is the per-process temp file an item is written through before
// its rename publishes it: ".<name>.<pid>.tmp" beside the item. The pid is in
// the name because two daemons starting together each redrive the parked
// items before either has stamped the redrive, and with one shared temp name
// the second truncates the first's file mid-write; whichever rename wins then
// publishes a torn item that Lease quarantines as corrupt. The dot prefix is
// what keeps Lease from returning it and lets sweepTemp find it.
func tempName(path string) string {
	return filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+"."+strconv.Itoa(os.Getpid())+".tmp")
}

// Redrive moves quarantined items back to pending with their counters reset.
// This is the ops CTA for "we fixed the server bug, resend everything".
func (s *Spool) Redrive() (int, error) {
	return s.redriveFrom(quarantineDir)
}

// RedriveParked moves parked items back to pending. The daemon calls it on a
// binary change and daily; an operator reaches it through `doctor --redrive`.
func (s *Spool) RedriveParked() (int, error) {
	return s.redriveFrom(parkedDir)
}

func (s *Spool) redriveFrom(sub string) (int, error) {
	dir := filepath.Join(s.opts.Dir, sub)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	var n int
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		src := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		var it Item
		if err := json.Unmarshal(b, &it); err != nil {
			continue // still corrupt, leave it for inspection
		}
		it.Attempts = 0
		it.Undecided = 0
		nb, err := json.Marshal(it)
		if err != nil {
			continue
		}
		// The reason tag is stripped from the name so the item sorts back into
		// its chronological place and a second refusal can carry a new reason.
		dst := filepath.Join(s.opts.Dir, pendingDir, baseName(e.Name()))
		tmp := tempName(dst)
		if err := os.WriteFile(tmp, nb, 0o600); err != nil {
			continue
		}
		if err := os.Rename(tmp, dst); err != nil {
			_ = os.Remove(tmp)
			continue
		}
		_ = os.Remove(src)
		s.adjustPendingBytes(int64(len(nb)))
		n++
	}
	return n, nil
}

// Stats reports spool health for the heartbeat. It measures pending/ for real
// and writes the measurement back as the cached byte total, which is what keeps
// the estimate Add trusts honest between reports.
func (s *Spool) Stats() (Stats, error) {
	st := Stats{Dropped: s.snapshotDropped()}
	names, err := s.pendingNames()
	if err != nil {
		return st, err
	}
	st.Pending = len(names)
	var oldest time.Time
	for _, n := range names {
		fi, err := os.Stat(filepath.Join(s.opts.Dir, pendingDir, n))
		if err != nil {
			continue
		}
		st.Bytes += fi.Size()
		if oldest.IsZero() || fi.ModTime().Before(oldest) {
			oldest = fi.ModTime()
		}
	}
	if !oldest.IsZero() {
		// Clamped at zero: a file mtime can sit slightly in the future after a
		// clock adjustment or an NTP step, and a negative backlog age would be
		// meaningless in the heartbeat and could trip an alert comparison.
		if age := s.opts.Now().Sub(oldest); age > 0 {
			st.OldestAge = age
		}
	}
	st.Quarantine = s.countItems(quarantineDir)
	st.Parked = s.countItems(parkedDir)
	measured := st.Bytes
	s.mutateState(func(x *State) { x.PendingBytes, x.PendingMeasured = measured, true })
	return st, nil
}

func (s *Spool) countItems(sub string) int {
	var n int
	if ents, err := os.ReadDir(filepath.Join(s.opts.Dir, sub)); err == nil {
		for _, e := range ents {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				n++
			}
		}
	}
	return n
}

func (s *Spool) pendingNames() ([]string, error) {
	dir := filepath.Join(s.opts.Dir, pendingDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		// Skip in-flight temp files; they are not yet published.
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names) // millisecond-prefixed, so lexical order is chronological
	return names, nil
}

// sweepTemp removes temp files a dead process left in pending/.
//
// Add publishes by writing ".<name>.tmp" and renaming it; rewrite and a
// redrive write ".<name>.<pid>.tmp" the same way, and releases before this one
// wrote "<name>.json.tmp" with no dot at all. A process killed between write
// and rename (the harness tearing down a `claude -p` run, a laptop closing, a
// daemon that died mid-redrive) leaves the temp file behind, and because
// Lease returns neither dotfiles nor anything that is not a .json, nothing
// would ever look at it again. Its contents are not recoverable from here (it
// may be half-written), so every shape is simply removed once it is old enough
// that no live process can still be about to rename it. The transcript
// recovery pass is what brings a lost event itself back.
func (s *Spool) sweepTemp() {
	dir := filepath.Join(s.opts.Dir, pendingDir)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := s.opts.Now().Add(-tmpSweepAge)
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.ModTime().Before(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			s.drop("tmp_swept")
		}
	}
}

// moveOut relocates an item into quarantine/ or parked/, optionally tagging the
// filename with why. The tag goes between the id and the extension so the file
// still ends in .json (every counter here counts .json files) and the base name
// can be restored on redrive.
func (s *Spool) moveOut(path, sub, tag string) error {
	size := fileSize(path)
	name := baseName(filepath.Base(path))
	if tag != "" {
		name = strings.TrimSuffix(name, ".json") + "." + tag + ".json"
	}
	dst := filepath.Join(s.opts.Dir, sub, name)
	if err := os.Rename(path, dst); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("spool: move to %s (%s): %w", sub, tag, err)
	}
	s.adjustPendingBytes(-size)
	return nil
}

func (s *Spool) quarantine(path, reason string) error {
	return s.moveOut(path, quarantineDir, reason)
}

// baseName strips any reason tag: "<ms>-<id>.<tag>.json" -> "<ms>-<id>.json".
func baseName(name string) string {
	stem := strings.TrimSuffix(name, ".json")
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	return stem + ".json"
}

// reasonTag turns the server's free-text reason into something that can live
// in a filename and a metric label: lowercase, [a-z0-9_], bounded. Two reasons
// that differ only in punctuation collapse together, which is what a counter
// wants.
func reasonTag(reason string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range strings.ToLower(strings.TrimSpace(reason)) {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		switch {
		case ok:
			b.WriteRune(r)
			lastUnderscore = false
		case !lastUnderscore && b.Len() > 0:
			b.WriteByte('_')
			lastUnderscore = true
		}
		if b.Len() >= 48 {
			break
		}
	}
	out := strings.TrimRight(b.String(), "_")
	if out == "" {
		return "unspecified"
	}
	return out
}

// enforceCap drops the oldest pending items until the incoming write fits.
// Dropping the oldest rather than rejecting the newest is deliberate: on a
// laptop that has been offline a long time, the freshest session is the one
// someone is about to ask about.
//
// The directory is measured only when the cached total says the cap is near,
// and only once it descends from a measurement at all (a state file from
// before the cache existed says nothing about the queue). A stat per queued
// file on every hook was fine at a dozen items and is a visible stall at ten
// thousand, which is exactly the backlog an offline laptop carries when it
// comes back and starts firing hooks again.
func (s *Spool) enforceCap(incoming int64) error {
	if st, err := s.State(); err == nil && st.PendingMeasured && st.PendingBytes >= 0 {
		if float64(st.PendingBytes+incoming) < float64(s.opts.MaxBytes)*capScanRatio {
			return nil
		}
	}
	names, err := s.pendingNames()
	if err != nil {
		return err
	}
	var total int64
	sizes := make(map[string]int64, len(names))
	for _, n := range names {
		fi, err := os.Stat(filepath.Join(s.opts.Dir, pendingDir, n))
		if err != nil {
			continue
		}
		sizes[n] = fi.Size()
		total += fi.Size()
	}
	for i := 0; total+incoming > s.opts.MaxBytes && i < len(names); i++ {
		p := filepath.Join(s.opts.Dir, pendingDir, names[i])
		if err := os.Remove(p); err == nil {
			total -= sizes[names[i]]
			s.drop("queue_overflow")
		}
	}
	measured := total
	s.mutateState(func(x *State) { x.PendingBytes, x.PendingMeasured = measured, true })
	if total+incoming > s.opts.MaxBytes {
		s.drop("queue_overflow")
		return fmt.Errorf("spool: item exceeds cap (%d > %d)", incoming, s.opts.MaxBytes)
	}
	return nil
}

func (s *Spool) adjustPendingBytes(delta int64) {
	if delta == 0 {
		return
	}
	s.mutateState(func(st *State) {
		st.PendingBytes += delta
		if st.PendingBytes < 0 {
			st.PendingBytes = 0
		}
	})
}

func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// DiskFree measures the filesystem holding dir, using the same probe the spool
// guards writes with. It is exported so that whatever reports on the spool
// measures free space the way the spool decides to refuse a write: two probes
// with different answers would let `status` call a machine healthy at the exact
// moment capture is dropping events.
func DiskFree(dir string) (free, total uint64, err error) { return diskFree(dir) }

func (s *Spool) guardDisk() error {
	free, total, err := s.opts.DiskFree(s.opts.Dir)
	if err != nil || total == 0 {
		return nil // cannot tell; do not block capture on a stat failure
	}
	if ok, threshold := s.guard.Allowed(free, total); !ok {
		return fmt.Errorf("spool: refusing to write, free disk %d bytes is below the %d byte floor", free, threshold)
	}
	return nil
}

// Record adds n to a named counter, durably. It is the same tally drops land
// in, because the health report reads one map and the conditions it derives
// ("this machine abandoned 12 hooks", "this machine recovered 40 events from
// transcripts") belong beside "this machine dropped 3 events" rather than in
// a second table with a second failure mode.
func (s *Spool) Record(reason string, n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	s.dropped[reason] += n
	s.mu.Unlock()

	s.mutateState(func(st *State) {
		if st.Dropped == nil {
			st.Dropped = map[string]int{}
		}
		st.Dropped[reason] += n
	})
}

// drop records discarded data both in memory and on disk.
//
// On disk because the process that discards is almost never the process that
// reports: capture drops events inside a hook that exits milliseconds later, and
// the person who asks `status` an hour afterwards is running a different program
// entirely. A counter that dies with the hook makes the single most consequential
// condition in this package (capture refusing writes because the disk is full)
// unobservable by the only command anyone runs to check.
func (s *Spool) drop(reason string) { s.Record(reason, 1) }

// snapshotDropped merges what this process saw with what the directory records.
//
// Per-reason maximum rather than a sum: the durable count already includes this
// process's own drops, so adding them would double-count, while a durable count
// that is lower than what we just observed means the state file could not be
// written and memory is the better witness. Either way the answer is never
// smaller than the truth this process knows.
func (s *Spool) snapshotDropped() map[string]int {
	s.mu.Lock()
	mem := make(map[string]int, len(s.dropped))
	for k, v := range s.dropped {
		mem[k] = v
	}
	s.mu.Unlock()

	if st, err := s.State(); err == nil {
		for k, v := range st.Dropped {
			if v > mem[k] {
				mem[k] = v
			}
		}
	}
	if len(mem) == 0 {
		return nil
	}
	return mem
}
