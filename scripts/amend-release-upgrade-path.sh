#!/usr/bin/env bash
# Adds versions to the upgrade_path of a published SKE release's
# release-metadata.json, wherever that file has been published:
#   - s3://syntasso-enterprise-releases/ske/<TAG>/
#   - s3://syntasso-enterprise-releases/ske/latest/ (only if <TAG> is the latest release)
#   - the <TAG> GitHub release on syntasso/enterprise-kratix
#
# Required env vars:
#   TAG            full release tag, e.g. v0.13.0 (release candidates are refused)
#   UPGRADE_PATHS  versions to add, separated by spaces and/or commas,
#                  e.g. "v0.11.0, v0.10.0"
#   GH_TOKEN       token able to upload assets to syntasso/enterprise-kratix releases
#   AWS credentials able to read and write the releases bucket
#
# Existing entries are kept in order; versions already present are not duplicated.

set -euo pipefail

: "${TAG:?TAG is required}"
: "${UPGRADE_PATHS:?UPGRADE_PATHS is required}"

REPO="syntasso/enterprise-kratix"
S3_ROOT="s3://syntasso-enterprise-releases/ske"
METADATA_FILE="release-metadata.json"
VERSION_PATTERN='^v[0-9]+\.[0-9]+\.[0-9]+$'

if [[ ! "$TAG" =~ $VERSION_PATTERN ]]; then
	echo "TAG '$TAG' is not a full release tag (expected vX.Y.Z)." >&2
	exit 1
fi

read -r -a new_versions <<<"${UPGRADE_PATHS//,/ }"
if [[ ${#new_versions[@]} -eq 0 ]]; then
	echo "UPGRADE_PATHS contains no versions." >&2
	exit 1
fi
for v in "${new_versions[@]}"; do
	if [[ ! "$v" =~ $VERSION_PATTERN ]]; then
		echo "Upgrade path version '$v' is not valid (expected vX.Y.Z)." >&2
		exit 1
	fi
done

if ! gh release view "$TAG" --repo "$REPO" >/dev/null 2>&1; then
	echo "Release $TAG has not been published on $REPO." >&2
	exit 1
fi

workdir=$(mktemp -d)
trap 'rm -rf "$workdir"' EXIT
metadata="$workdir/$METADATA_FILE"

aws s3 cp "$S3_ROOT/$TAG/$METADATA_FILE" "$metadata"

new_versions_json=$(printf '%s\n' "${new_versions[@]}" | jq -R . | jq -s .)
jq --argjson new "$new_versions_json" '
  .upgrade_path = reduce (.upgrade_path + $new)[] as $v
    ([]; if index([$v]) then . else . + [$v] end)
' "$metadata" >"$metadata.tmp"
mv "$metadata.tmp" "$metadata"

echo "Updated $METADATA_FILE for $TAG:"
cat "$metadata"

aws s3 cp "$metadata" "$S3_ROOT/$TAG/$METADATA_FILE"

latest_version=$(aws s3 cp "$S3_ROOT/latest/$METADATA_FILE" - | jq -r '.version')
if [[ "$latest_version" == "$TAG" ]]; then
	echo "$TAG is the latest release; updating $S3_ROOT/latest/ too."
	aws s3 cp "$metadata" "$S3_ROOT/latest/$METADATA_FILE"
fi

gh release upload "$TAG" "$metadata" --repo "$REPO" --clobber
