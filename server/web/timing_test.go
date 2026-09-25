package web

import (
	"testing"
	"time"
)

// TestLongSpansUseDays: a span past a day is said in days (a session left
// open over a weekend reads "2d 3h 04m", not "51h 04m"), and the elapsed
// gutter label agrees. The days branch of durLabel has no other test since
// the timeline left; spanLabel still reads it for every work summary.
func TestLongSpansUseDays(t *testing.T) {
	for _, tc := range []struct {
		duration time.Duration
		want     string
	}{
		{162*time.Hour + 54*time.Minute, "6d 18h 54m"},
		{24 * time.Hour, "1d 0h 00m"},
		{23*time.Hour + 59*time.Minute, "23h 59m"},
		{3*time.Minute + 4*time.Second, "3m 04s"},
		{42 * time.Second, "42s"},
	} {
		if got := durLabel(tc.duration); got != tc.want {
			t.Errorf("durLabel(%s) = %q, want %q", tc.duration, got, tc.want)
		}
	}
	if got := ElapsedLabel(162*time.Hour + 54*time.Minute); got != "+6d18h54m" {
		t.Errorf("ElapsedLabel over a week = %q", got)
	}
}
