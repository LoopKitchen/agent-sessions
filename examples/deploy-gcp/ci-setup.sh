#!/bin/sh
#
# One-time setup for the GitHub Actions release job (examples/deploy-gcp/release.yml.example).
#
# Creates, idempotently, everything the job authenticates through and writes
# to, and nothing else:
#
#   1. the loop-sessions-release service account, which the ungated push job
#      uses, and the loop-sessions-promote service account, which only the
#      manually dispatched promote job can reach
#   2. their least-privilege bindings:
#        release: roles/storage.objectAdmin on gs://__RELEASE_BUCKET__,
#                 conditioned to everything EXCEPT objects/latest/ (publish
#                 builds/ and canary/; the fleet's pointer is out of reach)
#        release: roles/artifactregistry.writer on the loop-sessions Docker
#                 repository (push the server image)
#        promote: roles/storage.objectAdmin on gs://__RELEASE_BUCKET__,
#                 unconditional (it reads builds/<sha>/ and writes latest/)
#   3. the Workload Identity pool "github" and its OIDC provider "loop-sessions",
#      whose attribute condition admits ONLY __GITHUB_REPO__
#   4. roles/iam.workloadIdentityUser on each account: the release account for
#      that repository, the promote account for that repository's "production"
#      environment only
#
# That is one binding more than the two the PR D contract names (bucket
# objectAdmin and workloadIdentityUser): release.yml builds the server image
# from the same sha as the client ("both halves") and pushes it from the
# runner, which needs write on one Artifact Registry repository and nothing
# else. The account holds no project-level role and may act as no other
# account. An earlier revision of this script had the workflow submit the
# build to Cloud Build instead, which needed cloudbuild.builds.editor,
# objectAdmin on the _cloudbuild bucket and actAs on the Cloud Build default
# service account; that account holds run.admin and Secret Manager access on
# this project, so the ungated build-and-canary job could have deployed a
# Cloud Run revision or read a secret through a build config. If that
# revision was ever applied, revoke it by hand (read-only checks first):
#   gcloud projects remove-iam-policy-binding "$PROJECT" --member="serviceAccount:$SA" --role=roles/cloudbuild.builds.editor
#   gcloud storage buckets remove-iam-policy-binding "gs://${PROJECT}_cloudbuild" --member="serviceAccount:$SA" --role=roles/storage.objectAdmin
#   gcloud iam service-accounts remove-iam-policy-binding "$CLOUDBUILD_SA" --member="serviceAccount:$SA" --role=roles/iam.serviceAccountUser
# r7 R2's roles/run.admin and actAs on loop-sessions-run@ are likewise NOT
# granted: the workflow has no deploy step, and an account that can only
# build and publish cannot replace what is serving.
#
# No service-account key is created anywhere. The job's OIDC token lives five
# minutes and is exchanged for the account per run.
#
# What this script cannot do, and a person must, before the promote job works:
#   - in the GitHub repository settings, an environment named "production"
#     (Settings > Environments). The promote job declares
#     `environment: production` so its runs are attributed to it. A
#     required-reviewers rule on that environment is the approval step if
#     your GitHub plan allows one; without it, the human gate is that the
#     promote job runs only by manual workflow_dispatch, started by a person
#     with write access who names the build. Nothing in the workflow needs
#     to change either way.
#   - enable object versioning on the bucket if it is not (a one-line safety
#     net independent of builds/<sha>/):
#       gcloud storage buckets update gs://__RELEASE_BUCKET__ --versioning
#
# The two accounts exist because otherwise only the workflow's text stops the
# ungated push job from moving latest/: one account with objectAdmin over the
# whole bucket, reachable by any run of the repository, is one edited workflow
# away from shipping an unreviewed build to every laptop. Now IAM says no.
#
# How the promote account is reached and nothing else is: GitHub puts an
# "environment" claim in a job's OIDC token only when the job declares one, so
# the provider maps a composite attribute
#   attribute.repo_env = assertion.repository + ':' +
#                        (has(assertion.environment) ? assertion.environment : '')
# and the promote account's workloadIdentityUser binding names exactly
# "__GITHUB_REPO__:production". A push job's token carries no
# environment, so its repo_env ends in ':' and matches no binding on that
# account; a job in another environment gets that environment's name and
# likewise matches nothing. The composite is needed because a principalSet
# names one attribute: binding on attribute.environment alone would admit any
# repository's "production". The ternary is needed because a mapping that
# reads a claim the token lacks fails the exchange, which would break the push
# job.
#
# Usage:
#   examples/deploy-gcp/ci-setup.sh --dry-run   print every mutation without running it
#   examples/deploy-gcp/ci-setup.sh             apply
#
# Every mutation is preceded by a describe of the same resource, so a second
# run changes nothing and prints what already exists. Read-only commands run
# in --dry-run too; nothing is created, bound or deleted.

