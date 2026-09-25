// Package analytics holds no Go code; this test guards the provisioning
// assets beside it. provision.sh and scheduler.sh are never run against GCP
// from a test: they are run in --dry-run mode against a recording stub that
// stands in for gcloud and bq, answers the reads the way an empty project
// (or a provisioned one) would, and refuses any write that reaches it. What
// the dry run prints is the contract an operator reads before the real
// apply, so it is what these tests read.
package analytics

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The stub gcloud. Reads are answered per STUB_MODE (absent: nothing exists
// yet; present and complete: everything does); anything else is a write
// that a dry run must never have made, and it fails loudly.
const stubGcloud = `#!/usr/bin/env bash
printf 'gcloud %s\n' "$*" >> "$STUB_LOG"
args=" $* "
exists=0
case "$STUB_MODE" in present|complete|present-wrong-tag|present-no-column|present-show-denied) exists=1 ;; esac
case "$args" in
  *" projects describe "*) echo 123456789012; exit 0 ;;
  *" config get-value account "*) echo owner@example.com; exit 0 ;;
  *" storage buckets describe "*|*" iam service-accounts describe "*|*" run jobs describe "*|*" scheduler jobs describe "*)
    if [ "$exists" = 1 ]; then echo "name: x"; exit 0; fi
    echo "not found" >&2; exit 1 ;;
  *" data-catalog taxonomies list "*)
    if [ "$exists" = 1 ]; then echo "projects/my-project/locations/asia-south1/taxonomies/111"; fi
    exit 0 ;;
  *" data-catalog taxonomies policy-tags list "*)
    if [ "$exists" = 1 ]; then echo "projects/my-project/locations/asia-south1/taxonomies/111/policyTags/222"; fi
    exit 0 ;;
  *) echo "MUTATION reached the stub: $*" >&2; exit 97 ;;
esac
`

// A present dataset answers with what BigQuery attaches to a dataset created
// without an access list (REST reference, Dataset.access: projectReaders
// READER, projectWriters WRITER, projectOwners OWNER, the creator OWNER);
// present-extra adds the export writer entry carrying a field the script
// never writes, the way a later bq could render it. A complete dataset
// answers with the list a first run leaves behind, so a run over it has
// nothing to add and nothing to remove: the raw one with the two owner
// entries, the writer, the admins group and the nine views; the governed
// one with projectReaders, the two owner entries, the two domains and the
// admins group, and no projectWriters. Keys come in an order the script
// never writes and the writer carries the extra field, because that is the
// shape bq's own rendering can take and the script must still see nothing
// to do. A table's schema (`show --schema`) is answered the same way: a
// present table carries no tag on its text columns, a complete one carries
// the tag with its keys in bq's own order, and an absent project has no
// table to read. Three modes stand in for what a real project can hand
// back and the script must not wave through: present-wrong-tag has a stale
// tag on a tagged column and a foreign tag on one the script must not
// touch, present-no-column has lost a column the tag list names, and
// present-show-denied fails the read with something that is not "not
// found".
const stubBQ = `#!/usr/bin/env bash
printf 'bq %s\n' "$*" >> "$STUB_LOG"
sa=loop-sessions-export@my-project.iam.gserviceaccount.com
defaults='{"role":"READER","specialGroup":"projectReaders"},{"role":"WRITER","specialGroup":"projectWriters"},{"role":"OWNER","specialGroup":"projectOwners"},{"role":"OWNER","userByEmail":"owner@example.com"}'
writer_extra="{\"role\":\"WRITER\",\"userByEmail\":\"$sa\",\"iamMember\":\"serviceAccount:$sa\"}"
owners='{"specialGroup":"projectOwners","role":"OWNER"},{"userByEmail":"owner@example.com","role":"OWNER"}'
admins='{"groupByEmail":"loop-sessions-admins@example.com","role":"READER"}'
views=''
for v in turns sessions messages events health_hourly events_latest sessions_latest turns_latest messages_latest vendor_sessions vendor_messages all_sessions all_messages; do
  views="$views,{\"view\":{\"tableId\":\"$v\",\"datasetId\":\"loop_sessions\",\"projectId\":\"my-project\"}}"
done
raw_complete="$owners,$writer_extra,$admins$views"
governed_complete="{\"specialGroup\":\"projectReaders\",\"role\":\"READER\"},$owners,{\"domain\":\"__DOMAINS__\",\"role\":\"READER\"},$admins"
tag=''
if [ "$STUB_MODE" = complete ]; then tag=',"policyTags":{"names":["projects/my-project/locations/asia-south1/taxonomies/111/policyTags/222"]}'; fi
tbl=""
for a in "$@"; do case "$a" in my-project:loop_sessions_raw.*) tbl="${a##*.}" ;; esac; done
case " $* " in
  *" show --schema "*)
    case "$STUB_MODE" in
      present-show-denied) echo "BigQuery error in show operation: Access Denied: Table my-project:loop_sessions_raw.$tbl: Permission bigquery.tables.get denied" >&2; exit 1 ;;
      present|present-extra|complete|present-wrong-tag|present-no-column) ;;
      *) echo "BigQuery error in show operation: Not found: Table my-project:loop_sessions_raw.$tbl" >&2; exit 1 ;;
    esac
    if [ "$STUB_MODE" = present-wrong-tag ] && [ "$tbl" = messages ]; then
      echo '[{"name":"event_id","type":"STRING","mode":"REQUIRED","policyTags":{"names":["projects/my-project/locations/asia-south1/taxonomies/999/policyTags/888"]}},{"type":"STRING","name":"text","policyTags":{"names":["projects/my-project/locations/asia-south1/taxonomies/111/policyTags/333"]}}]'
      exit 0
    fi
    if [ "$STUB_MODE" = present-no-column ] && [ "$tbl" = messages ]; then
      echo '[{"name":"event_id","type":"STRING","mode":"REQUIRED"},{"name":"text_redacted","type":"STRING"}]'
      exit 0
    fi
    case "$tbl" in
      messages) echo "[{\"name\":\"event_id\",\"type\":\"STRING\",\"mode\":\"REQUIRED\"},{\"type\":\"STRING\",\"name\":\"text\"$tag},{\"name\":\"exported_at\",\"type\":\"TIMESTAMP\"}]" ;;
      events) echo "[{\"name\":\"id\",\"type\":\"STRING\",\"mode\":\"REQUIRED\"},{\"type\":\"JSON\",\"name\":\"body\"$tag},{\"name\":\"exported_at\",\"type\":\"TIMESTAMP\"}]" ;;
      sessions) echo "[{\"name\":\"session_id\",\"type\":\"STRING\",\"mode\":\"REQUIRED\"},{\"type\":\"STRING\",\"name\":\"first_prompt\"$tag},{\"type\":\"STRING\",\"name\":\"harness_title\"$tag},{\"name\":\"exported_at\",\"type\":\"TIMESTAMP\"}]" ;;
      vendor_messages) echo "[{\"name\":\"event_id\",\"type\":\"STRING\",\"mode\":\"REQUIRED\"},{\"type\":\"STRING\",\"name\":\"text\"$tag},{\"name\":\"imported_at\",\"type\":\"TIMESTAMP\"}]" ;;
      vendor_sessions) echo "[{\"name\":\"session_id\",\"type\":\"STRING\",\"mode\":\"REQUIRED\"},{\"type\":\"STRING\",\"name\":\"first_prompt\"$tag},{\"type\":\"STRING\",\"name\":\"harness_title\"$tag},{\"name\":\"imported_at\",\"type\":\"TIMESTAMP\"}]" ;;
      *) echo "Not found" >&2; exit 1 ;;
    esac
    exit 0 ;;
  *" show "*)
    case "$STUB_MODE" in
      present|present-wrong-tag|present-no-column|present-show-denied) echo "{\"access\":[$defaults]}"; exit 0 ;;
      present-extra) echo "{\"access\":[$defaults,$writer_extra]}"; exit 0 ;;
      complete)
        case " $* " in
          *":loop_sessions_raw "*) echo "{\"access\":[$raw_complete]}" ;;
          *) echo "{\"access\":[$governed_complete]}" ;;
        esac
        exit 0 ;;
    esac
    echo "Not found" >&2; exit 1 ;;
  *) echo "MUTATION reached the stub: $*" >&2; exit 97 ;;
esac
`

// The accepting stubs stand in for a real apply: every write succeeds and,
// like the real tools, prints something on STDOUT (gcloud dumps the whole
// policy after a binding; bq mk prints "Dataset '...' successfully
// created."), which is exactly what the first live run tripped on. The
// bq stub keeps state under STUB_DIR so a dataset that was created answers
// `show` afterwards, and `update --source` stores the payload it was given
// and refuses one that is not a JSON object with an access list, the way
// the real bq refuses a file it cannot decode. A raw table answers `show
// --schema` once tables.sql has been applied, untagged, and `update
// --schema` stores what it was handed (refusing anything that is not a
// schema array) so the next read returns it.
const stubGcloudAccept = `#!/usr/bin/env bash
printf 'gcloud %s\n' "$*" >> "$STUB_LOG"
args=" $* "
case "$args" in
  *" projects describe "*) echo 123456789012; exit 0 ;;
  *" config get-value account "*) echo owner@example.com; exit 0 ;;
  *" storage buckets describe "*|*" iam service-accounts describe "*|*" run jobs describe "*|*" scheduler jobs describe "*)
    echo "not found" >&2; exit 1 ;;
  *" data-catalog taxonomies list "*)
    if [ -f "$STUB_DIR/taxonomy.imported" ]; then echo "projects/my-project/locations/asia-south1/taxonomies/111"; fi
    exit 0 ;;
  *" data-catalog taxonomies import "*) touch "$STUB_DIR/taxonomy.imported"; echo "Imported taxonomies: 1"; exit 0 ;;
  *" data-catalog taxonomies policy-tags list "*)
    echo "projects/my-project/locations/asia-south1/taxonomies/111/policyTags/222"; exit 0 ;;
  *) echo "bindings:"; echo "- members: [everyone]"; echo "  role: roles/junk"; exit 0 ;;
esac
`

const stubBQAccept = `#!/usr/bin/env bash
printf 'bq %s\n' "$*" >> "$STUB_LOG"
defaults='{"role":"READER","specialGroup":"projectReaders"},{"role":"WRITER","specialGroup":"projectWriters"},{"role":"OWNER","specialGroup":"projectOwners"},{"role":"OWNER","userByEmail":"owner@example.com"}'
ds=""
for a in "$@"; do case "$a" in my-project:*) ds="${a#*:}" ;; esac; done
case " $* " in
  *" show --schema "*)
    if [ -f "$STUB_DIR/$ds.schema.json" ]; then cat "$STUB_DIR/$ds.schema.json"; exit 0; fi
    if [ ! -f "$STUB_DIR/tables.created" ]; then echo "Not found" >&2; exit 1; fi
    case "$ds" in
      loop_sessions_raw.messages) echo '[{"name":"event_id","type":"STRING","mode":"REQUIRED"},{"name":"text","type":"STRING"}]' ;;
      loop_sessions_raw.events) echo '[{"name":"id","type":"STRING","mode":"REQUIRED"},{"name":"body","type":"JSON"}]' ;;
      loop_sessions_raw.sessions) echo '[{"name":"session_id","type":"STRING","mode":"REQUIRED"},{"name":"first_prompt","type":"STRING"},{"name":"harness_title","type":"STRING"}]' ;;
      loop_sessions_raw.vendor_messages) echo '[{"name":"event_id","type":"STRING","mode":"REQUIRED"},{"name":"text","type":"STRING"}]' ;;
      loop_sessions_raw.vendor_sessions) echo '[{"name":"session_id","type":"STRING","mode":"REQUIRED"},{"name":"first_prompt","type":"STRING"},{"name":"harness_title","type":"STRING"}]' ;;
      *) echo "Not found" >&2; exit 1 ;;
    esac
    exit 0 ;;
  *" show "*)
    if [ -f "$STUB_DIR/$ds.json" ]; then cat "$STUB_DIR/$ds.json"; exit 0; fi
    echo "Not found" >&2; exit 1 ;;
  *" update --schema "*)
    src=""
    prev=""
    for a in "$@"; do if [ "$prev" = "--schema" ]; then src="$a"; fi; prev="$a"; done
    if ! jq -e 'type == "array" and length > 0 and all(.[]; has("name") and has("type"))' "$src" >/dev/null 2>&1; then
      echo "BigQuery error in update operation: Error decoding JSON schema from file $src" >&2; exit 1
    fi
    cp "$src" "$STUB_DIR/$ds.schema.json"
    echo "Table 'my-project:$ds' successfully updated."
    exit 0 ;;
  *" mk "*)
    echo "{\"kind\":\"bigquery#dataset\",\"etag\":\"e1\",\"id\":\"my-project:$ds\",\"access\":[$defaults]}" > "$STUB_DIR/$ds.json"
    echo "Dataset 'my-project:$ds' successfully created."
    exit 0 ;;
  *" update "*)
    src=""
    prev=""
    for a in "$@"; do if [ "$prev" = "--source" ]; then src="$a"; fi; prev="$a"; done
    if ! jq -e 'type == "object" and (.access | type == "array")' "$src" >/dev/null 2>&1; then
      echo "BigQuery error in update operation: Error decoding JSON schema from file $src" >&2; exit 1
    fi
    n=$(ls "$STUB_DIR" | grep -c "^$ds.update" || true)
    cp "$src" "$STUB_DIR/$ds.update.$n.json"
    cp "$src" "$STUB_DIR/$ds.json"
    echo "Dataset 'my-project:$ds' successfully updated."
    exit 0 ;;
  *" query "*)
    if grep -q "CREATE TABLE IF NOT EXISTS" -; then touch "$STUB_DIR/tables.created"; fi
    echo "Waiting on bqjob_r1 ... (0s) Current status: DONE"; exit 0 ;;
  *) echo "unexpected bq call: $*" >&2; exit 97 ;;
esac
`

// liveRun applies provision.sh for real against the accepting stubs (no
// --dry-run), so what the helpers capture and what they hand bq is what a
// live run would. It returns the combined output, the stub log and the stub
// state dir, and fails the test if the script did not finish.
// testAdminGrantee and testAdminEmails are what the helpers pass when a
// test does not name its own: --admin-grantee and --admin-emails are
// required arguments with no defaults (provision.sh, "Who the admins
// are"), and every assertion written before they were required expects
// this group. A group principal is not checked against the admins table,
// so the two lists need not agree here.
const (
	testAdminGrantee = "group:loop-sessions-admins@example.com"
	testAdminEmails  = "loop-sessions-admin@example.com"
)

// withAdminFlags appends the required admin arguments unless the caller
// supplied its own.
func withAdminFlags(extra []string) []string {
	var grantee, emails bool
	for _, a := range extra {
		grantee = grantee || strings.HasPrefix(a, "--admin-grantee=")
		emails = emails || strings.HasPrefix(a, "--admin-emails=")
	}
	if !grantee {
		extra = append(extra, "--admin-grantee="+testAdminGrantee)
	}
	if !emails {
		extra = append(extra, "--admin-emails="+testAdminEmails)
	}
	return extra
}

func liveRun(t *testing.T) (out, stubLog, stubDir string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is not available")
	}
	dir := t.TempDir()
	for name, body := range map[string]string{"gcloud": stubGcloudAccept, "bq": stubBQAccept} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "stub.log")
	stubDir = filepath.Join(dir, "state")
	if err := os.MkdirAll(stubDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", append([]string{"provision.sh", "--tag=stub-tag"}, withAdminFlags(nil)...)...)
	cmd.Env = append(os.Environ(),
		"PROJECT=my-project", "REGION=asia-south1", "BUCKET=my-analytics-bucket",
		"GCLOUD="+filepath.Join(dir, "gcloud"),
		"BQ="+filepath.Join(dir, "bq"),
		"STUB_LOG="+log,
		"STUB_DIR="+stubDir,
	)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("provision.sh (live, against stubs) failed: %v\n%s", err, raw)
	}
	logRaw, _ := os.ReadFile(log)
	return string(raw), string(logRaw), stubDir
}

