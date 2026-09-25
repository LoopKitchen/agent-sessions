package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// cloudLines parses every line the buffer holds. One map per line, because
// the property under test is which keys each line carries, not what one of
// them says.
func cloudLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %v (%s)", err, line)
		}
		out = append(out, entry)
	}
	return out
}

// TestCloudLoggerEmitsTheKeysCloudLoggingPromotes is the logging contract.
//
// Cloud Logging lifts severity, message and timestamp out of a JSON payload
// only under those names. slog's defaults are level, msg and time, and for as
// long as the server wrote those every line arrived with no severity, so a
// filter on severity>=ERROR matched nothing this service ever logged. The
// version is asserted on every line for the same reason it is stamped on
// every line: during a rollout two builds write to the same log.
func TestCloudLoggerEmitsTheKeysCloudLoggingPromotes(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := NewCloudLogger(slog.LevelDebug, &buf, "abc1234")
	log.Debug("d")
	log.Info("i", "k", "v")
	log.Warn("w")
	log.Error("e")
	log.With("component", "ingest").Warn("nested")
	log.WithGroup("g").Info("grouped", "level", "inside-a-group")

	lines := cloudLines(t, &buf)
	if len(lines) != 6 {
		t.Fatalf("got %d lines, want 6:\n%s", len(lines), buf.String())
	}
	wantSeverity := []string{"DEBUG", "INFO", "WARNING", "ERROR", "WARNING", "INFO"}
	wantMessage := []string{"d", "i", "w", "e", "nested", "grouped"}
	for i, entry := range lines {
		for _, key := range []string{"severity", "message", "timestamp", "version"} {
			if _, ok := entry[key]; !ok {
				t.Errorf("line %d lacks %q: %v", i, key, entry)
			}
		}
		for _, key := range []string{"level", "msg", "time"} {
			if _, ok := entry[key]; ok {
				t.Errorf("line %d still carries slog's %q, which Cloud Logging leaves inside jsonPayload: %v", i, key, entry)
			}
		}
		if entry["severity"] != wantSeverity[i] {
			t.Errorf("line %d severity = %v, want %s", i, entry["severity"], wantSeverity[i])
		}
		if entry["message"] != wantMessage[i] {
			t.Errorf("line %d message = %v, want %s", i, entry["message"], wantMessage[i])
		}
		if entry["version"] != "abc1234" {
			t.Errorf("line %d version = %v, want abc1234", i, entry["version"])
		}
	}
	if lines[1]["k"] != "v" {
		t.Errorf("an ordinary attribute did not survive the rename: %v", lines[1])
	}
	if lines[4]["component"] != "ingest" {
		t.Errorf("With() lost its attribute through the wrapper: %v", lines[4])
	}
	// A user attribute called "level" inside a group is that user's own and
	// must not be mistaken for the built-in.
	if g, _ := lines[5]["g"].(map[string]any); g == nil || g["level"] != "inside-a-group" {
		t.Errorf("a grouped attribute named level was renamed or lost: %v", lines[5])
	}
}

// TestCloudLoggerSpellsWarnTheWayCloudDoes pins the one mapping that is not a
// rename. Cloud's LogSeverity enum has WARNING and no WARN; a line carrying
// "WARN" is stored at DEFAULT, which is to say below DEBUG.
func TestCloudLoggerSpellsWarnTheWayCloudDoes(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	NewCloudLogger(slog.LevelWarn, &buf, "v").Warn("careful")
	lines := cloudLines(t, &buf)
	if len(lines) != 1 || lines[0]["severity"] != "WARNING" {
		t.Fatalf("WARN did not become WARNING: %s", buf.String())
	}
	for _, tc := range []struct {
		level slog.Level
		want  string
	}{
		{slog.LevelDebug, "DEBUG"},
		{slog.LevelDebug + 2, "DEBUG"},
		{slog.LevelInfo, "INFO"},
		{slog.LevelInfo + 1, "INFO"},
		{slog.LevelWarn, "WARNING"},
		{slog.LevelError, "ERROR"},
		{slog.LevelError + 4, "ERROR"},
	} {
		if got := cloudSeverity(tc.level); got != tc.want {
			t.Errorf("cloudSeverity(%v) = %s, want %s", tc.level, got, tc.want)
		}
	}
}

