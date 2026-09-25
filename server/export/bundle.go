package export

import (
	"bytes"
	"encoding/json"
	"io"
)

// capWriter bounds the large text fields of the event lines in a bundle.
//
// A bundle is the unit an LLM batch job or a jq user takes whole, and one
// tool result can be half a megabyte of build output. The full text stays in
// the events table; the bundle keeps the first EventCap bytes of each large
// field and says which fields it cut, so a reader knows to go to events for
// the rest rather than mistaking the cut for the end of the output.
//
// The cap is applied here, in Go, rather than in the COPY's SQL, because a
// nested jsonb_set per field per row is both slower on the primary and
// harder to test than a function over one line.
type capWriter struct {
	w     io.Writer
	limit int
	buf   bytes.Buffer
}

func newCapWriter(w io.Writer, limit int) *capWriter {
	return &capWriter{w: w, limit: limit}
}

// Write buffers until a newline and passes each complete line through
// capLine.
func (c *capWriter) Write(p []byte) (int, error) {
	c.buf.Write(p)
	for {
		i := bytes.IndexByte(c.buf.Bytes(), '\n')
		if i < 0 {
			return len(p), nil
		}
		line := make([]byte, i+1)
		copy(line, c.buf.Next(i+1))
		if _, err := c.w.Write(capLine(line, c.limit)); err != nil {
			return 0, err
		}
	}
}

// Flush writes a trailing line that had no newline. COPY ends every line
// with one, so this is for completeness.
func (c *capWriter) Flush() error {
	if c.buf.Len() == 0 {
		return nil
	}
	line := c.buf.Bytes()
	c.buf.Reset()
	_, err := c.w.Write(capLine(line, c.limit))
	return err
}

// The fields a bundle caps, as paths into the event line. text is the
// prompt or the answer and is never cut: it is what the bundle is for.
var cappedFields = [][]string{
	{"body", "tool", "output"},
	{"body", "tool", "input"},
	{"body", "tool", "diff", "before"},
	{"body", "tool", "diff", "after"},
	{"body", "tool", "diff", "patch"},
}

// capLine returns the line with each capped field cut to limit bytes and a
// "bundle_truncated" list naming the fields it cut. Lines that are not
// events, lines with nothing over the cap, and lines that do not parse are
// returned as they are: the bundle must never lose a line to the cap.
func capLine(line []byte, limit int) []byte {
	trimmed := bytes.TrimRight(line, "\n")
	if !bytes.Contains(trimmed, []byte(`"record":"event"`)) {
		return line
	}
	var row map[string]any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&row); err != nil || row["record"] != "event" {
		return line
	}
	var cut []string
	for _, path := range cappedFields {
		if name, ok := capField(row, path, limit); ok {
			cut = append(cut, name)
		}
	}
	if len(cut) == 0 {
		return line
	}
	row["bundle_truncated"] = cut
	out, err := json.Marshal(row)
	if err != nil {
		return line
	}
	return append(out, '\n')
}

// capField cuts one path when it is a string longer than limit, or a
// non-string value (tool input is an object) whose encoding is longer than
// limit, in which case the value is replaced by its cut encoding as a
// string. Reports the dotted path it cut.
func capField(row map[string]any, path []string, limit int) (string, bool) {
	cur := any(row)
	for _, key := range path[:len(path)-1] {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = m[key]
		if !ok {
			return "", false
		}
	}
	parent, ok := cur.(map[string]any)
	if !ok {
		return "", false
	}
	last := path[len(path)-1]
	v, ok := parent[last]
	if !ok || v == nil {
		return "", false
	}
	var text string
	switch s := v.(type) {
	case string:
		text = s
	default:
		enc, err := json.Marshal(v)
		if err != nil {
			return "", false
		}
		text = string(enc)
	}
	if len(text) <= limit {
		return "", false
	}
	parent[last] = cutUTF8(text, limit)
	name := ""
	for i, p := range path {
		if i > 0 {
			name += "."
		}
		name += p
	}
	return name, true
}

// cutUTF8 cuts s to at most limit bytes on a rune boundary, so the cut
// never leaves an invalid sequence that json.Marshal would replace.
func cutUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && limit < len(s) && s[limit]&0xC0 == 0x80 {
		limit--
	}
	return s[:limit]
}
