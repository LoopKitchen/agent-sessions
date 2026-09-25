package derive

import (
	"context"
	"errors"
	"testing"
	"time"
)

// IST is a fixture zone: the window is expressed in whatever zone it is
// given, and a fixed offset keeps these tests independent of the host TZ.
var IST = time.FixedZone("IST", 5*3600+30*60)

func TestParseWindow(t *testing.T) {
	w, err := ParseWindow("")
	if err != nil || w.Start != 2 || w.End != 6 || w.Always {
		t.Errorf("default = %+v %v", w, err)
	}
	if w, err := ParseWindow("always"); err != nil || !w.Always {
		t.Errorf("always = %+v %v", w, err)
	}
	if w, err := ParseWindow("22-04"); err != nil || w.Start != 22 || w.End != 4 {
		t.Errorf("wrap = %+v %v", w, err)
	}
	for _, bad := range []string{"2", "a-b", "25-03", "03-03", "-1-5"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("ParseWindow(%q) accepted", bad)
		}
	}
	if got := (Window{Start: 2, End: 6}).String(); got != "02-06 "+time.Local.String() {
		t.Errorf("String = %q", got)
	}
}

// The scheduler sleeps toward the window rather than polling a ticker that
// can miss a four-hour window forever (review-design-ops F4): a boot at
// 07:00 IST waits until 02:00 the next day, and a tick at 03:30 runs now.
func TestWindowUntilAndContains(t *testing.T) {
	w := Window{Start: 2, End: 6, Loc: IST}
	at := func(h, m int) time.Time { return time.Date(2026, 9, 11, h, m, 0, 0, IST) }

	if !w.Contains(at(3, 30)) || w.Contains(at(6, 0)) || w.Contains(at(1, 59)) || !w.Contains(at(2, 0)) {
		t.Error("Contains is not [start, end)")
	}
	if got := w.Until(at(3, 30)); got != 0 {
		t.Errorf("inside the window Until = %s, want 0", got)
	}
	if got, want := w.Until(at(7, 0)), 19*time.Hour; got != want {
		t.Errorf("from 07:00 Until = %s, want %s (02:00 tomorrow)", got, want)
	}
	if got, want := w.Until(at(1, 30)), 30*time.Minute; got != want {
		t.Errorf("from 01:30 Until = %s, want %s", got, want)
	}
	// Expressed in IST whatever zone the clock reports in.
	utc := at(7, 0).In(time.UTC)
	if got := w.Until(utc); got != 19*time.Hour {
		t.Errorf("from 07:00 IST given as UTC Until = %s", got)
	}

	wrap := Window{Start: 22, End: 4, Loc: IST}
	if !wrap.Contains(at(23, 0)) || !wrap.Contains(at(1, 0)) || wrap.Contains(at(12, 0)) {
		t.Error("a wrapping window does not contain the hours across midnight")
	}
	if got, want := wrap.Until(at(12, 0)), 10*time.Hour; got != want {
		t.Errorf("wrap Until = %s, want %s", got, want)
	}
	if got := (Window{Always: true}).Until(at(12, 0)); got != 0 {
		t.Errorf("always Until = %s", got)
	}
}

func TestWindowWaitForSleepsTowardTheWindowAndStopsOnCancel(t *testing.T) {
	w := Window{Start: 2, End: 6, Loc: IST}
	now := func() time.Time { return time.Date(2026, 9, 11, 7, 0, 0, 0, IST) }
	var slept time.Duration
	ok := w.WaitFor(context.Background(), now, func(_ context.Context, d time.Duration) error { slept = d; return nil })
	if !ok || slept != 19*time.Hour {
		t.Errorf("slept %s ok=%v", slept, ok)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	ok = w.WaitFor(cancelled, now, func(ctx context.Context, d time.Duration) error { return ctx.Err() })
	if ok {
		t.Error("a cancelled wait reported the window as open")
	}
	inside := func() time.Time { return time.Date(2026, 9, 11, 3, 0, 0, 0, IST) }
	slept = 0
	if !w.WaitFor(context.Background(), inside, func(_ context.Context, d time.Duration) error { slept = d; return nil }) || slept != 0 {
		t.Errorf("inside the window WaitFor slept %s", slept)
	}
	if err := Sleep(cancelled, time.Hour); !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep on a cancelled context = %v", err)
	}
}

func TestPacerDelay(t *testing.T) {
	p := Pacer{RowsPerSec: 1000}
	if got := p.Delay(5000, time.Second); got != 4*time.Second {
		t.Errorf("5000 rows in 1 s at 1000/s: delay = %s, want 4s", got)
	}
	if got := p.Delay(5000, 10*time.Second); got != 0 {
		t.Errorf("a batch slower than the cap waits %s", got)
	}
	if got := (Pacer{}).Delay(5000, 0); got != 0 {
		t.Errorf("no cap waits %s", got)
	}
	if got := p.Delay(0, 0); got != 0 {
		t.Errorf("no rows waits %s", got)
	}
}
