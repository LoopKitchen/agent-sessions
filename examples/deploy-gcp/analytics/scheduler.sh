#!/usr/bin/env bash
# The Cloud Scheduler half of the analytics export: one job that runs the
# Cloud Run job loop-sessions-export every hour, and the two bindings it
# needs to be allowed to. provision.sh calls this after the Cloud Run job
# exists; it is also the operator's handle on the schedule (pause, resume,
# run now) once everything is provisioned.
#
# Update-or-create, so running it twice is safe: the scheduler job is
# described first and updated if it is there; IAM bindings are idempotent
# by construction (adding a binding that exists changes nothing).
#
# How the trigger authenticates, and why it is OAuth and not OIDC: the
# schedule calls the Cloud Run Admin API's jobs.run endpoint on
# run.googleapis.com, and Google APIs take an OAuth access token; an OIDC
# identity token is what a Cloud Run service or a Cloud Function URL takes.
# The Cloud Run documentation's own recipe for scheduling a job uses
# --oauth-service-account-email against
# https://run.googleapis.com/v2/projects/P/locations/R/jobs/J:run
# (docs.cloud.google.com/run/docs/execute/jobs-on-schedule). The identity is the export job's own service account, which
# needs roles/run.invoker on the job; the Cloud Scheduler service agent
# needs its own role on the project to mint tokens for that account, which
# projects created after 2019-03 carry already and this script asserts
# anyway.
#
# Usage:
#   examples/deploy-gcp/analytics/scheduler.sh [--dry-run] [--project=P] [--region=R]
#   examples/deploy-gcp/analytics/scheduler.sh --pause | --resume | --run-now
#
#   --dry-run   read everything, print every write, mutate nothing.
#   --pause     pause the schedule (an incident on the primary, a bad DDL)
#   --resume    resume it
#   --run-now   start one execution of the Cloud Run job outside the
#               schedule (after provisioning, or to catch up faster)
#
# GCLOUD may be overridden from the environment so the script can be
# exercised against a recording stub; the tests do that.
set -euo pipefail

PROJECT="${PROJECT:-__PROJECT__}"
REGION="${REGION:-__REGION__}"
JOB="${JOB:-loop-sessions-export}"
SCHEDULER_JOB="${SCHEDULER_JOB:-loop-sessions-export-hourly}"
# Seven minutes past the hour: off the top of the hour that every other
# cron in the project favours, and after the derive runner's dirty pass
# has had a few cycles over the hour's ingest.
SCHEDULE="${SCHEDULE:-7 * * * *}"
SA_NAME="${SA_NAME:-loop-sessions-export}"
GCLOUD="${GCLOUD:-gcloud}"
DRY_RUN=0
ACTION=apply

log() { printf '%s\n' "$*" >&2; }
die() { log "scheduler.sh: $*"; exit 1; }

for arg in "$@"; do
	case "$arg" in
	--dry-run) DRY_RUN=1 ;;
	--project=*) PROJECT="${arg#--project=}" ;;
	--region=*) REGION="${arg#--region=}" ;;
	--pause) ACTION=pause ;;
	--resume) ACTION=resume ;;
	--run-now) ACTION=run ;;
	-h | --help)
		sed -n '2,38p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) die "unknown argument: $arg (see --help)" ;;
	esac
done

command -v "$GCLOUD" >/dev/null 2>&1 || die "$GCLOUD is required"
[[ "$PROJECT" =~ ^[a-z][a-z0-9-]+$ ]] || die "PROJECT=$PROJECT is not a project id"
[[ "$REGION" =~ ^[a-z0-9-]+$ ]] || die "REGION=$REGION is not a region"

SA="${SA_NAME}@${PROJECT}.iam.gserviceaccount.com"

gc() { "$GCLOUD" --project="$PROJECT" "$@"; }
mutate() {
	if [ "$DRY_RUN" = 1 ]; then
		log "  dry-run: gcloud $*"
	else
		gc --quiet "$@"
	fi
}

case "$ACTION" in
pause)
	log "== pause $SCHEDULER_JOB"
	mutate scheduler jobs pause "$SCHEDULER_JOB" --location="$REGION"
	exit 0
	;;
resume)
	log "== resume $SCHEDULER_JOB"
	mutate scheduler jobs resume "$SCHEDULER_JOB" --location="$REGION"
	exit 0
	;;
run)
	log "== execute $JOB once"
	mutate run jobs execute "$JOB" --region="$REGION" --wait
	exit 0
	;;
esac

# ---------------------------------------------------------------------------
# The trigger identity may invoke the job, and Scheduler may act as it.
# ---------------------------------------------------------------------------

log "== invoker"
mutate run jobs add-iam-policy-binding "$JOB" --region="$REGION" \
	--member="serviceAccount:$SA" --role=roles/run.invoker

project_number="$(gc projects describe "$PROJECT" --format='value(projectNumber)')"
[ -n "$project_number" ] || die "could not read the project number"
scheduler_agent="service-${project_number}@gcp-sa-cloudscheduler.iam.gserviceaccount.com"
log "== scheduler service agent ($scheduler_agent)"
# --condition=None for the same reason provision.sh gives on its project
# bindings: the project policy carries conditional bindings and gcloud
# refuses an unconditional add in non-interactive mode without the flag.
mutate projects add-iam-policy-binding "$PROJECT" \
	--member="serviceAccount:$scheduler_agent" --role=roles/cloudscheduler.serviceAgent --condition=None --format=none

# ---------------------------------------------------------------------------
# The schedule itself, update-or-create.
# ---------------------------------------------------------------------------

log "== schedule"
uri="https://run.googleapis.com/v2/projects/${PROJECT}/locations/${REGION}/jobs/${JOB}:run"
if gc scheduler jobs describe "$SCHEDULER_JOB" --location="$REGION" >/dev/null 2>&1; then
	verb=update
else
	verb=create
fi
log "scheduler job $SCHEDULER_JOB: $verb ($SCHEDULE UTC -> $uri)"
# --attempt-deadline bounds the HTTP call that STARTS the execution, not the
# execution; jobs.run answers as soon as the execution is created.
mutate scheduler jobs "$verb" http "$SCHEDULER_JOB" --location="$REGION" \
	--schedule="$SCHEDULE" --time-zone=Etc/UTC \
	--uri="$uri" --http-method=POST \
	--oauth-service-account-email="$SA" \
	--attempt-deadline=180s \
	--description="loop-sessions analytics export, hourly (examples/deploy-gcp/analytics)"

if [ "$DRY_RUN" = 1 ]; then
	log "== scheduler dry run complete; nothing was changed"
else
	log "== scheduler applied"
fi
