#!/usr/bin/env bash
# Applies the monitoring assets in this directory to the GCP project:
# log-based metrics, the /readyz uptime check, and the alert policies.
#
# Everything is update-or-create, so running it twice is safe and running it
# after editing a file is how a change ships. Three things make that true and
# each of them was a way the naive version broke:
#
#   - `gcloud logging metrics create` fails when the metric exists, so every
#     metric is described first and updated if it is there.
#   - `gcloud monitoring uptime create` creates a second check on every run,
#     so the check is looked up by displayName first and its id is read from
#     whichever of the two paths produced it.
#   - A policy that names a log-based metric created seconds earlier fails
#     until the metric descriptor has propagated to Monitoring, so the
#     policies phase waits (up to DESCRIPTOR_WAIT seconds) for every metric a
#     policy references before creating any policy.
#
# Policies are found by `userLabels.app="loop-sessions"` and their displayName,
# never by a stored id, so the files here are the whole desired state and the
# console is not a second place to edit them.
#
# Placeholders in policies/*.json (__PROJECT__, __PUBLIC_URL__,
# __RELEASE_BUCKET__, __ANALYTICS_BUCKET__, __SLACK_INFRA__, __SLACK_FLEET__,
# __EMAIL_OWNER__, __UPTIME_CHECK_ID__) are rendered from the project,
# PUBLIC_URL, RELEASE_BUCKET and ANALYTICS_BUCKET in the environment,
# channels.env and the uptime check; __PUBLIC_HOST__ in uptime/readyz.json
# is the host of PUBLIC_URL. Any placeholder left after rendering
# fails the run: a policy shipped with a channel of "" is a policy nobody
# hears, and an operator who forgot to fill channels.env should find out here
# rather than in an incident.
#
# Usage:
#   examples/deploy-gcp/monitoring/apply.sh [--dry-run] [--project=PROJECT]
#
#   --dry-run   read everything, including the metric descriptor GETs, print
#               what would be created or updated, mutate nothing. Run it
#               before every real apply.
#
# Needs: gcloud (authenticated with roles/monitoring.editor and
# roles/logging.configWriter on the project), jq, curl. GCLOUD, CURL and
# DESCRIPTOR_WAIT may be overridden from the environment; the first two exist
# so the script can be exercised against a recording stub.
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT="${PROJECT:-__PROJECT__}"
# The service's public base URL and the release and analytics buckets,
# spliced into the policies' documentation. All come from the environment.
PUBLIC_URL="${PUBLIC_URL:-}"
RELEASE_BUCKET="${RELEASE_BUCKET:-}"
ANALYTICS_BUCKET="${ANALYTICS_BUCKET:-}"
PUBLIC_HOST="${PUBLIC_URL#*://}"
PUBLIC_HOST="${PUBLIC_HOST%%/*}"
GCLOUD="${GCLOUD:-gcloud}"
CURL="${CURL:-curl}"
DESCRIPTOR_WAIT="${DESCRIPTOR_WAIT:-60}"
DRY_RUN=0

log() { printf '%s\n' "$*" >&2; }
die() { log "apply.sh: $*"; exit 1; }

# id_shape refuses a value that is not a bare resource id. The values are
# spliced into the policy files by sed, whose replacement side reads |, & and
# \ as syntax, so a stray character would corrupt a policy silently and past
# the placeholder check. Every legitimate value (a project id, a notification
# channel id, an uptime check id) fits the shape, and checking it once here
# beats escaping it in four places.
id_shape() {
	[[ "$2" =~ ^[A-Za-z0-9._-]+$ ]] || die "$1=$2 is not a resource id (letters, digits, . _ -)"
}

for arg in "$@"; do
	case "$arg" in
	--dry-run) DRY_RUN=1 ;;
	--project=*) PROJECT="${arg#--project=}" ;;
	-h | --help)
		sed -n '2,36p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*) die "unknown argument: $arg (see --help)" ;;
	esac
done

command -v jq >/dev/null 2>&1 || die "jq is required"
command -v "$GCLOUD" >/dev/null 2>&1 || die "$GCLOUD is required"

# gc runs a read; mutate runs a write, or prints it under --dry-run. Every
# gcloud call goes through one of the two so the dry run cannot slip a write.
gc() { "$GCLOUD" --project="$PROJECT" "$@"; }
mutate() {
	if [ "$DRY_RUN" = 1 ]; then
		log "  dry-run: gcloud $*"
	else
		gc --quiet "$@"
	fi
}

# ---------------------------------------------------------------------------
# Channels: every id must be present before anything is rendered.
# ---------------------------------------------------------------------------

