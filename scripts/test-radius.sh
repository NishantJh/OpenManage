#!/usr/bin/env bash
set -euo pipefail

compose=(docker compose -f compose.radius.yaml)
cleanup() {
  "${compose[@]}" down --remove-orphans >/dev/null 2>&1 || true
  docker network rm openmanage-radius-lab >/dev/null 2>&1 || true
}
trap cleanup EXIT

"${compose[@]}" config --quiet
"${compose[@]}" up -d
sleep 2

# Use a disposable client at a fixed address on the dedicated Compose network.
# This avoids relying on host networking or permitting a broad client subnet.
output="$(docker run --rm --network openmanage-radius-lab --ip 172.30.99.3 alpine:3.22 sh -ec 'apk add --no-cache freeradius-utils >/dev/null; radtest testuser testpass 172.30.99.2 0 testing123' 2>&1 || true)"
if grep -q "Access-Accept" <<<"$output"; then
  echo "RADIUS smoke test passed: lab identity received Access-Accept."
  exit 0
fi

echo "RADIUS smoke test failed; radtest output:"
printf '%s\n' "$output"
"${compose[@]}" logs radius || true
exit 1
