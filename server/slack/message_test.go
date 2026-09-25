package slack

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func summaryAt(mutate func(*Summary)) Summary {
	start := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	s := Summary{
		SessionID: "abc123",
		Email:     "ana@example.org",
		Repo:      "loop-sessions",
		Branch:    "main",
		StartedAt: start,
		EndedAt:   start.Add(35 * time.Minute),
		Ended:     true,
		UserTurns: 6,
		ToolCalls: 40,
	}
	if mutate != nil {
		mutate(&s)
	}
	return s
}

// TestRenderSaysWhatHappenedWithoutQuotingAnyOfIt walks the whole message
// surface. Each case names the property it defends because each is a way the
// message misleads rather than a formatting preference.
func TestRenderSaysWhatHappenedWithoutQuotingAnyOfIt(t *testing.T) {
	tests := []struct {
		name    string
		summary Summary
		mode    Mode
		want    []string
		absent  []string
	}{
		{
			name:    "a channel message names whose work it is, because a channel carries several people's",
			summary: summaryAt(nil),
			mode:    ModeChannel,
			want:    []string{"*ana@example.org*", "finished", "`loop-sessions`", "`main`"},
		},
		{
			name:    "a DM does not name its recipient, which would read as a form letter about your own work",
			summary: summaryAt(nil),
			mode:    ModeDM,
			want:    []string{"Your session finished"},
			absent:  []string{"ana@example.org"},
		},
		{
			name:    "a session that merely stopped arriving is not reported as one that ended",
			summary: summaryAt(func(s *Summary) { s.Ended = false }),
			mode:    ModeChannel,
			want:    []string{"went quiet"},
			absent:  []string{"finished"},
		},
		{
			name:    "the link back is always present, because it is what keeps the permission model in the loop",
			summary: summaryAt(nil),
			mode:    ModeChannel,
			want:    []string{"<https://sessions.example.com/sessions/abc123|Open in loop-sessions>"},
		},
		{
			name:    "counts that are zero are left out rather than said three times as nothing",
			summary: summaryAt(func(s *Summary) { s.ToolCalls = 0; s.Subagents = 0; s.Errors = 0; s.CostUSD = 0 }),
			mode:    ModeChannel,
			want:    []string{"6 turns"},
			absent:  []string{"0 tool calls", "0 subagents", "0 errors", "$0.00"},
		},
		{
			name:    "errors are stated when present, because that is the session somebody wants to look at",
			summary: summaryAt(func(s *Summary) { s.Errors = 3 }),
			mode:    ModeChannel,
			want:    []string{"3 errors"},
		},
		{
			name:    "one of a thing is singular, so the message does not read like a machine wrote it",
			summary: summaryAt(func(s *Summary) { s.UserTurns = 1; s.ToolCalls = 1; s.Subagents = 1; s.Errors = 1 }),
			mode:    ModeChannel,
			want:    []string{"1 turn", "1 tool call", "1 subagent", "1 error"},
			absent:  []string{"1 turns", "1 tool calls"},
		},
		{
			name:    "a cost is printed only when something was actually priced",
			summary: summaryAt(func(s *Summary) { s.CostUSD = 12.5 }),
			mode:    ModeChannel,
			want:    []string{"$12.50"},
		},
		{
			name:    "a branch that looks like markup cannot silently re-render the message",
			summary: summaryAt(func(s *Summary) { s.Branch = "feat/<script>&stuff" }),
			mode:    ModeChannel,
			want:    []string{"&lt;script&gt;&amp;stuff"},
			absent:  []string{"<script>"},
		},
		{
			name:    "an address that looks like markup is escaped too",
			summary: summaryAt(func(s *Summary) { s.Email = "a<b@example.org" }),
			mode:    ModeChannel,
			want:    []string{"a&lt;b@example.org"},
		},
		{
			name:    "a session with no repo or branch still produces a sentence",
			summary: summaryAt(func(s *Summary) { s.Repo = ""; s.Branch = "" }),
			mode:    ModeChannel,
			want:    []string{"finished\n"},
			absent:  []string{"in ``", "on ``"},
		},
		{
			name:    "a repo with no branch does not print a dangling preposition",
			summary: summaryAt(func(s *Summary) { s.Branch = "" }),
			mode:    ModeChannel,
			want:    []string{"in `loop-sessions`"},
			absent:  []string{" on `"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := render(tc.summary, "https://sessions.example.com", tc.mode)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("message does not contain %q:\n%s", w, got)
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(got, a) {
					t.Errorf("message contains %q and should not:\n%s", a, got)
				}
			}
		})
	}
}

