#!/usr/bin/env bash
# Determines whether a Dependabot PR is eligible for auto-merge.
#
# Required env vars (set from GitHub Actions step outputs):
#   UPDATE_TYPE        e.g. version-update:semver-patch
#   PREV_VERSION       e.g. 1.2.3
#   NEW_VERSION        e.g. 1.2.4
#   DEPENDENCY_NAMES   e.g. github.com/some/dep
#   PACKAGE_ECOSYSTEM  e.g. gomod, github-actions, npm
#   GITHUB_REPOSITORY  e.g. syntasso/enterprise-kratix
#   PR_HEAD_SHA        head commit SHA of the PR
#   GITHUB_OUTPUT      path to the step output file (set by Actions runner)
#
# Outputs (written to $GITHUB_OUTPUT):
#   result=true|false
#   reason=<human-readable explanation>
#   label=<label to apply when result=false, omitted when requires-human-review>
#
# Exit codes: 0 always (failure written to GITHUB_OUTPUT, not exit code)

set -euo pipefail

: "${GITHUB_REPOSITORY:?}"
: "${PR_HEAD_SHA:?}"
: "${GITHUB_OUTPUT:?}"

TYPE="${UPDATE_TYPE:-}"
PREV="${PREV_VERSION:-}"
NEW="${NEW_VERSION:-}"
DEPS="${DEPENDENCY_NAMES:-}"
ECOSYSTEM="${PACKAGE_ECOSYSTEM:-}"

# fetch-metadata can emit empty values for non-semver or digest-only updates.
# Treat missing metadata as ineligible rather than failing the job.
if [[ -z "$TYPE" || -z "$DEPS" || -z "$ECOSYSTEM" ]]; then
	echo "result=false" >>"$GITHUB_OUTPUT"
	echo "reason=missing or non-semver metadata — requires human review" >>"$GITHUB_OUTPUT"
	exit 0
fi

echo "Update type: $TYPE"
echo "Version: $PREV → $NEW"
echo "Dependencies: $DEPS"
echo "Ecosystem: $ECOSYSTEM"

if echo "$DEPS" | grep -qE '(^|,\s*)(k8s\.io/|sigs\.k8s\.io/controller-runtime)'; then
	echo "result=false" >>"$GITHUB_OUTPUT"
	echo "reason=k8s ecosystem bump — must be coordinated manually" >>"$GITHUB_OUTPUT"
	exit 0
fi

if echo "$DEPS" | grep -qiE '(^|,\s*)(headlamp|backstage)'; then
	echo "result=false" >>"$GITHUB_OUTPUT"
	echo "reason=fork dep (headlamp/backstage) — requires human review" >>"$GITHUB_OUTPUT"
	exit 0
fi

# Major bumps always require human review — checked before the age gate so a
# major github-actions bump can't be labelled actions-age-wait and auto-merged.
if [[ "$TYPE" != "version-update:semver-patch" && "$TYPE" != "version-update:semver-minor" ]]; then
	echo "result=false" >>"$GITHUB_OUTPUT"
	echo "reason=major bump — manual review required" >>"$GITHUB_OUTPUT"
	exit 0
fi

# fetch-metadata emits "github_actions" (underscore), while dependabot.yml
# config uses "github-actions" (hyphen). Normalise so the age gate matches
# either form — the hyphen comparison alone never matched, silently
# disabling the gate (verified live on enterprise-kratix#1401).
if [[ "${ECOSYSTEM//-/_}" == "github_actions" ]]; then
	MIN_AGE_DAYS=5
	NOW=$(date +%s)
	# Age from the head commit, not PR creation — a synchronize push with a new
	# action SHA must restart the cooling-off wait.
	COMMIT_DATE=$(gh api "repos/$GITHUB_REPOSITORY/commits/$PR_HEAD_SHA" \
		--jq '.commit.committer.date' 2>/dev/null || true)
	# If the API call fails (commit not yet indexed), treat as 0 days old —
	# conservative default that always triggers the age gate.
	if [[ -z "$COMMIT_DATE" ]]; then
		COMMIT_TS="$NOW"
	else
		# Parse ISO-8601 date portably: GNU date (Linux/GH Actions) uses -d,
		# BSD date (macOS) uses -jf.
		COMMIT_TS=$(date -d "$COMMIT_DATE" +%s 2>/dev/null ||
			date -jf "%Y-%m-%dT%H:%M:%SZ" "$COMMIT_DATE" +%s 2>/dev/null ||
			echo "$NOW")
	fi
	PR_AGE_DAYS=$(((NOW - COMMIT_TS) / 86400))
	echo "PR age: ${PR_AGE_DAYS} days (minimum: ${MIN_AGE_DAYS})"
	if [[ "$PR_AGE_DAYS" -lt "$MIN_AGE_DAYS" ]]; then
		echo "result=false" >>"$GITHUB_OUTPUT"
		echo "reason=github-actions bump is ${PR_AGE_DAYS}d old — waiting until ${MIN_AGE_DAYS}d minimum for supply chain safety (will retry via daily schedule)" >>"$GITHUB_OUTPUT"
		echo "label=actions-age-wait" >>"$GITHUB_OUTPUT"
		exit 0
	fi
fi

echo "result=true" >>"$GITHUB_OUTPUT"
echo "reason=${TYPE} ($PREV → $NEW)" >>"$GITHUB_OUTPUT"
