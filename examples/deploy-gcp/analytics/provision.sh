#!/usr/bin/env bash
# Provisions everything the analytics export needs, in the GCP project, as
# the owner: the bucket, the export service account and its bindings, the
# two BigQuery datasets (the raw one the job loads into and the
# governed one of authorized views analysts query), the policy-tag taxonomy,
# the tables, the admins table, the views and their authorization, the
# column tags, the Cloud Run job and the Cloud Scheduler trigger. It is run
# by a human with owner-level roles; the job's own service account can do
# none of this and is not meant to (ops review F6).
#
# Everything is update-or-create or idempotent by construction, so running
# it twice is safe and running it after editing a file under this directory
# is how a change ships:
#
#   - the bucket, the service account, the datasets and the taxonomy are
#     described (or listed by display name) first and created only when
#     absent; IAM bindings are additive no-ops when present; a dataset's
#     access list is read, the entries it lacks appended, the project
#     groups BigQuery attaches at creation taken off (projectReaders and
#     projectWriters from the raw dataset, projectWriters from the governed
#     one), and the list written back only when something changed;
#   - the DDL is CREATE TABLE IF NOT EXISTS, CREATE OR REPLACE TABLE ... AS
#     SELECT (the admins list) and CREATE OR REPLACE VIEW; the column policy
#     tags are not DDL, they go on with `bq update --schema` (phase 5a);
#   - the Cloud Run job is `gcloud run jobs replace` of job.yaml, which
#     creates or updates; the schedule is scheduler.sh, update-or-create.
#
# Every write goes through one of two wrappers (mutate for gcloud,
# bq_mutate for bq), and --dry-run turns both into a printed line, so the
# dry run reads the project (describes, lists, `bq show`) and prints every
# command it would run, in order, with its arguments rendered. Run it
# before every real apply and read what it prints.
#
# The access model (bigquery/views.sql says why it is views and not row
# access policies on the loaded tables):
#   raw dataset (loop_sessions_raw)   WRITER: the export service account;
#                                     READER: --admin-grantee; OWNER:
#                                     projectOwners and the creator, as
#                                     BigQuery leaves them. The projectReaders
#                                     and projectWriters entries BigQuery
#                                     attaches at creation (every project
#                                     viewer and editor) are removed on every
#                                     run. What no access list can do is keep
#                                     out project-level IAM: a principal with
#                                     roles/bigquery.admin, dataViewer,
#                                     dataEditor, dataOwner or a basic role on
#                                     the project reads the raw rows by
#                                     inheritance (README, The access model;
#                                     the owner decision is recorded there)
#   governed dataset (loop_sessions)  READER: the members grantee (the two
#                                     Workspace domains by default) and the
#                                     admin grantees; holds views only. The
#                                     projectWriters entry (every project
#                                     editor as WRITER) is removed on every
#                                     run: a WRITER here can CREATE OR
#                                     REPLACE a view without its WHERE, and
#                                     the raw dataset authorizes a view by
#                                     name, so the redefined view would hand
#                                     every raw row to everyone the dataset
#                                     admits. projectReaders, projectOwners
#                                     and the creator stay as BigQuery leaves
#                                     them: a reader through them gets what
#                                     the views' WHERE gives
#   each view                         authorized on the raw dataset, and
#                                     filters rows to the querying user's
#                                     viewer_emails or to the admins table
#   admins table (raw)                the signed-in addresses that see every
#                                     row, from --admin-emails
#   pii_text policy tag               on the raw text columns; Fine-Grained
#                                     Reader for --admin-grantee only
#
# Placeholders in bigquery/*.sql and job.yaml (__PROJECT__, __DATASET__,
# __RAW_DATASET__, __REGION__, __INSTANCE__, __TAG__, __BUCKET__,
# __POLICY_TAG__, __ADMIN_EMAILS__) are rendered here; a placeholder left
# after rendering fails the run, for the reason monitoring/apply.sh gives:
# a file applied with a literal __X__ in it is a table nobody can find or a
# view that admits nobody.
#
# What the export service account gets, and what it does not:
#   roles/storage.objectAdmin           on the bucket only
#   WRITER (roles/bigquery.dataEditor)  on the raw dataset only
#   roles/bigquery.jobUser              on the project (load jobs are
#                                       project-scoped)
#   roles/cloudsql.client               on the project
#   roles/logging.logWriter             on the project
#   secretAccessor                      on loop-sessions-db-password
#   roles/run.invoker                   on the job (scheduler.sh; it is the
#                                       schedule's identity)
# and NOT roles/datacatalog.categoryFineGrainedReader, and nothing on the
# governed dataset: the job writes the raw tables and never reads them
# back. --admin-grantee gets the reader role on the tag instead.
#
# Usage:
#   examples/deploy-gcp/analytics/provision.sh --dry-run --tag=<image sha> \
#       --admin-grantee=<members> --admin-emails=<addresses>
#   examples/deploy-gcp/analytics/provision.sh --tag=<image sha>
#       --admin-grantee='user:a@x,user:b@y' --admin-emails='a@x,b@y,b@z'
#       [--project=P] [--region=R]
#       [--members-grantee='"domain:a", "domain:b"'] [--raw-dataset=loop_sessions_raw]
#
#   --tag is the image tag the service runs (README section 7); the job is
#   built from the same image. Re-run with the new tag after every deploy.
#
#   --admin-grantee and --admin-emails are required, and have no defaults on
#   purpose: both name people, both are read every run, and neither has a
#   value that is right for longer than the list of admins is stable. They
#   are two lists because they answer two different questions.
#
#   --admin-grantee is the IAM principals that get READER on both datasets
#   and Fine-Grained Reader on the pii_text tag, comma-separated. Every one
#   must resolve: BigQuery rejects an unresolvable group or user outright
#   (`Group ... does not exist`), mid-run, after the IAM phase has already
#   written. Read the live list before you change it:
#       bq show --format=json <project>:<raw dataset> | jq '.access'
#
#   --admin-emails is the whole admins table: every address those principals
#   may be signed in as, including Workspace aliases, because the views match
#   SESSION_USER() against it. It is applied with CREATE OR REPLACE, so it is
#   the whole list and not an addition: an address dropped from it stops
#   seeing other people's rows on that run, with no error. Read the live list
#   before you change it:
#       bq query --nouse_legacy_sql 'SELECT email FROM `<project>.<raw dataset>.admins`'
#
#   Every `user:` in --admin-grantee must appear in --admin-emails, or the run
#   refuses: a principal granted the raw dataset but missing from the admins
#   table reads the governed views as an ordinary user and sees only their own
#   rows, which looks like a permissions bug and is a typo.
#
# Needs: gcloud, bq, jq, as someone holding roles/owner or the union of
# storage.admin, iam.serviceAccountAdmin, resourcemanager.projectIamAdmin,
# bigquery.admin, datacatalog.categoryAdmin, run.admin,
# cloudscheduler.admin. GCLOUD and BQ may be overridden from the
# environment so the script can be exercised against a recording stub.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT="${PROJECT:-__PROJECT__}"
REGION="${REGION:-__REGION__}"
INSTANCE="${INSTANCE:-loop-sessions}"
BUCKET="${BUCKET:-__ANALYTICS_BUCKET__}"
DATASET="${DATASET:-loop_sessions}"
RAW_DATASET="${RAW_DATASET:-loop_sessions_raw}"
TAXONOMY="${TAXONOMY:-loop_sessions_pii}"
POLICY_TAG_NAME="${POLICY_TAG_NAME:-pii_text}"
SA_NAME="${SA_NAME:-loop-sessions-export}"
JOB="${JOB:-loop-sessions-export}"
DB_SECRET="${DB_SECRET:-loop-sessions-db-password}"
TAG="${TAG:-}"
# No defaults: both name people, and a default that names the wrong people
# is worse here than no value at all. BigQuery silently drops an
# unresolvable `domain:` entry but REJECTS an unresolvable group or user: a
# run with a placeholder group dies at the first dataset with `Group ...
# does not exist`, after phase 2 has written IAM. And the admins list is not
# additive, it is CREATE OR REPLACE, so a placeholder that did resolve would
# replace the live admin addresses and take every admin's cross-row
# visibility away without an error. Both are required arguments, checked
# before anything is written.
ADMIN_GRANTEE="${ADMIN_GRANTEE:-}"
ADMIN_EMAILS="${ADMIN_EMAILS:-}"
# Only a Workspace domain of THIS organisation can be a `domain:` entry.
# BigQuery accepts any other one, says it succeeded and stores nothing, so a
# wrong value here is invisible rather than loud (the read-back in
# dataset_apply exists because of exactly that). People whose addresses are
# on a domain the organisation does not own have to be named individually
# here or left out.
MEMBERS_GRANTEE="${MEMBERS_GRANTEE:-\"domain:__DOMAINS__\"}"
GCLOUD="${GCLOUD:-gcloud}"
BQ="${BQ:-bq}"
DRY_RUN=0
# The entries BigQuery attaches to every new dataset that the datasets must
# not keep (access-control-basic-roles): projectReaders is every holder of
# roles/viewer on the project as READER, projectWriters every holder of
# roles/editor as WRITER. The raw dataset loses both: it holds the tables
# with every row. The governed dataset loses projectWriters only: a WRITER
# there can CREATE OR REPLACE a governed view without its WHERE, and the
# raw dataset's authorization is keyed on the view's name, not its text, so
# the redefined view would still read every raw row, and every reader the
# governed dataset admits (the two Workspace domains) would see them.
# projectReaders stays on the governed dataset: it holds views only, and a
# reader gets what each view's WHERE gives and no more. projectOwners and
# the creator stay on both. (A project-level roles/editor, dataEditor,
# dataOwner or admin can redefine a view by inheritance regardless; the
# README's access model says so, as it does for the raw rows, and the
# runbook reads the view definitions back.)
RAW_UNWANTED_GROUPS='["projectReaders","projectWriters"]'
GOVERNED_UNWANTED_GROUPS='["projectWriters"]'