// TestCloudLoggerGatesOnTheConfiguredLevel keeps the property NewLogger's own
// test pins: the rename must not defeat the level filter.
func TestCloudLoggerGatesOnTheConfiguredLevel(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := NewCloudLogger(slog.LevelWarn, &buf, "v")
	log.Info("suppressed")
	log.Warn("emitted")
	if lines := cloudLines(t, &buf); len(lines) != 1 || lines[0]["message"] != "emitted" {
		t.Fatalf("got %s, want exactly the warning", buf.String())
	}
}

// TestCloudLoggerLeavesALineWithoutARequestAlone: the trace attributes are for
// lines logged inside a request. A boot line, or a background sweep's line, has
// no trace and must not carry an empty one.
func TestCloudLoggerLeavesALineWithoutARequestAlone(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	log := NewCloudLogger(slog.LevelInfo, &buf, "v")
	log.Info("plain")
	log.InfoContext(context.Background(), "with a bare context")
	for _, entry := range cloudLines(t, &buf) {
		for _, key := range []string{cloudTraceKey, cloudSpanKey, cloudTraceSampledKey} {
			if _, ok := entry[key]; ok {
				t.Errorf("a line logged outside any request carries %q: %v", key, entry)
			}
		}
	}
}

// TestCloudLoggerKeepsTheTraceAtTheTopLevelUnderAGroup: Cloud Logging reads
// the trace keys at the top level of the line only. A component logger built
// with WithGroup would otherwise carry them inside its group, present and
// correlated with nothing, and nothing would fail visibly. Two loggers are
// also derived from one grouped parent, because the handler records its
// derivations in a slice and a shared backing array would give the second
// logger the first one's attributes.
func TestCloudLoggerKeepsTheTraceAtTheTopLevelUnderAGroup(t *testing.T) {
	var buf bytes.Buffer
	log := NewCloudLogger(slog.LevelInfo, &buf, "v9")
	ctx := context.WithValue(context.Background(), traceContextKey{},
		requestTrace{trace: "projects/p/traces/abc", span: "0000000000000001", sampled: true})

	log.With("component", "slack").WithGroup("g").With("k", "v").InfoContext(ctx, "grouped")
	parent := log.WithGroup("outer")
	parent.With("a", 1).InfoContext(ctx, "first")
	parent.With("b", 2).InfoContext(ctx, "second")
	// The same grouped logger without a request: no trace keys anywhere.
	parent.Info("no request")

	lines := cloudLines(t, &buf)
	if len(lines) != 4 {
		t.Fatalf("got %d lines, want 4:\n%s", len(lines), buf.String())
	}
	grouped := lines[0]
	if grouped[cloudTraceKey] != "projects/p/traces/abc" || grouped[cloudSpanKey] != "0000000000000001" || grouped[cloudTraceSampledKey] != true {
		t.Errorf("the trace keys are not at the top level of a grouped logger's line: %v", grouped)
	}
	if grouped["version"] != "v9" || grouped["component"] != "slack" || grouped["message"] != "grouped" {
		t.Errorf("attributes added before the group were lost: %v", grouped)
	}
	g, _ := grouped["g"].(map[string]any)
	if g["k"] != "v" {
		t.Errorf("the group's own attributes were lost: %v", grouped)
	}
	if _, inside := g[cloudTraceKey]; inside {
		t.Errorf("the trace is inside the group as well: %v", grouped)
	}

	first, _ := lines[1]["outer"].(map[string]any)
	second, _ := lines[2]["outer"].(map[string]any)
	if first["a"] != float64(1) || first["b"] != nil {
		t.Errorf("first derived logger wrote %v, want only a=1 in its group", lines[1])
	}
	if second["b"] != float64(2) || second["a"] != nil {
		t.Errorf("second derived logger wrote %v, want only b=2 in its group", lines[2])
	}
	for _, l := range lines[1:3] {
		if l[cloudTraceKey] != "projects/p/traces/abc" {
			t.Errorf("a derived grouped logger lost the top-level trace: %v", l)
		}
	}
	for _, key := range []string{cloudTraceKey, cloudSpanKey, cloudTraceSampledKey} {
		if _, ok := lines[3][key]; ok {
			t.Errorf("a line logged without a request carries %s: %v", key, lines[3])
		}
	}
}
