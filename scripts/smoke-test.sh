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

# Persistent monitoring settings survive an application-container restart.
settings="$(curl --silent --show-error --fail -b "$cookie_jar" "$base_url/api/monitoring/settings")"
python3 -c 'import json,sys; d=json.load(sys.stdin); assert d.get("enabled") is False and d.get("interval_seconds")==300, d' <<< "$settings"
status="$(curl --silent --show-error -o "$device_body" -w '%{http_code}' \
  -X PUT -b "$cookie_jar" -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' \
  --data '{"enabled":true,"interval_seconds":60}' "$base_url/api/monitoring/settings")"
[[ "$status" == "200" ]] || { cat "$device_body" >&2; echo "ERROR: enabling monitoring returned HTTP $status" >&2; exit 1; }
python3 -c 'import json,sys; d=json.load(sys.stdin); assert d.get("enabled") is True and d.get("interval_seconds")==60, d' < "$device_body"
echo "PASS: monitoring settings can be enabled and validated"

status="$(curl --silent --show-error -o "$device_body" -w '%{http_code}' \
  -X PUT -b "$cookie_jar" -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' \
  --data '{"enabled":true,"interval_seconds":59}' "$base_url/api/monitoring/settings")"
[[ "$status" == "400" ]] || { echo "ERROR: invalid monitoring interval returned HTTP $status (expected 400)" >&2; exit 1; }

docker compose --project-name "$project" --env-file "$env_file" restart app >/dev/null
ready=0
for attempt in $(seq 1 30); do
  if curl --silent --show-error --fail "$base_url/api/healthz" >/dev/null 2>&1; then ready=1; break; fi
  sleep 1
done
[[ "$ready" == 1 ]] || { echo "ERROR: app did not recover after restart" >&2; exit 1; }
settings="$(curl --silent --show-error --fail -b "$cookie_jar" "$base_url/api/monitoring/settings")"
python3 -c 'import json,sys; d=json.load(sys.stdin); assert d.get("enabled") is True and d.get("interval_seconds")==60, d' <<< "$settings"
history="$(curl --silent --show-error --fail -b "$cookie_jar" "$base_url/api/monitoring/history?limit=10")"
python3 -c 'import json,sys; d=json.load(sys.stdin); assert isinstance(d,list), d' <<< "$history"
echo "PASS: monitoring configuration persists across app restart and history API is available"

status="$(curl --silent --show-error -o "$device_body" -w '%{http_code}' \
  -X PUT -b "$cookie_jar" -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' \
  --data '{"enabled":false,"interval_seconds":300}' "$base_url/api/monitoring/settings")"
[[ "$status" == "200" ]] || { cat "$device_body" >&2; echo "ERROR: disabling monitoring returned HTTP $status" >&2; exit 1; }
echo "PASS: persistent monitoring can be disabled"

# Force an audit insert failure and prove that device creation is rolled back.
docker compose --project-name "$project" --env-file "$env_file" exec -T db psql -U openmanage -d openmanage -v ON_ERROR_STOP=1 \
  -c "CREATE FUNCTION smoke_fail_audit() RETURNS trigger LANGUAGE plpgsql AS \$\$ BEGIN RAISE EXCEPTION 'intentional smoke-test audit failure'; END; \$\$; CREATE TRIGGER smoke_fail_audit_trigger BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION smoke_fail_audit();"
status="$(curl --silent --show-error -o "$device_body" -w '%{http_code}' \
  -b "$cookie_jar" -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' \
  --data '{"name":"audit-failure-router","address":"192.0.2.11","model":"test-model","firmware":"test"}' \
  "$base_url/api/devices")"
[[ "$status" == "500" ]] || { echo "ERROR: device creation with failed audit returned HTTP $status (expected 500)" >&2; exit 1; }
docker compose --project-name "$project" --env-file "$env_file" exec -T db psql -U openmanage -d openmanage -v ON_ERROR_STOP=1 \
  -c "DROP TRIGGER smoke_fail_audit_trigger ON audit_events; DROP FUNCTION smoke_fail_audit();"
devices="$(curl --silent --show-error --fail -b "$cookie_jar" "$base_url/api/devices")"
python3 -c 'import json,sys; ds=json.load(sys.stdin); assert not any(d.get("name")=="audit-failure-router" for d in ds), ds' <<< "$devices"
echo "PASS: failed audit insert rolls back device creation"

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