log() { printf '%s\n' "$*" >&2; }
die() { log "provision.sh: $*"; exit 1; }

for arg in "$@"; do
	case "$arg" in
	--dry-run) DRY_RUN=1 ;;
	--project=*) PROJECT="${arg#--project=}" ;;
	--region=*) REGION="${arg#--region=}" ;;
	--tag=*) TAG="${arg#--tag=}" ;;
	--admin-grantee=*) ADMIN_GRANTEE="${arg#--admin-grantee=}" ;;
	--admin-emails=*) ADMIN_EMAILS="${arg#--admin-emails=}" ;;
	--members-grantee=*) MEMBERS_GRANTEE="${arg#--members-grantee=}" ;;
	--raw-dataset=*) RAW_DATASET="${arg#--raw-dataset=}" ;;
	-h | --help)
		# The header, up to the first non-comment line, so it cannot fall
		# out of step with its own length.
		awk 'NR > 1 && /^set -euo pipefail/ { exit } NR > 1' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) die "unknown argument: $arg (see --help)" ;;
	esac
done

command -v jq >/dev/null 2>&1 || die "jq is required"
command -v "$GCLOUD" >/dev/null 2>&1 || die "$GCLOUD is required"
command -v "$BQ" >/dev/null 2>&1 || die "$BQ is required"

