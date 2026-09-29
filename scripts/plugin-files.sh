#!/usr/bin/env bash
# Prints the files that make up the plugin as Traefik loads it, one per line:
# the manifest, go.mod, the Go source without its tests, and the licences the
# source is distributed under. Run from anywhere.
set -euo pipefail

cd "$(dirname "$0")/.."
printf '%s\n' .traefik.yml go.mod LICENSE NOTICE
find . -maxdepth 1 -name '*.go' ! -name '*_test.go' | sed 's|^\./||' | LC_ALL=C sort