// dryRunRefused is dryRun for a mode the script must refuse: it returns
// what the run printed and fails the test if the script exited 0.
func dryRunRefused(t *testing.T, mode string, extra ...string) string {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	dir := t.TempDir()
	for name, body := range map[string]string{"gcloud": stubGcloud, "bq": stubBQ} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	args := append([]string{"provision.sh", "--dry-run"}, withAdminFlags(extra)...)
	cmd := exec.Command("bash", args...)
	cmd.Env = append(os.Environ(),
		"PROJECT=my-project", "REGION=asia-south1", "BUCKET=my-analytics-bucket",
		"GCLOUD="+filepath.Join(dir, "gcloud"),
		"BQ="+filepath.Join(dir, "bq"),
		"STUB_LOG="+filepath.Join(dir, "stub.log"),
		"STUB_MODE="+mode,
		"RENDER_DIR="+filepath.Join(dir, "rendered"),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("provision.sh --dry-run (%s) succeeded; it must refuse:\n%s", mode, out)
	}
	return string(out)
}

// dryRun runs provision.sh --dry-run with the stubs in the given mode and
// returns what it printed, what the stubs were asked, and the render dir.
func dryRun(t *testing.T, mode string, extra ...string) (stderr, stubLog, renderDir string) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	dir := t.TempDir()
	for name, body := range map[string]string{"gcloud": stubGcloud, "bq": stubBQ} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "stub.log")
	renderDir = filepath.Join(dir, "rendered")
	args := append([]string{"provision.sh", "--dry-run"}, withAdminFlags(extra)...)
	cmd := exec.Command("bash", args...)
	cmd.Env = append(os.Environ(),
		"PROJECT=my-project", "REGION=asia-south1", "BUCKET=my-analytics-bucket",
		"GCLOUD="+filepath.Join(dir, "gcloud"),
		"BQ="+filepath.Join(dir, "bq"),
		"STUB_LOG="+log,
		"STUB_MODE="+mode,
		"RENDER_DIR="+renderDir,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("provision.sh --dry-run (%s) failed: %v\n%s", mode, err, out)
	}
	raw, _ := os.ReadFile(log)
	return string(out), string(raw), renderDir
}

