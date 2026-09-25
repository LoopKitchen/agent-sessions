package web

import (
	"bytes"
	"context"
	"html/template"
	"net/http"
	"strings"
	"testing"
	"time"
)

func renderConversation(t *testing.T, name string, data any) string {
	t.Helper()
	tpl, err := template.New("partials.html").Funcs(templateFuncs(time.Now, func(s string) string { return s })).ParseFS(assets, "templates/partials.html")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := tpl.ExecuteTemplate(&out, name, data); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

type inspectorData struct {
	*fakeData
	contentReads int
}

func (f *inspectorData) ArtifactContent(ctx context.Context, v Viewer, id int64, version string) (string, error) {
	f.contentReads++
	return f.fakeData.ArtifactContent(ctx, v, id, version)
}

func TestSessionInspectorLoadsOnlyItsOwnCapturedVersions(t *testing.T) {
	f := &inspectorData{fakeData: artifactFixture(t)}
	f.seedSession("other", "dev@example.com")
	srv, err := New(Options{Data: f, Viewer: func(*http.Request) (Viewer, bool) { return Viewer{Email: "dev@example.com"}, true }, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/sessions/other?file=7", "/sessions/s1?file=7&version=not-captured", "/sessions/s1?file=invalid"} {
		if rec := get(t, srv, path); rec.Code != http.StatusNotFound {
			t.Errorf("%s: got %d", path, rec.Code)
		}
	}
	if f.contentReads != 0 {
		t.Fatalf("read content before validating session and version: %d", f.contentReads)
	}
	f.contents["e1"] = "<script>untrusted</script>"
	rec := get(t, srv, "/sessions/s1?file=7&version=e1&agent=test&hl=find")
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	for _, want := range []string{"Captured file preview", "&lt;script&gt;untrusted&lt;/script&gt;", "file=7", "agent=test", "hl=find", "version=e2", "before the final captured edit"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("missing %s", want)
		}
	}
	if f.contentReads != 1 {
		t.Fatal(f.contentReads)
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'none'") {
		t.Fatal("inspector relaxed CSP")
	}
}

func TestInspectorLinksPreserveConversationWindow(t *testing.T) {
	v := detailView{Session: SessionDetail{Session: Session{ID: "s1"}}, InspectorQuery: map[string][]string{"after": {"cursor"}, "agent": {"worker"}, "hl": {"needle"}}}
	if got := v.FileURL(7, "event"); got != "/sessions/s1?after=cursor&agent=worker&file=7&hl=needle&version=event#inspector-h" {
		t.Fatal(got)
	}
}

func TestConversationMarkdownEscapesUntrustedContent(t *testing.T) {
	out := renderConversation(t, "markdown", Plain("# Summary\n**done** with `code` and *care*\n- first\n> quote\n```html\n<script>alert(1)</script>\n```\n<img src=x onerror=alert(1)>\n\n[bad](javascript:alert) [safe](https://example.com/?a=1&b=2)"))
	for _, want := range []string{"<h2>Summary</h2>", "<strong>done</strong>", "<code>code</code>", "<em>care</em>", "<li>first</li>", "<blockquote><p>quote</p></blockquote>", "&lt;script&gt;", `href="https://example.com/`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	for _, bad := range []string{"<script>", "<img", `href="javascript:`} {
		if strings.Contains(out, bad) {
			t.Errorf("unsafe markup: %s", out)
		}
	}
}

func TestConversationMarkdownPreservesSearchMatches(t *testing.T) {
	out := renderConversation(t, "markdown", Highlight("**search** <img>", []string{"search"}))
	if !strings.Contains(out, "**<mark>search</mark>** &lt;img&gt;") {
		t.Fatal(out)
	}
}

func TestConversationRunsRetainOrderAndCollapseOnlyWork(t *testing.T) {
	g := Group{Blocks: []Block{
		{Seq: 11, Kind: KindPrompt, Text: Plain("request")},
		{Seq: 12, Kind: KindTool, Tool: &ToolView{Name: "read"}},
		{Seq: 13, Kind: KindNotice, Title: "captured"},
		{Seq: 14, Kind: KindAssistant, Text: Plain("answer")},
		{Seq: 15, Kind: KindAssistant, Text: Plain("next answer")},
		{Seq: 16, Kind: KindTool, Failed: true, Tool: &ToolView{Name: "test"}},
	}}
	runs := g.Runs()
	if len(runs) != 5 || !runs[1].Work || len(runs[1].Blocks) != 2 || runs[4].Errors != 1 {
		t.Fatalf("unexpected runs: %+v", runs)
	}
	out := renderConversation(t, "group", g)
	if strings.Count(out, `<details class="work-run">`) != 2 {
		t.Fatal(out)
	}
	last := -1
	for _, id := range []string{`id="e11"`, `id="e12"`, `id="e13"`, `id="e14"`, `id="e15"`, `id="e16"`} {
		idx := strings.Index(out, id)
		if idx <= last {
			t.Fatalf("missing or reordered %s", id)
		}
		last = idx
	}
}

func TestConversationSubagentsCollapsedExceptSearchHits(t *testing.T) {
	g := Group{AgentID: "agent", Blocks: []Block{{Seq: 3, Kind: KindTool, Tool: &ToolView{Name: "read", Output: Plain("needle")}}}}
	out := renderConversation(t, "group", g)
	if !strings.Contains(out, `<details class="grp grp-agent">`) {
		t.Fatal(out)
	}
	g.Blocks[0].Tool.Output = Highlight("needle", []string{"needle"})
	out = renderConversation(t, "group", g)
	if !strings.Contains(out, `<details class="grp grp-agent" open>`) || !strings.Contains(out, `<details class="work-run" open>`) || !strings.Contains(out, `<details class="tool" open>`) {
		t.Fatal(out)
	}
}

// TestMessagePreviewBoundsWithoutRemovingFullContent: a long prompt is
// collapsed behind a bounded preview with the whole text still in the page,
// and a search hit inside it opens the disclosure so the match is visible.
func TestMessagePreviewBoundsWithoutRemovingFullContent(t *testing.T) {
	b := Block{Kind: KindPrompt, Text: Plain(strings.Repeat("Readable message. ", 80) + "END OF ORIGINAL")}
	if !b.LongMessage() {
		t.Fatal("a 1,400-character prompt is not collapsed")
	}
	body := renderConversation(t, "message-body", b)
	if !strings.Contains(body, `class="message-expand"`) || !strings.Contains(body, "END OF ORIGINAL") {
		t.Fatal("the disclosure or the full original is missing")
	}
	if got := len([]rune(b.MessagePreview()[0].Text)); got > 321 {
		t.Errorf("preview is %d runes, want it bounded", got)
	}
	b.Text = Highlight(strings.Repeat("text ", 200)+"needle", []string{"needle"})
	if body := renderConversation(t, "message-body", b); !strings.Contains(body, `class="message-expand" open`) || !strings.Contains(body, "<mark>needle</mark>") {
		t.Fatal("a search hit inside a collapsed message stays hidden")
	}
}
