package skillusage

import (
	"container/list"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// semaphore bounds how many requests hold a pool connection at once. Four
// per instance: the route shares the pool with the upload path, and a
// flood that queued ingest behind skill posts would cost more than the
// posts are worth. A request that finds it full answers 429 at once
// rather than waiting, since the emitters drop a 429 and try again on
// their next call.
type semaphore chan struct{}

func (s semaphore) tryAcquire() bool {
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s semaphore) release() { <-s }

// shed is the in-process load shed ahead of the verify: a fixed number of
// minute buckets keyed on the client address Cloud Run forwards, and on
// the token hash once the verify has returned it live. A junk bearer
// never gets a bucket of its own, since each would be a new key and a
// per-hash map would grow without bound on every instance; the address
// is the bounded key the enrolment limiter already trusts. When the map
// is full the least recently touched bucket goes, so a burst of new
// addresses evicts idle ones and never grows the map. The row counter
// stays the authority; this only keeps a flood off the database.
type shed struct {
	mu     sync.Mutex
	cap    int
	perMin int
	// order is most recently touched first; seen indexes it by key.
	order *list.List
	seen  map[string]*list.Element
}

type shedBucket struct {
	key   string
	start time.Time
	count int
}

func newShed(buckets, perMin int) *shed {
	return &shed{cap: buckets, perMin: perMin, order: list.New(), seen: map[string]*list.Element{}}
}

// allow counts one call against key's minute and reports whether it is
// within the allowance.
func (s *shed) allow(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.seen[key]
	if !ok {
		if s.order.Len() >= s.cap {
			oldest := s.order.Back()
			s.order.Remove(oldest)
			delete(s.seen, oldest.Value.(*shedBucket).key)
		}
		el = s.order.PushFront(&shedBucket{key: key, start: now})
		s.seen[key] = el
	} else {
		s.order.MoveToFront(el)
	}
	b := el.Value.(*shedBucket)
	if now.Sub(b.start) >= time.Minute {
		b.start, b.count = now, 0
	}
	b.count++
	return b.count <= s.perMin
}

// size is how many buckets the shed holds, for the test that proves the
// bound.
func (s *shed) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// bucketLimiter is the per-device bound on lsd_ posts, the enrolment
// limiter's shape (server/app/enroll.go): a token bucket per key measured
// against an injected clock, per process, since a laptop posting through
// its own credential is bounded well enough by an order of magnitude and
// a shared counter would mean a Redis this service does not run.
type bucketLimiter struct {
	mu       sync.Mutex
	burst    float64
	interval time.Duration
	seen     map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// bucketSweepAt is the map size at which a new key first sweeps full
// buckets out, which are indistinguishable from keys never seen.
const bucketSweepAt = 4096

func newBucketLimiter(burst float64, interval time.Duration) *bucketLimiter {
	return &bucketLimiter{burst: burst, interval: interval, seen: map[string]*bucket{}}
}

func (l *bucketLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.seen[key]
	if !ok {
		l.sweep(now)
		b = &bucket{tokens: l.burst, last: now}
		l.seen[key] = b
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens += float64(elapsed) / float64(l.interval)
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *bucketLimiter) sweep(now time.Time) {
	if len(l.seen) < bucketSweepAt {
		return
	}
	full := time.Duration(l.burst * float64(l.interval))
	for k, b := range l.seen {
		if now.Sub(b.last) >= full {
			delete(l.seen, k)
		}
	}
}

// clientAddr is the address the shed keys on before the verify: the last
// hop of X-Forwarded-For, which Cloud Run's front end appends from the
// connection it accepted, else the connection's own host. Anything the
// client put in the header sits before that hop, so keying on the first
// hop would hand a flood a fresh bucket per request and the shed would
// never refuse. Nothing sits between the front end and the service (the
// IAP note above: no load balancer), so the last hop is the client's and
// not a balancer's. The port is dropped so one client on many connections
// is one bucket.
func clientAddr(r *http.Request) string {
	// Every header line, last first: a client may send its own line and
	// the front end appends to the header it received.
	values := r.Header.Values("X-Forwarded-For")
	for v := len(values) - 1; v >= 0; v-- {
		hops := strings.Split(values[v], ",")
		for i := len(hops) - 1; i >= 0; i-- {
			if hop := strings.TrimSpace(hops[i]); hop != "" {
				return hop
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