func mustContain(t *testing.T, where, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("%s lacks %q", where, w)
		}
	}
}

// The views the governed dataset holds, in the order views.sql defines
// them; each is authorized on the raw dataset.
// governedViews is every view provision.sh authorizes on the raw dataset,
// read off views.sql. unionViews are the two that carry no rule of their
// own: they select from the governed views above them, so the rule is
// written once and SESSION_USER() is still the querying identity.
var governedViews = []string{"turns", "sessions", "messages", "events", "health_hourly",
	"events_latest", "sessions_latest", "turns_latest", "messages_latest",
	"vendor_sessions", "vendor_messages", "all_sessions", "all_messages"}

var unionViews = []string{"all_sessions", "all_messages"}

// taggedColumns is the column-level access contract: the raw columns that
// carry the pii_text tag (the transcript text and what is derived from it).
var taggedColumns = map[string][]string{
	"messages": {"text"},
	"events":   {"body"},
	"sessions": {"first_prompt", "harness_title"},
}

const stubPolicyTag = "projects/my-project/locations/asia-south1/taxonomies/111/policyTags/222"

// schemaCarriesTagOn reads a schema file provision.sh handed to bq and
// checks that exactly the named columns carry the stub's policy tag and no
// other column carries any, and that the other fields came through as bq
// rendered them (name, type and mode are still there).
func schemaCarriesTagOn(t *testing.T, path string, cols []string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Errorf("schema %s: %v", filepath.Base(path), err)
		return
	}
	var schema []map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil || len(schema) == 0 {
		t.Errorf("schema %s is not a field array: %v\n%s", filepath.Base(path), err, raw)
		return
	}
	want := map[string]bool{}
	for _, c := range cols {
		want[c] = true
	}
	seen := map[string]bool{}
	for _, f := range schema {
		name, _ := f["name"].(string)
		if _, ok := f["type"]; !ok {
			t.Errorf("schema %s: column %s lost its type", filepath.Base(path), name)
		}
		tags, tagged := f["policyTags"]
		if want[name] {
			seen[name] = true
			if !tagged {
				t.Errorf("schema %s: column %s carries no policy tag", filepath.Base(path), name)
				continue
			}
			names, _ := tags.(map[string]any)["names"].([]any)
			if len(names) != 1 || names[0] != stubPolicyTag {
				t.Errorf("schema %s: column %s carries %v, want [%s]", filepath.Base(path), name, tags, stubPolicyTag)
			}
		} else if tagged {
			t.Errorf("schema %s: column %s carries a policy tag it should not: %v", filepath.Base(path), name, tags)
		}
	}
	for c := range want {
		if !seen[c] {
			t.Errorf("schema %s: column %s is missing", filepath.Base(path), c)
		}
	}
}

