#!/usr/bin/env bash
# Assertions against the demo (`docker compose up -d` in this directory).
# "prod" is go-httpbin (JSON bodies), "sut" is whoami ("Hostname: …" bodies).
set -euo pipefail

# DEMO_PORT is the port docker-compose.yaml published the demo on.
base="${DEMO_URL:-http://localhost:${DEMO_PORT:-8000}}"
failures=0

# Wait for Traefik to load the plugin and the router.
for _ in $(seq 1 30); do
  curl -fsS -o /dev/null "$base/get" 2>/dev/null && break
  sleep 1
done

# check NAME WANT_BACKEND WANT_DEBUG_SUBSTRING [curl args…]
# WANT_DEBUG_SUBSTRING "-" means the debug header must be absent.
check() {
  local name=$1 want=$2 debug=$3
  shift 3
  local headers body got
  headers=$(mktemp)
  body=$(curl -sS -D "$headers" "$@" "$base/anything/cats?id=1")
  if grep -q '^Hostname:' <<<"$body"; then got=sut; else got=prod; fi
  local dbg
  dbg=$(grep -i '^x-siding:' "$headers" | cut -d' ' -f2- | tr -d '\r' || true)
  rm -f "$headers"
  local ok=no
  if [[ $debug == - ]]; then
    [[ -z $dbg ]] && ok=yes
  elif [[ $dbg == *"$debug"* ]]; then
    ok=yes
  fi
  if [[ $got == "$want" && $ok == yes ]]; then
    echo "ok   $name → $got${dbg:+ ($dbg)}"
  else
    echo "FAIL $name → $got${dbg:+ ($dbg)}; want $want with debug ~ '$debug'"
    failures=$((failures + 1))
  fi
}

# expect NAME PATTERN [curl args…]: a line of the response body matches
# PATTERN, once spaces are removed from it.
expect() {
  local name=$1 pattern=$2
  shift 2
  if curl -sS "$@" | tr -d ' ' | grep -qiE "$pattern"; then
    echo "ok   $name"
  else
    echo "FAIL $name"
    failures=$((failures + 1))
  fi
}

overrides() { # base64url JSON routing-overrides for cats-openapi → $1
  printf '{"services":[{"name":"cats-openapi","url":"%s"}]}' "$1" | base64 | tr '+/' '-_' | tr -d '=\n'
}

tenancy='baggage: request-tenancy=test/env-1'

echo "# file registry"
check "plain request" prod -
check "baggage tenancy" sut "routed=http://sut:80" -H "$tenancy"
check "test account" sut "routed=http://sut:80" -H 'X-User-Id: tester-1'
check "unknown tenancy" prod "is not registered" \
  -H 'baggage: request-tenancy=test/nope'
check "non-test tenancy" prod 'is not under "test/"' \
  -H 'baggage: request-tenancy=production'
check "header overrides" sut "routed=http://sut" \
  -H "baggage: routing-overrides=$(overrides http://sut)"
check "disallowed host" prod "not in allowedHosts" \
  -H "baggage: routing-overrides=$(overrides http://prod:8080)"
check "disallowed port" prod '"sut:8081" is not in allowedHosts' \
  -H "baggage: routing-overrides=$(overrides http://sut:8081)"
check "non-test tenancy with overrides" prod 'is not under "test/"' \
  -H "baggage: request-tenancy=production,routing-overrides=$(overrides http://sut)"

# The SUT sees the injected routing for its own downstream calls.
expect "baggage injected into the SUT request" \
  '^baggage:.*request-tenancy=test/env-1.*routing-overrides=' \
  -H 'X-User-Id: tester-1' "$base/"
# An escaped slash reaches the SUT as it would reach prod.
expect "escaped path kept" '^GET/files/a%2FbHTTP' -H "$tenancy" "$base/files/a%2Fb"

echo "# inline registry (inline.localhost)"
inline=(-H 'Host: inline.localhost')
check "plain request" prod - "${inline[@]}"
check "baggage tenancy" sut "routed=http://sut:80" "${inline[@]}" -H "$tenancy"
check "test account" sut "routed=http://sut:80" "${inline[@]}" -H 'X-User-Id: tester-1'
check "expired tenancy" prod "expired at 2020-01-01T00:00:00Z" "${inline[@]}" \
  -H 'baggage: request-tenancy=test/gone'

echo "# trustedSources (edge.localhost): curl is not a trusted peer"
edge=(-H 'Host: edge.localhost')
check "baggage tenancy ignored" prod - "${edge[@]}" -H "$tenancy"
check "test account still routes" sut - "${edge[@]}" -H 'X-User-Id: tester-1'
# go-httpbin echoes the request headers: the tenancy went no further, and
# the rest of the baggage did.
echoed=$(curl -sS "${edge[@]}" -H "$tenancy,keep=1" "$base/headers")
if grep -q 'keep=1' <<<"$echoed" && ! grep -q 'request-tenancy' <<<"$echoed"; then
  echo "ok   tenancy stripped, other baggage kept"
else
  echo "FAIL tenancy stripped, other baggage kept"
  failures=$((failures + 1))
fi

if ((failures)); then
  echo "$failures check(s) failed"
  exit 1
fi
echo "all checks passed"
