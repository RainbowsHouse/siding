#!/usr/bin/env bash
# Lays the plugin's files out in a directory the way Kubernetes mounts a
# ConfigMap volume: the files in a timestamped directory, a "..data" link to
# it, and each file a link through "..data". The demo loads the plugin from
# such a directory to show that Traefik reads it through those links.
#
#   scripts/configmap-layout.sh /tmp/siding-plugin
set -euo pipefail

target=${1:?usage: configmap-layout.sh <directory>}
root=$(cd "$(dirname "$0")/.." && pwd)

rm -rf "$target"
mkdir -p "$target/..2026_01_01_00_00_00.0000000000"
ln -s "..2026_01_01_00_00_00.0000000000" "$target/..data"
"$root/scripts/plugin-files.sh" | while IFS= read -r file; do
  cp "$root/$file" "$target/..data/$file"
  ln -s "..data/$file" "$target/$file"
done
