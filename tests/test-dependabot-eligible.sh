#!/usr/bin/env bash
# Unit tests for scripts/dependabot-eligible.sh
#
# Usage: bash tests/test-dependabot-eligible.sh
# Requires: bash, jq

set -uo pipefail

SCRIPT="$(cd "$(dirname "$0")/.." && pwd)/scripts/dependabot-eligible.sh"
PASS=0
FAIL=0
SKIP=0

# ── helpers ──────────────────────────────────────────────────────────────────

assert_output() {
	local name="$1"
	local tmpout="$2"
	local expected_result="$3"
	local expected_label="${4:-}"

	local actual_result
	actual_result=$(grep "^result=" "$tmpout" | cut -d= -f2)

	local actual_label
	actual_label=$(grep "^label=" "$tmpout" | cut -d= -f2 || true)

	local ok=1
	if [[ "$actual_result" != "$expected_result" ]]; then
		echo "FAIL [$name]: expected result=$expected_result, got result=${actual_result:-<empty>}"
		ok=0
	fi
	if [[ -n "$expected_label" && "$actual_label" != "$expected_label" ]]; then
		echo "FAIL [$name]: expected label=$expected_label, got label=${actual_label:-<empty>}"
		ok=0
	fi
	if [[ $ok -eq 1 ]]; then
		echo "PASS [$name]"
		PASS=$((PASS + 1))
	else
		FAIL=$((FAIL + 1))
	fi
}

# Run the eligible script with supplied env and a mock gh function.
# $1      test name
# $2      expected result (true|false)
# $3      expected label (or empty string to skip label check)
# rest    additional env=value pairs passed to the script
run_eligible() {
	local name="$1"
	shift
	local expected_result="$1"
	shift
	local expected_label="$1"
	shift

	local tmpout
	tmpout=$(mktemp)
	# GH_MOCK_DATE is consumed by the mock gh function below.
	local env_pairs=("$@")

	# Write a mock gh binary for this test run.
	local mock_dir
	mock_dir=$(mktemp -d)
	cat >"$mock_dir/gh" <<'EOF'
#!/usr/bin/env bash
# Mock gh — returns GH_MOCK_DATE for commit date queries; empty otherwise.
if [[ "$*" == *"commits/"* && "$*" == *"committer.date"* ]]; then
  if [[ -n "${GH_MOCK_DATE:-}" ]]; then
    echo "$GH_MOCK_DATE"
    exit 0
  fi
  # Simulate API not yet indexed — return nothing
  exit 1
fi
# pr checks, pr comment, etc. — not needed for eligible tests
exit 0
EOF
	chmod +x "$mock_dir/gh"

	env \
		GITHUB_OUTPUT="$tmpout" \
		PATH="$mock_dir:$PATH" \
		GITHUB_REPOSITORY="syntasso/test-repo" \
		PR_HEAD_SHA="abc123" \
		"${env_pairs[@]}" \
		bash "$SCRIPT" >/dev/null 2>&1
	local rc=$?

	rm -rf "$mock_dir"
	assert_output "$name" "$tmpout" "$expected_result" "$expected_label"
	rm -f "$tmpout"
	return $rc
}

# ── eligible tests ────────────────────────────────────────────────────────────

echo "=== dependabot-eligible.sh ==="

# go_modules patch — should auto-merge
run_eligible "gomod patch" true "" \
	UPDATE_TYPE=version-update:semver-patch \
	PREV_VERSION=1.2.3 \
	NEW_VERSION=1.2.4 \
	DEPENDENCY_NAMES=github.com/some/dep \
	PACKAGE_ECOSYSTEM=gomod

# go_modules minor — should auto-merge
run_eligible "gomod minor" true "" \
	UPDATE_TYPE=version-update:semver-minor \
	PREV_VERSION=1.2.3 \
	NEW_VERSION=1.3.0 \
	DEPENDENCY_NAMES=github.com/some/dep \
	PACKAGE_ECOSYSTEM=gomod

# go_modules major — must block
run_eligible "gomod major" false "" \
	UPDATE_TYPE=version-update:semver-major \
	PREV_VERSION=1.2.3 \
	NEW_VERSION=2.0.0 \
	DEPENDENCY_NAMES=github.com/some/dep \
	PACKAGE_ECOSYSTEM=gomod

# k8s dep — must block regardless of bump type
run_eligible "k8s.io dep" false "" \
	UPDATE_TYPE=version-update:semver-patch \
	PREV_VERSION=0.29.0 \
	NEW_VERSION=0.29.1 \
	DEPENDENCY_NAMES=k8s.io/client-go \
	PACKAGE_ECOSYSTEM=gomod

# controller-runtime — must block
run_eligible "controller-runtime dep" false "" \
	UPDATE_TYPE=version-update:semver-patch \
	PREV_VERSION=0.17.0 \
	NEW_VERSION=0.17.1 \
	DEPENDENCY_NAMES=sigs.k8s.io/controller-runtime \
	PACKAGE_ECOSYSTEM=gomod

