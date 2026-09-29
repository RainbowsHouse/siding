#!/usr/bin/env bash
# Checks the ConfigMap that scripts/plugin-configmap.sh writes: that it is
# one ConfigMap of the right name, holding each of the plugin's files and
# nothing else, every one byte for byte what is in the repository.
#
# It reads the manifest with yq and jq rather than handing it to kubectl,
# which validates against a cluster's API even for a client-side dry run.
#
#   scripts/plugin-configmap-test.sh [version]
set -euo pipefail

version=${1:-0.0.0}
cd "$(dirname "$0")/.."

manifest=$(mktemp)
trap 'rm -f "$manifest"' EXIT
scripts/plugin-configmap.sh "$version" | yq -o=json '.' >"$manifest"

fail() {
  echo "FAIL $*" >&2
  exit 1
}

[ "$(jq -r 'type' "$manifest")" = object ] || fail "the manifest is not one document"
[ "$(jq -r '.apiVersion + "/" + .kind' "$manifest")" = v1/ConfigMap ] || fail "not a v1 ConfigMap"
[ "$(jq -r '.metadata.name' "$manifest")" = "siding-plugin-$version" ] || fail "name is not siding-plugin-$version"
[ "$(jq -r '.metadata.namespace // ""' "$manifest")" = "" ] || fail "it names a namespace"

expected=$(scripts/plugin-files.sh | LC_ALL=C sort)
actual=$(jq -r '.data | keys[]' "$manifest" | LC_ALL=C sort)
[ "$expected" = "$actual" ] || fail "files differ: expected [$(tr '\n' ' ' <<<"$expected")] got [$(tr '\n' ' ' <<<"$actual")]"

while IFS= read -r file; do
  jq -j --arg file "$file" '.data[$file]' "$manifest" | cmp -s - "$file" || fail "$file is not what is in the repository"
done <<<"$expected"

# A ConfigMap holds at most 1 MiB.
size=$(jq -r '[.data[] | utf8bytelength] | add' "$manifest")
[ "$size" -lt 1000000 ] || fail "the plugin's source is $size bytes, too large for a ConfigMap"

echo "ok   siding-plugin-$version: $(wc -l <<<"$expected" | tr -d ' ') files, $size bytes, byte for byte"