# The values spliced into files by sed and into resource names; each is
# checked against the shape it must have, because a stray | or & in a sed
# replacement corrupts a file silently.
shape() { [[ "$2" =~ $3 ]] || die "$1=$2 does not look like $4"; }
shape PROJECT "$PROJECT" '^[a-z][a-z0-9-]+$' 'a project id'
shape REGION "$REGION" '^[a-z0-9-]+$' 'a region'
shape INSTANCE "$INSTANCE" '^[a-z][a-z0-9-]+$' 'a Cloud SQL instance name'
shape BUCKET "$BUCKET" '^[a-z0-9][a-z0-9._-]+$' 'a bucket name'
shape DATASET "$DATASET" '^[A-Za-z0-9_]+$' 'a dataset id'
shape RAW_DATASET "$RAW_DATASET" '^[A-Za-z0-9_]+$' 'a dataset id'
[ "$DATASET" != "$RAW_DATASET" ] || die "the governed dataset and the raw dataset must differ ($DATASET); the views cannot live beside the tables they hide"
[ -n "$ADMIN_GRANTEE" ] || die "--admin-grantee=<members> is required and has no default: it is the IAM principals that read both datasets and the pii_text tag. See --help; read the live list with \`bq show --format=json $PROJECT:$RAW_DATASET | jq '.access'\`"
[ -n "$ADMIN_EMAILS" ] || die "--admin-emails=<addresses> is required and has no default: it is the WHOLE admins table, applied with CREATE OR REPLACE, so a short list silently removes admins. See --help; read the live list with \`bq query --nouse_legacy_sql 'SELECT email FROM \`$PROJECT.$RAW_DATASET.admins\`'\`"
shape ADMIN_GRANTEE "$ADMIN_GRANTEE" '^(group|user|serviceAccount|domain):[A-Za-z0-9._@-]+(,(group|user|serviceAccount|domain):[A-Za-z0-9._@-]+)*$' 'a comma-separated list of IAM members (group:..., user:..., domain:...)'
shape ADMIN_EMAILS "$ADMIN_EMAILS" '^[A-Za-z0-9._+-]+@[A-Za-z0-9.-]+(,[A-Za-z0-9._+-]+@[A-Za-z0-9.-]+)*$' 'a comma-separated list of email addresses'

# Every `user:` principal must be in the admins table, or it reads the
# governed views as an ordinary user and sees only its own rows: a grant
# that looks applied and is half applied. A group, a domain or a service
# account has no single address to match SESSION_USER() against, so only
# `user:` is checked; a group of admins still needs its members' addresses
# in --admin-emails, which nothing here can enumerate.
IFS=',' read -ra admin_members <<<"$ADMIN_GRANTEE"
for member in "${admin_members[@]}"; do
	[ "${member%%:*}" = user ] || continue
	case ",$ADMIN_EMAILS," in
	*",${member#*:},"*) ;;
	*) die "--admin-grantee names ${member#*:} but --admin-emails does not: a user granted the raw dataset and left out of the admins table sees only their own rows through the views" ;;
	esac
done
shape MEMBERS_GRANTEE "$MEMBERS_GRANTEE" '^"(group|user|serviceAccount|domain):[A-Za-z0-9._@-]+"(, *"(group|user|serviceAccount|domain):[A-Za-z0-9._@-]+")*$' 'a quoted, comma-separated list of IAM members'
if [ -z "$TAG" ]; then
	if [ "$DRY_RUN" = 1 ]; then
		TAG="dry-run-tag"
	else
		die "--tag=<image sha> is required: the job runs the same image as the service (README section 7)"
	fi
fi
shape TAG "$TAG" '^[A-Za-z0-9._-]+$' 'an image tag'

SA="${SA_NAME}@${PROJECT}.iam.gserviceaccount.com"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
# Where the rendered DDL and job.yaml are kept for reading. A dry run keeps
# them (its whole point is that they can be read); a real run renders into
# the temp dir that goes with the trap. RENDER_DIR may be set to choose the
# place; the tests do.
RENDER_DIR="${RENDER_DIR:-}"
if [ "$DRY_RUN" = 1 ] && [ -z "$RENDER_DIR" ]; then
	RENDER_DIR="$(mktemp -d -t __ANALYTICS_BUCKET__-KEEP)"
fi
if [ -n "$RENDER_DIR" ]; then
	mkdir -p "$RENDER_DIR"
fi
keep_rendered() {
	if [ -n "$RENDER_DIR" ]; then
		cp "$1" "$RENDER_DIR/$(basename "$1")"
		log "  rendered: $RENDER_DIR/$(basename "$1")"
	fi
}

