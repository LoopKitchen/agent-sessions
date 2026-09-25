package derive

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Window is the daily interval in which the runner's body-reading steps are
// allowed to run: [Start, End) as hours of the day in Loc. The steps that
// detoast event bodies are minutes of sustained disk reads on the instance
// that also serves ingest, and the window keeps them to the hours nobody is
// working. Steps that read columns only are not gated by it.
type Window struct {
	Start, End int
	Loc        *time.Location
	// Always disables the gate. It is a field rather than a nil Window so a
	// configuration that switched the gate off is visible in the log line
	// that prints the window.
	Always bool
}

// defaultLocation is the timezone the window is expressed in: the server's
// own zone, which TZ sets and main.go logs at boot, so "02-06" is the
// deployment's overnight wherever it runs.
func defaultLocation() *time.Location { return time.Local }

// ParseWindow reads "HH-HH" (start hour inclusive, end hour exclusive, in
// the server's TZ), "always" to switch the gate off, or "" for the default
// 02-06. A
// window that wraps midnight ("22-04") is allowed; a zero-length one is not,
// because it would mean the body-reading steps never run and nothing would
// say so.
func ParseWindow(s string) (Window, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	switch s {
	case "":
		return Window{Start: 2, End: 6, Loc: defaultLocation()}, nil
	case "always", "any", "off":
		return Window{Always: true, Loc: defaultLocation()}, nil
	}
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return Window{}, fmt.Errorf("derive: window %q is not HH-HH", s)
	}
	start, err1 := strconv.Atoi(strings.TrimSpace(a))
	end, err2 := strconv.Atoi(strings.TrimSpace(b))
	if err1 != nil || err2 != nil || start < 0 || start > 23 || end < 0 || end > 24 {
		return Window{}, fmt.Errorf("derive: window %q needs hours between 00 and 24", s)
	}
	if start == end {
		return Window{}, fmt.Errorf("derive: window %q is empty; use \"always\" to run at any hour", s)
	}
	return Window{Start: start, End: end, Loc: defaultLocation()}, nil
}

// String renders the window the way ParseWindow reads it.
func (w Window) String() string {
	if w.Always {
		return "always"
	}
	return fmt.Sprintf("%02d-%02d %s", w.Start, w.End, w.loc().String())
}

func (w Window) loc() *time.Location {
	if w.Loc == nil {
		return defaultLocation()
	}
	return w.Loc
}

// Contains reports whether t falls inside the window.
func (w Window) Contains(t time.Time) bool {
	if w.Always {
		return true
	}
	h := t.In(w.loc()).Hour()
	if w.Start < w.End {
		return h >= w.Start && h < w.End
	}
	return h >= w.Start || h < w.End
}

// Until reports how long from t until the window next opens: zero when t is
// already inside it. This is what the scheduler sleeps for, so the runner
// wakes at the window's start rather than polling a fixed ticker that can
// miss a four-hour window forever.
func (w Window) Until(t time.Time) time.Duration {
	if w.Contains(t) {
		return 0
	}
	local := t.In(w.loc())
	next := time.Date(local.Year(), local.Month(), local.Day(), w.Start, 0, 0, 0, w.loc())
	if !next.After(local) {
		next = next.Add(24 * time.Hour)
	}
	return next.Sub(local)
}

// WaitFor sleeps toward the window and reports false if the context ended
// first. sleep is a parameter so a test can observe the duration rather than
// live it.
func (w Window) WaitFor(ctx context.Context, now func() time.Time, sleep func(context.Context, time.Duration) error) bool {
	d := w.Until(now())
	if d == 0 {
		return true
	}
	if err := sleep(ctx, d); err != nil {
		return false
	}
	return ctx.Err() == nil
}

// Sleep is the sleep WaitFor uses in production: a timer that the context
// can cut short.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Pacer bounds the rows per second a body-reading step processes. Postgres
// exposes no CPU signal, so the cap is on the one thing the step controls:
// after each sub-batch it sleeps for whatever is left of the time the batch
// would have taken at the cap. The measured single-stream rate on the
// production clone was about 1,690 rows/s (design.md section 11), and the
// default cap below leaves the instance the remainder for ingest.
type Pacer struct {
	RowsPerSec int
}

// DefaultRowsPerSec is the cap when the deployment names none.
const DefaultRowsPerSec = 1500

// Delay reports how long a batch of rows that took elapsed should be
// followed by a pause to stay under the cap. Zero when the batch was already
// slower than the cap allows, or when no cap is set.
func (p Pacer) Delay(rows int, elapsed time.Duration) time.Duration {
	if p.RowsPerSec <= 0 || rows <= 0 {
		return 0
	}
	budget := time.Duration(float64(rows) / float64(p.RowsPerSec) * float64(time.Second))
	if budget <= elapsed {
		return 0
	}
	return budget - elapsed
}