set -eu

PROJECT="${PROJECT:-__PROJECT__}"
PROJECT_NUMBER="${PROJECT_NUMBER:-__PROJECT_NUMBER__}"
REGION="${REGION:-__REGION__}"
BUCKET="${BUCKET:-__RELEASE_BUCKET__}"
REPO="${REPO:-__GITHUB_REPO__}"
SA_NAME="${SA_NAME:-loop-sessions-release}"
PROMOTE_SA_NAME="${PROMOTE_SA_NAME:-loop-sessions-promote}"
IMAGE_REPO="${IMAGE_REPO:-loop-sessions}"
POOL="${POOL:-github}"
PROVIDER="${PROVIDER:-loop-sessions}"

SA="${SA_NAME}@${PROJECT}.iam.gserviceaccount.com"
PROMOTE_SA="${PROMOTE_SA_NAME}@${PROJECT}.iam.gserviceaccount.com"
# Named only so the revocation commands in the header can be pasted as is.
CLOUDBUILD_SA="${PROJECT_NUMBER}@cloudbuild.gserviceaccount.com"
export CLOUDBUILD_SA
PRINCIPAL="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${POOL}/attribute.repository/${REPO}"
PROMOTE_PRINCIPAL="principalSet://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${POOL}/attribute.repo_env/${REPO}:production"
# The mapping the provider must carry. repo_env is what makes the promote
# account reachable only from a job that declares the production environment;
# see the header for why it is a composite and why the ternary is not optional.
ATTRIBUTE_MAPPING="google.subject=assertion.sub,attribute.actor=assertion.actor,attribute.repository=assertion.repository,attribute.repository_owner=assertion.repository_owner,attribute.repo_env=assertion.repository + ':' + (has(assertion.environment) ? assertion.environment : '')"
# The release account may write everything in the bucket except the fleet's
# pointer. A bucket-level call (a list) carries the bucket's own resource
# name, which does not start with the objects prefix, so it stays allowed.
NO_LATEST_TITLE="no-latest"
NO_LATEST_EXPRESSION="!resource.name.startsWith(\"projects/_/buckets/${BUCKET}/objects/latest/\")"

DRY_RUN=0
for arg in "$@"; do
	case "$arg" in
	--dry-run) DRY_RUN=1 ;;
	-h | --help)
		awk 'NR > 1 && /^set -eu/ { exit } NR > 1' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	*)
		printf 'unknown argument: %s\n' "$arg" >&2
		exit 2
		;;
	esac
done

command -v jq >/dev/null 2>&1 || {
	printf 'jq is required: the bucket policy is read as JSON, because a conditional binding and an unconditional one for the same account and role differ only in a field that a text match cannot separate reliably.\n' >&2
	exit 1
}

say() { printf '==> %s\n' "$*"; }

# run prints a mutation and executes it unless --dry-run.
run() {
	printf '    %s\n' "$*"
	if [ "$DRY_RUN" -eq 1 ]; then
		return 0
	fi
	"$@"
}

# exists runs a describe quietly and reports its status.
exists() {
	"$@" >/dev/null 2>&1
}

