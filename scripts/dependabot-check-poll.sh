#!/usr/bin/env bash
# Polls CI check runs for a Dependabot PR until all pass, one fails, or timeout.
#
# Required env vars:
#   PR_NUMBER          PR number
#   GITHUB_REPOSITORY  e.g. syntasso/enterprise-kratix
#   PR_HEAD_SHA        head commit SHA
#
# Exit codes:
#   0  all checks passed
#   1  a check failed, or timed out waiting, or circuit breaker fired

set -uo pipefail

: "${PR_NUMBER:?}"
: "${GITHUB_REPOSITORY:?}"
: "${PR_HEAD_SHA:?}"

sleep 20

EMPTY_ROUNDS=0
PREV_TOTAL=-1
STABLE_ROUNDS=0

for i in $(seq 1 120); do
	# gh pr checks can return empty even when check runs exist when the workflow
	# trigger context differs from pull_request_target. Fall back to the
	# commit-level APIs which query by SHA and are unaffected.
	STATUS=$(gh pr checks "$PR_NUMBER" --repo "$GITHUB_REPOSITORY" --json name,state \
		--jq '[.[] | select(.name != "auto-merge" and (.name | endswith("/ auto-merge") | not))]' \
		2>/dev/null) || true
	STATUS="${STATUS:-[]}"
	TOTAL=$(echo "$STATUS" | jq 'length')

	if [ "$TOTAL" -eq 0 ]; then
		# Both API calls must succeed; if either errors, keep polling rather than
		# merging on incomplete data (fail-closed on API unavailability).
		FALLBACK_ERR=false

		# Identify non-cancelled check suites for this commit. When a PR is
		# closed and reopened, GitHub cancels the previous workflow runs and
		# marks their suites conclusion="cancelled". Excluding those suites
		# drops the stale failed/cancelled runs without needing actions:read.
		# Multiple distinct workflow files each create their own suite; all
		# non-cancelled suites are kept so no active workflow is silently dropped.
		ACTIVE_SUITES=$(gh api --paginate \
			"repos/$GITHUB_REPOSITORY/commits/$PR_HEAD_SHA/check-suites" \
			2>/dev/null |
			jq -s '[.[].check_suites[]
			  | select(.conclusion != "cancelled" and .conclusion != "stale")
			  | .id]') ||
			FALLBACK_ERR=true
		ACTIVE_SUITES="${ACTIVE_SUITES:-[]}"

		# Paginate check-runs filtered to active suites. Within each (name, suite)
		# pair keep the highest-id run: a re-run adds new check runs to the same
		# suite (same suite id, new run ids), so max_by(.id) supersedes the old
		# failed attempt with the latest result.
		CHECKRUNS=$(gh api --paginate \
			"repos/$GITHUB_REPOSITORY/commits/$PR_HEAD_SHA/check-runs" \
			2>/dev/null |
			jq -s --argjson active_suites "$ACTIVE_SUITES" '[
			  [.[].check_runs[]
			    | select((.name != "auto-merge") and (.name | endswith("/ auto-merge") | not))
			    | select(
			        if ($active_suites | length) > 0
			        then (.check_suite.id as $s | ($active_suites | index($s)) != null)
			        else true
			        end)]
			  | group_by([.name, .check_suite.id])[]
			  | max_by(.id)
			  | {name: .name, state: (if .conclusion != null then (.conclusion | ascii_upcase) else (.status | ascii_upcase) end)}
			]') ||
			FALLBACK_ERR=true

		# Legacy commit statuses (external CI systems using the Statuses API) are
		# not returned by check-runs. Use the paginated /statuses endpoint and
		# deduplicate by context (API returns newest-first, so first per context
		# is the most recent). The combined /status endpoint truncates results.
		LEGACY=$(gh api --paginate \
			"repos/$GITHUB_REPOSITORY/commits/$PR_HEAD_SHA/statuses" \
			2>/dev/null |
			jq -s '[
				[.[].[] | select(.context != "auto-merge")]
				| group_by(.context)[]
				| .[0]
				| {name: .context, state: (.state | ascii_upcase)}
			]') || FALLBACK_ERR=true

		if [ "$FALLBACK_ERR" = true ]; then
			STATUS="[]"
		else
			STATUS=$(jq -n --argjson cr "$CHECKRUNS" --argjson st "$LEGACY" '$cr + $st')
		fi
		TOTAL=$(echo "$STATUS" | jq 'length')
	fi

	if [ "$TOTAL" -eq 0 ]; then
		EMPTY_ROUNDS=$((EMPTY_ROUNDS + 1))
		PREV_TOTAL=-1
		STABLE_ROUNDS=0
		echo "Round $i: no external checks registered yet (${EMPTY_ROUNDS} consecutive empty rounds)"
		if [ "$EMPTY_ROUNDS" -ge 10 ]; then
			gh pr comment "$PR_NUMBER" --repo "$GITHUB_REPOSITORY" \
				--body "Warning: **auto-merge**: CI checks unreadable after 5 minutes. Possible GitHub API issue -- check CI manually and merge if green." \
				2>/dev/null || true
			echo "Circuit breaker: exiting after $EMPTY_ROUNDS empty rounds"
			exit 1
		fi
		sleep 30
		continue
	fi

	EMPTY_ROUNDS=0
	# REQUESTED = check run queued before the app accepts it; treat as pending.
	PENDING=$(echo "$STATUS" | jq '[.[] | select(
    .state == "IN_PROGRESS" or .state == "QUEUED" or
    .state == "PENDING" or .state == "WAITING" or .state == "REQUESTED"
  )] | length')
	# Fail closed: anything not SUCCESS/SKIPPED/pending blocks the merge.
	FAILED=$(echo "$STATUS" | jq '[.[] | select(
    .state != "SUCCESS" and .state != "SKIPPED" and
    .state != "IN_PROGRESS" and .state != "QUEUED" and
    .state != "PENDING" and .state != "WAITING" and .state != "REQUESTED"
  )] | length')
	echo "Round $i: failed=$FAILED pending=$PENDING total=$TOTAL"

	if [ "$FAILED" -gt 0 ]; then
		echo "CI checks failed"
		exit 1
	fi

	if [ "$PENDING" -eq 0 ]; then
		# Guard against succeeding on a partial snapshot: require TOTAL to be
		# stable across two consecutive polls before declaring success. Checks
		# can register late, so a single all-green poll when TOTAL just
		# increased may still be missing registrations.
		if [ "$TOTAL" -eq "$PREV_TOTAL" ]; then
			STABLE_ROUNDS=$((STABLE_ROUNDS + 1))
		else
			STABLE_ROUNDS=0
		fi

		if [ "$STABLE_ROUNDS" -ge 1 ]; then
			echo "All checks passed"
			exit 0
		fi
		echo "Round $i: all green but check count changed ($PREV_TOTAL -> $TOTAL), confirming stability"
	else
		STABLE_ROUNDS=0
	fi

	PREV_TOTAL="$TOTAL"
	sleep 30
done

echo "Timed out waiting for CI checks"
exit 1