// accessEntries reads a rendered dataset access file (what `bq update
// --source` would be given) back as its access list.
func accessEntries(t *testing.T, renderDir, dataset string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(renderDir, dataset+".access.json"))
	if err != nil {
		t.Fatalf("rendered access list for %s: %v", dataset, err)
	}
	var doc struct {
		Access []map[string]any `json:"access"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s access file is not JSON: %v\n%s", dataset, err, raw)
	}
	return doc.Access
}

func hasEntry(entries []map[string]any, want string) bool {
	for _, e := range entries {
		b, _ := json.Marshal(e)
		if string(b) == want {
			return true
		}
	}
	return false
}

// specialGroups lists the specialGroup values an access list carries, in
// order; BigQuery's project groups are the only entries of that kind.
func specialGroups(entries []map[string]any) []string {
	var out []string
	for _, e := range entries {
		if g, ok := e["specialGroup"].(string); ok {
			out = append(out, g)
		}
	}
	return out
}

// rawDatasetKeepsOnlyProjectOwners is the review-2 I1 property: the raw
// dataset's rendered list has no project special group but projectOwners
// (projectReaders and projectWriters, which BigQuery attaches at creation,
// are removed), and still carries projectOwners and the creator, which the
// script does not take away.
func rawDatasetKeepsOnlyProjectOwners(t *testing.T, raw []map[string]any) {
	t.Helper()
	for _, g := range specialGroups(raw) {
		if g != "projectOwners" {
			t.Errorf("the raw dataset still carries the special group %s:\n%v", g, raw)
		}
	}
	for _, want := range []string{
		`{"role":"OWNER","specialGroup":"projectOwners"}`,
		`{"role":"OWNER","userByEmail":"owner@example.com"}`,
	} {
		if !hasEntry(raw, want) {
			t.Errorf("the raw dataset lost %s, which the script must keep:\n%v", want, raw)
		}
	}
}

// exportSA is the export job's identity: WRITER on the raw dataset, and the
// one principal the governed dataset must never list.
const exportSA = "loop-sessions-export@my-project.iam.gserviceaccount.com"

// governedDatasetKeepsReadersAndDropsWriters is the review-3 M3 property:
// the governed dataset's rendered list carries no projectWriters entry and
// no WRITER of any kind (a WRITER there can CREATE OR REPLACE a governed
// view without its WHERE, and the raw dataset authorizes a view by its
// name, not its text, so the redefined view would still read every raw
// row), never the export identity, and still carries projectReaders,
// projectOwners and the creator, which the script does not take away.
func governedDatasetKeepsReadersAndDropsWriters(t *testing.T, governed []map[string]any) {
	t.Helper()
	for _, e := range governed {
		switch {
		case e["specialGroup"] == "projectWriters":
			t.Errorf("the governed dataset still carries the special group projectWriters:\n%v", governed)
		case e["role"] == "WRITER" || e["userByEmail"] == exportSA:
			t.Errorf("the governed dataset grants a writer or the export identity: %v", e)
		}
	}
	for _, want := range []string{
		`{"role":"READER","specialGroup":"projectReaders"}`,
		`{"role":"OWNER","specialGroup":"projectOwners"}`,
		`{"role":"OWNER","userByEmail":"owner@example.com"}`,
	} {
		if !hasEntry(governed, want) {
			t.Errorf("the governed dataset lost %s, which the script must keep:\n%v", want, governed)
		}
	}
}

// stubsSawReadsOnly fails on any call to the stubs that is not a read. The
// stubs refuse a write with their own MUTATION line; this is the second net,
// for a verb neither side anticipated.
func stubsSawReadsOnly(t *testing.T, stubs string) {
	t.Helper()
	if strings.Contains(stubs, "MUTATION") {
		t.Errorf("a write reached the stub during a dry run:\n%s", stubs)
	}
	for _, call := range strings.Split(strings.TrimSpace(stubs), "\n") {
		if call == "" {
			continue
		}
		read := false
		for _, verb := range []string{" describe ", " list ", " show ", " print-access-token", " config get-value "} {
			if strings.Contains(call+" ", verb) {
				read = true
			}
		}
		if !read {
			t.Errorf("the dry run made a call that is not a read: %s", call)
		}
	}
}

// TestDryRunOnAnEmptyProjectPrintsEveryCreateAndMutatesNothing is the
// contract F(d) list, read off the dry run: bucket, the two datasets and
// who reads them, taxonomy and tag, service account and its bindings (and
// the one it must not get), the DDL in dependency order, the views
// authorized on the raw dataset once they exist, the Cloud Run job, the
// schedule. And the stubs saw reads only.
func TestDryRunOnAnEmptyProjectPrintsEveryCreateAndMutatesNothing(t *testing.T) {
	out, stubs, renderDir := dryRun(t, "absent")
	sa := exportSA
	mustContain(t, "dry run", out,
		"bucket: create",
		"dry-run: gcloud storage buckets create gs://my-analytics-bucket --location=asia-south1 --uniform-bucket-level-access --public-access-prevention",
		"dry-run: gcloud storage buckets update gs://my-analytics-bucket --versioning",
		"service account: create",
		"dry-run: gcloud iam service-accounts create loop-sessions-export",
		"dry-run: gcloud storage buckets add-iam-policy-binding gs://my-analytics-bucket --member=serviceAccount:"+sa+" --role=roles/storage.objectAdmin",
		"--member=serviceAccount:"+sa+" --role=roles/bigquery.jobUser",
		"--member=serviceAccount:"+sa+" --role=roles/cloudsql.client",
		"--member=serviceAccount:"+sa+" --role=roles/logging.logWriter",
		"dry-run: gcloud secrets add-iam-policy-binding loop-sessions-db-password --member=serviceAccount:"+sa+" --role=roles/secretmanager.secretAccessor",
		"dataset loop_sessions_raw: create",
		"dry-run: bq --project_id=my-project --location=asia-south1 mk --dataset --description=loop-sessions analytics export, raw tables",
		"my-project:loop_sessions_raw",
		"dataset loop_sessions_raw access: add 2 entries, remove 2 entries",
		"dry-run: bq --project_id=my-project --location=asia-south1 update --source",
		"dataset loop_sessions: create",
		"dry-run: bq --project_id=my-project --location=asia-south1 mk --dataset --description=loop-sessions analytics export, governed views over loop_sessions_raw",
		"dataset loop_sessions access: add 2 entries, remove 1 entry",
		"taxonomy: import analytics/taxonomy.json",
		"dry-run: gcloud data-catalog taxonomies import",
		"--location=asia-south1",
		// Short taxonomy id on the per-tag command: the full resource name
		// 404s there (doubled path), which is where the first live run died.
		"policy-tags add-iam-policy-binding dry-run-policy-tag --taxonomy=dry-run-taxonomy --location=asia-south1 --member=group:loop-sessions-admins@example.com --role=roles/datacatalog.categoryFineGrainedReader",
		"ddl tables.sql: apply",
		"ddl admins.sql: apply",
		"ddl views.sql: apply",
		"query --use_legacy_sql=false --nouse_cache <",
		"== column tags on loop_sessions_raw (pii_text)",
		"table messages: no schema to read yet; a real run tags text once the DDL has run",
		"dry-run: bq --project_id=my-project --location=asia-south1 update --schema <messages's schema with pii_text on text> my-project:loop_sessions_raw.messages",
		"table events: no schema to read yet; a real run tags body once the DDL has run",
		"dry-run: bq --project_id=my-project --location=asia-south1 update --schema <events's schema with pii_text on body> my-project:loop_sessions_raw.events",
		"table sessions: no schema to read yet; a real run tags first_prompt harness_title once the DDL has run",
		"dry-run: bq --project_id=my-project --location=asia-south1 update --schema <sessions's schema with pii_text on first_prompt harness_title> my-project:loop_sessions_raw.sessions",
		"== authorized views on loop_sessions_raw",
		"views: "+strings.Join(governedViews, " "),
		"dataset loop_sessions_raw access: add 13 entries",
		"job: create with image tag dry-run-tag",
		"dry-run: gcloud run jobs replace",
		"--region=asia-south1",
		"dry-run: gcloud run jobs add-iam-policy-binding loop-sessions-export --region=asia-south1 --member=serviceAccount:"+sa+" --role=roles/run.invoker",
		"--member=serviceAccount:service-123456789012@gcp-sa-cloudscheduler.iam.gserviceaccount.com --role=roles/cloudscheduler.serviceAgent",
		"scheduler job loop-sessions-export-hourly: create",
		"dry-run: gcloud scheduler jobs create http loop-sessions-export-hourly --location=asia-south1 --schedule=7 * * * * --time-zone=Etc/UTC --uri=https://run.googleapis.com/v2/projects/my-project/locations/asia-south1/jobs/loop-sessions-export:run --http-method=POST --oauth-service-account-email="+sa,
		"dry run complete; nothing was changed",
	)
	// The bindings the export identity must never get: Fine-Grained Reader
	// anywhere, and anything on the governed dataset.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "categoryFineGrainedReader") && strings.Contains(line, "serviceAccount:"+sa) {
			t.Errorf("the export service account is granted Fine-Grained Reader: %s", line)
		}
	}
	if strings.Contains(out, "MUTATION") {
		t.Errorf("a write reached the stub during a dry run:\n%s", stubs)
	}
	// Every project-level binding carries --condition=None. The project's IAM
	// policy holds conditional bindings, and gcloud refuses to add an
	// unconditional one in non-interactive mode without the flag; the first
	// live run stopped at the jobUser binding for exactly that.
	projectBindings := 0
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "gcloud projects add-iam-policy-binding") {
			continue
		}
		projectBindings++
		if !strings.Contains(line, "--condition=None") {
			t.Errorf("project binding without --condition=None: %s", line)
		}
	}
	if projectBindings != 4 {
		t.Errorf("expected 4 project-level bindings (3 export roles + the scheduler agent), saw %d", projectBindings)
	}
	stubsSawReadsOnly(t, stubs)
	// DDL order: tables, then the admins list, then the views over both,
	// then the tags on the tables; the views are authorized only after they
	// exist.
	order := []string{"ddl tables.sql", "ddl vendor.sql", "ddl admins.sql", "ddl views.sql", "== column tags on loop_sessions_raw", "== authorized views on loop_sessions_raw", "== cloud run job"}
	for i := 1; i < len(order); i++ {
		if a, b := strings.Index(out, order[i-1]), strings.Index(out, order[i]); a < 0 || b < 0 || a > b {
			t.Errorf("%q does not come before %q", order[i-1], order[i])
		}
	}

	// What the two access lists would be written as. The raw dataset, as a
	// fresh create leaves it (the dry run models BigQuery's four default
	// entries): the service account writes, the admins group reads, the
	// nine views are authorized, projectOwners and the creator stay, the
	// projectReaders and projectWriters groups are gone, and nobody else
	// appears. The governed dataset: the two Workspace domains and the
	// admins group read; projectReaders and the two owner entries stay (it
	// holds views only, each of which filters rows; review-2 I1, lead
	// decision); projectWriters is gone (review-3 M3); the service account
	// is absent; and nothing else appears.
	raw := accessEntries(t, renderDir, "loop_sessions_raw")
	for _, want := range []string{
		`{"role":"WRITER","userByEmail":"` + sa + `"}`,
		`{"groupByEmail":"loop-sessions-admins@example.com","role":"READER"}`,
	} {
		if !hasEntry(raw, want) {
			t.Errorf("raw dataset access lacks %s:\n%v", want, raw)
		}
	}
	for _, v := range governedViews {
		if !hasEntry(raw, `{"view":{"datasetId":"loop_sessions","projectId":"my-project","tableId":"`+v+`"}}`) {
			t.Errorf("view %s is not authorized on the raw dataset:\n%v", v, raw)
		}
	}
	rawDatasetKeepsOnlyProjectOwners(t, raw)
	if want := 4 + len(governedViews); len(raw) != want {
		t.Errorf("raw dataset access has %d entries, want %d (the writer, the admins, projectOwners, the creator and %d views):\n%v", len(raw), want, len(governedViews), raw)
	}
	for _, e := range raw {
		if e["domain"] != nil {
			t.Errorf("the raw dataset is readable by a whole domain: %v", e)
		}
	}
	governed := accessEntries(t, renderDir, "loop_sessions")
	for _, want := range []string{
		`{"domain":"__DOMAINS__","role":"READER"}`,
		`{"groupByEmail":"loop-sessions-admins@example.com","role":"READER"}`,
	} {
		if !hasEntry(governed, want) {
			t.Errorf("governed dataset access lacks %s:\n%v", want, governed)
		}
	}
	governedDatasetKeepsReadersAndDropsWriters(t, governed)
	if want := 5; len(governed) != want {
		t.Errorf("governed dataset access has %d entries, want %d (the one Workspace domain, the admins group, projectReaders, projectOwners and the creator):\n%v", len(governed), want, governed)
	}
	// Only a Workspace domain of this organisation may appear. BigQuery
	// accepts any other domain entry, reports success and stores nothing, so
	// a second domain here would be a grant that silently never exists.
	for _, e := range governed {
		if d, ok := e["domain"].(string); ok && d != "__DOMAINS__" {
			t.Errorf("the governed dataset grants domain %q, which is not a Workspace domain of this organisation and would be dropped in silence", d)
		}
	}
}

// TestDryRunOnAProvisionedProjectTakesTheUpdatePathsAndIsRepeatable: with
// everything present, nothing is created, the dataset gains the WRITER
// entry it lacks, the job is replaced and the schedule updated; and two
// runs print the same thing, which is what idempotent means for a script
// whose writes are printed.
func TestDryRunOnAProvisionedProjectTakesTheUpdatePathsAndIsRepeatable(t *testing.T) {
	out, _, renderDir := dryRun(t, "present", "--tag=abc1234", "--admin-emails=alice@example.com,bob@example.com")
	mustContain(t, "dry run (present)", out,
		"bucket: exists",
		"service account: exists",
		"dataset loop_sessions_raw: exists",
		"dataset loop_sessions_raw access: add 2 entries, remove 2 entries",
		"dry-run: bq --project_id=my-project --location=asia-south1 update --source",
		"dataset loop_sessions: exists",
		"dataset loop_sessions access: add 2 entries, remove 1 entry",
		"taxonomy: exists (projects/my-project/locations/asia-south1/taxonomies/111)",
		"policy tag: projects/my-project/locations/asia-south1/taxonomies/111/policyTags/222",
		"dataset loop_sessions_raw access: add 13 entries",
		"job: exists, replace with image tag abc1234",
		"scheduler job loop-sessions-export-hourly: update",
		"dry-run: gcloud scheduler jobs update http loop-sessions-export-hourly",
	)
	for _, absent := range []string{"mk --dataset", "taxonomies import", "buckets create", "service-accounts create", "scheduler jobs create"} {
		if strings.Contains(out, absent) {
			t.Errorf("a provisioned project still gets %q", absent)
		}
	}
	// The rendered files carry no placeholder, the real tag name and the
	// real image tag.
	for _, f := range []string{"tables.sql", "vendor.sql", "admins.sql", "views.sql", "job.yaml"} {
		raw, err := os.ReadFile(filepath.Join(renderDir, f))
		if err != nil {
			t.Fatalf("rendered %s: %v", f, err)
		}
		if m := regexp.MustCompile(`__[A-Z_]+__`).Find(raw); m != nil {
			t.Errorf("rendered %s still carries %s", f, m)
		}
		if !strings.Contains(string(raw), "my-project") {
			t.Errorf("rendered %s does not name the project", f)
		}
	}
	// The tables exist and their text columns carry no tag yet, so each of
	// the three gets its schema read back and written with the tag on
	// exactly the named columns; every other column is handed back as bq
	// rendered it.
	mustContain(t, "dry run (present)", out,
		"== column tags on loop_sessions_raw (pii_text)",
		"table messages: tag text",
		"table events: tag body",
		"table sessions: tag first_prompt harness_title",
		"update --schema ",
		"my-project:loop_sessions_raw.messages",
		"my-project:loop_sessions_raw.events",
		"my-project:loop_sessions_raw.sessions")
	for table, cols := range taggedColumns {
		schemaCarriesTagOn(t, filepath.Join(renderDir, table+".schema.json"), cols)
	}
	// The admins list is exactly the flag, quoted, in the raw dataset.
	admins, _ := os.ReadFile(filepath.Join(renderDir, "admins.sql"))
	mustContain(t, "rendered admins.sql", string(admins),
		"CREATE OR REPLACE TABLE `my-project.loop_sessions_raw.admins`",
		"UNNEST(['alice@example.com', 'bob@example.com'])")
	// The views live in the governed dataset, read the raw one, and carry
	// the rule.
	views, _ := os.ReadFile(filepath.Join(renderDir, "views.sql"))
	mustContain(t, "rendered views.sql", string(views),
		"CREATE OR REPLACE VIEW `my-project.loop_sessions.turns` AS",
		"FROM `my-project.loop_sessions_raw.turns`",
		"SESSION_USER() IN (SELECT email FROM `my-project.loop_sessions_raw.admins`)")
	if strings.Contains(string(views), "CREATE OR REPLACE VIEW `my-project.loop_sessions_raw.") {
		t.Error("a view is created in the raw dataset")
	}
	job, _ := os.ReadFile(filepath.Join(renderDir, "job.yaml"))
	mustContain(t, "rendered job.yaml", string(job),
		"image: asia-south1-docker.pkg.dev/my-project/loop-sessions/server:abc1234",
		"run.googleapis.com/cloudsql-instances: my-project:asia-south1:loop-sessions",
		"value: /cloudsql/my-project:asia-south1:loop-sessions",
		"value: my-analytics-bucket",
		"value: loop_sessions_raw")
	if m := regexp.MustCompile(`name: EXPORT_DATASET\s+value: (\S+)`).FindStringSubmatch(string(job)); m == nil || m[1] != "loop_sessions_raw" {
		t.Errorf("job.yaml points EXPORT_DATASET at %v; it must load the raw dataset", m)
	}
	// The access lists on a provisioned project keep what was there (the
	// owners; projectReaders on the governed dataset), add what is missing,
	// take the two project groups off the raw dataset and projectWriters
	// off the governed one.
	raw := accessEntries(t, renderDir, "loop_sessions_raw")
	rawDatasetKeepsOnlyProjectOwners(t, raw)
	if !hasEntry(raw, `{"view":{"datasetId":"loop_sessions","projectId":"my-project","tableId":"messages_latest"}}`) {
		t.Errorf("messages_latest is not authorized on the raw dataset:\n%v", raw)
	}
	governedDatasetKeepsReadersAndDropsWriters(t, accessEntries(t, renderDir, "loop_sessions"))

	again, _, _ := dryRun(t, "present", "--tag=abc1234", "--admin-emails=alice@example.com,bob@example.com")
	strip := func(s string) string {
		// The render dir, the temp dir the dataset JSON is written to and
		// their paths differ per run; nothing else may.
		return regexp.MustCompile(`(?m)^\s+rendered: .*$|Rendered files: .*$|--source \S+|--schema \S+|< \S+|replace \S+`).ReplaceAllString(s, "")
	}
	if a, b := strip(out), strip(again); a != b {
		al, bl := strings.Split(a, "\n"), strings.Split(b, "\n")
		for i := 0; i < len(al) || i < len(bl); i++ {
			var x, y string
			if i < len(al) {
				x = al[i]
			}
			if i < len(bl) {
				y = bl[i]
			}
			if x != y {
				t.Errorf("two dry runs differ at line %d:\n first: %s\nsecond: %s", i+1, x, y)
				break
			}
		}
	}
}

