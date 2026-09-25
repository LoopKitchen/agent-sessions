package web

import (
	"strings"
	"testing"
)

func joined(segs []Segment) (text string, marked []string) {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.Text)
		if s.Match {
			marked = append(marked, s.Text)
		}
	}
	return b.String(), marked
}

// TestHighlightPreservesTheText is the property that matters most: the segments
// must reassemble into exactly the input, because any drift is a byte that
// reaches the browser wrong.
func TestHighlightPreservesTheText(t *testing.T) {
	for _, in := range []string{
		"plain text with no match",
		"a MATCH in the middle",
		"match at the start",
		"ending in a match",
		"日本語のテキスト match here",
		"Ünicode İstanbul ŞCRIPT",
		"",
	} {
		got, _ := joined(Highlight(in, []string{"match", "şcript"}))
		if got != in {
			t.Errorf("Highlight(%q) reassembled as %q", in, got)
		}
	}
}

func TestHighlightIsCaseInsensitive(t *testing.T) {
	_, marked := joined(Highlight("A Match and a MATCH", []string{"match"}))
	if len(marked) != 2 {
		t.Fatalf("marked %v, want both occurrences", marked)
	}
	if marked[0] != "Match" || marked[1] != "MATCH" {
		t.Errorf("marked %v, want the original casing preserved", marked)
	}
}

// TestHighlightHandlesLengthChangingLowercase is the reason the comparison runs
// rune by rune. Lowercasing the whole string first can change its byte length,
// and the resulting index drift would slice a UTF-8 sequence in half.
func TestHighlightHandlesLengthChangingLowercase(t *testing.T) {
	in := "İstanbul deployment"
	got, marked := joined(Highlight(in, []string{"deployment"}))
	if got != in {
		t.Fatalf("text drifted: %q", got)
	}
	if len(marked) != 1 || marked[0] != "deployment" {
		t.Errorf("marked %v", marked)
	}
}

func TestHighlightPrefersTheLongestTerm(t *testing.T) {
	_, marked := joined(Highlight("testing", []string{"test", "testing"}))
	if len(marked) != 1 || marked[0] != "testing" {
		t.Errorf("marked %v, want the whole word", marked)
	}
}

func TestHighlightCapsTheNumberOfMarks(t *testing.T) {
	in := strings.Repeat("a ", 5000)
	_, marked := joined(Highlight(in, []string{"a"}))
	if len(marked) > maxHighlights {
		t.Fatalf("marked %d spans, above the ceiling", len(marked))
	}
	got, _ := joined(Highlight(in, []string{"a"}))
	if got != in {
		t.Error("text was lost once the mark ceiling was reached")
	}
}

func TestQueryTermsKeepsQuotedPhrasesWhole(t *testing.T) {
	got := QueryTerms(`deploy "connection refused" retry`)
	want := []string{"deploy", "connection refused", "retry"}
	if len(got) != len(want) {
		t.Fatalf("terms = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("terms = %v, want %v", got, want)
		}
	}
}

func TestQueryTermsDropsSearchOperators(t *testing.T) {
	for _, in := range []string{"& | ! ( ) :", "  ", `""`} {
		if got := QueryTerms(in); len(got) != 0 {
			t.Errorf("QueryTerms(%q) = %v, want nothing to highlight", in, got)
		}
	}
}

func TestSnippetCentresOnTheMatch(t *testing.T) {
	in := strings.Repeat("filler ", 100) + "NEEDLE" + strings.Repeat(" tail", 100)
	got := Snippet(in, []string{"needle"}, 60)

	if !strings.Contains(got, "NEEDLE") {
		t.Fatalf("snippet %q lost the match", got)
	}
	if len([]rune(got)) > 64 {
		t.Errorf("snippet is %d runes, want roughly the window", len([]rune(got)))
	}
	if !strings.HasPrefix(got, "…") {
		t.Error("a snippet cut from the middle should say so")
	}
}

func TestSnippetLeavesShortTextAlone(t *testing.T) {
	if got := Snippet("short", []string{"x"}, 60); got != "short" {
		t.Errorf("Snippet = %q", got)
	}
}
