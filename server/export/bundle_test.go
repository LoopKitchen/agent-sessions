package export

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestBundleCapsLargeToolFieldsAndSaysSo: an event line with half a
// megabyte of tool output leaves the bundle with the first EventCap bytes
// and a bundle_truncated list; the prompt text, other lines and lines the
// cap has nothing to do with pass through byte for byte.
func TestBundleCapsLargeToolFieldsAndSaysSo(t *testing.T) {
	big := strings.Repeat("x", 40000)
	lines := []string{
		`{"record":"session","session_id":"s1","first_prompt":"` + strings.Repeat("p", 20000) + `"}`,
		`{"record":"turn","turn_index":0}`,
		`{"record":"event","id":"e1","body":{"text":"` + strings.Repeat("t", 20000) + `","tool":{"name":"Bash","output":"` + big + `","input":{"command":"` + big + `"},"diff":{"before":"` + big + `","after":"short"}}}}`,
		`{"record":"event","id":"e2","body":{"text":"hi"}}`,
		`not json at all`,
	}
	var out bytes.Buffer
	w := newCapWriter(&out, 16*1024)
	// Written in odd chunks, so the line buffering is exercised.
	all := strings.Join(lines, "\n") + "\n"
	for i := 0; i < len(all); i += 7000 {
		end := i + 7000
		if end > len(all) {
			end = len(all)
		}
		if _, err := w.Write([]byte(all[i:end])); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(got) != len(lines) {
		t.Fatalf("%d lines out, want %d", len(got), len(lines))
	}
	for _, i := range []int{0, 1, 3, 4} {
		if got[i] != lines[i] {
			t.Errorf("line %d changed:\n%s", i, got[i])
		}
	}
	var e struct {
		Body struct {
			Text string `json:"text"`
			Tool struct {
				Output string          `json:"output"`
				Input  json.RawMessage `json:"input"`
				Diff   struct {
					Before, After string
				} `json:"diff"`
			} `json:"tool"`
		} `json:"body"`
		Truncated []string `json:"bundle_truncated"`
	}
	if err := json.Unmarshal([]byte(got[2]), &e); err != nil {
		t.Fatalf("capped line is not JSON: %v", err)
	}
	if len(e.Body.Text) != 20000 {
		t.Error("the prompt/answer text was cut; it is what the bundle is for")
	}
	if len(e.Body.Tool.Output) != 16*1024 || len(e.Body.Tool.Diff.Before) != 16*1024 || e.Body.Tool.Diff.After != "short" {
		t.Errorf("output %d, before %d, after %q", len(e.Body.Tool.Output), len(e.Body.Tool.Diff.Before), e.Body.Tool.Diff.After)
	}
	// A non-string field over the cap becomes its cut encoding, as a string.
	var inputText string
	if err := json.Unmarshal(e.Body.Tool.Input, &inputText); err != nil || len(inputText) != 16*1024 {
		t.Errorf("input was not cut to a string of the cap: %v %d", err, len(inputText))
	}
	if strings.Join(e.Truncated, ",") != "body.tool.output,body.tool.input,body.tool.diff.before" {
		t.Errorf("bundle_truncated = %v", e.Truncated)
	}
}

// TestCutUTF8NeverSplitsARune: a cap that lands inside a multi-byte
// character backs up to the boundary, so the line stays valid JSON text.
func TestCutUTF8NeverSplitsARune(t *testing.T) {
	s := strings.Repeat("é", 10) // two bytes each
	if got := cutUTF8(s, 5); got != strings.Repeat("é", 2) {
		t.Errorf("cut at 5 bytes gave %q", got)
	}
	if got := cutUTF8("abc", 10); got != "abc" {
		t.Errorf("short string changed: %q", got)
	}
}
