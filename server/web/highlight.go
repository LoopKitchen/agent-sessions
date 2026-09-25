package web

import (
	"strings"
	"unicode"
)

// Segment is a run of transcript text that is either a search match or is not.
//
// Highlighting is the one place a dashboard is tempted to build HTML from user
// content, and transcripts are the worst possible input for that: they contain
// pages the agent fetched, complete with script tags. So the match is expressed
// as data and the <mark> element is emitted by the template around
// still-escaped text. There is no path in this package that produces
// template.HTML from anything a client sent, and the segment type is what makes
// that possible without giving up highlighting.
type Segment struct {
	Text  string
	Match bool
}

// Plain wraps text that has no search terms applied, so templates can render
// every piece of transcript content through the same partial regardless of
// whether the page is a search result.
func Plain(s string) []Segment {
	if s == "" {
		return nil
	}
	return []Segment{{Text: s}}
}

// maxHighlights bounds the number of marks in one field. A query matching a
// common word against a megabyte of tool output would otherwise produce tens of
// thousands of elements, which is a slow page for no added meaning.
const maxHighlights = 200

// Highlight splits text on case-insensitive occurrences of the given terms.
//
// The comparison runs rune by rune against a lowercased copy of equal rune
// length rather than over strings.ToLower output, because lowercasing can
// change a string's byte length for some scripts and the resulting index drift
// would slice UTF-8 sequences in half. Correctness here is not cosmetic: a
// mangled slice boundary lands directly in the HTML.
func Highlight(s string, terms []string) []Segment {
	if s == "" {
		return nil
	}
	needles := prepareTerms(terms)
	if len(needles) == 0 {
		return Plain(s)
	}

	hay := []rune(s)
	lower := make([]rune, len(hay))
	for i, r := range hay {
		lower[i] = unicode.ToLower(r)
	}

	var out []Segment
	var plain []rune
	flush := func() {
		if len(plain) > 0 {
			out = append(out, Segment{Text: string(plain)})
			plain = nil
		}
	}

	marks := 0
	for i := 0; i < len(lower); {
		n := 0
		if marks < maxHighlights {
			n = matchAt(lower, i, needles)
		}
		if n == 0 {
			plain = append(plain, hay[i])
			i++
			continue
		}
		flush()
		out = append(out, Segment{Text: string(hay[i : i+n]), Match: true})
		marks++
		i += n
	}
	flush()
	return out
}

// matchAt returns the length of the longest needle matching at i, or zero.
// Longest wins so that searching for "test" and "testing" against "testing"
// marks the whole word rather than leaving a stray "ing" outside the mark.
func matchAt(lower []rune, i int, needles [][]rune) int {
	best := 0
	for _, n := range needles {
		if len(n) <= best || i+len(n) > len(lower) {
			continue
		}
		ok := true
		for j, r := range n {
			if lower[i+j] != r {
				ok = false
				break
			}
		}
		if ok {
			best = len(n)
		}
	}
	return best
}

func prepareTerms(terms []string) [][]rune {
	var out [][]rune
	seen := map[string]bool{}
	for _, t := range terms {
		t = strings.TrimSpace(t)
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		seen[strings.ToLower(t)] = true
		rs := []rune(t)
		for i, r := range rs {
			rs[i] = unicode.ToLower(r)
		}
		out = append(out, rs)
	}
	return out
}

// QueryTerms splits a search box into the words to highlight.
//
// Quoted phrases stay whole because a user who typed "connection refused"
// wants the phrase marked, not every "connection" on the page. Everything else
// is whitespace-separated, and Postgres operators the query syntax might carry
// are dropped rather than highlighted as literal text.
func QueryTerms(q string) []string {
	var out []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		t := strings.Trim(cur.String(), "&|!():*")
		cur.Reset()
		if len(t) > 0 {
			out = append(out, t)
		}
	}
	for _, r := range q {
		switch {
		case r == '"':
			inQuote = !inQuote
			if !inQuote {
				flush()
			}
		case unicode.IsSpace(r) && !inQuote:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// Snippet trims text to a window around its first match so a search result row
// shows the hit rather than the first line of a 40 KB message.
func Snippet(s string, terms []string, width int) string {
	if width <= 0 || len(s) <= width {
		return s
	}
	rs := []rune(s)
	if len(rs) <= width {
		return s
	}
	start := 0
	if idx := firstMatch(rs, terms); idx > 0 {
		start = idx - width/3
		if start < 0 {
			start = 0
		}
	}
	end := start + width
	if end > len(rs) {
		end = len(rs)
		start = end - width
	}
	out := string(rs[start:end])
	if start > 0 {
		out = "…" + out
	}
	if end < len(rs) {
		out += "…"
	}
	return out
}

func firstMatch(rs []rune, terms []string) int {
	needles := prepareTerms(terms)
	if len(needles) == 0 {
		return -1
	}
	lower := make([]rune, len(rs))
	for i, r := range rs {
		lower[i] = unicode.ToLower(r)
	}
	for i := range lower {
		if matchAt(lower, i, needles) > 0 {
			return i
		}
	}
	return -1
}