gc() { "$GCLOUD" --project="$PROJECT" "$@"; }
# A mutation's stdout goes to stderr, beside the log lines. Nothing this
# script does reads what a write prints, and two of the helpers below run
# inside command substitutions that capture stdout: `bq mk` prints
# "Dataset '...' successfully created." on stdout, and on a live
# run that line landed in front of the dataset JSON dataset_ensure hands to
# dataset_apply, jq refused the mix, and both access lists went unwritten
# while the run carried on (see dataset_apply for why it carried on).
mutate() {
	if [ "$DRY_RUN" = 1 ]; then
		log "  dry-run: gcloud $*"
	else
		gc --quiet "$@" >&2
	fi
}
# bq's global flags go before the command.
bqr() { "$BQ" --project_id="$PROJECT" --location="$REGION" --quiet "$@"; }
bq_mutate() {
	if [ "$DRY_RUN" = 1 ]; then
		log "  dry-run: bq --project_id=$PROJECT --location=$REGION $*"
	else
		bqr "$@" >&2
	fi
}
# bq_mutate_stdin runs a bq command with a file on stdin (the DDL), or
# prints it with the file's path so the dry run can be read alongside it.
bq_mutate_stdin() {
	local file="$1"
	shift
	if [ "$DRY_RUN" = 1 ]; then
		log "  dry-run: bq --project_id=$PROJECT --location=$REGION $* < $file"
	else
		bqr "$@" <"$file" >&2
	fi
}
# dataset_json_or_die NAME JSON fails the run unless JSON is one object.
# `set -e` does not reach a function that runs inside a command
# substitution (bash 3.2 and 5.3 both carry on after a failed command
# there), so the helpers below check their inputs and their writes
# themselves and die explicitly: an explicit exit 1 does propagate through
# the substitution and stops the script.
dataset_json_or_die() {
	local name="$1" json="$2"
	printf '%s' "$json" | jq -e 'type == "object"' >/dev/null 2>&1 ||
		die "dataset $name: expected the dataset's JSON object, got: $(printf '%s' "$json" | head -c 120)"
}

# ---------------------------------------------------------------------------
# Dataset access helpers. A dataset's access is a list on the dataset
# resource rather than an IAM binding command, so it is read as JSON, the
# entries it lacks are appended, and it is written back whole with
# `bq update --source` (bq update --help: "Path to file with JSON payload
# for an update"). Authorized views are entries of the same list ({"view":
# {...}}), which is why they are handled here too.
# ---------------------------------------------------------------------------

# access_entry ROLE MEMBER renders one access entry from an IAM-style
# member (domain:, group:, user:, serviceAccount:).
access_entry() {
	local role="$1" member="$2" kind="${2%%:*}" id="${2#*:}"
	case "$kind" in
	domain) printf '{"role":"%s","domain":"%s"}' "$role" "$id" ;;
	group) printf '{"role":"%s","groupByEmail":"%s"}' "$role" "$id" ;;
	user | serviceAccount) printf '{"role":"%s","userByEmail":"%s"}' "$role" "$id" ;;
	*) die "cannot grant dataset access to $member" ;;
	esac
}

# admin_entries ROLE renders the access entries for every principal in
# ADMIN_GRANTEE, as a JSON array, so both datasets grant the same list.
admin_entries() {
	local role="$1" member out=()
	for member in "${admin_members[@]}"; do
		out+=("$(access_entry "$role" "$member")")
	done
	printf '%s' "$(IFS=,; printf '[%s]' "${out[*]}")"
}

# view_entry NAME renders the authorized-view entry for a view of the
# governed dataset.
view_entry() {
	printf '{"view":{"projectId":"%s","datasetId":"%s","tableId":"%s"}}' "$PROJECT" "$DATASET" "$1"
}

# dataset_ensure NAME DESCRIPTION shows or creates the dataset and prints
# its JSON. After a real create the dataset is read back, so the access
# grants that follow land on the first run and not the second. A dry run
# has nothing to read back, so it models what BigQuery attaches to a
# dataset created without an access list (REST reference, Dataset.access:
# projectReaders READER, projectWriters WRITER, projectOwners OWNER and the
# creator OWNER); the creator is the active gcloud account, because that is
# who a real run would create it as. Modelled rather than left empty so the
# dry run prints the removal the real run makes.
dataset_ensure() {
	local name="$1" description="$2" json creator
	if json="$(bqr show --format=json "$PROJECT:$name" 2>/dev/null)"; then
		log "dataset $name: exists"
	else
		log "dataset $name: create"
		bq_mutate mk --dataset --description="$description" "$PROJECT:$name" || die "dataset $name: bq mk failed"
		if [ "$DRY_RUN" = 1 ]; then
			creator="$(gc config get-value account 2>/dev/null || true)"
			[ -n "$creator" ] || creator="the-creator@example.com"
			json="$(jq -n --arg c "$creator" '{access: [
				{role: "READER", specialGroup: "projectReaders"},
				{role: "WRITER", specialGroup: "projectWriters"},
				{role: "OWNER", specialGroup: "projectOwners"},
				{role: "OWNER", userByEmail: $c}]}')"
		else
			json="$(bqr show --format=json "$PROJECT:$name")" || die "dataset $name was created and cannot be read back"
		fi
	fi
	dataset_json_or_die "$name" "$json"
	printf '%s' "$json"
}