// TestAnAccessEntryWithExtraFieldsCountsAsPresent (review-2 M3): the script
// decides "already there" by the fields it wants, not by whole-entry
// equality, so a bq that one day renders an entry with an added field does
// not make every run append a duplicate. The entry is kept as bq gave it.
func TestAnAccessEntryWithExtraFieldsCountsAsPresent(t *testing.T) {
	out, _, renderDir := dryRun(t, "present-extra")
	sa := exportSA
	mustContain(t, "dry run (present-extra)", out,
		"dataset loop_sessions_raw access: add 1 entry, remove 2 entries")
	raw := accessEntries(t, renderDir, "loop_sessions_raw")
	writers := 0
	for _, e := range raw {
		if e["userByEmail"] == sa {
			writers++
			if e["role"] != "WRITER" || e["iamMember"] == nil {
				t.Errorf("the writer entry was rewritten rather than kept as bq rendered it: %v", e)
			}
		}
	}
	if writers != 1 {
		t.Errorf("the raw dataset carries %d entries for the export identity, want the one bq already had:\n%v", writers, raw)
	}
	rawDatasetKeepsOnlyProjectOwners(t, raw)
}

// TestASecondRunOverCompleteAccessListsWritesNothing (review-3 M2): with
// both datasets carrying exactly what a first run leaves, a re-run (the one
// an operator makes after every deploy, for the image tag) logs "access:
// complete" for the raw dataset twice (phase 3 and the view authorization)
// and for the governed dataset once, issues no `bq update --source`,
// renders no access list, and asks the stubs for reads only. This is the
// property "idempotent on a second run" rests on; the present modes cannot
// show it, since they always leave the script something to add or remove.
func TestASecondRunOverCompleteAccessListsWritesNothing(t *testing.T) {
	out, stubs, renderDir := dryRun(t, "complete", "--tag=abc1234")
	if n := strings.Count(out, "dataset loop_sessions_raw access: complete"); n != 2 {
		t.Errorf("the raw dataset's access was reported complete %d times, want 2 (phase 3 and the view authorization):\n%s", n, out)
	}
	if n := strings.Count(out, "dataset loop_sessions access: complete"); n != 1 {
		t.Errorf("the governed dataset's access was reported complete %d times, want 1:\n%s", n, out)
	}
	rewrite := regexp.MustCompile(`update --source|update --schema|access: (add|remove)|table [a-z_]+: tag `)
	for _, line := range strings.Split(out, "\n") {
		if rewrite.MatchString(line) {
			t.Errorf("a complete access list was rewritten: %s", line)
		}
	}
	if files, _ := filepath.Glob(filepath.Join(renderDir, "*.access.json")); len(files) != 0 {
		t.Errorf("a complete access list was rendered: %v", files)
	}
	// The tags are on the columns already (with bq's own key order and the
	// tag object in the shape it renders), so no schema is written or
	// rendered.
	mustContain(t, "dry run (complete)", out,
		"table messages: text tagged",
		"table events: body tagged",
		"table sessions: first_prompt harness_title tagged")
	if files, _ := filepath.Glob(filepath.Join(renderDir, "*.schema.json")); len(files) != 0 {
		t.Errorf("a tagged schema was rendered again: %v", files)
	}
	// The rest of the run is what it is on any run: the DDL re-applied, the
	// job replaced with the tag.
	mustContain(t, "dry run (complete)", out,
		"dataset loop_sessions_raw: exists",
		"dataset loop_sessions: exists",
		"ddl views.sql: apply",
		"job: exists, replace with image tag abc1234",
		"dry run complete; nothing was changed")
	stubsSawReadsOnly(t, stubs)
}