# headlamp dep — must block
run_eligible "headlamp dep" false "" \
	UPDATE_TYPE=version-update:semver-patch \
	PREV_VERSION=1.0.0 \
	NEW_VERSION=1.0.1 \
	DEPENDENCY_NAMES=headlamp-plugin \
	PACKAGE_ECOSYSTEM=npm

# github-actions patch, commit date API returns empty (race condition) — must age-gate
run_eligible "github-actions: commit API empty → age-gate" false "actions-age-wait" \
	UPDATE_TYPE=version-update:semver-patch \
	PREV_VERSION=4.2.0 \
	NEW_VERSION=4.3.0 \
	DEPENDENCY_NAMES=docker/setup-buildx-action \
	PACKAGE_ECOSYSTEM=github-actions \
	GH_MOCK_DATE=""

# github-actions patch, 0 days old — must age-gate
NOW=$(date +%s)
ZERO_DAYS_AGO=$(date -d "@$NOW" --iso-8601=seconds 2>/dev/null || date -r "$NOW" "+%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || echo "")
if [[ -n "$ZERO_DAYS_AGO" ]]; then
	run_eligible "github-actions: 0 days old → age-gate" false "actions-age-wait" \
		UPDATE_TYPE=version-update:semver-patch \
		PREV_VERSION=4.2.0 \
		NEW_VERSION=4.3.0 \
		DEPENDENCY_NAMES=docker/setup-buildx-action \
		PACKAGE_ECOSYSTEM=github-actions \
		GH_MOCK_DATE="$ZERO_DAYS_AGO"
else
	echo "SKIP [github-actions: 0 days old → age-gate]: date command not portable on this OS"
	SKIP=$((SKIP + 1))
fi

# github-actions patch, 4 days old — must still age-gate
FOUR_DAYS_AGO=$(date -d "4 days ago" --iso-8601=seconds 2>/dev/null || date -v-4d "+%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || echo "")
if [[ -n "$FOUR_DAYS_AGO" ]]; then
	run_eligible "github-actions: 4 days old → age-gate" false "actions-age-wait" \
		UPDATE_TYPE=version-update:semver-patch \
		PREV_VERSION=4.2.0 \
		NEW_VERSION=4.3.0 \
		DEPENDENCY_NAMES=docker/setup-buildx-action \
		PACKAGE_ECOSYSTEM=github-actions \
		GH_MOCK_DATE="$FOUR_DAYS_AGO"
else
	echo "SKIP [github-actions: 4 days old → age-gate]: date command not portable on this OS"
	SKIP=$((SKIP + 1))
fi

# github-actions patch, 5 days old — should auto-merge
FIVE_DAYS_AGO=$(date -d "5 days ago" --iso-8601=seconds 2>/dev/null || date -v-5d "+%Y-%m-%dT%H:%M:%SZ" 2>/dev/null || echo "")
if [[ -n "$FIVE_DAYS_AGO" ]]; then
	run_eligible "github-actions: 5 days old → eligible" true "" \
		UPDATE_TYPE=version-update:semver-patch \
		PREV_VERSION=4.2.0 \
		NEW_VERSION=4.3.0 \
		DEPENDENCY_NAMES=docker/setup-buildx-action \
		PACKAGE_ECOSYSTEM=github-actions \
		GH_MOCK_DATE="$FIVE_DAYS_AGO"
else
	echo "SKIP [github-actions: 5 days old → eligible]: date command not portable on this OS"
	SKIP=$((SKIP + 1))
fi

# github-actions major bump — blocked before reaching age gate
run_eligible "github-actions: major → blocked before age gate" false "" \
	UPDATE_TYPE=version-update:semver-major \
	PREV_VERSION=3.0.0 \
	NEW_VERSION=4.0.0 \
	DEPENDENCY_NAMES=actions/checkout \
	PACKAGE_ECOSYSTEM=github-actions \
	GH_MOCK_DATE=""

# npm patch (non-special ecosystem) — should auto-merge
run_eligible "npm patch" true "" \
	UPDATE_TYPE=version-update:semver-patch \
	PREV_VERSION=1.0.0 \
	NEW_VERSION=1.0.1 \
	DEPENDENCY_NAMES=lodash \
	PACKAGE_ECOSYSTEM=npm

# ── jq filter tests (check-poll logic) ───────────────────────────────────────

echo ""
echo "=== jq filter: auto-merge check exclusion ==="

jq_exclude_filter='[.[] | select(.name != "auto-merge" and (.name | endswith("/ auto-merge") | not))]'

check_jq() {
	local name="$1"
	local input="$2"
	local expected_length="$3"

	local actual_length
	actual_length=$(echo "$input" | jq "$jq_exclude_filter | length")

	if [[ "$actual_length" == "$expected_length" ]]; then
		echo "PASS [$name]: length=$actual_length"
		PASS=$((PASS + 1))
	else
		echo "FAIL [$name]: expected length=$expected_length, got $actual_length"
		FAIL=$((FAIL + 1))
	fi
}

# bare "auto-merge" check excluded
check_jq "exclude bare auto-merge" \
	'[{"name":"auto-merge","state":"FAILURE"},{"name":"tests","state":"SUCCESS"}]' \
	1

# reusable workflow composite name excluded
check_jq "exclude composite auto-merge" \
	'[{"name":"dependabot-auto-merge / auto-merge","state":"FAILURE"},{"name":"tests","state":"SUCCESS"}]' \
	1

# unrelated checks kept
check_jq "keep unrelated checks" \
	'[{"name":"build","state":"SUCCESS"},{"name":"lint","state":"SUCCESS"}]' \
	2

# no checks → empty
check_jq "empty input" '[]' 0

# ── state interpretation (what FAILED counts) ─────────────────────────────────

echo ""
echo "=== jq filter: failed state detection ==="

failed_filter='[.[] | select(.state != "SUCCESS" and .state != "SKIPPED" and .state != "IN_PROGRESS" and .state != "QUEUED" and .state != "PENDING" and .state != "WAITING")] | length'

check_jq_state() {
	local name="$1"
	local input="$2"
	local expected="$3"

	local actual
	actual=$(echo "$input" | jq "$failed_filter")

	if [[ "$actual" == "$expected" ]]; then
		echo "PASS [$name]"
		PASS=$((PASS + 1))
	else
		echo "FAIL [$name]: expected $expected, got $actual"
		FAIL=$((FAIL + 1))
	fi
}

check_jq_state "SUCCESS not failed" '[{"name":"t","state":"SUCCESS"}]' 0
check_jq_state "SKIPPED not failed" '[{"name":"t","state":"SKIPPED"}]' 0
check_jq_state "IN_PROGRESS not failed" '[{"name":"t","state":"IN_PROGRESS"}]' 0
check_jq_state "PENDING not failed (legacy status)" '[{"name":"t","state":"PENDING"}]' 0
check_jq_state "FAILURE is failed" '[{"name":"t","state":"FAILURE"}]' 1
check_jq_state "TIMED_OUT is failed" '[{"name":"t","state":"TIMED_OUT"}]' 1
check_jq_state "ERROR is failed (legacy status)" '[{"name":"t","state":"ERROR"}]' 1
check_jq_state "mixed: 1 fail 1 pass" '[{"name":"a","state":"SUCCESS"},{"name":"b","state":"FAILURE"}]' 1
check_jq_state "legacy FAILURE blocks merge despite green check-run" \
	'[{"name":"ci/build","state":"SUCCESS"},{"name":"external-ci","state":"FAILURE"}]' 1

# ── legacy status merge (check-poll fallback) ─────────────────────────────────

echo ""
echo "=== jq: legacy status merge ==="

merge_jq() {
	local name="$1"
	local checkruns="$2"
	local legacy="$3"
	local expected_total="$4"
	local expected_failed="$5"

	local merged
	merged=$(jq -n --argjson cr "$checkruns" --argjson st "$legacy" '$cr + $st')
	local total
	total=$(echo "$merged" | jq 'length')
	local failed
	failed=$(echo "$merged" | jq "$failed_filter")

	local ok=1
	if [[ "$total" != "$expected_total" ]]; then
		echo "FAIL [$name]: expected total=$expected_total, got $total"
		ok=0
	fi
	if [[ "$failed" != "$expected_failed" ]]; then
		echo "FAIL [$name]: expected failed=$expected_failed, got $failed"
		ok=0
	fi
	[[ $ok -eq 1 ]] && {
		echo "PASS [$name]"
		PASS=$((PASS + 1))
	} || FAIL=$((FAIL + 1))
}

merge_jq "green check-run + green legacy = 0 failed" \
	'[{"name":"build","state":"SUCCESS"}]' \
	'[{"name":"ext","state":"SUCCESS"}]' \
	2 0

merge_jq "green check-run + pending legacy = 0 failed, not all done" \
	'[{"name":"build","state":"SUCCESS"}]' \
	'[{"name":"ext","state":"PENDING"}]' \
	2 0

merge_jq "green check-run + failing legacy = 1 failed" \
	'[{"name":"build","state":"SUCCESS"}]' \
	'[{"name":"ext","state":"FAILURE"}]' \
	2 1

merge_jq "empty check-runs + failing legacy still caught" \
	'[]' \
	'[{"name":"ext","state":"FAILURE"}]' \
	1 1

# ── summary ───────────────────────────────────────────────────────────────────

echo ""
echo "Results: $PASS passed, $FAIL failed, $SKIP skipped"
[[ $FAIL -eq 0 ]]
