#!/usr/bin/env bash
set -Eeuo pipefail

# Starts a disposable OpenManage + PostgreSQL stack, exercises the live HTTP API,
# and removes the stack and its test database volume on exit.
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

command -v docker >/dev/null || { echo "ERROR: docker is required" >&2; exit 2; }
docker compose version >/dev/null
command -v curl >/dev/null || { echo "ERROR: curl is required" >&2; exit 2; }
command -v python3 >/dev/null || { echo "ERROR: python3 is required" >&2; exit 2; }

project="openmanage-smoke-${GITHUB_RUN_ID:-local}-$$"
port="${OPENMANAGE_SMOKE_PORT:-18080}"
base_url="http://127.0.0.1:${port}"
db_password="ci-smoke-db-password"
admin_password="ci-smoke-admin-password-123"
env_file="$(mktemp)"
cookie_jar="$(mktemp)"
login_body="$(mktemp)"
device_body="$(mktemp)"

cleanup() {
  docker compose --project-name "$project" --env-file "$env_file" down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -f "$env_file" "$cookie_jar" "$login_body" "$device_body"
}
trap cleanup EXIT

cat > "$env_file" <<EOF
POSTGRES_PASSWORD=$db_password
OPENMANAGE_ADMIN_USERNAME=admin
OPENMANAGE_ADMIN_PASSWORD=$admin_password
OPENMANAGE_COOKIE_SECURE=false
OPENMANAGE_ENCRYPTION_KEY=
OPENMANAGE_PORT=127.0.0.1:$port
EOF

echo "Starting disposable OpenManage stack on 127.0.0.1:$port ..."
docker compose --project-name "$project" --env-file "$env_file" up -d --build

ready=0
for attempt in $(seq 1 60); do
  if curl --silent --show-error --fail "$base_url/api/healthz" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 2
done
if [[ "$ready" != 1 ]]; then
  docker compose --project-name "$project" --env-file "$env_file" logs --no-color
  echo "ERROR: OpenManage did not become healthy within 120 seconds" >&2
  exit 1
fi
health="$(curl --silent --show-error --fail "$base_url/api/healthz")"
python3 -c 'import json,sys; d=json.load(sys.stdin); assert d.get("status")=="ok" and d.get("database")=="ok", d' <<< "$health"
echo "PASS: startup, database migrations, and health endpoint"

status="$(curl --silent --show-error -o /dev/null -w '%{http_code}' "$base_url/api/devices")"
[[ "$status" == "401" ]] || { echo "ERROR: unauthenticated device list returned HTTP $status (expected 401)" >&2; exit 1; }
echo "PASS: unauthenticated API access is rejected"

status="$(curl --silent --show-error -o "$login_body" -w '%{http_code}' \
  -c "$cookie_jar" -H 'Content-Type: application/json' \
  --data "{\"username\":\"admin\",\"password\":\"$admin_password\"}" \
  "$base_url/api/auth/login")"
[[ "$status" == "200" ]] || { cat "$login_body" >&2; echo "ERROR: login returned HTTP $status" >&2; exit 1; }
csrf="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["csrf_token"])' < "$login_body")"
[[ -n "$csrf" ]] || { echo "ERROR: login did not return a CSRF token" >&2; exit 1; }
echo "PASS: admin login and session/CSRF token issuance"

status="$(curl --silent --show-error -o /dev/null -w '%{http_code}' \
  -b "$cookie_jar" "$base_url/api/audit")"
[[ "$status" == "200" ]] || { echo "ERROR: admin audit request returned HTTP $status" >&2; exit 1; }
echo "PASS: admin audit API authorization"

status="$(curl --silent --show-error -o "$device_body" -w '%{http_code}' \
  -b "$cookie_jar" -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' \
  --data '{"name":"smoke-test-router","address":"192.0.2.10","model":"test-model","firmware":"test"}' \
  "$base_url/api/devices")"
[[ "$status" == "201" ]] || { cat "$device_body" >&2; echo "ERROR: device creation returned HTTP $status" >&2; exit 1; }
device_id="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' < "$device_body")"
[[ -n "$device_id" ]] || { echo "ERROR: device creation returned no ID" >&2; exit 1; }
echo "PASS: authenticated device creation with CSRF protection"

devices="$(curl --silent --show-error --fail -b "$cookie_jar" "$base_url/api/devices")"
python3 -c 'import json,sys; ds=json.load(sys.stdin); assert any(d.get("id")==sys.argv[1] for d in ds), ds' "$device_id" <<< "$devices"
echo "PASS: device inventory read-after-write"

status="$(curl --silent --show-error -o /dev/null -w '%{http_code}' \
  -X DELETE -b "$cookie_jar" -H "X-CSRF-Token: $csrf" "$base_url/api/devices/$device_id")"
[[ "$status" == "204" ]] || { echo "ERROR: device deletion returned HTTP $status (expected 204)" >&2; exit 1; }
echo "PASS: authenticated device deletion"

status="$(curl --silent --show-error -o /dev/null -w '%{http_code}' \
  -X POST -b "$cookie_jar" -H "X-CSRF-Token: $csrf" "$base_url/api/auth/logout")"
[[ "$status" == "204" ]] || { echo "ERROR: logout returned HTTP $status (expected 204)" >&2; exit 1; }
status="$(curl --silent --show-error -o /dev/null -w '%{http_code}' "$base_url/api/devices")"
[[ "$status" == "401" ]] || { echo "ERROR: device list after logout returned HTTP $status (expected 401)" >&2; exit 1; }
echo "PASS: logout invalidates the session"

echo "All OpenManage runtime smoke tests passed."