// TestColumnTagsAreCorrectedAndABadSchemaIsRefused covers what the tagging
// step must do to a table that is neither untagged nor already right: a
// stale tag on a tagged column is replaced, a tag on a column the script
// does not name is left alone (it is somebody else's decision, and
// dropping it would widen access silently), a column the tag list names
// and the table lacks stops the run rather than tagging nothing, and a
// failed schema read that is not "not found" is a failure rather than an
// absent table.
func TestColumnTagsAreCorrectedAndABadSchemaIsRefused(t *testing.T) {
	out, _, renderDir := dryRun(t, "present-wrong-tag", "--tag=abc1234")
	mustContain(t, "dry run (present-wrong-tag)", out, "table messages: tag text")
	raw, err := os.ReadFile(filepath.Join(renderDir, "messages.schema.json"))
	if err != nil {
		t.Fatalf("rendered messages schema: %v", err)
	}
	var schema []map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("rendered messages schema is not a field array: %v\n%s", err, raw)
	}
	for _, f := range schema {
		names, _ := f["policyTags"].(map[string]any)["names"].([]any)
		switch f["name"] {
		case "text":
			if len(names) != 1 || names[0] != stubPolicyTag {
				t.Errorf("the stale tag on messages.text was not replaced: %v", f)
			}
		case "event_id":
			if len(names) != 1 || !strings.HasSuffix(names[0].(string), "/policyTags/888") {
				t.Errorf("the tag on a column the script does not name was not left alone: %v", f)
			}
		}
	}

	refused := dryRunRefused(t, "present-no-column", "--tag=abc1234")
	mustContain(t, "dry run (present-no-column)", refused,
		"column text is not in the schema")
	denied := dryRunRefused(t, "present-show-denied", "--tag=abc1234")
	mustContain(t, "dry run (present-show-denied)", denied,
		"cannot read the schema", "Permission bigquery.tables.get denied")
	if strings.Contains(denied, "no schema to read yet") {
		t.Error("a denied schema read was reported as a table that does not exist yet")
	}
}

// TestProvisionRefusesWhatItCannotRenderSafely: a real run needs the image
// tag; a value that would corrupt a sed replacement is refused up front.
func TestProvisionRefusesWhatItCannotRenderSafely(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	truePath, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true binary")
	}
	// A gcloud and a bq that exist and do nothing: the checks under test
	// run before either is called.
	inert := []string{"GCLOUD=" + truePath, "BQ=" + truePath, "PROJECT=my-project", "REGION=asia-south1", "BUCKET=my-analytics-bucket"}
	run := func(env []string, args ...string) (string, error) {
		cmd := exec.Command("bash", append([]string{"provision.sh"}, args...)...)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	admin := withAdminFlags(nil)
	// Without --dry-run and without --tag, before touching gcloud at all.
	out, err := run(inert, admin...)
	if err == nil || !strings.Contains(out, "--tag=<image sha> is required") {
		t.Errorf("a real run without a tag was accepted: %v\n%s", err, out)
	}
	out, err = run(inert, append([]string{"--dry-run", "--project=bad|project"}, admin...)...)
	if err == nil || !strings.Contains(out, "does not look like a project id") {
		t.Errorf("a project id with a pipe was accepted: %v\n%s", err, out)
	}
	out, err = run(inert, "--dry-run", "--admin-grantee=everyone", "--admin-emails="+testAdminEmails)
	if err == nil || !strings.Contains(out, "does not look like a comma-separated list of IAM members") {
		t.Errorf("a bare grantee was accepted: %v\n%s", err, out)
	}
	// The admins list is spliced into SQL as quoted literals: only email
	// addresses, comma-separated, nothing a quote or a semicolon could ride
	// in on.
	out, err = run(inert, "--dry-run", "--admin-grantee="+testAdminGrantee, "--admin-emails=alice@example.com,'); DROP TABLE x; --")
	if err == nil || !strings.Contains(out, "does not look like a comma-separated list of email addresses") {
		t.Errorf("a non-address admin email was accepted: %v\n%s", err, out)
	}
	// Both admin arguments are required and have no default: the old
	// defaults named a group that does not exist and an admins list of one
	// placeholder address, so a bare run died mid-way with IAM already
	// written, and would have replaced the live admins table had the group
	// resolved. Neither is a default any more.
	out, err = run(inert, "--dry-run")
	if err == nil || !strings.Contains(out, "--admin-grantee=<members> is required and has no default") {
		t.Errorf("a run with no admin grantee was accepted: %v\n%s", err, out)
	}
	out, err = run(inert, "--dry-run", "--admin-grantee="+testAdminGrantee)
	if err == nil || !strings.Contains(out, "--admin-emails=<addresses> is required and has no default") {
		t.Errorf("a run with no admin emails was accepted: %v\n%s", err, out)
	}
	// A user granted the datasets and left out of the admins table reads
	// the governed views as an ordinary user: a half-applied grant that
	// looks like a permissions bug, so the script refuses it.
	out, err = run(inert, "--dry-run", "--admin-grantee=user:alice@example.com,user:bob@example.com", "--admin-emails=alice@example.com")
	if err == nil || !strings.Contains(out, "--admin-grantee names bob@example.com but --admin-emails does not") {
		t.Errorf("a grantee missing from the admins table was accepted: %v\n%s", err, out)
	}
	// A group has no single address to match SESSION_USER() against, so it
	// is not checked against the admins list.
	if _, err = run(inert, "--dry-run", "--admin-grantee=group:g@example.com", "--admin-emails=alice@example.com", "--project=bad|project"); err == nil {
		t.Error("the shape checks stopped running after a group grantee")
	}
	// The views cannot live in the dataset they hide.
	out, err = run(inert, "--dry-run", "--raw-dataset=loop_sessions")
	if err == nil || !strings.Contains(out, "must differ") {
		t.Errorf("the raw dataset was allowed to be the governed one: %v\n%s", err, out)
	}
	out, err = run(nil, "--dry-run", "--bogus")
	if err == nil || !strings.Contains(out, "unknown argument") {
		t.Errorf("an unknown flag was accepted: %v\n%s", err, out)
	}
}

// TestEveryAdminGranteeReachesBothDatasetsAndTheTag: --admin-grantee is a
// list, and each principal in it has to land in three places or the grant
// is half applied. It was a single member for a while, which is why the
// people who actually held this access were built up by repeated
// runs and nothing checked that the three surfaces still agreed.
func TestEveryAdminGranteeReachesBothDatasetsAndTheTag(t *testing.T) {
	const grantee = "user:alice@example.com,group:g@example.com,user:bob@example.com"
	out, stubs, renderDir := dryRun(t, "absent", "--tag=abc1234",
		"--admin-grantee="+grantee,
		"--admin-emails=alice@example.com,bob@example.com,carol@example.com")
	for _, dataset := range []string{"loop_sessions_raw", "loop_sessions"} {
		entries := accessEntries(t, renderDir, dataset)
		for _, want := range []string{
			`{"role":"READER","userByEmail":"alice@example.com"}`,
			`{"groupByEmail":"g@example.com","role":"READER"}`,
			`{"role":"READER","userByEmail":"bob@example.com"}`,
		} {
			if !hasEntry(entries, want) {
				t.Errorf("%s access lacks %s:\n%v", dataset, want, entries)
			}
		}
	}
	// Fine-Grained Reader on the pii_text tag is per principal: one
	// binding each, and no binding for the list as a single member.
	for _, member := range strings.Split(grantee, ",") {
		want := "--member=" + member + " --role=roles/datacatalog.categoryFineGrainedReader"
		if !strings.Contains(stubs+out, want) {
			t.Errorf("no Fine-Grained Reader binding for %s:\n%s", member, stubs+out)
		}
	}
	if strings.Contains(stubs+out, "--member="+grantee+" ") {
		t.Error("the whole grantee list was passed as one IAM member")
	}
	// The admins table is the emails list, not the grantee list: carol is
	// an admin without a dataset grant (she reaches the raw rows another
	// way), and the group has no address to put in it.
	sql, err := os.ReadFile(filepath.Join(renderDir, "admins.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"'alice@example.com'", "'bob@example.com'", "'carol@example.com'"} {
		if !strings.Contains(string(sql), want) {
			t.Errorf("admins.sql lacks %s:\n%s", want, sql)
		}
	}
	if strings.Contains(string(sql), "g@example.com") {
		t.Errorf("admins.sql names the group, which matches no SESSION_USER():\n%s", sql)
	}
}

// TestScriptsParseAndPassShellcheck: bash -n on both scripts, and
// shellcheck where it is installed.
func TestScriptsParseAndPassShellcheck(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not available")
	}
	for _, script := range []string{"provision.sh", "scheduler.sh", "import-dv-sessions.sh"} {
		info, err := os.Stat(script)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("%s is not executable", script)
		}
		if out, err := exec.Command("bash", "-n", script).CombinedOutput(); err != nil {
			t.Errorf("bash -n %s: %v\n%s", script, err, out)
		}
		raw, _ := os.ReadFile(script)
		if !strings.HasPrefix(string(raw), "#!/usr/bin/env bash") || !strings.Contains(string(raw), "--dry-run") {
			t.Errorf("%s lacks the bash shebang or a --dry-run", script)
		}
	}
	if _, err := exec.LookPath("shellcheck"); err != nil {
		t.Log("shellcheck not installed; skipping")
		return
	}
	if out, err := exec.Command("shellcheck", "-s", "bash", "provision.sh", "scheduler.sh").CombinedOutput(); err != nil {
		t.Errorf("shellcheck: %v\n%s", err, out)
	}
}

