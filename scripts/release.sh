#!/usr/bin/env bash
# Builds what a release publishes, into dist/:
#
#   siding-X.Y.Z.tgz          the Helm chart
#   siding-plugin-X.Y.Z.yaml  the plugin's source as a ConfigMap
#   index.yaml                the chart repository's index, with this release
#
#   scripts/release.sh 0.1.0 [existing-index.yaml]
#
# It publishes nothing: .github/workflows/release.yml does, from what this
# leaves in dist/. The index points at the release's assets on GitHub, so the
# chart repository is one file on GitHub Pages and needs no tags of its own.
set -euo pipefail

version=${1:?usage: release.sh <version> [existing-index.yaml]}
existing=${2:-}
if ! [[ $version =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
  echo "version must be X.Y.Z, got '$version'" >&2
  exit 1
fi
repo=${GITHUB_REPOSITORY:-RainbowsHouse/siding}

cd "$(dirname "$0")/.."
rm -rf dist/release
mkdir -p dist/release

scripts/plugin-configmap.sh "$version" >"dist/release/siding-plugin-$version.yaml"
helm package charts/siding --version "$version" --app-version "$version" --destination dist/release >/dev/null

if [ -n "$existing" ] && [ -s "$existing" ]; then
  if grep -qE "^    version: \"?$version\"?\$" "$existing"; then
    echo "chart $version is already in the index: a released version is never published again" >&2
    exit 1
  fi
  cp "$existing" dist/release/previous-index.yaml
  helm repo index dist/release --url "https://github.com/$repo/releases/download/v$version" --merge dist/release/previous-index.yaml
  rm dist/release/previous-index.yaml
else
  helm repo index dist/release --url "https://github.com/$repo/releases/download/v$version"
fi

ls -1 dist/release
