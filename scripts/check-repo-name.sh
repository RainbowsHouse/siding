#!/usr/bin/env bash
# Checks that the module path and the manifest's import spell the repository
# the way GitHub does.
#
# Traefik's plugin catalog refuses a plugin whose module path does not start
# with "github.com/<owner>/<repo>" exactly as GitHub spells them, upper case
# included (piceus, pkg/core/yaegi.go, checkRepoName). siding v0.1.0 was
# refused for "github.com/rainbowshouse/siding" against RainbowsHouse/siding
# (issue #1).
#
#   GITHUB_REPOSITORY=RainbowsHouse/siding scripts/check-repo-name.sh
#
# In GitHub Actions GITHUB_REPOSITORY is set, and spelled as GitHub spells it.
set -euo pipefail

repo=${GITHUB_REPOSITORY:?set GITHUB_REPOSITORY to <owner>/<repo> as GitHub spells it}
cd "$(dirname "$0")/.."

status=0
check() { # <what> <file> <want>
  local got
  got=$(sed -n "s/^$3//p" "$2" | head -1)
  if [ "$got" = "github.com/$repo" ]; then
    echo "ok   $2: $1 is github.com/$repo"
  else
    echo "FAIL $2: $1 is '$got', the catalog wants 'github.com/$repo'" >&2
    status=1
  fi
}
check "module path" go.mod 'module '
check "import" .traefik.yml 'import: '
exit "$status"
