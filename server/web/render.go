package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// assets holds the templates and stylesheet inside the binary.
//
// Embedding rather than reading from disk is what lets the container image be
// a single static file with no filesystem layout to get wrong, and it removes
// the class of outage where a deploy ships a binary whose templates did not
// come along with it.
//
//go:embed templates static
var assets embed.FS

// pages maps a template name to the files it needs. Each page is parsed into
// its own set rather than one global set, so two pages can both define a
// "content" block without one silently winning.
var pages = []string{
	"sessions.html",
	"session.html",
	"event.html",
	"artifact.html",
	"analytics.html",
	"skills.html",
	"principals.html",
	"fleet.html",
	"access.html",
	"settings.html",
	"error.html",
}

type renderer struct {
	tpl map[string]*template.Template
	// staticETags fingerprints each embedded asset so the stylesheet can be
	// served immutable and cached forever while still changing the moment a
	// deploy changes it.
	staticETags map[string]string
	log         *slog.Logger
}

func newRenderer(now func() time.Time, log *slog.Logger) (*renderer, error) {
	r := &renderer{
		tpl:         map[string]*template.Template{},
		staticETags: map[string]string{},
		log:         log,
	}
	funcs := templateFuncs(now, r.assetURL)
	for _, p := range pages {
		t, err := template.New("base.html").Funcs(funcs).ParseFS(assets,
			"templates/base.html", "templates/partials.html", "templates/"+p)
		if err != nil {
			return nil, fmt.Errorf("web: parse %s: %w", p, err)
		}
		r.tpl[p] = t
	}

	err := fs.WalkDir(assets, "static", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := assets.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		r.staticETags[strings.TrimPrefix(path, "static/")] = hex.EncodeToString(sum[:])[:12]
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("web: fingerprint static: %w", err)
	}
	return r, nil
}

func (r *renderer) assetURL(name string) string {
	if v, ok := r.staticETags[name]; ok {
		return "/static/" + name + "?v=" + v
	}
	return "/static/" + name
}

// render writes a page, buffering first so a template error that fires halfway
// through produces an error page rather than a half-written one with a 200 on
// it. Truncated HTML with a success status is the failure mode that gets
// reported as "the dashboard is blank" and takes an hour to find.
func (r *renderer) render(w http.ResponseWriter, status int, page string, data any) {
	t, ok := r.tpl[page]
	if !ok {
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		r.log.Error("template execution failed", "page", page, "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

func templateFuncs(now func() time.Time, asset func(string) string) template.FuncMap {
	return template.FuncMap{
		"markdown": markdown,
		"readerURL": func(path string) string {
			u, err := url.Parse(path)
			if err != nil {
				return path
			}
			u.Path += "/conversation"
			return u.String()
		},
		"asset":   asset,
		"elapsed": ElapsedLabel,
		"stamp":   stamp,
		"day":     day,
		"clock":   clock,
		"ago":     func(t time.Time) string { return ago(now(), t) },
		"dur":     durLabel,
		"cost":    costLabel,
		"num":     numLabel,
		"tokens":  tokenLabel,
		"short":   shortID,
		"bytes":   byteLabel,
		// sessionType names a session type the way the pages say it.
		"sessionType": SessionTypeLabel,
		// Version lists render newest first but are numbered oldest first, so
		// the label has to count down from the total. Doing the arithmetic in
		// the template avoids carrying a display-only field on every row.
		"sub": func(a, b int) int { return a - b },
		// Float arithmetic for chart geometry, where the template positions
		// panel children relative to a computed anchor.
		"addf": func(a, b float64) float64 { return a + b },
		"subf": func(a, b float64) float64 { return a - b },
	}
}

// byteLabel renders a file size the way a person reads one.
//
// Exact bytes below a kilobyte because "0.1 KB" is a worse answer than "94 B",
// and one decimal place above it because the difference between 1.2 MB and
// 1.9 MB is the difference between a file somebody will open and one they will
// not.
func byteLabel(n int64) string {
	switch {
	case n < 1024:
		return strconv.FormatInt(n, 10) + " B"
	case n < 1024*1024:
		return strconv.FormatFloat(float64(n)/1024, 'f', 1, 64) + " KB"
	default:
		return strconv.FormatFloat(float64(n)/(1024*1024), 'f', 1, 64) + " MB"
	}
}

// stamp is the machine-precise timestamp used in tooltips, where the reader is
// asking exactly when something happened.
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05 MST")
}

func day(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("Mon 2 Jan")
}

func clock(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Local().Format("15:04")
}

// ago renders relative time, which is what a list reader actually compares on.
// It stops at days rather than continuing into months because a session older
// than a few weeks is read by its date, not by its distance from now.
func ago(now, t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Local().Format("2 Jan")
	}
}

// durLabel formats a span at the precision a reader cares about at that scale.
func durLabel(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm %02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd %dh %02dm", int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}

// costLabel keeps small figures legible. Sessions routinely cost fractions of a
// cent, and rounding every one of them to $0.00 would make the column useless
// for exactly the comparisons people want to make.
func costLabel(v float64) string {
	switch {
	case v <= 0:
		return "—"
	case v < 0.01:
		return fmt.Sprintf("$%.4f", v)
	case v < 100:
		return fmt.Sprintf("$%.2f", v)
	default:
		return "$" + numLabel(int64(v+0.5))
	}
}

// numLabel groups digits. It accepts any integer width because the values it
// formats arrive as both int and int64 depending on whether they are counts or
// token totals, and a template is not the place to convert between them.
func numLabel(v any) string {
	var n int64
	switch t := v.(type) {
	case int:
		n = int64(t)
	case int32:
		n = int64(t)
	case int64:
		n = t
	case float64:
		n = int64(t)
	default:
		return fmt.Sprintf("%v", v)
	}
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

// tokenLabel compresses token counts, which run to eight figures and are read
// as magnitudes rather than as exact values.
//
// It stops at billions rather than at millions because cache reads reach them:
// the deployed corpus has 2.66 billion of them, which "2660.7M" states correctly
// and communicates badly — the reader has to count digits in a number that was
// abbreviated so they would not have to. Ten figures is where a token count
// stops being a magnitude and starts being a wall.
func tokenLabel(n int64) string {
	switch {
	case n <= 0:
		return "0"
	case n < 1000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	case n < 1_000_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	default:
		return fmt.Sprintf("%.2fB", float64(n)/1_000_000_000)
	}
}