# n_entries N prints "1 entry" or "N entries".
n_entries() {
	if [ "$1" = 1 ]; then printf '1 entry'; else printf '%s entries' "$1"; fi
}

# dataset_apply NAME JSON WANTED [UNWANTED_GROUPS] appends the entries of
# the WANTED array that JSON's access list lacks, drops every entry whose
# specialGroup is in the UNWANTED_GROUPS array, and writes the dataset back
# when either changed anything. Prints the merged JSON so a later phase can
# build on it.
#
# An entry is present when an existing entry carries every field the
# wanted one has; extra fields bq may render (it is bq's JSON, not ours)
# do not make it missing, which is what keeps a run from appending a
# duplicate every hour of its life. Entries that are kept are kept as bq
# gave them.
dataset_apply() {
	local name="$1" json="$2" wanted="$3" unwanted="${4:-[]}" result merged missing removed
	dataset_json_or_die "$name" "$json"
	result="$(jq --argjson w "$wanted" --argjson u "$unwanted" '
		def present($have; $e): any($have[]; . as $x | ($e | to_entries | all($x[.key] == .value)));
		def dropped: .specialGroup as $g | ($g != null) and any($u[]; . == $g);
		(.access // []) as $have
		| [ $w[] | select(present($have; .) | not) ] as $add
		| { missing: ($add | length),
		    removed: ([ $have[] | select(dropped) ] | length),
		    doc: (.access = ([ $have[] | select(dropped | not) ] + $add)) }' <<<"$json")"
	missing="$(jq '.missing' <<<"$result")"
	removed="$(jq '.removed' <<<"$result")"
	merged="$(jq -c '.doc' <<<"$result")"
	# Counts, not empty strings: an empty $missing reads as "not 0" below and
	# would send bq an empty payload; that is the shape the polluted-JSON run
	# took before dataset_json_or_die existed.
	[[ "$missing" =~ ^[0-9]+$ && "$removed" =~ ^[0-9]+$ ]] || die "dataset $name: the access merge produced no counts (missing=$missing removed=$removed)"
	if [ "$missing" = 0 ] && [ "$removed" = 0 ]; then
		log "dataset $name access: complete"
	else
		local what=""
		if [ "$missing" != 0 ]; then what="add $(n_entries "$missing")"; fi
		if [ "$removed" != 0 ]; then what="${what:+$what, }remove $(n_entries "$removed")"; fi
		log "dataset $name access: $what"
		printf '%s\n' "$merged" >"$TMP/$name.access.json"
		bq_mutate update --source "$TMP/$name.access.json" "$PROJECT:$name" || die "dataset $name: bq update of the access list failed"
		keep_rendered "$TMP/$name.access.json"
		# Read the list back and check that what was asked for is there.
		# BigQuery accepts an access entry naming a principal it cannot
		# resolve, answers "successfully updated", and stores nothing: a
		# `domain:` entry for a domain that is not a Workspace domain of this
		# organisation is dropped in silence. A governed
		# dataset can go days without a domain grant its default asks for
		# while every run logs success. The script reads
		# back what it wrote, which is the only thing that can tell an
		# applied grant from an accepted one.
		if [ "$DRY_RUN" != 1 ]; then
			local after
			after="$(bqr show --format=json "$PROJECT:$name")" || die "dataset $name: cannot read the access list back after writing it"
			dataset_json_or_die "$name" "$after"
			local unapplied
			unapplied="$(jq -c --argjson want "$(jq '.access' <<<"$merged")" '
				def key: [.role // "", .userByEmail // "", .groupByEmail // "", .domain // "", .specialGroup // "", (.view.tableId // "")] | join("|");
				(.access // []) | map(key) as $have
				| [ $want[] | select((key) as $k | $have | index($k) | not) ]' <<<"$after")"
			if [ "$(jq 'length' <<<"$unapplied")" != 0 ]; then
				die "dataset $name: BigQuery accepted the update and did not store $(jq -c '.' <<<"$unapplied"). A domain entry is dropped when the domain is not a Workspace domain of this organisation (this one is $(gc organizations list --format='value(displayName)' 2>/dev/null | head -1)); name those people individually with --members-grantee instead."
			fi
		fi
	fi
	printf '%s' "$merged"
}

# The members grantee, one access entry per member.
members_entries='[]'
IFS=',' read -ra members <<<"$MEMBERS_GRANTEE"
for m in "${members[@]}"; do
	m="${m//\"/}"
	m="${m// /}"
	members_entries="$(jq --argjson e "$(access_entry READER "$m")" '. + [$e]' <<<"$members_entries")"
done

# The views the governed dataset holds; each is authorized on the raw
# dataset. Read off views.sql so the list cannot drift from the DDL.
governed_views="$(sed -n "s/^CREATE OR REPLACE VIEW \`__PROJECT__\.__DATASET__\.\([A-Za-z0-9_]*\)\`.*/\1/p" "$DIR/bigquery/views.sql")"
[ -n "$governed_views" ] || die "bigquery/views.sql defines no governed view"
view_entries='[]'
for v in $governed_views; do
	view_entries="$(jq --argjson e "$(view_entry "$v")" '. + [$e]' <<<"$view_entries")"
done

# ---------------------------------------------------------------------------
# Phase 1: the bucket. Regional, in the dataset's region (a load job reads
# only from a bucket in the dataset's location), uniform access (object
# ACLs off, so the IAM binding below is the whole story), public access
# prevented, versioned (a partition rewritten with a bad projection is
# recoverable from the previous generation).
# ---------------------------------------------------------------------------

log "== bucket gs://$BUCKET"
if gc storage buckets describe "gs://$BUCKET" >/dev/null 2>&1; then
	log "bucket: exists"
else
	log "bucket: create"
	mutate storage buckets create "gs://$BUCKET" --location="$REGION" \
		--uniform-bucket-level-access --public-access-prevention
fi
mutate storage buckets update "gs://$BUCKET" --versioning

# ---------------------------------------------------------------------------
# Phase 2: the export service account and its bindings.
# ---------------------------------------------------------------------------

log "== service account $SA"
if gc iam service-accounts describe "$SA" >/dev/null 2>&1; then
	log "service account: exists"
else
	log "service account: create"
	mutate iam service-accounts create "$SA_NAME" --display-name="loop-sessions analytics export job"
fi
mutate storage buckets add-iam-policy-binding "gs://$BUCKET" \
	--member="serviceAccount:$SA" --role=roles/storage.objectAdmin
# --condition=None is required, not decorative: the project's IAM policy
# carries conditional bindings (a couple of dozen, typically), and gcloud refuses to add
# an unconditional binding to such a policy in non-interactive mode without
# it ("Adding a binding without specifying a condition to a policy containing
# conditions is prohibited in non-interactive mode"). The first live run
# stopped exactly here.
for role in roles/bigquery.jobUser roles/cloudsql.client roles/logging.logWriter; do
	# --format=none: gcloud otherwise prints the project's whole policy
	# (hundreds of lines) after every binding, which is what buried the
	# failure on the first live run.
	mutate projects add-iam-policy-binding "$PROJECT" --member="serviceAccount:$SA" --role="$role" --condition=None --format=none
done
mutate secrets add-iam-policy-binding "$DB_SECRET" \
	--member="serviceAccount:$SA" --role=roles/secretmanager.secretAccessor

# ---------------------------------------------------------------------------
# Phase 3: the two datasets and who may read them. The raw dataset takes
# the service account as WRITER and every admin grantee as READER, keeps the
# owner entries BigQuery made, and loses the projectReaders and
# projectWriters entries it was created with: it holds the tables with
# every row, and those two groups are every viewer and editor of the
# project. (Project-level BigQuery roles still reach it; the README says so
# and the owner has decided.) The governed dataset takes the members grantee
# and every admin grantee as READER and loses projectWriters (every project
# editor could otherwise redefine a view without its WHERE; see
# GOVERNED_UNWANTED_GROUPS); it holds views only, and the views decide
# which rows a reader gets. The service account gets nothing on the governed
# dataset.
# ---------------------------------------------------------------------------

log "== dataset $PROJECT:$RAW_DATASET ($REGION, loaded by the job; readers come through the views)"
raw_json="$(dataset_ensure "$RAW_DATASET" "loop-sessions analytics export, raw tables (examples/deploy-gcp/analytics). Not for querying: use the views in $DATASET; see examples/deploy-gcp/README.md, Analytics export.")"
raw_wanted="$(jq -n --argjson w "$(access_entry WRITER "serviceAccount:$SA")" --argjson a "$(admin_entries READER)" '[$w] + $a')"
raw_json="$(dataset_apply "$RAW_DATASET" "$raw_json" "$raw_wanted" "$RAW_UNWANTED_GROUPS")"

log "== dataset $PROJECT:$DATASET ($REGION, governed views)"
governed_json="$(dataset_ensure "$DATASET" "loop-sessions analytics export, governed views over $RAW_DATASET (examples/deploy-gcp/analytics). Each view shows a reader their own rows, or every row to the admins; see examples/deploy-gcp/README.md, Analytics export.")"
governed_wanted="$(jq --argjson a "$(admin_entries READER)" '. + $a' <<<"$members_entries")"
governed_json="$(dataset_apply "$DATASET" "$governed_json" "$governed_wanted" "$GOVERNED_UNWANTED_GROUPS")"
: "$governed_json"

# ---------------------------------------------------------------------------
# Phase 4: the taxonomy and its one policy tag, in the dataset's region.
# gcloud creates taxonomies by importing a serialized file (taxonomy.json);
# display names are unique per project and location, so the list decides
# whether to import.
# ---------------------------------------------------------------------------

log "== taxonomy $TAXONOMY ($REGION)"
taxonomy_name="$(gc data-catalog taxonomies list --location="$REGION" \
	--filter="displayName=$TAXONOMY" --format='value(name)' 2>/dev/null || true)"
if [ -n "$taxonomy_name" ]; then
	log "taxonomy: exists ($taxonomy_name)"
else
	log "taxonomy: import $(basename "$DIR")/taxonomy.json"
	mutate data-catalog taxonomies import "$DIR/taxonomy.json" --location="$REGION"
	if [ "$DRY_RUN" = 1 ]; then
		taxonomy_name="projects/$PROJECT/locations/$REGION/taxonomies/dry-run-taxonomy"
	else
		taxonomy_name="$(gc data-catalog taxonomies list --location="$REGION" \
			--filter="displayName=$TAXONOMY" --format='value(name)')"
		[ -n "$taxonomy_name" ] || die "the taxonomy was imported and cannot be listed; check the import output"
	fi
fi
policy_tag=""
if [ "$taxonomy_name" != "projects/$PROJECT/locations/$REGION/taxonomies/dry-run-taxonomy" ]; then
	policy_tag="$(gc data-catalog taxonomies policy-tags list --taxonomy="$taxonomy_name" --location="$REGION" \
		--filter="displayName=$POLICY_TAG_NAME" --format='value(name)' 2>/dev/null || true)"
fi
if [ -z "$policy_tag" ]; then
	if [ "$DRY_RUN" = 1 ]; then
		policy_tag="$taxonomy_name/policyTags/dry-run-policy-tag"
	else
		die "taxonomy $taxonomy_name has no policy tag named $POLICY_TAG_NAME; taxonomy.json defines it, so the import did not land as written"
	fi
fi
log "policy tag: $policy_tag"
# Admins read the tagged columns. The export service account is not bound
# here, on purpose.
#
# --taxonomy takes the taxonomy's short id, not its full resource name:
# `policy-tags list` accepts either, but the per-tag commands (describe,
# add-iam-policy-binding) build the URL as taxonomies/<taxonomy>/policyTags/<tag>
# and a full name in that slot yields a 404 with a doubled path. The
# first live run stopped exactly here, after the taxonomy import.
for member in "${admin_members[@]}"; do
	mutate data-catalog taxonomies policy-tags add-iam-policy-binding "${policy_tag##*/}" \
		--taxonomy="${taxonomy_name##*/}" --location="$REGION" \
		--member="$member" --role=roles/datacatalog.categoryFineGrainedReader
done

# ---------------------------------------------------------------------------
# Phase 5: the DDL, rendered and applied in order: the raw tables, the
# vendor tables, the admins list, the governed views over them. The admin
# emails become a quoted SQL list. The column tags follow in phase 5a; they
# are not DDL. vendor.sql comes before views.sql because all_sessions and
# all_messages select from the tables it declares.
# ---------------------------------------------------------------------------

admin_emails_sql="$(printf '%s' "$ADMIN_EMAILS" | tr ',' '\n' | sed "s/.*/'&'/" | paste -sd, - | sed 's/,/, /g')"
log "== DDL"
for name in tables vendor admins views; do
	src="$DIR/bigquery/$name.sql"
	rendered="$TMP/$name.sql"
	[ -f "$src" ] || die "$src is missing"
	sed -e "s|__PROJECT__|$PROJECT|g" \
		-e "s|__DATASET__|$DATASET|g" \
		-e "s|__RAW_DATASET__|$RAW_DATASET|g" \
		-e "s|__ADMIN_EMAILS__|$admin_emails_sql|g" \
		"$src" >"$rendered"
	if grep -nE '__[A-Z_]+__' "$rendered" >&2; then
		die "$name.sql still carries a placeholder after rendering (above)"
	fi
	log "ddl $name.sql: apply"
	bq_mutate_stdin "$rendered" query --use_legacy_sql=false --nouse_cache
	keep_rendered "$rendered"
done

# ---------------------------------------------------------------------------
# Phase 5a: the column tags on the raw tables. What is tagged and why:
# messages.text and events.body are the transcript itself (contract F);
# sessions.first_prompt and sessions.harness_title are derived from it (the
# opening prompt's first line and the harness's own title), so they are
# tagged too (design.md section 12: text columns are tagged). The tag is
# the one policy tag of the taxonomy above, and it reaches the governed
# views: the column-level security documentation says a user can read a
# tagged column through an authorized view only if they have access to the
# policy tags on the view's underlying tables, so reading messages.text
# through the views needs Fine-Grained Reader on pii_text, which this
# script grants to the admins grantee and never to the export service
# account. A query that does not name a tagged column runs for anyone the
# view's own rule admits.
#
# How: BigQuery's DDL has no column option for policy tags. The first live
# run issued ALTER TABLE ... ALTER COLUMN text SET OPTIONS
# (policy_tags = (...)) and got "Unknown option: policy_tags"; the
# documented ways are the console, the API's tables.patch and `bq update
# --schema`, each of which takes the table's whole schema with the tag on
# the column. So each table's schema is read back, the named columns get
# the tag, and the schema goes back through `bq update --schema`. A table
# whose columns already carry exactly the tag is left alone, which is what
# makes a re-run write nothing. The schema is read, never composed from
# tables.sql: a schema that omits a column the table has would be rejected
# by bq, and one that gets a column's type wrong would be a load failure
# later; the table is the source of truth, this step only decorates it.
# On a dry run over a project without the tables there is no schema to
# read, so the plan names the columns and prints the update without its
# payload.
#
# Known limit: the same documentation says WRITE_TRUNCATE operations
# remove a destination table's policy tags unless the load supplies them
# in its schema, and does not say whether a load into a partition
# decorator (what the job issues) is exempt. If a run ever strips them,
# the exposure is a member reading the text of their OWN rows without the
# reader role (the views still filter rows), until the next provision run
# re-asserts the tags. The runbook's post-run check (README, Runbook:
# export) reads the schema back for exactly this.
# ---------------------------------------------------------------------------

# tag_columns TABLE COLUMN... puts the policy tag on the named columns of a
# raw table, if they do not carry it already.
tag_columns() {
	local table="$1" schema wanted col
	shift
	local cols="$*"
	# bq's stderr is kept rather than dropped: "Not found" is the table that
	# tables.sql has not created yet (a dry run over an empty project), and
	# anything else -- expired credentials, a denied permission, the wrong
	# project -- is a failure that must not be reported as an absent table.
	local err="$TMP/$table.show.err"
	if ! schema="$(bqr show --schema --format=json "$PROJECT:$RAW_DATASET.$table" 2>"$err")"; then
		grep -qi 'not found' "$err" ||
			die "table $table: cannot read the schema: $(tr '\n' ' ' <"$err" | head -c 300)"
		if [ "$DRY_RUN" = 1 ]; then
			log "table $table: no schema to read yet; a real run tags $cols once the DDL has run"
			log "  dry-run: bq --project_id=$PROJECT --location=$REGION update --schema <$table's schema with $POLICY_TAG_NAME on $cols> $PROJECT:$RAW_DATASET.$table"
			return 0
		fi
		die "table $table: the table is absent after the DDL ran: $(tr '\n' ' ' <"$err" | head -c 300)"
	fi
	printf '%s' "$schema" | jq -e 'type == "array" and length > 0' >/dev/null 2>&1 ||
		die "table $table: expected the table's schema as a JSON array, got: $(printf '%s' "$schema" | head -c 120)"
	for col in $cols; do
		printf '%s' "$schema" | jq -e --arg c "$col" 'any(.[]; .name == $c)' >/dev/null 2>&1 ||
			die "table $table: column $col is not in the schema; the DDL and the tag list here disagree"
	done
	wanted="$(printf '%s' "$schema" | jq --arg tag "$policy_tag" --arg cols " $cols " \
		'map(. as $f | if ($cols | index(" " + $f.name + " ")) != null then $f + {policyTags: {names: [$tag]}} else $f end)')"
	if [ "$(printf '%s' "$schema" | jq -cS .)" = "$(printf '%s' "$wanted" | jq -cS .)" ]; then
		log "table $table: $cols tagged"
		return 0
	fi
	log "table $table: tag $cols"
	printf '%s\n' "$wanted" >"$TMP/$table.schema.json"
	bq_mutate update --schema "$TMP/$table.schema.json" "$PROJECT:$RAW_DATASET.$table"
	keep_rendered "$TMP/$table.schema.json"
}

log "== column tags on $RAW_DATASET ($POLICY_TAG_NAME)"
tag_columns messages text
tag_columns events body
tag_columns sessions first_prompt harness_title
tag_columns vendor_messages text
tag_columns vendor_sessions first_prompt harness_title

# ---------------------------------------------------------------------------
# Phase 5b: authorize the governed views on the raw dataset, now that the
# views exist. The raw dataset is read again on a real run because the
# views were just created or replaced; a dry run has only what phase 3
# left it.
# ---------------------------------------------------------------------------

log "== authorized views on $RAW_DATASET"
if [ "$DRY_RUN" != 1 ]; then
	raw_json="$(bqr show --format=json "$PROJECT:$RAW_DATASET")" || die "cannot read dataset $RAW_DATASET back before authorizing the views"
fi
log "views: $(printf '%s' "$governed_views" | tr '\n' ' ')"
raw_json="$(dataset_apply "$RAW_DATASET" "$raw_json" "$view_entries" "$RAW_UNWANTED_GROUPS")"
: "$raw_json"

# ---------------------------------------------------------------------------
# Phase 6: the Cloud Run job, from job.yaml.
# ---------------------------------------------------------------------------

log "== cloud run job $JOB"
rendered="$TMP/job.yaml"
sed -e "s|__PROJECT__|$PROJECT|g" \
	-e "s|__REGION__|$REGION|g" \
	-e "s|__INSTANCE__|$INSTANCE|g" \
	-e "s|__TAG__|$TAG|g" \
	-e "s|__BUCKET__|$BUCKET|g" \
	-e "s|__RAW_DATASET__|$RAW_DATASET|g" \
	"$DIR/job.yaml" >"$rendered"
if grep -nE '__[A-Z_]+__' "$rendered" >&2; then
	die "job.yaml still carries a placeholder after rendering (above)"
fi
if gc run jobs describe "$JOB" --region="$REGION" >/dev/null 2>&1; then
	log "job: exists, replace with image tag $TAG"
else
	log "job: create with image tag $TAG"
fi
mutate run jobs replace "$rendered" --region="$REGION"
keep_rendered "$rendered"

# ---------------------------------------------------------------------------
# Phase 7: the schedule.
# ---------------------------------------------------------------------------

scheduler_args=(--project="$PROJECT" --region="$REGION")
if [ "$DRY_RUN" = 1 ]; then
	scheduler_args+=(--dry-run)
fi
PROJECT="$PROJECT" REGION="$REGION" JOB="$JOB" SA_NAME="$SA_NAME" GCLOUD="$GCLOUD" \
	"$DIR/scheduler.sh" "${scheduler_args[@]}"

if [ "$DRY_RUN" = 1 ]; then
	log "== dry run complete; nothing was changed. Rendered files: $RENDER_DIR"
else
	log "== provisioned. Next: scheduler.sh --run-now, then the post-run checks (README, Runbook: export)"
fi