# The environment wins over the file, so an operator can apply with an id
# that is not committed (the owner's email channel) and a test can run the
# script without editing it.
env_SLACK_INFRA="${SLACK_INFRA:-}"
env_SLACK_FLEET="${SLACK_FLEET:-}"
env_EMAIL_OWNER="${EMAIL_OWNER:-}"
# shellcheck source=channels.env
# shellcheck disable=SC1091
. "$DIR/channels.env"
SLACK_INFRA="${env_SLACK_INFRA:-${SLACK_INFRA:-}}"
SLACK_FLEET="${env_SLACK_FLEET:-${SLACK_FLEET:-}}"
EMAIL_OWNER="${env_EMAIL_OWNER:-${EMAIL_OWNER:-}}"
# SLACK_FLEET is empty in the repository until the console step is done;
# the refusal below names it, which is the point: the fleet alerts must not
# ship to nobody.
for var in PUBLIC_URL RELEASE_BUCKET ANALYTICS_BUCKET; do
	[ -n "${!var:-}" ] || die "$var is empty; export it (the policies link to the service and name the buckets)"
	case "${!var}" in *[\|\&\\]*) die "$var must not contain |, & or \\" ;; esac
done
for var in SLACK_INFRA SLACK_FLEET EMAIL_OWNER; do
	[ -n "${!var:-}" ] || die "channels.env: $var is empty. A policy with no channel is one nobody hears; see the comment in channels.env for how to create it, or pass it in the environment."
	id_shape "$var" "${!var}"
done
id_shape PROJECT "$PROJECT"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

# ---------------------------------------------------------------------------
# Phase 1: log-based metrics, update-or-create by name.
# ---------------------------------------------------------------------------

log "== metrics"
for f in "$DIR"/metrics/*.yaml; do
	name="$(sed -n 's/^name: *//p' "$f" | head -1)"
	[ -n "$name" ] || die "$f has no name: line"
	if gc logging metrics describe "$name" >/dev/null 2>&1; then
		log "metric $name: update ($(basename "$f"))"
		mutate logging metrics update "$name" --config-from-file="$f"
	else
		log "metric $name: create ($(basename "$f"))"
		mutate logging metrics create "$name" --config-from-file="$f"
	fi
done

# ---------------------------------------------------------------------------
# Phase 2: the uptime check, looked up by displayName first.
# ---------------------------------------------------------------------------

log "== uptime"
UPTIME="$DIR/uptime/readyz.json"
display="$(jq -r .displayName "$UPTIME")"
existing="$(gc monitoring uptime list-configs --filter="displayName=\"$display\"" --format='value(name)' 2>/dev/null || true)"
if [ -n "$existing" ]; then
	# One line expected; a second check with the same name is somebody's
	# console experiment and this script refuses to guess which one is real.
	if [ "$(printf '%s\n' "$existing" | wc -l | tr -d ' ')" != 1 ]; then
		die "more than one uptime check is named \"$display\"; delete the extra in the console before applying"
	fi
	UPTIME_CHECK_ID="${existing##*/}"
	log "uptime \"$display\": exists ($UPTIME_CHECK_ID)"
else
	args=(
		"$display"
		--resource-type=uptime-url
		--resource-labels="host=$(jq -r .host "$UPTIME" | sed "s|__PUBLIC_HOST__|$PUBLIC_HOST|"),project_id=$PROJECT"
		--protocol="$(jq -r .protocol "$UPTIME")"
		--port="$(jq -r .port "$UPTIME")"
		--path="$(jq -r .path "$UPTIME")"
		--period="$(jq -r .period "$UPTIME")"
		--timeout="$(jq -r .timeout "$UPTIME")"
		--regions="$(jq -r '.regions | join(",")' "$UPTIME")"
		--matcher-type="$(jq -r .matcherType "$UPTIME")"
		--matcher-content="$(jq -r .matcherContent "$UPTIME")"
		"--user-labels=app=loop-sessions,tier=infra"
	)
	if [ "$DRY_RUN" = 1 ]; then
		log "uptime \"$display\": create"
		log "  dry-run: gcloud monitoring uptime create ${args[*]}"
		# The policy that needs the id is rendered against this marker so the
		# dry run can still show the rest; it is never a value a real apply
		# accepts, because the real apply takes the other branch.
		UPTIME_CHECK_ID="dry-run-uptime-check-id"
	else
		log "uptime \"$display\": create"
		created="$(gc --quiet monitoring uptime create "${args[@]}" --format='value(name)')"
		[ -n "$created" ] || die "uptime create returned no name"
		UPTIME_CHECK_ID="${created##*/}"
	fi
fi
id_shape UPTIME_CHECK_ID "$UPTIME_CHECK_ID"

# ---------------------------------------------------------------------------
# Phase 3: wait for every metric a policy references to exist as a metric
# descriptor in Monitoring. Creation returns before propagation, and a policy
# created in that window is rejected. The GET is a read, so the dry run makes
# it too and reports what it finds: the first version skipped it under
# --dry-run and shipped a URL the API rejects, which no rehearsal could see.
# ---------------------------------------------------------------------------

referenced="$(grep -ho 'logging\.googleapis\.com/user/[A-Za-z0-9_/]*' "$DIR"/policies/*.json | sort -u)"
log "== descriptors"
token="$(gc auth print-access-token)"

# descriptor_status prints the HTTP status of a GET on the metric's
# descriptor, 000 when curl got no answer at all.
descriptor_status() {
	# The metric type goes into the path as it is, slashes and all. The API
	# answers 400 "Invalid metric name" to the form with them escaped as %2F,
	# and the first version of this loop sent exactly that, so every real
	# apply died here telling the operator to wait for a descriptor that had
	# been there all along.
	local code
	code="$("$CURL" -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" \
		"https://monitoring.googleapis.com/v3/projects/$PROJECT/metricDescriptors/$1")" || code=000
	printf '%s' "$code"
}