say "project ${PROJECT} (${PROJECT_NUMBER}), repository ${REPO}"
if [ "$DRY_RUN" -eq 1 ]; then
	say "dry run: nothing below is executed"
fi

# 1. Service account.
say "service account ${SA}"
if exists gcloud iam service-accounts describe "$SA" --project="$PROJECT"; then
	printf '    exists\n'
else
	run gcloud iam service-accounts create "$SA_NAME" --project="$PROJECT" \
		--display-name="loop-sessions release (GitHub Actions)"
fi

say "service account ${PROMOTE_SA}"
if exists gcloud iam service-accounts describe "$PROMOTE_SA" --project="$PROJECT"; then
	printf '    exists\n'
else
	run gcloud iam service-accounts create "$PROMOTE_SA_NAME" --project="$PROJECT" \
		--display-name="loop-sessions promote to latest (GitHub Actions, production environment)"
fi

# 2. Bindings. add-iam-policy-binding is idempotent by itself (a binding that
# exists is left as is), but each is checked first so a dry run reports the
# truth rather than "would add" for everything. No project-level binding is
# made: everything the job may do is scoped to one bucket and one repository.

bucket_policy() { gcloud storage buckets get-iam-policy "gs://${BUCKET}" --format=json 2>/dev/null; }

# has_binding ROLE ACCOUNT CONDITION_TITLE: is there a binding for that role
# and account whose condition title matches? An empty title asks for the
# unconditional binding. The policy is read as JSON because the two differ
# only by the presence of a condition object.
has_binding() {
	bucket_policy | jq -e --arg role "$1" --arg member "serviceAccount:$2" --arg title "$3" \
		'any(.bindings[]?;
		     .role == $role
		     and (.members // [] | index($member)) != null
		     and (if $title == "" then (.condition | not) else (.condition.title? == $title) end))' >/dev/null 2>&1
}

say "bucket binding roles/storage.objectAdmin for ${PROMOTE_SA} on gs://${BUCKET}"
if has_binding roles/storage.objectAdmin "$PROMOTE_SA" ""; then
	printf '    exists\n'
else
	run gcloud storage buckets add-iam-policy-binding "gs://${BUCKET}" \
		--member="serviceAccount:${PROMOTE_SA}" --role=roles/storage.objectAdmin
fi

# The release account's binding is conditional, and the order below matters:
# the conditional binding is added first and the unconditional one removed
# after, so a release run in flight never finds the account without access.
# gcloud has no "make this binding conditional" verb; these two calls are it.
# The removal passes --condition=None, not --all: --all would take the
# conditional binding with it and leave the account unable to publish at all.
say "bucket binding roles/storage.objectAdmin for ${SA} on gs://${BUCKET}, everything except latest/"
if has_binding roles/storage.objectAdmin "$SA" "$NO_LATEST_TITLE"; then
	printf '    exists\n'
else
	run gcloud storage buckets add-iam-policy-binding "gs://${BUCKET}" \
		--member="serviceAccount:${SA}" --role=roles/storage.objectAdmin \
		--condition="title=${NO_LATEST_TITLE},description=the fleet pointer is the promote account's,expression=${NO_LATEST_EXPRESSION}"
fi
if has_binding roles/storage.objectAdmin "$SA" ""; then
	say "removing the unconditional objectAdmin binding for ${SA}"
	run gcloud storage buckets remove-iam-policy-binding "gs://${BUCKET}" \
		--member="serviceAccount:${SA}" --role=roles/storage.objectAdmin --condition=None
else
	printf '    no unconditional binding to remove\n'
fi

# The Docker repository the server image is pushed to (examples/deploy-gcp/README.md
# section 1 creates it). Writer on this one repository is the whole of what
# a push needs; no project-level Artifact Registry role is granted.
say "repository binding roles/artifactregistry.writer on ${IMAGE_REPO} (${REGION})"
if gcloud artifacts repositories get-iam-policy "$IMAGE_REPO" --project="$PROJECT" --location="$REGION" --format=json 2>/dev/null |
	tr -d '\n ' | grep -q "\"members\":\[[^]]*\"serviceAccount:${SA}\"[^]]*\],\"role\":\"roles/artifactregistry.writer\""; then
	printf '    exists\n'
