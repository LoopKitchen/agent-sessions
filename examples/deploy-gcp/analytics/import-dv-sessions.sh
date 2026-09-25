#!/usr/bin/env bash
# Imports a vendor session dump from GCS into the vendor tables, so that
# sessions people ran on someone else's agent platform sit in the same
# dataset, under the same access rule, as the ones our client captures.
# Run by a human with write access to the raw dataset; the export service
# account has nothing to do with this path and never runs it.
#
# Why a dump and not an API. No vendor offered a bulk session API when this
# was written; what exists is a frozen export in a bucket (an index plus one
# JSON file per session). This imports it. When a real API appears, the
# ongoing path is a job and not this script; the tables and the views it
# fills are the same either way.
#
# What it does, in order:
#   1. reads the dump's index and per-session layout and refuses a dump
#      that has neither;
#   2. converts the index (one JSON array) to newline-delimited JSON and
#      stages it in the analytics bucket, because `bq load` reads NDJSON
#      and a 10,000-element array is not that;
#   3. loads the index and the per-session files into two staging tables,
#      WRITE_TRUNCATE, with the messages array kept whole as JSON;
#   4. runs bigquery/import-vendor.sql, which is one transaction that
#      deletes this platform's rows and inserts them again, so a re-run
#      replaces rather than doubles;
#   5. counts what landed and prints it beside what the dump held.
#
# The staging tables are left in place: they are the dump as BigQuery read
# it, they are what a disagreement between the dump and the vendor tables
# has to be settled against, and they cost a few hundred megabytes.
#
# The vendor tables must exist first (provision.sh applies bigquery/
# vendor.sql). This script does not create them: it is an import, and an
# import that quietly creates its destination hides a run against the
# wrong dataset.
#
# Usage:
#   examples/deploy-gcp/analytics/import-dv-sessions.sh --dry-run
#   examples/deploy-gcp/analytics/import-dv-sessions.sh [--dump=gs://__DUMP_BUCKET__]
#       [--platform=devin] [--project=P] [--raw-dataset=loop_sessions_raw]
#       [--bucket=__ANALYTICS_BUCKET__] [--region=__REGION__]
#
#   --dry-run reads the dump and the dataset, prints every command it would
#   run with its arguments rendered, and writes nothing.
#   --platform is the vendor id and the key the import replaces on: it must
#   be one of the five skill_invocations.platform values, and it must not be
#   claude_code or codex, which are what the client captures.
#
# Needs: gcloud, bq, jq, python3.
set -euo pipefail

PROJECT="${PROJECT:-__PROJECT__}"
REGION="${REGION:-__REGION__}"
RAW_DATASET="${RAW_DATASET:-loop_sessions_raw}"
BUCKET="${BUCKET:-__ANALYTICS_BUCKET__}"
DUMP="${DUMP:-gs://__DUMP_BUCKET__}"
PLATFORM="${PLATFORM:-devin}"
GCLOUD="${GCLOUD:-gcloud}"
BQ="${BQ:-bq}"
DRY_RUN=0

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
log() { printf '%s\n' "$*" >&2; }
die() {
	log "import-dv-sessions.sh: $*"
	exit 1
}

for arg in "$@"; do
	case "$arg" in
	--dry-run) DRY_RUN=1 ;;
	--project=*) PROJECT="${arg#--project=}" ;;
	--region=*) REGION="${arg#--region=}" ;;
	--raw-dataset=*) RAW_DATASET="${arg#--raw-dataset=}" ;;
	--bucket=*) BUCKET="${arg#--bucket=}" ;;
	--dump=*) DUMP="${arg#--dump=}" ;;
	--platform=*) PLATFORM="${arg#--platform=}" ;;
	-h | --help)
		awk 'NR > 1 && /^set -euo pipefail/ { exit } NR > 1' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) die "unknown argument: $arg (see --help)" ;;
	esac
done

for tool in "$GCLOUD" "$BQ" jq python3; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done

shape() { [[ "$2" =~ $3 ]] || die "$1=$2 does not look like $4"; }
shape PROJECT "$PROJECT" '^[a-z][a-z0-9-]+$' 'a project id'
shape REGION "$REGION" '^[a-z0-9-]+$' 'a region'
shape RAW_DATASET "$RAW_DATASET" '^[A-Za-z0-9_]+$' 'a dataset id'
shape BUCKET "$BUCKET" '^[a-z0-9][a-z0-9._-]+$' 'a bucket name'
shape DUMP "$DUMP" '^gs://[a-z0-9][a-z0-9._/-]+$' 'a gs:// URI'
# The vendor id is the key the import deletes on. A typo here would delete
# nothing and insert a second copy of the corpus under a name no view
# knows, so it is checked against the vocabulary rather than trusted.
case "$PLATFORM" in
devin | capy | vorflux) ;;
claude_code | codex) die "--platform=$PLATFORM is what the client captures: those sessions come in through the export, and importing them here would duplicate them in all_sessions" ;;
*) die "--platform=$PLATFORM is not one of the skill_invocations platforms (devin, capy, vorflux)" ;;
esac