for metric in $referenced; do
	waited=0
	while :; do
		status="$(descriptor_status "$metric")"
		case "$status" in
		2*)
			log "descriptor $metric: present"
			break
			;;
		404)
			if [ "$DRY_RUN" = 1 ]; then
				# The expected state before the first apply; the real run
				# creates the metric and then waits for this.
				log "descriptor $metric: absent (a real apply waits up to ${DESCRIPTOR_WAIT}s for it after creating the metric)"
				break
			fi
			if [ "$waited" -ge "$DESCRIPTOR_WAIT" ]; then
				die "metric descriptor $metric did not appear within ${DESCRIPTOR_WAIT}s; re-run once it has (gcloud logging metrics describe ${metric#logging.googleapis.com/user/})"
			fi
			sleep 5
			waited=$((waited + 5))
			;;
		*)
			# Not propagation: a bad token (401), a missing role (403), a
			# malformed name (400), no network (000). Waiting on any of them
			# would only turn it into the propagation message above.
			die "GET metricDescriptors/$metric answered HTTP $status, which is not a propagation delay; check the token, the role and the URL"
			;;
		esac
	done
done

# ---------------------------------------------------------------------------
# Phase 4: policies, rendered then update-or-create by label and displayName.
# ---------------------------------------------------------------------------

log "== policies"
for f in "$DIR"/policies/*.json; do
	rendered="$TMP/$(basename "$f")"
	sed -e "s|__PROJECT__|$PROJECT|g" \
		-e "s|__PUBLIC_URL__|$PUBLIC_URL|g" \
		-e "s|__RELEASE_BUCKET__|$RELEASE_BUCKET|g" \
		-e "s|__ANALYTICS_BUCKET__|$ANALYTICS_BUCKET|g" \
		-e "s|__SLACK_INFRA__|$SLACK_INFRA|g" \
		-e "s|__SLACK_FLEET__|$SLACK_FLEET|g" \
		-e "s|__EMAIL_OWNER__|$EMAIL_OWNER|g" \
		-e "s|__UPTIME_CHECK_ID__|$UPTIME_CHECK_ID|g" \
		"$f" >"$rendered"
	if grep -nE '__[A-Z_]+__' "$rendered" >&2; then
		die "$(basename "$f") still carries a placeholder after rendering (above); it has no value in this script"
	fi
	jq -e . "$rendered" >/dev/null || die "$(basename "$f") is not valid JSON after rendering"
	display="$(jq -r .displayName "$rendered")"
	# The lookup key is userLabels.asset, which is this file's name, and NOT
	# the display name. A display name is prose an operator is meant to
	# improve, and keying on it means the first such improvement silently
	# orphans the live policy and creates a second one beside it. That is not
	# hypothetical: when policy 08's title changed from "more than
	# 5%" to "more than 10%", the apply created a second policy, and the
	# stale one went on alerting at the old threshold until it was deleted by
	# hand. The label is asserted to equal the file stem by the asset test,
	# so a new policy file cannot forget it.
	asset="$(jq -r '.userLabels.asset // empty' "$rendered")"
	[ -n "$asset" ] || die "$(basename "$f") has no userLabels.asset; it is the key this script matches on"
	if [ "$asset" != "$(basename "$f" .json)" ]; then
		die "$(basename "$f") carries userLabels.asset=\"$asset\"; it must equal the file name"
	fi
	existing="$(gc monitoring policies list --filter="userLabels.app=\"loop-sessions\" AND userLabels.asset=\"$asset\"" --format='value(name)' 2>/dev/null || true)"
	if [ -z "$existing" ]; then
		# Adoption, for one apply only: every policy live before this label
		# existed carries no asset, so the lookup above finds nothing and the
		# run would create a duplicate of all of them. Fall back to the
		# display name once; the update writes the label, and the next apply
		# finds it by key. Delete this branch when no live policy is missing
		# an asset label, which the log line below makes visible.
		existing="$(gc monitoring policies list --filter="userLabels.app=\"loop-sessions\" AND displayName=\"$display\"" --format='value(name)' 2>/dev/null || true)"
		if [ -n "$existing" ]; then
			log "policy \"$display\": adopting a policy that predates the asset label"
		fi
	fi
	if [ -n "$existing" ]; then
		if [ "$(printf '%s\n' "$existing" | wc -l | tr -d ' ')" != 1 ]; then
			die "more than one policy carries asset=\"$asset\" under app=loop-sessions; delete the extra before applying"
		fi
		log "policy \"$display\": update ($(basename "$f"))"
		mutate monitoring policies update "$existing" --policy-from-file="$rendered"
	else
		log "policy \"$display\": create ($(basename "$f"))"
		mutate monitoring policies create --policy-from-file="$rendered"
	fi
done

if [ "$DRY_RUN" = 1 ]; then
	log "== dry run complete; nothing was changed"
else
	log "== applied"
fi