# A failed delete audit must restore the deleted inventory row.
docker compose --project-name "$project" --env-file "$env_file" exec -T db psql -U openmanage -d openmanage -v ON_ERROR_STOP=1 \
  -c "CREATE FUNCTION smoke_fail_audit() RETURNS trigger LANGUAGE plpgsql AS \$\$ BEGIN RAISE EXCEPTION 'intentional smoke-test audit failure'; END; \$\$; CREATE TRIGGER smoke_fail_audit_trigger BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION smoke_fail_audit();"
status="$(curl --silent --show-error -o /dev/null -w '%{http_code}' \
  -X DELETE -b "$cookie_jar" -H "X-CSRF-Token: $csrf" "$base_url/api/devices/$device_id")"
[[ "$status" == "500" ]] || { echo "ERROR: device deletion with failed audit returned HTTP $status (expected 500)" >&2; exit 1; }
docker compose --project-name "$project" --env-file "$env_file" exec -T db psql -U openmanage -d openmanage -v ON_ERROR_STOP=1 \
  -c "DROP TRIGGER smoke_fail_audit_trigger ON audit_events; DROP FUNCTION smoke_fail_audit();"
status="$(curl --silent --show-error -o /dev/null -w '%{http_code}' -b "$cookie_jar" "$base_url/api/devices/$device_id")"
[[ "$status" == "200" ]] || { echo "ERROR: device row was not preserved after audit failure (HTTP $status)" >&2; exit 1; }
echo "PASS: failed audit insert rolls back device deletion"

status="$(curl --silent --show-error -o /dev/null -w '%{http_code}' \
  -X DELETE -b "$cookie_jar" -H "X-CSRF-Token: $csrf" "$base_url/api/devices/$device_id")"
[[ "$status" == "204" ]] || { echo "ERROR: device deletion returned HTTP $status (expected 204)" >&2; exit 1; }
echo "PASS: authenticated device deletion"

