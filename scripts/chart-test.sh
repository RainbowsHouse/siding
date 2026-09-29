#!/usr/bin/env bash
# Checks the chart against the plugin it configures.
#
# Renders the chart with each file in charts/siding/examples/, then hands the
# plugin configuration of every Middleware it rendered to the plugin's own
# New (TestChartConfigs, in chart_test.go). A chart that renders a
# configuration the plugin refuses fails here rather than in a cluster, where
# it would cost the route the Middleware is attached to.
#
# Needs helm, yq (mikefarah, v4) and go.
set -euo pipefail

cd "$(dirname "$0")/.."
chart=charts/siding
out=dist/chart-configs

helm lint "$chart" >/dev/null

rm -rf "$out"
mkdir -p "$out"
for values in "$chart"/examples/*.yaml; do
  example=$(basename "$values" .yaml)
  rendered=$(helm template siding "$chart" --namespace traefik --values "$values")
  names=$(yq --no-doc -r 'select(.kind == "Middleware") | .metadata.name' <<<"$rendered")
  if [ -z "$names" ]; then
    echo "FAIL $example: no Middleware rendered" >&2
    exit 1
  fi
  for name in $names; do
    NAME=$name yq --no-doc -o=json 'select(.kind == "Middleware" and .metadata.name == strenv(NAME)) | .spec.plugin.siding' \
      <<<"$rendered" >"$out/$example.$name.json"
  done
  echo "ok   $example: $(wc -w <<<"$names" | tr -d ' ') Middleware(s)"
done

go test -count=1 -run 'TestChartConfigs' .