STAGE_DUMP="vendor_${PLATFORM}_dump"
STAGE_INDEX="vendor_${PLATFORM}_index"
STAGE_URI="gs://$BUCKET/import/$PLATFORM"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# mutate runs a gcloud command, or prints it under --dry-run. bq_mutate and
# bq_query are the same for bq, reading from a file where bq needs stdin.
gc() { "$GCLOUD" "$@" --project="$PROJECT" --quiet; }
bqr() { "$BQ" --project_id="$PROJECT" --location="$REGION" "$@"; }
mutate() {
	if [ "$DRY_RUN" = 1 ]; then
		log "  dry-run: gcloud $*"
		return 0
	fi
	gc "$@"
}
bq_mutate() {
	if [ "$DRY_RUN" = 1 ]; then
		log "  dry-run: bq $*"
		return 0
	fi
	bqr "$@"
}

# ---------------------------------------------------------------------------
# 1. The dump. Both halves must be there: the per-session files hold the
# transcripts and no owner, the index holds the owner and no transcript,
# and an import with only one of them is either anonymous or empty.
# ---------------------------------------------------------------------------

log "== dump $DUMP"
index_uri="$DUMP/sessions-index.json"
"$GCLOUD" storage ls "$index_uri" >/dev/null 2>&1 || die "$index_uri is not there; this is not a session dump of the shape this imports"
sessions_glob="$DUMP/sessions/*.json"
dump_files="$("$GCLOUD" storage ls "$sessions_glob" 2>/dev/null | wc -l | tr -d ' ')"
[ "$dump_files" -gt 0 ] || die "$DUMP/sessions/ holds no .json files"
log "dump: $dump_files session files"

# ---------------------------------------------------------------------------
# 2. The index, as NDJSON. `bq load` reads one JSON object per line; the
# dump's index is a single array, so it is converted here rather than
# loaded as a 17 MB string and taken apart in SQL.
# ---------------------------------------------------------------------------

log "== index"
"$GCLOUD" storage cp "$index_uri" "$TMP/index.json" >/dev/null 2>&1 || die "cannot read $index_uri"
jq -e 'type == "array"' "$TMP/index.json" >/dev/null || die "$index_uri is not a JSON array"
jq -c '.[] | {session_id, requesting_user_email}' "$TMP/index.json" >"$TMP/index.ndjson"
index_rows="$(wc -l <"$TMP/index.ndjson" | tr -d ' ')"
with_owner="$(jq -r 'select(.requesting_user_email != null) | .session_id' "$TMP/index.ndjson" | wc -l | tr -d ' ')"
log "index: $index_rows sessions, $with_owner with an owner, $((index_rows - with_owner)) without"
[ "$index_rows" -gt 0 ] || die "the index is empty"

if [ "$DRY_RUN" = 1 ]; then
	log "  dry-run: gcloud storage cp <index.ndjson> $STAGE_URI/sessions-index.ndjson"
else
	"$GCLOUD" storage cp "$TMP/index.ndjson" "$STAGE_URI/sessions-index.ndjson" >/dev/null 2>&1 ||
		die "cannot stage the index at $STAGE_URI/sessions-index.ndjson"
fi

# ---------------------------------------------------------------------------
# 3. The two staging tables. The per-session files are one JSON object each
# on a single line, which is NDJSON with one row, so the whole directory
# loads under one wildcard. messages stays JSON: it is an array of objects
# whose shape is the vendor's business, and import-vendor.sql reads it with
# JSON_VALUE rather than pinning a schema that the next dump may widen.
# --ignore_unknown_values is on for the same reason: a field the vendor
# adds must not fail the load.
# ---------------------------------------------------------------------------

