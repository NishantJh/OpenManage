#!/usr/bin/env bash
set -euo pipefail

compose=(docker compose -f compose.radius.yaml)
network_name="openmanage-radius-lab"
client_name="openmanage-radius-lab-test-client-$$"
cleanup() {
  status=$?
  trap - EXIT
  cleanup_failed=0

  # Remove the disposable client even if the test command or readiness loop failed.
  docker rm -f "$client_name" >/dev/null 2>&1 || true
  "${compose[@]}" down --remove-orphans >/dev/null 2>&1 || cleanup_failed=1

  # Compose normally removes this network; remove it explicitly if it remains.
  if docker network inspect "$network_name" >/dev/null 2>&1; then
    docker network rm "$network_name" >/dev/null 2>&1 || cleanup_failed=1
  fi

  if docker ps -aq --filter "name=^/${client_name}$" | grep -q .; then
    echo "RADIUS smoke test cleanup failed: disposable client container remains." >&2
    cleanup_failed=1
  fi
  if "${compose[@]}" ps -aq | grep -q .; then
    echo "RADIUS smoke test cleanup failed: Compose service container remains." >&2
    cleanup_failed=1
  fi
  if docker network inspect "$network_name" >/dev/null 2>&1; then
    echo "RADIUS smoke test cleanup failed: lab network remains." >&2
    cleanup_failed=1
  fi

  if (( cleanup_failed != 0 )); then
    status=1
  fi
  exit "$status"
}
trap cleanup EXIT

"${compose[@]}" config --quiet
"${compose[@]}" up -d

# Keep one disposable client available for bounded readiness retries and both
# authentication assertions. The fixed address is confined to the lab network.
docker run --name "$client_name" --network "$network_name" --ip 172.30.99.3 -d alpine:3.22 \
  sh -ec 'apk add --no-cache freeradius-utils >/dev/null; sleep 300' >/dev/null

ready=0
for attempt in $(seq 1 20); do
  if ! docker inspect -f '{{.State.Running}}' "$client_name" 2>/dev/null | grep -qx true; then
    echo "RADIUS smoke test failed: disposable client stopped during startup." >&2
    docker logs "$client_name" 2>&1 || true
    exit 1
  fi

  if docker exec "$client_name" sh -ec 'command -v radtest >/dev/null'; then
    output=""
    if output="$(docker exec "$client_name" radtest testuser testpass 172.30.99.2 0 testing123 2>&1)" &&
      grep -Eq '^Received Access-Accept Id [0-9]+' <<<"$output"; then
      ready=1
      break
    fi
  fi
  sleep 1
done

if (( ready == 0 )); then
  echo "RADIUS smoke test failed: server did not become ready within 20 attempts." >&2
  "${compose[@]}" logs radius || true
  exit 1
fi

# Assert both radtest's exit status and the protocol response for valid credentials.
if output="$(docker exec "$client_name" radtest testuser testpass 172.30.99.2 0 testing123 2>&1)"; then
  if ! grep -Eq '^Received Access-Accept Id [0-9]+' <<<"$output"; then
    echo "RADIUS smoke test failed: valid credentials did not produce Access-Accept." >&2
    printf '%s\n' "$output"
    "${compose[@]}" logs radius || true
    exit 1
  fi
else
  status=$?
  echo "RADIUS smoke test failed: radtest exited $status for valid credentials." >&2
  printf '%s\n' "$output"
  "${compose[@]}" logs radius || true
  exit 1
fi

# Wrong credentials must be rejected; do not treat any other response as success.
if output="$(docker exec "$client_name" radtest testuser definitely-wrong-password 172.30.99.2 0 testing123 2>&1)"; then
  if ! grep -Eq '^Received Access-Reject Id [0-9]+' <<<"$output"; then
    echo "RADIUS smoke test failed: wrong password did not produce Access-Reject." >&2
    printf '%s\n' "$output"
    "${compose[@]}" logs radius || true
    exit 1
  fi
else
  status=$?
  # radtest versions may use a non-zero exit status for Access-Reject. The
  # protocol response is the assertion for this negative case.
  if ! grep -Eq '^Received Access-Reject Id [0-9]+' <<<"$output"; then
    echo "RADIUS smoke test failed: wrong-password request did not produce Access-Reject (radtest exit $status)." >&2
    printf '%s\n' "$output"
    "${compose[@]}" logs radius || true
    exit 1
  fi
fi

echo "RADIUS smoke test passed: valid credentials received Access-Accept; wrong password received Access-Reject."
