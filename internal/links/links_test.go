package links

import (
	"strings"
	"testing"
)

// The classifications are what the session page renders, so they are pinned
// against the URL shapes that actually appear in the corpus rather than against
// invented ones.
func TestClassification(t *testing.T) {
	for _, tc := range []struct {
		name, text string
		wantKind   Kind
		wantRef    string
		wantURL    string
	}{
		{
			name:     "a pull request, which is the whole reason for this package",
			text:     "opened https://github.com/example-org/example-repo/pull/4 for review",
			wantKind: KindPR, wantRef: "example-org/example-repo#4",
			wantURL: "https://github.com/example-org/example-repo/pull/4",
		},
		{
			name:     "an issue",
			text:     "see https://github.com/example-org/example-repo/issues/17832",
			wantKind: KindIssue, wantRef: "example-org/example-repo#17832",
			wantURL: "https://github.com/example-org/example-repo/issues/17832",
		},
		{
			name:     "a commit, shortened the way people write shas",
			text:     "https://github.com/example-org/example-repo/commit/9e62a9ecafebabe1234",
			wantKind: KindCommit, wantRef: "example-org/example-repo@9e62a9e",
			wantURL: "https://github.com/example-org/example-repo/commit/9e62a9ecafebabe1234",
		},
		{
			name:     "a bare repository",
			text:     "https://github.com/example-org/example-repo",
			wantKind: KindRepo, wantRef: "example-org/example-repo",
			wantURL: "https://github.com/example-org/example-repo",
		},
		{
			name:     "a github path nobody modelled still resolves to its repo",
			text:     "https://github.com/example-org/example-repo/actions/runs/123",
			wantKind: KindRepo, wantRef: "example-org/example-repo",
			wantURL: "https://github.com/example-org/example-repo/actions/runs/123",
		},
		{
			name:     "a notion runbook",
			text:     "SOP at https://www.notion.so/example/SOP-Deploying-000000",
			wantKind: KindDoc, wantURL: "https://www.notion.so/example/SOP-Deploying-000000",
		},
		{
			name:     "a slack permalink",
			text:     "https://example.slack.com/archives/CEXAMPLE01/p1785933659029219",
			wantKind: KindSlack,
			wantURL:  "https://example.slack.com/archives/CEXAMPLE01/p1785933659029219",
		},
		{
			name:     "anything else is still a link worth keeping",
			text:     "https://pkg.go.dev/net/url",
			wantKind: KindOther, wantURL: "https://pkg.go.dev/net/url",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Extract(tc.text)
			if len(got) != 1 {
				t.Fatalf("Extract returned %d links, want 1: %+v", len(got), got)
			}
			if got[0].Kind != tc.wantKind {
				t.Errorf("kind = %q, want %q", got[0].Kind, tc.wantKind)
			}
			if tc.wantRef != "" && got[0].Ref != tc.wantRef {
				t.Errorf("ref = %q, want %q", got[0].Ref, tc.wantRef)
			}
			if got[0].URL != tc.wantURL {
				t.Errorf("url = %q, want %q", got[0].URL, tc.wantURL)
			}
		})
	}
}

// URLs in real transcripts are surrounded by prose and markdown, and a link that
// carries a trailing full stop is a link that 404s when somebody clicks it.
func TestPunctuationAroundAURLIsNotPartOfIt(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"a sentence ending", "merged https://github.com/o/r/pull/4.", "https://github.com/o/r/pull/4"},
		{"a comma in a list", "https://github.com/o/r/pull/4, and more", "https://github.com/o/r/pull/4"},
		{"a markdown link", "[PR](https://github.com/o/r/pull/4)", "https://github.com/o/r/pull/4"},
		{"parenthesised prose", "(see https://github.com/o/r/pull/4)", "https://github.com/o/r/pull/4"},
		{"balanced parens belong to the url", "https://en.wikipedia.org/wiki/Go_(language)", "https://en.wikipedia.org/wiki/Go_(language)"},
		{"a semicolon", "https://github.com/o/r/pull/4;", "https://github.com/o/r/pull/4"},
		{"trailing brackets", "[https://github.com/o/r/pull/4]", "https://github.com/o/r/pull/4"},
		{"quoted", `"https://github.com/o/r/pull/4"`, "https://github.com/o/r/pull/4"},
		{"in backticks", "`https://github.com/o/r/pull/4`", "https://github.com/o/r/pull/4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Extract(tc.text)
			if len(got) != 1 {
				t.Fatalf("Extract returned %d links, want 1: %+v", len(got), got)
			}
			if got[0].URL != tc.want {
				t.Errorf("url = %q, want %q", got[0].URL, tc.want)
			}
		})
	}
}

// A session mentions the same PR many times. The panel should say it once.
func TestTheSameURLIsReportedOnce(t *testing.T) {
	text := `opened https://github.com/o/r/pull/4
	then pushed to https://github.com/o/r/pull/4
	and merged https://github.com/o/r/pull/4`
	got := Extract(text)
	if len(got) != 1 {
		t.Fatalf("Extract returned %d links, want 1: %+v", len(got), got)
	}
}

// Order is the order a reader meets them, so the panel reads as a narrative.
func TestLinksComeBackInTheOrderTheyAppear(t *testing.T) {
	text := "first https://github.com/o/r/issues/1 then https://github.com/o/r/pull/2 then https://notion.so/x"
	got := Extract(text)
	if len(got) != 3 {
		t.Fatalf("got %d links, want 3", len(got))
	}
	if got[0].Kind != KindIssue || got[1].Kind != KindPR || got[2].Kind != KindDoc {
		t.Errorf("order is %q, %q, %q", got[0].Kind, got[1].Kind, got[2].Kind)
	}
}

// A URL longer than a unique btree index can hold would fail the INSERT and take
// the whole ingest batch with it, so it is not a link as far as this package is
// concerned. Transcripts contain base64 and data URIs that start with http.
func TestAnAbsurdlyLongURLIsNotALink(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("a", MaxURL)
	if got := Extract(long); len(got) != 0 {
		t.Fatalf("a %d-byte URL was accepted; it would be rejected by the index", len(long))
	}
	// The boundary itself is still a link: the cap is a limit, not a margin.
	ok := "https://example.com/" + strings.Repeat("a", MaxURL-len("https://example.com/"))
	if len(ok) != MaxURL {
		t.Fatalf("test built a %d-byte URL, meant to build %d", len(ok), MaxURL)
	}
	if got := Extract(ok); len(got) != 1 {
		t.Errorf("a URL of exactly MaxURL was dropped")
	}
}

// Text with no links must cost nothing and find nothing. Most events are this
// case, and it runs on every one of them.
func TestTextWithoutLinks(t *testing.T) {
	for _, s := range []string{
		"", "just some prose about a file",
		"a path like /usr/local/bin is not a link",
		"ftp://example.com/file is not one we follow",
		"mailto:someone@example.com is not one either",
		"http:// on its own is not a url",
	} {
		if got := Extract(s); len(got) != 0 {
			t.Errorf("Extract(%q) found %+v, want nothing", s, got)
		}
	}
}

// Transcripts contain code, and code contains URL-shaped strings. Anything that
// parses as an absolute http URL is kept, but it must not panic or mangle.
func TestOddInputIsHandledRatherThanTrusted(t *testing.T) {
	for _, s := range []string{
		"https://",
		"https://:::::",
		"https://" + strings.Repeat("a", 5000),
		"https://example.com/" + strings.Repeat("%", 100),
		"see https://ex.com/a)b(c",
	} {
		_ = Extract(s) // must not panic
	}
}