// TestJobYAMLPinsTheTaskTimeoutAndRetries: the two numbers contract F(a)
// documents, the subcommand, the identity, and only placeholders the script
// renders.
func TestJobYAMLPinsTheTaskTimeoutAndRetries(t *testing.T) {
	raw, err := os.ReadFile("job.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	mustContain(t, "job.yaml", doc,
		"kind: Job",
		"name: loop-sessions-export",
		"timeoutSeconds: 1800",
		"maxRetries: 0",
		"taskCount: 1",
		`command: ["/server"]`,
		`args: ["export"]`,
		"serviceAccountName: loop-sessions-export@__PROJECT__.iam.gserviceaccount.com",
		"run.googleapis.com/cloudsql-instances: __PROJECT__:__REGION__:__INSTANCE__",
		"name: EXPORT_BUCKET", "name: EXPORT_DATASET", "name: EXPORT_PROJECT", "name: EXPORT_LOCATION",
		"name: DATABASE_PASSWORD", "name: loop-sessions-db-password",
	)
	if strings.Contains(doc, "EXPORT_ACCESS_TOKEN\n") || strings.Contains(doc, "name: EXPORT_ACCESS_TOKEN") {
		t.Error("job.yaml sets EXPORT_ACCESS_TOKEN; the job's identity is the metadata server's")
	}
	known := map[string]bool{"PROJECT": true, "REGION": true, "INSTANCE": true, "TAG": true, "BUCKET": true, "RAW_DATASET": true}
	for _, m := range regexp.MustCompile(`__([A-Z_]+)__`).FindAllStringSubmatch(doc, -1) {
		if !known[m[1]] {
			t.Errorf("job.yaml uses placeholder %s, which provision.sh does not render", m[0])
		}
	}
	// The job loads the raw dataset and never names the governed
	// one.
	if !strings.Contains(doc, "value: __RAW_DATASET__") || strings.Contains(doc, "__DATASET__") {
		t.Error("job.yaml does not point EXPORT_DATASET at the raw dataset alone")
	}
}

// exportedTables is the set the export job writes (export.Tables); the DDL
// must define each.
var exportedTables = []string{"events", "health_hourly", "messages", "sessions", "turns"}

