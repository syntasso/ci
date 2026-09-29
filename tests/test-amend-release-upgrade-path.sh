#!/usr/bin/env bash
# Unit tests for scripts/amend-release-upgrade-path.sh
#
# Usage: bash tests/test-amend-release-upgrade-path.sh
# Requires: bash, jq
#
# aws and gh are replaced by mocks on PATH. The mock aws maps
# s3://<bucket>/<key> to $MOCK_S3_ROOT/<bucket>/<key>, so each test can seed
# and inspect the "bucket" as local files. The mock gh logs every call to
# $MOCK_GH_LOG and treats a release as published when MOCK_GH_RELEASE_EXISTS=true.

set -uo pipefail

SCRIPT="$(cd "$(dirname "$0")/.." && pwd)/scripts/amend-release-upgrade-path.sh"
BUCKET="syntasso-enterprise-releases"
PASS=0
FAIL=0

MOCK_BIN=$(mktemp -d)
trap 'rm -rf "$MOCK_BIN"' EXIT

cat >"$MOCK_BIN/aws" <<'EOF'
#!/usr/bin/env bash
# Mock aws — supports only `aws s3 cp <src> <dst>`.
[[ "$1 $2" == "s3 cp" ]] || { echo "mock aws: unsupported: $*" >&2; exit 2; }
resolve() {
  if [[ "$1" == s3://* ]]; then echo "$MOCK_S3_ROOT/${1#s3://}"; else echo "$1"; fi
}
src=$(resolve "$3")
dst=$(resolve "$4")
[[ -f "$src" ]] || { echo "mock aws: $3 does not exist" >&2; exit 1; }
if [[ "$4" == "-" ]]; then cat "$src"; exit 0; fi
mkdir -p "$(dirname "$dst")"
cp "$src" "$dst"
EOF

cat >"$MOCK_BIN/gh" <<'EOF'
#!/usr/bin/env bash
# Mock gh — logs calls; `release view` succeeds only when MOCK_GH_RELEASE_EXISTS=true.
echo "$*" >>"$MOCK_GH_LOG"
if [[ "$1 $2" == "release view" ]]; then
  [[ "${MOCK_GH_RELEASE_EXISTS:-true}" == "true" ]] || exit 1
fi
exit 0
EOF
chmod +x "$MOCK_BIN/aws" "$MOCK_BIN/gh"

# ── helpers ──────────────────────────────────────────────────────────────────

# Fresh fake bucket and gh log for each test. Sets S3_ROOT, GH_LOG, OUT, STATUS.
setup() {
	S3_ROOT=$(mktemp -d)
	GH_LOG=$(mktemp)
}

seed_metadata() {
	local dir="$1" json="$2"
	mkdir -p "$S3_ROOT/$BUCKET/ske/$dir"
	echo "$json" >"$S3_ROOT/$BUCKET/ske/$dir/release-metadata.json"
}

run_script() {
	OUT=$(env PATH="$MOCK_BIN:$PATH" MOCK_S3_ROOT="$S3_ROOT" MOCK_GH_LOG="$GH_LOG" \
		"$@" bash "$SCRIPT" 2>&1)
	STATUS=$?
}

upgrade_path_in() {
	jq -c '.upgrade_path' "$S3_ROOT/$BUCKET/ske/$1/release-metadata.json"
}

check() {
	local name="$1" expected="$2" actual="$3"
	if [[ "$expected" == "$actual" ]]; then
		echo "PASS [$name]"
		PASS=$((PASS + 1))
	else
		echo "FAIL [$name]: expected '$expected', got '$actual'"
		echo "$OUT" | sed 's/^/    /'
		FAIL=$((FAIL + 1))
	fi
}

# ── tests ────────────────────────────────────────────────────────────────────

# Appends new versions to the release's own metadata, keeping other fields
setup
seed_metadata v0.12.0 '{"version":"v0.12.0","pre_release":false,"upgrade_path":["v0.11.0"]}'
seed_metadata latest '{"version":"v0.13.0","pre_release":false,"upgrade_path":["v0.12.0"]}'
run_script TAG=v0.12.0 UPGRADE_PATHS="v0.10.0 v0.10.1"
check "appends versions: exit 0" 0 "$STATUS"
check "appends versions: upgrade_path" '["v0.11.0","v0.10.0","v0.10.1"]' "$(upgrade_path_in v0.12.0)"
check "appends versions: other fields kept" '"v0.12.0" false' \
	"$(jq -r '"\"" + .version + "\" " + (.pre_release | tostring)' "$S3_ROOT/$BUCKET/ske/v0.12.0/release-metadata.json")"

# Accepts comma-separated input and does not duplicate existing entries
setup
seed_metadata v0.12.0 '{"version":"v0.12.0","pre_release":false,"upgrade_path":["v0.11.0"]}'
seed_metadata latest '{"version":"v0.13.0","pre_release":false,"upgrade_path":["v0.12.0"]}'
run_script TAG=v0.12.0 UPGRADE_PATHS="v0.11.0, v0.10.0,v0.10.0"
check "no duplicates: upgrade_path" '["v0.11.0","v0.10.0"]' "$(upgrade_path_in v0.12.0)"

# Replaces the metadata attached to the GitHub release
setup
seed_metadata v0.12.0 '{"version":"v0.12.0","pre_release":false,"upgrade_path":["v0.11.0"]}'
seed_metadata latest '{"version":"v0.13.0","pre_release":false,"upgrade_path":["v0.12.0"]}'
run_script TAG=v0.12.0 UPGRADE_PATHS="v0.10.0"
check "github release: asset replaced" 1 \
	"$(grep -cE '^release upload v0.12.0 [^ ]*/release-metadata.json .*--clobber' "$GH_LOG")"

# Leaves latest/ alone when the release is not the latest one
check "not latest: latest/ untouched" '["v0.12.0"]' "$(upgrade_path_in latest)"

# Updates latest/ as well when the release is the latest one
setup
seed_metadata v0.13.0 '{"version":"v0.13.0","pre_release":false,"upgrade_path":["v0.12.0"]}'
seed_metadata latest '{"version":"v0.13.0","pre_release":false,"upgrade_path":["v0.12.0"]}'
run_script TAG=v0.13.0 UPGRADE_PATHS="v0.11.0"
check "latest: exit 0" 0 "$STATUS"
check "latest: latest/ updated" '["v0.12.0","v0.11.0"]' "$(upgrade_path_in latest)"

# Refuses release candidates — this is for published releases only
setup
seed_metadata v0.13.0-rc1 '{"version":"v0.13.0-rc1","pre_release":true,"upgrade_path":["v0.12.0"]}'
run_script TAG=v0.13.0-rc1 UPGRADE_PATHS="v0.11.0"
check "rc tag: non-zero exit" 1 "$([[ $STATUS -ne 0 ]] && echo 1 || echo 0)"
check "rc tag: metadata untouched" '["v0.12.0"]' "$(upgrade_path_in v0.13.0-rc1)"

# Refuses malformed upgrade path versions and changes nothing
setup
seed_metadata v0.12.0 '{"version":"v0.12.0","pre_release":false,"upgrade_path":["v0.11.0"]}'
run_script TAG=v0.12.0 UPGRADE_PATHS="v0.10.0 0.9"
check "bad version: non-zero exit" 1 "$([[ $STATUS -ne 0 ]] && echo 1 || echo 0)"
check "bad version: metadata untouched" '["v0.11.0"]' "$(upgrade_path_in v0.12.0)"

# Refuses an empty list of versions
setup
seed_metadata v0.12.0 '{"version":"v0.12.0","pre_release":false,"upgrade_path":["v0.11.0"]}'
run_script TAG=v0.12.0 UPGRADE_PATHS=" , "
check "empty versions: non-zero exit" 1 "$([[ $STATUS -ne 0 ]] && echo 1 || echo 0)"

# Refuses a release that has not been published on GitHub
setup
seed_metadata v0.12.0 '{"version":"v0.12.0","pre_release":false,"upgrade_path":["v0.11.0"]}'
run_script TAG=v0.12.0 UPGRADE_PATHS="v0.10.0" MOCK_GH_RELEASE_EXISTS=false
check "unpublished: non-zero exit" 1 "$([[ $STATUS -ne 0 ]] && echo 1 || echo 0)"
check "unpublished: metadata untouched" '["v0.11.0"]' "$(upgrade_path_in v0.12.0)"

# ── summary ──────────────────────────────────────────────────────────────────

echo ""
echo "Results: $PASS passed, $FAIL failed"
[[ $FAIL -eq 0 ]]
