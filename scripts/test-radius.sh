#!/usr/bin/env bash
set -euo pipefail

compose=(docker compose -f compose.radius.yaml)
cleanup() {
  "${compose[@]}" down --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

"${compose[@]}" config --quiet
"${compose[@]}" up -d
sleep 2

# The slim FreeRADIUS server image intentionally omits client utilities.
# Run the test client in a disposable Alpine container using host networking
# so the request originates from loopback, the only configured RADIUS client.
output="$(docker run --rm --network host alpine:3.22 sh -ec 'apk add --no-cache freeradius-utils >/dev/null; radtest testuser testpass 127.0.0.1 0 testing123' 2>&1 || true)"
if grep -q "Access-Accept" <<<"$output"; then
  echo "RADIUS smoke test passed: lab identity received Access-Accept."
  exit 0
fi

echo "RADIUS smoke test failed; radtest output:"
printf '%s\n' "$output"
"${compose[@]}" logs radius || true
exit 1