else
	run gcloud artifacts repositories add-iam-policy-binding "$IMAGE_REPO" --project="$PROJECT" --location="$REGION" \
		--member="serviceAccount:${SA}" --role=roles/artifactregistry.writer
fi

# 3. Workload Identity pool and provider.
say "workload identity pool ${POOL}"
if exists gcloud iam workload-identity-pools describe "$POOL" --project="$PROJECT" --location=global; then
	printf '    exists\n'
else
	run gcloud iam workload-identity-pools create "$POOL" --project="$PROJECT" --location=global \
		--display-name="GitHub Actions Pool"
fi

say "workload identity provider ${PROVIDER}"
if exists gcloud iam workload-identity-pools providers describe "$PROVIDER" --project="$PROJECT" \
	--location=global --workload-identity-pool="$POOL"; then
	printf '    exists\n'
else
	run gcloud iam workload-identity-pools providers create-oidc "$PROVIDER" --project="$PROJECT" \
		--location=global --workload-identity-pool="$POOL" \
		--issuer-uri=https://token.actions.githubusercontent.com \
		--attribute-mapping="$ATTRIBUTE_MAPPING" \
		--attribute-condition="assertion.repository == '${REPO}'"
fi

# A provider created before the promote account carries the four original
# attributes and not repo_env; the update adds it. --attribute-mapping
# replaces the whole mapping, so ATTRIBUTE_MAPPING above is the full list and
# not a delta.
say "provider attribute mapping carries repo_env"
if gcloud iam workload-identity-pools providers describe "$PROVIDER" --project="$PROJECT" \
	--location=global --workload-identity-pool="$POOL" --format=json 2>/dev/null |
	tr -d '\n ' | grep -q '"attribute.repo_env"'; then
	printf '    exists\n'
else
	run gcloud iam workload-identity-pools providers update-oidc "$PROVIDER" --project="$PROJECT" \
		--location=global --workload-identity-pool="$POOL" \
		--attribute-mapping="$ATTRIBUTE_MAPPING"
fi

# 4. The repository may impersonate the account.
say "workload identity user for ${REPO}"
if gcloud iam service-accounts get-iam-policy "$SA" --project="$PROJECT" --format=json 2>/dev/null |
	tr -d '\n ' | grep -q "\"${PRINCIPAL}\""; then
	printf '    exists\n'
else
	run gcloud iam service-accounts add-iam-policy-binding "$SA" --project="$PROJECT" \
		--role=roles/iam.workloadIdentityUser --member="$PRINCIPAL"
fi

say "workload identity user for ${REPO} in the production environment"
if gcloud iam service-accounts get-iam-policy "$PROMOTE_SA" --project="$PROJECT" --format=json 2>/dev/null |
	tr -d '\n ' | grep -q "\"${PROMOTE_PRINCIPAL}\""; then
	printf '    exists\n'
else
	run gcloud iam service-accounts add-iam-policy-binding "$PROMOTE_SA" --project="$PROJECT" \
		--role=roles/iam.workloadIdentityUser --member="$PROMOTE_PRINCIPAL"
fi

say "done"
printf '\nRemaining human steps (see the header): the GitHub "production" environment (its required-reviewers rule only if the plan allows it),\n'
printf 'and bucket versioning on gs://%s.\n' "$BUCKET"
printf 'The workflow needs: workload_identity_provider=projects/%s/locations/global/workloadIdentityPools/%s/providers/%s\n' "$PROJECT_NUMBER" "$POOL" "$PROVIDER"
printf '                    service_account=%s   (push job)\n' "$SA"
printf '                    service_account=%s   (promote job, production environment)\n' "$PROMOTE_SA"
