#!/usr/bin/env bash
set -euo pipefail

compose=(docker compose -f compose.radius.yaml)
cleanup() {
  "${compose[@]}" down --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

"${compose[@]}" config --quiet
"${compose[@]}" up -d

for attempt in $(seq 1 20); do
  output="$( "${compose[@]}" exec -T radius radtest testuser testpass 127.0.0.1 0 testing123 2>&1 || true )"
  if grep -q "Access-Accept" <<<"$output"; then
    echo "RADIUS smoke test passed: lab identity received Access-Accept."
    exit 0
  fi
  sleep 1
done

echo "RADIUS smoke test failed; final radtest output:"
printf '%s\n' "$output"
"${compose[@]}" logs radius || true
exit 1