log "== staging tables"
# The schemas go in files, not in bq's inline `name:TYPE,...` form: that
# form has no way to say REPEATED, and tags is an array of strings.
cat >"$TMP/index.schema.json" <<'JSON'
[
  {"name": "session_id", "type": "STRING"},
  {"name": "requesting_user_email", "type": "STRING"}
]
JSON
cat >"$TMP/dump.schema.json" <<'JSON'
[
  {"name": "session_id", "type": "STRING"},
  {"name": "status", "type": "STRING"},
  {"name": "title", "type": "STRING"},
  {"name": "created_at", "type": "TIMESTAMP"},
  {"name": "updated_at", "type": "TIMESTAMP"},
  {"name": "snapshot_id", "type": "STRING"},
  {"name": "playbook_id", "type": "STRING"},
  {"name": "tags", "type": "STRING", "mode": "REPEATED"},
  {"name": "pull_request", "type": "JSON"},
  {"name": "structured_output", "type": "JSON"},
  {"name": "status_enum", "type": "STRING"},
  {"name": "messages", "type": "JSON"}
]
JSON
bq_mutate load --source_format=NEWLINE_DELIMITED_JSON --replace --ignore_unknown_values \
	"$PROJECT:$RAW_DATASET.$STAGE_INDEX" "$STAGE_URI/sessions-index.ndjson" "$TMP/index.schema.json"
bq_mutate load --source_format=NEWLINE_DELIMITED_JSON --replace --ignore_unknown_values \
	"$PROJECT:$RAW_DATASET.$STAGE_DUMP" "$sessions_glob" "$TMP/dump.schema.json"

# ---------------------------------------------------------------------------
# 4. The transform, in one transaction.
# ---------------------------------------------------------------------------

log "== transform"
src="$DIR/bigquery/import-vendor.sql"
[ -f "$src" ] || die "$src is missing"
rendered="$TMP/import-vendor.sql"
sed -e "s|__PROJECT__|$PROJECT|g" \
	-e "s|__RAW_DATASET__|$RAW_DATASET|g" \
	-e "s|__PLATFORM__|$PLATFORM|g" \
	-e "s|__STAGE_DUMP__|$STAGE_DUMP|g" \
	-e "s|__STAGE_INDEX__|$STAGE_INDEX|g" \
	-e "s|__DUMP_URI__|$DUMP|g" \
	"$src" >"$rendered"
if grep -nE '__[A-Z_]+__' "$rendered" >&2; then
	die "import-vendor.sql still carries a placeholder after rendering (above)"
fi
if [ "$DRY_RUN" = 1 ]; then
	log "  dry-run: bq query --use_legacy_sql=false < $rendered"
	log "== dry run complete; nothing was changed"
	exit 0
fi
bqr query --use_legacy_sql=false --nouse_cache <"$rendered" >/dev/null

# ---------------------------------------------------------------------------
# 5. What landed. The counts are read back rather than assumed: a load that
# skipped rows and a transform that dropped them look identical from here
# unless the numbers are put side by side.
# ---------------------------------------------------------------------------

log "== result"
read -r sessions messages owners no_owner first_day last_day <<<"$(
	bqr query --use_legacy_sql=false --nouse_cache --format=csv "
	  SELECT
	    (SELECT COUNT(*) FROM \`$PROJECT.$RAW_DATASET.vendor_sessions\` WHERE platform = '$PLATFORM'),
	    (SELECT COUNT(*) FROM \`$PROJECT.$RAW_DATASET.vendor_messages\` WHERE platform = '$PLATFORM'),
	    (SELECT COUNT(DISTINCT email) FROM \`$PROJECT.$RAW_DATASET.vendor_sessions\` WHERE platform = '$PLATFORM' AND email IS NOT NULL),
	    (SELECT COUNTIF(email IS NULL) FROM \`$PROJECT.$RAW_DATASET.vendor_sessions\` WHERE platform = '$PLATFORM'),
	    (SELECT CAST(MIN(DATE(started_at)) AS STRING) FROM \`$PROJECT.$RAW_DATASET.vendor_sessions\` WHERE platform = '$PLATFORM'),
	    (SELECT CAST(MAX(DATE(started_at)) AS STRING) FROM \`$PROJECT.$RAW_DATASET.vendor_sessions\` WHERE platform = '$PLATFORM')" |
		tail -1 | tr ',' ' '
)"
log "$PLATFORM: $sessions sessions, $messages messages, $owners owners, $no_owner without an owner, $first_day to $last_day"
if [ "$sessions" != "$dump_files" ]; then
	log "note: the dump held $dump_files files and $sessions sessions landed; the difference is files with no created_at, which cannot be partitioned. Find them with: SELECT session_id FROM \`$PROJECT.$RAW_DATASET.$STAGE_DUMP\` WHERE created_at IS NULL"
fi
log "== import complete; query \`$PROJECT.loop_sessions.all_sessions\`"
