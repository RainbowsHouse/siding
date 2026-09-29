#!/usr/bin/env bash
# Writes the plugin's source as a ConfigMap manifest, for clusters that load
# the plugin locally instead of from Traefik's catalog.
#
#   scripts/plugin-configmap.sh 0.1.0 > siding-plugin-0.1.0.yaml
#
# The ConfigMap is named siding-plugin-<version>. The version is in the name
# on purpose: Traefik reads a plugin's source once, when it starts, so a new
# version has to reach it as a change to its pod template (the volume's
# ConfigMap name), which rolls its replicas together onto the new code. A
# ConfigMap updated in place would leave running pods on the old one.
#
# It has no namespace: apply it to Traefik's, with `kubectl apply -n`.
set -euo pipefail

version=${1:?usage: plugin-configmap.sh <version>}
if ! [[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "version must be X.Y.Z, got '$version'" >&2
  exit 1
fi

root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root"

cat <<YAML
apiVersion: v1
kind: ConfigMap
metadata:
  name: siding-plugin-$version
  labels:
    app.kubernetes.io/name: siding
    app.kubernetes.io/component: plugin
    app.kubernetes.io/version: "$version"
data:
YAML
scripts/plugin-files.sh | while IFS= read -r file; do
  # A literal block keeps the file byte for byte; "|+" keeps its final
  # newline whatever follows, and the indentation indicator allows a first
  # line that is itself indented.
  printf '  %s: |2+\n' "$file"
  sed 's/^/    /' "$file"
done