// TestDurationUsesTheCoarsestUnitThatIsStillTrue. Seconds of precision on an
// hour-long session is noise, and a duration that reads "0m" for a session that
// took forty seconds is wrong.
func TestDurationUsesTheCoarsestUnitThatIsStillTrue(t *testing.T) {
	start := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"under a minute is reported in seconds", 42 * time.Second, "42s"},
		{"under an hour is reported in minutes", 35 * time.Minute, "35m"},
		{"an hour or more carries both units", 95 * time.Minute, "1h 35m"},
		{"a long run does not lose its hours", 26*time.Hour + 5*time.Minute, "26h 5m"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := duration(Summary{StartedAt: start, EndedAt: start.Add(tc.d)})
			if got != tc.want {
				t.Errorf("duration = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDurationIsOmittedWhenItWouldBeAFiction. A session whose end is unknown or
// precedes its start has no duration, and printing "0s" would assert one.
func TestDurationIsOmittedWhenItWouldBeAFiction(t *testing.T) {
	start := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		s    Summary
	}{
		{"no end time at all", Summary{StartedAt: start}},
		{"an end that precedes the start", Summary{StartedAt: start, EndedAt: start.Add(-time.Hour)}},
		{"an end equal to the start", Summary{StartedAt: start, EndedAt: start}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := duration(tc.s); got != "" {
				t.Errorf("duration = %q, want it omitted", got)
			}
		})
	}
}

// TestAPathologicalRepoNameCannotProduceAMessageSlackRejects. Repo and branch
// arrive from a laptop, and length is the one property nothing upstream bounds.
func TestAPathologicalRepoNameCannotProduceAMessageSlackRejects(t *testing.T) {
	long := strings.Repeat("x", 5000)
	got := render(summaryAt(func(s *Summary) { s.Repo = long; s.Branch = long }), "https://sessions.example.com", ModeChannel)
	if len(got) > 1000 {
		t.Errorf("a message built from two 5000-character names came to %d characters", len(got))
	}
	if !strings.Contains(got, "…") {
		t.Error("the over-long value was not clipped")
	}
}

// TestTheCeilingNoticeExplainsTheSilenceAndAddressesTheRightAudience. A feed
// that goes quiet for a reason nobody can see is indistinguishable from one
// that broke.
func TestTheCeilingNoticeExplainsTheSilenceAndAddressesTheRightAudience(t *testing.T) {
	tests := []struct {
		name string
		mode Mode
		want string
	}{
		{"a DM tells the person it is their own ceiling", ModeDM, "You have"},
		{"a channel does not accuse one reader of it", ModeChannel, "This channel has"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := renderCapNotice("https://sessions.example.com", 50, tc.mode)
			if !strings.Contains(got, tc.want) {
				t.Errorf("notice does not open with %q: %s", tc.want, got)
			}
			if !strings.Contains(got, "50") {
				t.Error("the notice does not say what the ceiling was")
			}
			// The silence is only survivable if the notice says the work is
			// still being captured; otherwise it reads as data loss.
			if !strings.Contains(got, "still captured") {
				t.Errorf("the notice does not say the sessions are still captured: %s", got)
			}
		})
	}
}

// TestClippingNeverProducesInvalidUTF8. A branch name carrying an accented
// character, a CJK word or an emoji would otherwise be cut through the middle
// of a rune, and the message would arrive in a channel rendering as a
// replacement character.
func TestClippingNeverProducesInvalidUTF8(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"a multi-byte rune straddling the bound", strings.Repeat("é", 200)},
		{"three-byte runes", strings.Repeat("世", 200)},
		{"four-byte runes", strings.Repeat("🙂", 200)},
		{"a multi-byte rune at exactly the bound", strings.Repeat("a", 79) + strings.Repeat("é", 20)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := clip(tc.in, 80)
			if !utf8.ValidString(got) {
				t.Errorf("clip produced invalid UTF-8: %q", got)
			}
			if len(got) > 80+len("…") {
				t.Errorf("clip produced %d bytes, want at most %d", len(got), 80+len("…"))
			}
			// And the whole message stays valid, which is what actually
			// reaches Slack.
			msg := render(summaryAt(func(s *Summary) { s.Branch = tc.in }), "https://x.example", ModeChannel)
			if !utf8.ValidString(msg) {
				t.Errorf("the rendered message is not valid UTF-8: %q", msg)
			}
		})
	}
}

// TestPromptWordsSpeakAsThePersonDid: the exchange quotes what the person
// typed as the one normalizer reads it, a slash command as "/name args" and
// a prompt without the harness blocks that rode on it.
func TestPromptWordsSpeakAsThePersonDid(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain ask", "plain ask"},
		{"<command-message>x</command-message>\n<command-name>/mirror</command-name>\n<command-args>review-e2e</command-args>", "/mirror review-e2e"},
		{"<command-message>x</command-message>\n<command-name>/standup</command-name>", "/standup"},
		{"/git --autonomous", "/git --autonomous"},
		{"fix the flaky test\n<system-reminder>\nbe brief\n</system-reminder>", "fix the flaky test"},
	} {
		if got := promptWords(tc.in); got != tc.want {
			t.Errorf("promptWords(%.30q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	// The exchange renderer applies it.
	got := liveTurnText(liveTurn{Kind: "slash_command", Text: "<command-message>m</command-message>\n<command-name>/mirror</command-name>\n<command-args>do the thing</command-args>", Reply: "done"}, "ana@example.org")
	if !strings.Contains(got, "*ana:* /mirror do the thing") || !strings.Contains(got, "*agent:* done") {
		t.Errorf("rendered exchange = %q", got)
	}
}
