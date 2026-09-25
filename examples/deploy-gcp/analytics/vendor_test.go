package analytics

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// readSQL returns a file under bigquery/ or fails.
func readSQL(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("bigquery/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestVendorTablesAreShapedLikeTheExportedOnes: the vendor tables are
// loaded by a different path and read through the same views, so they have
// to carry the same access key and the same partitioning discipline. A
// vendor table without viewer_emails is a table the governed views cannot
// filter; one without a partition on the session's start day is a table
// the union views cannot prune.
func TestVendorTablesAreShapedLikeTheExportedOnes(t *testing.T) {
	vendor := readSQL(t, "vendor.sql")
	for table, partition := range map[string]string{
		"vendor_sessions": "PARTITION BY DATE(started_at)",
		"vendor_messages": "PARTITION BY DATE(session_started_at)",
	} {
		head := "CREATE TABLE IF NOT EXISTS `__PROJECT__.__RAW_DATASET__." + table + "` ("
		i := strings.Index(vendor, head)
		if i < 0 {
			t.Errorf("vendor.sql lacks %s", head)
			continue
		}
		body := vendor[i:]
		if end := strings.Index(body[1:], "CREATE TABLE"); end >= 0 {
			body = body[:end+1]
		}
		flat := strings.Join(strings.Fields(body), " ")
		for _, want := range []string{partition, "CLUSTER BY ", "viewer_emails ARRAY<STRING>", "platform STRING NOT NULL", "imported_at"} {
			if !strings.Contains(flat, want) {
				t.Errorf("vendor.sql %s lacks %q", table, want)
			}
		}
	}
	if strings.Contains(vendor, "CREATE TABLE `") {
		t.Error("vendor.sql has a CREATE TABLE without IF NOT EXISTS; provision.sh applies it on every run")
	}
	if strings.Contains(vendor, "__DATASET__.") {
		t.Error("vendor.sql names the governed dataset; the tables live in the raw one")
	}
	// The export rewrites whole day partitions of its own five tables with
	// WRITE_TRUNCATE. A vendor row in one of those tables would be deleted
	// by the next export of its day, so the vendor tables must be their
	// own and must not be named in tables.sql.
	tables := readSQL(t, "tables.sql")
	for _, name := range []string{"vendor_sessions", "vendor_messages"} {
		if strings.Contains(tables, name) {
			t.Errorf("tables.sql declares %s; the export would truncate its partitions", name)
		}
	}
}

// selectOutputNames returns the output column names of one SELECT list, in
// order: the alias after AS where there is one, else the last dotted part.
func selectOutputNames(list string) []string {
	var out []string
	for _, col := range strings.Split(list, ",") {
		col = strings.TrimSpace(strings.Join(strings.Fields(col), " "))
		if col == "" {
			continue
		}
		if i := strings.LastIndex(strings.ToUpper(col), " AS "); i >= 0 {
			col = strings.TrimSpace(col[i+4:])
		}
		if i := strings.LastIndex(col, "."); i >= 0 {
			col = col[i+1:]
		}
		out = append(out, col)
	}
	return out
}

// TestTheUnionViewsLineUpOnBothSides: all_sessions and all_messages are a
// UNION ALL, which lines its two sides up by position and not by name, so
// a column added to one side and not the other silently renames every
// column after it. Both sides must produce the same output names in the
// same order.
//
// It also refuses a literal platform. all_messages carried
// `'claude_code' AS platform` in its first draft, which was wrong for the
// 1,360 codex sessions in the captured corpus; the platform has to be read
// from the session.
func TestTheUnionViewsLineUpOnBothSides(t *testing.T) {
	views := readSQL(t, "views.sql")
	head := regexp.MustCompile("CREATE OR REPLACE VIEW `__PROJECT__\\.__DATASET__\\.(all_[a-z]+)` AS")
	found := 0
	for _, m := range head.FindAllStringSubmatchIndex(views, -1) {
		found++
		name := views[m[2]:m[3]]
		body := views[m[1]:]
		if end := strings.Index(body, "CREATE OR REPLACE VIEW"); end >= 0 {
			body = body[:end]
		}
		halves := strings.Split(body, "UNION ALL")
		if len(halves) != 2 {
			t.Errorf("%s is not one UNION ALL of two selects", name)
			continue
		}
		var names [2][]string
		for i, half := range halves {
			sel := strings.Index(half, "SELECT")
			from := strings.Index(half, "FROM")
			if sel < 0 || from < sel {
				t.Fatalf("%s half %d has no SELECT ... FROM", name, i)
			}
			names[i] = selectOutputNames(half[sel+len("SELECT") : from])
		}
		if strings.Join(names[0], ",") != strings.Join(names[1], ",") {
			t.Errorf("%s: the two sides do not line up:\n captured: %v\n imported: %v", name, names[0], names[1])
		}
		if len(names[0]) == 0 {
			t.Errorf("%s selects nothing", name)
		}
		// The captured half must not hardcode the platform.
		if strings.Contains(halves[0], "'claude_code'") || strings.Contains(halves[0], "'codex'") {
			t.Errorf("%s names a platform as a literal on the captured side; it has to come from the session", name)
		}
	}
	if found != 2 {
		t.Errorf("views.sql defines %d all_* views, want 2 (all_sessions, all_messages)", found)
	}
}

// TestTheImporterRefusesAPlatformItMustNotWrite: --platform is the key the
// import deletes on before it inserts. claude_code and codex are what the
// client captures and the export loads, so importing under either name
// would put a second copy of those sessions in all_sessions; a name
// outside the vocabulary would delete nothing and insert a corpus no view
// knows about.
func TestTheImporterRefusesAPlatformItMustNotWrite(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true binary")
	}
	run := func(args ...string) (string, error) {
		cmd := exec.Command("bash", append([]string{"import-dv-sessions.sh", "--dry-run"}, args...)...)
		cmd.Env = append(os.Environ(), "GCLOUD="+truePath, "BQ="+truePath, "PROJECT=my-project", "REGION=asia-south1", "BUCKET=my-analytics-bucket", "DUMP=gs://my-dump-bucket")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	for _, c := range []struct{ platform, want string }{
		{"claude_code", "is what the client captures"},
		{"codex", "is what the client captures"},
		{"openai", "is not one of the skill_invocations platforms"},
		{"", "is not one of the skill_invocations platforms"},
	} {
		out, err := run("--platform=" + c.platform)
		if err == nil || !strings.Contains(out, c.want) {
			t.Errorf("--platform=%q was accepted: %v\n%s", c.platform, err, out)
		}
	}
	// A dump URI that is not gs:// never reaches gcloud.
	if out, err := run("--dump=/tmp/dump"); err == nil || !strings.Contains(out, "does not look like a gs:// URI") {
		t.Errorf("a local dump path was accepted: %v\n%s", err, out)
	}
}
