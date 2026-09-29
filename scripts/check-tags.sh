#!/usr/bin/env bash
# Fails if the repository has a git tag that is not a plain version, vX.Y.Z.
#
# Traefik's plugin catalog refuses a repository for one such tag, and then
# stops looking at it until the issue it opened is closed. Tags that would do
# it: a chart release tool's "siding-0.1.0", a nested Go module's
# "controller/v0.1.0", a pre-release's "v0.1.0-rc.1".
#
#   scripts/check-tags.sh            # the tags of origin
#   scripts/check-tags.sh --local    # the tags of this checkout
set -euo pipefail

if [ "${1:-}" = "--local" ]; then
  tags=$(git tag --list)
else
  tags=$(git ls-remote --tags --refs origin | sed 's|.*refs/tags/||')
fi

bad=$(grep -vE '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' <<<"$tags" | sed '/^$/d' || true)
if [ -n "$bad" ]; then
  echo "tags that are not vX.Y.Z, which Traefik's plugin catalog refuses the repository for:" >&2
  sed 's/^/  /' <<<"$bad" >&2
  exit 1
fi
echo "tags: $(sed '/^$/d' <<<"$tags" | wc -l | tr -d ' '), all plain versions"