# Floor plan background image: PNG/JPEG only, served back with a fixed type, removable.
plan_png="$(mktemp)"
python3 - "$plan_png" <<'PY'
import struct, sys, zlib
def chunk(t, d): c = struct.pack(">I", len(d)) + t + d; return c + struct.pack(">I", zlib.crc32(t + d))
raw = b"".join(b"\x00" + b"\xff\xff\xff" * 4 for _ in range(2))
open(sys.argv[1], "wb").write(b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", 4, 2, 8, 2, 0, 0, 0)) + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))
PY
plan_img() { curl --silent --show-error -o "$device_body" -w '%{http_code}' -X "$1" -b "$cookie_jar" -H "X-CSRF-Token: $csrf" "${@:3}" "$base_url/api/floor-plan/image$2"; }
[[ "$(plan_img GET)" == "404" ]] || { echo "ERROR: plan image existed before upload" >&2; exit 1; }
printf '<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>' > "$device_body.svg"
[[ "$(plan_img PUT "" --data-binary @"$device_body.svg" -H 'Content-Type: image/png')" == "400" ]] || { echo "ERROR: SVG plan image was accepted" >&2; exit 1; }
[[ "$(plan_img PUT "" --data-binary @"$plan_png" -H 'Content-Type: text/plain')" == "200" ]] || { cat "$device_body" >&2; echo "ERROR: PNG plan image upload failed" >&2; exit 1; }
python3 -c 'import json,sys; i=json.load(sys.stdin)["image"]; assert i["px_w"]==4 and i["px_h"]==2 and i["width_m"]>0 and i["version"], i' < "$device_body"
plan_headers="$(curl --silent --show-error -D - -o "$device_body" -b "$cookie_jar" "$base_url/api/floor-plan/image")"
grep -qi '^content-type: image/png' <<< "$plan_headers" && grep -qi '^x-content-type-options: nosniff' <<< "$plan_headers" || { echo "ERROR: plan image served with unexpected headers: $plan_headers" >&2; exit 1; }
status="$(curl --silent --show-error -o "$device_body" -w '%{http_code}' -X PUT -b "$cookie_jar" -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' \
  --data '{"rooms":[],"placements":[],"image":{"x":1,"y":2,"width_m":20}}' "$base_url/api/floor-plan")"
[[ "$status" == "200" ]] || { cat "$device_body" >&2; echo "ERROR: placing the plan image returned HTTP $status" >&2; exit 1; }
python3 -c 'import json,sys; i=json.load(sys.stdin)["image"]; assert (i["x"],i["y"],i["width_m"])==(1,2,20) and i["px_w"]==4, i' < "$device_body"
status="$(curl --silent --show-error -o "$device_body" -w '%{http_code}' -X PUT -b "$cookie_jar" -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' \
  --data '{"rooms":[],"placements":[],"image":{"x":1,"y":2,"width_m":0.1}}' "$base_url/api/floor-plan")"
[[ "$status" == "400" ]] || { echo "ERROR: out-of-range plan image width returned HTTP $status (expected 400)" >&2; exit 1; }
[[ "$(plan_img DELETE)" == "204" ]] || { echo "ERROR: plan image deletion failed" >&2; exit 1; }
[[ "$(plan_img GET)" == "404" ]] || { echo "ERROR: plan image still served after deletion" >&2; exit 1; }
plan="$(curl --silent --show-error --fail -b "$cookie_jar" "$base_url/api/floor-plan")"
python3 -c 'import json,sys; assert "image" not in json.load(sys.stdin)' <<< "$plan"
rm -f "$plan_png" "$device_body.svg"
echo "PASS: floor plan image upload, type checks, placement and removal"

# Browser check of sidebar navigation. Playwright is installed into a throwaway directory so the
# web/ lockfile is untouched. OPENMANAGE_UI_CHROMIUM may point at an existing Chromium binary.
if [[ "${OPENMANAGE_SKIP_UI_TEST:-0}" == 1 ]]; then
  echo "SKIP: browser navigation test (OPENMANAGE_SKIP_UI_TEST=1)"
else
  command -v node >/dev/null || { echo "ERROR: node is required for the browser test (set OPENMANAGE_SKIP_UI_TEST=1 to skip)" >&2; exit 2; }
  ui_dir="$(mktemp -d)"
  trap 'rm -rf "$ui_dir"; cleanup' EXIT
  (cd "$ui_dir" && npm init -y >/dev/null && npm install --no-audit --no-fund --silent playwright@1.63.0)
  if [[ -z "${OPENMANAGE_UI_CHROMIUM:-}" ]]; then
    (cd "$ui_dir" && npx playwright install chromium >/dev/null)
  fi
  (cd "$ui_dir" && node "$ROOT_DIR/scripts/ui-nav-test.mjs" "$base_url" admin "$admin_password")
fi

status="$(curl --silent --show-error -o /dev/null -w '%{http_code}' \
  -X POST -b "$cookie_jar" -H "X-CSRF-Token: $csrf" "$base_url/api/auth/logout")"
[[ "$status" == "204" ]] || { echo "ERROR: logout returned HTTP $status (expected 204)" >&2; exit 1; }
status="$(curl --silent --show-error -o /dev/null -w '%{http_code}' "$base_url/api/devices")"
[[ "$status" == "401" ]] || { echo "ERROR: device list after logout returned HTTP $status (expected 401)" >&2; exit 1; }
echo "PASS: logout invalidates the session"

# Ten failed attempts are permitted in the configured window; the next is throttled.
for attempt in $(seq 1 10); do
  status="$(curl --silent --show-error -o /dev/null -w '%{http_code}' \
    -H 'Content-Type: application/json' \
    --data '{"username":"admin","password":"intentionally-wrong-password"}' \
    "$base_url/api/auth/login")"
  [[ "$status" == "401" ]] || { echo "ERROR: failed login attempt $attempt returned HTTP $status (expected 401)" >&2; exit 1; }
done
status="$(curl --silent --show-error -D "$login_body" -o /dev/null -w '%{http_code}' \
  -H 'Content-Type: application/json' \
  --data '{"username":"admin","password":"intentionally-wrong-password"}' \
  "$base_url/api/auth/login")"
[[ "$status" == "429" ]] || { echo "ERROR: throttled login returned HTTP $status (expected 429)" >&2; exit 1; }
grep -Eiq '^Retry-After: [1-9][0-9]*' "$login_body" || { echo "ERROR: throttled login omitted Retry-After" >&2; exit 1; }
echo "PASS: repeated failed logins are rate-limited with Retry-After"

echo "All OpenManage runtime smoke tests passed."