// TestDDLIsIdempotentAndCoversEveryExportedTable pins contract F(c) as
// review-1 C2 reshaped it: a raw table per exported projection with
// PARTITION BY and CLUSTER BY in the raw dataset, idempotent statements
// only, a governed view over every raw table (and the four *_latest views)
// filtering on viewer_emails membership or the admins table, the admins
// table itself, the two named text columns tagged on the raw tables,
// events_latest keyed on capture_version, no row access policy file (a
// WRITE_TRUNCATE load is documented to drop those), and only known
// placeholders.
func TestDDLIsIdempotentAndCoversEveryExportedTable(t *testing.T) {
	read := func(name string) string {
		raw, err := os.ReadFile(filepath.Join("bigquery", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(raw)
	}
	tables := read("tables.sql")
	for _, table := range exportedTables {
		head := "CREATE TABLE IF NOT EXISTS `__PROJECT__.__RAW_DATASET__." + table + "`"
		i := strings.Index(tables, head)
		if i < 0 {
			t.Errorf("tables.sql lacks %s", head)
			continue
		}
		rest := tables[i:]
		if end := strings.Index(rest, ";"); end > 0 {
			rest = rest[:end]
		}
		mustContain(t, "tables.sql "+table, rest, "PARTITION BY DATE(", "CLUSTER BY ", "exported_at")
		if !regexp.MustCompile(`viewer_emails\s+ARRAY<STRING>`).MatchString(rest) {
			t.Errorf("tables.sql %s has no viewer_emails ARRAY<STRING> column", table)
		}
	}
	if strings.Contains(strings.ReplaceAll(tables, "CREATE TABLE IF NOT EXISTS", ""), "CREATE TABLE") {
		t.Error("tables.sql has a CREATE TABLE without IF NOT EXISTS")
	}
	if strings.Contains(tables, "__DATASET__") {
		t.Error("tables.sql names the governed dataset; the tables live in the raw one")
	}
	for _, table := range []string{"turns", "messages"} {
		if !regexp.MustCompile("`__PROJECT__.__RAW_DATASET__." + table + "`[^;]*PARTITION BY DATE\\(session_started_at\\)").MatchString(tables) {
			t.Errorf("%s is not partitioned by the session's start day", table)
		}
	}
	if !regexp.MustCompile("`__PROJECT__.__RAW_DATASET__.events`[^;]*PARTITION BY DATE\\(ingested_at\\)").MatchString(tables) {
		t.Error("events is not partitioned by ingested_at")
	}

	// No row access policies anywhere: the access rule is in the views.
	if _, err := os.Stat(filepath.Join("bigquery", "row_access_policies.sql")); err == nil {
		t.Error("row_access_policies.sql exists; WRITE_TRUNCATE loads are documented to remove row access policies, the views carry the rule")
	}
	for _, f := range []string{"tables.sql", "vendor.sql", "admins.sql", "views.sql"} {
		if strings.Contains(read(f), "ROW ACCESS POLICY") {
			t.Errorf("%s creates a row access policy", f)
		}
	}

	// The governed views: one per raw table plus the *_latest four, each in
	// the governed dataset, over the raw dataset, with the rule.
	views := read("views.sql")
	rule := "WHERE SESSION_USER() IN UNNEST(viewer_emails)\n   OR SESSION_USER() IN (SELECT email FROM `__PROJECT__.__RAW_DATASET__.admins`)"
	for _, table := range exportedTables {
		want := "CREATE OR REPLACE VIEW `__PROJECT__.__DATASET__." + table + "` AS\nSELECT * FROM `__PROJECT__.__RAW_DATASET__." + table + "`\n" + rule
		if !strings.Contains(views, want) {
			t.Errorf("views.sql lacks the governed view over %s with the access rule", table)
		}
	}
	for _, view := range governedViews {
		if !strings.Contains(views, "CREATE OR REPLACE VIEW `__PROJECT__.__DATASET__."+view+"`") {
			t.Errorf("views.sql lacks %s", view)
		}
	}
	if n := strings.Count(views, "CREATE OR REPLACE VIEW"); n != len(governedViews) {
		t.Errorf("views.sql defines %d views, want %d (the list provision.sh authorizes)", n, len(governedViews))
	}
	// Every view carries the rule except the two union views, which read
	// the governed views instead of the raw tables and inherit it.
	ruled := len(governedViews) - len(unionViews)
	if n := strings.Count(views, "SESSION_USER() IN UNNEST(viewer_emails)"); n != ruled {
		t.Errorf("%d of %d rule-carrying views filter on viewer_emails membership", n, ruled)
	}
	if n := strings.Count(views, "SESSION_USER() IN (SELECT email FROM `__PROJECT__.__RAW_DATASET__.admins`)"); n != ruled {
		t.Errorf("%d of %d rule-carrying views admit the admins table", n, ruled)
	}
	// A union view that read a raw table directly would be a rule written
	// twice, and the second copy is the one that gets forgotten.
	for _, v := range unionViews {
		body := views[strings.Index(views, "CREATE OR REPLACE VIEW `__PROJECT__.__DATASET__."+v+"`"):]
		if end := strings.Index(body[1:], "CREATE OR REPLACE VIEW"); end >= 0 {
			body = body[:end+1]
		}
		if strings.Contains(body, "__RAW_DATASET__") {
			t.Errorf("%s reads the raw dataset directly; it must select from the governed views so the rule is written once", v)
		}
	}
	if strings.Contains(views, "CREATE OR REPLACE VIEW `__PROJECT__.__RAW_DATASET__.") {
		t.Error("a view is defined in the raw dataset")
	}
	if strings.Contains(views, "CREATE VIEW") {
		t.Error("a view is CREATE rather than CREATE OR REPLACE")
	}
	mustContain(t, "views.sql", views,
		"PARTITION BY id ORDER BY capture_version DESC",
		"PARTITION BY session_id ORDER BY exported_at DESC",
		"PARTITION BY session_id, thread, turn_index ORDER BY exported_at DESC",
		"PARTITION BY event_id ORDER BY exported_at DESC")

	admins := read("admins.sql")
	mustContain(t, "admins.sql", admins,
		"CREATE OR REPLACE TABLE `__PROJECT__.__RAW_DATASET__.admins`",
		"SELECT email FROM UNNEST([__ADMIN_EMAILS__]) AS email")

	// The column tags are not DDL (BigQuery has no column option for them;
	// a live run got "Unknown option: policy_tags"): no SQL
	// file mentions them, and provision.sh names the four text columns
	// that get the tag, each declared in tables.sql.
	if _, err := os.Stat(filepath.Join("bigquery", "policy_tags.sql")); err == nil {
		t.Error("policy_tags.sql exists; policy tags go on through the schema (provision.sh tag_columns), never DDL")
	}
	for _, f := range []string{"tables.sql", "vendor.sql", "admins.sql", "views.sql"} {
		if strings.Contains(strings.ToLower(read(f)), "policy_tags") {
			t.Errorf("%s sets policy tags in DDL", f)
		}
	}
	script, err := os.ReadFile("provision.sh")
	if err != nil {
		t.Fatal(err)
	}
	mustContain(t, "provision.sh", string(script),
		"tag_columns messages text\n",
		"tag_columns events body\n",
		"tag_columns sessions first_prompt harness_title\n")
	for table, cols := range taggedColumns {
		for _, col := range cols {
			if !regexp.MustCompile("(?s)`__PROJECT__.__RAW_DATASET__." + table + "`[^;]*\\b" + col + " +(STRING|JSON)").MatchString(tables) {
				t.Errorf("tables.sql does not declare %s.%s, which provision.sh tags", table, col)
			}
		}
	}

	known := map[string]bool{"PROJECT": true, "DATASET": true, "RAW_DATASET": true, "ADMIN_EMAILS": true}
	for _, f := range []string{"tables.sql", "vendor.sql", "admins.sql", "views.sql"} {
		for _, m := range regexp.MustCompile(`__([A-Z_]+)__`).FindAllStringSubmatch(read(f), -1) {
			if !known[m[1]] {
				t.Errorf("%s uses placeholder %s, which provision.sh does not render", f, m[0])
			}
		}
	}
}

// TestDDLColumnsMatchTheStoreProjections cross-checks the two places a
// column lives: every column the DDL declares is named in
// server/store/export_reads.go, and every alias the projections produce
// is a DDL column. The loads run with ignoreUnknownValues off, so a
// mismatch here is a load that fails in production.
func TestDDLColumnsMatchTheStoreProjections(t *testing.T) {
	ddl, err := os.ReadFile(filepath.Join("bigquery", "tables.sql"))
	if err != nil {
		t.Fatal(err)
	}
	goSrc, err := os.ReadFile(filepath.Join("..", "..", "..", "server", "store", "export_reads.go"))
	if err != nil {
		t.Fatal(err)
	}
	// The projections are the var block from exportTurnColumns to the
	// closing paren; the rest of the file is Go, whose s.db and h.hour would
	// read as SQL aliases.
	src := string(goSrc)
	start := strings.Index(src, "exportTurnColumns = ")
	if start < 0 {
		t.Fatal("export_reads.go has no exportTurnColumns")
	}
	end := strings.Index(src[start:], "\n)\n")
	if end < 0 {
		t.Fatal("the projection var block does not close")
	}
	projections := src[start : start+end]
	// DDL columns: the first identifier of each indented line inside a
	// CREATE TABLE body.
	columns := map[string]bool{}
	inTable := false
	for _, line := range strings.Split(string(ddl), "\n") {
		switch {
		case strings.HasPrefix(line, "CREATE TABLE"):
			inTable = true
		case strings.HasPrefix(line, ")"):
			inTable = false
		case inTable && strings.HasPrefix(line, "  "):
			fields := strings.Fields(line)
			if len(fields) > 0 {
				columns[fields[0]] = true
			}
		}
	}
	if len(columns) < 40 {
		t.Fatalf("parsed only %d DDL columns; the parser lost the table bodies", len(columns))
	}
	for col := range columns {
		if !regexp.MustCompile(`\b` + col + `\b`).MatchString(projections) {
			t.Errorf("DDL column %s is named nowhere in the store projections", col)
		}
	}
	// Projection aliases: "AS name" inside the export*Columns strings. The
	// one alias that is not a column is the viewer_emails subquery's v.
	scaffolding := map[string]bool{"v": true}
	for _, m := range regexp.MustCompile(`\bAS ([a-z_0-9]+)`).FindAllStringSubmatch(projections, -1) {
		name := m[1]
		if scaffolding[name] {
			continue
		}
		if !columns[name] {
			t.Errorf("projection alias %s has no DDL column", name)
		}
	}
	// And the projected bare columns (t.x, s.x, m.x, e.x, h.x) are DDL
	// columns too, except the ones only used inside expressions.
	expression := map[string]bool{"email": true, "started_at": true, "body_expired_at": true, "body": true}
	for _, m := range regexp.MustCompile(`\b[tsmeh]\.([a-z_0-9]+)`).FindAllStringSubmatch(projections, -1) {
		name := m[1]
		if columns[name] || expression[name] {
			continue
		}
		t.Errorf("projected column %s has no DDL column", name)
	}
}

// TestLiveApplyHandsBQCleanAccessListsAndReachesTheSchedule is the first
// live run, replayed against tools that answer the way the real
// ones do. That run created both datasets and then wrote neither access
// list: `bq mk` prints its success line on stdout, dataset_ensure captured
// it in front of the dataset JSON, jq refused the mix, and because set -e
// does not reach a function inside a command substitution the script
// carried on, handed bq an empty payload, and later died on the policy-tag
// binding. So: every payload bq update was given parses, the raw dataset's
// final list carries the export writer and the admin reader and no project
// reader or writer group, the governed one keeps its readers and has no
// writer, the policy-tag binding names the taxonomy by its short id, and
// the run reaches the schedule.
func TestLiveApplyHandsBQCleanAccessListsAndReachesTheSchedule(t *testing.T) {
	out, stubs, stubDir := liveRun(t)
	mustContain(t, "live run", out,
		"dataset loop_sessions_raw: create",
		"dataset loop_sessions_raw access: add 2 entries, remove 2 entries",
		"dataset loop_sessions: create",
		"dataset loop_sessions access: add 2 entries, remove 1 entry",
		"== column tags on loop_sessions_raw (pii_text)",
		"table messages: tag text",
		"table events: tag body",
		"table sessions: tag first_prompt harness_title",
		"== authorized views on loop_sessions_raw",
		"dataset loop_sessions_raw access: add 13 entries",
		"job: create with image tag stub-tag",
		"scheduler job loop-sessions-export-hourly: create",
		"== provisioned.",
	)
	// The tags went on through `bq update --schema`, after the tables were
	// created and before the views were authorized, and what bq holds for
	// each table is its schema with the tag on the named columns only.
	for table, cols := range taggedColumns {
		schemaCarriesTagOn(t, filepath.Join(stubDir, "loop_sessions_raw."+table+".schema.json"), cols)
	}
	schemaAt := strings.Index(stubs, "update --schema")
	ddlAt := strings.Index(stubs, "query --use_legacy_sql=false")
	viewsAt := strings.LastIndex(stubs, "update --source")
	if schemaAt < 0 || ddlAt < 0 || viewsAt < 0 || schemaAt < ddlAt || schemaAt > viewsAt {
		t.Errorf("the schema update did not land between the DDL and the view authorization (ddl %d, schema %d, views %d)", ddlAt, schemaAt, viewsAt)
	}
	for _, bad := range []string{"parse error", "successfully created.\n{", "Error decoding JSON"} {
		if strings.Contains(out, bad) {
			t.Errorf("live run output carries %q:\n%s", bad, out)
		}
	}
	// Every payload handed to bq update parses and names an access list.
	payloads, _ := filepath.Glob(filepath.Join(stubDir, "*.update.*.json"))
	if len(payloads) != 3 {
		t.Fatalf("expected 3 bq update payloads (raw, governed, raw again for the views), found %d: %v", len(payloads), payloads)
	}
	for _, p := range payloads {
		raw, _ := os.ReadFile(p)
		var doc struct {
			Access []map[string]any `json:"access"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Access) == 0 {
			t.Errorf("payload %s is not a dataset JSON with an access list: %v\n%s", filepath.Base(p), err, raw)
		}
	}
	// The lists the stub holds after the run are what BigQuery would hold.
	rawFinal := readStubDataset(t, stubDir, "loop_sessions_raw")
	rawDatasetKeepsOnlyProjectOwners(t, rawFinal)
	for _, want := range []string{
		`{"role":"WRITER","userByEmail":"` + exportSA + `"}`,
		`{"groupByEmail":"loop-sessions-admins@example.com","role":"READER"}`,
	} {
		if !hasEntry(rawFinal, want) {
			t.Errorf("the raw dataset lacks %s after the live run:\n%v", want, rawFinal)
		}
	}
	views := 0
	for _, e := range rawFinal {
		if _, ok := e["view"]; ok {
			views++
		}
	}
	if views != len(governedViews) {
		t.Errorf("the raw dataset authorizes %d views, want %d", views, len(governedViews))
	}
	governedDatasetKeepsReadersAndDropsWriters(t, readStubDataset(t, stubDir, "loop_sessions"))
	// The policy-tag binding names the taxonomy by its short id.
	mustContain(t, "stub log", stubs,
		"policy-tags add-iam-policy-binding 222 --taxonomy=111 --location=asia-south1 --member=group:loop-sessions-admins@example.com --role=roles/datacatalog.categoryFineGrainedReader",
		"run jobs replace",
		"scheduler jobs create http loop-sessions-export-hourly",
	)
	// `policy-tags list` may take the full name (it does, and works); the
	// per-tag binding may not.
	for _, line := range strings.Split(stubs, "\n") {
		if strings.Contains(line, "policy-tags add-iam-policy-binding") && strings.Contains(line, "--taxonomy=projects/") {
			t.Errorf("the policy-tag binding still passes the taxonomy's full name: %s", line)
		}
	}
}

// readStubDataset reads the access list the accepting bq stub holds for a
// dataset after a live run.
func readStubDataset(t *testing.T, stubDir, dataset string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(stubDir, dataset+".json"))
	if err != nil {
		t.Fatalf("stub state for %s: %v", dataset, err)
	}
	var doc struct {
		Access []map[string]any `json:"access"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s stub state is not JSON: %v\n%s", dataset, err, raw)
	}
	return doc.Access
}
