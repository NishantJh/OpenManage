#!/usr/bin/env bash
set -Eeuo pipefail

# Starts a disposable OpenManage + PostgreSQL stack next to a real OpenWrt rootfs
# container (procd, ubus, dropbear), then exercises SSH enrollment, health checks,
# and persistent monitoring against it, including failure scenarios. Everything,
# including generated keys, is removed on exit. Nothing here touches real devices.
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

for tool in docker curl python3 ssh-keygen; do
  command -v "$tool" >/dev/null || { echo "ERROR: $tool is required" >&2; exit 2; }
done
docker compose version >/dev/null

openwrt_image="${OPENWRT_IMAGE:-openwrt/rootfs:x86-64-24.10.4}"
project="openmanage-openwrt-${GITHUB_RUN_ID:-local}-$$"
port="${OPENMANAGE_LAB_PORT:-18081}"
base_url="http://127.0.0.1:${port}"
router="${project}-router"
work="$(mktemp -d)"
env_file="$work/env"
cookie_jar="$work/cookies"
body="$work/body"

compose() { docker compose --project-name "$project" --env-file "$env_file" "$@"; }
cleanup() {
  docker rm -f "$router" >/dev/null 2>&1 || true
  compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT
fail() { echo "ERROR: $*" >&2; exit 1; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); $1" "${@:2}"; }

# Generated, throwaway credentials only.
ssh-keygen -q -t ed25519 -N '' -C openmanage-lab -f "$work/mgmt_key"
ssh-keygen -q -t ed25519 -N '' -C openmanage-lab-other -f "$work/other_key"
cat > "$env_file" <<EOF
POSTGRES_PASSWORD=lab-db-password
OPENMANAGE_ADMIN_USERNAME=admin
OPENMANAGE_ADMIN_PASSWORD=lab-admin-password-123
OPENMANAGE_COOKIE_SECURE=false
OPENMANAGE_ENCRYPTION_KEY=$(python3 -c 'import base64,secrets; print(base64.b64encode(secrets.token_bytes(32)).decode())')
OPENMANAGE_PORT=127.0.0.1:$port
EOF

echo "Starting disposable OpenManage stack on 127.0.0.1:$port ..."
# OPENMANAGE_LAB_SKIP_BUILD=1 reuses an existing openmanage:local image (for hosts whose
# Docker builds cannot reach package mirrors).
if [[ "${OPENMANAGE_LAB_SKIP_BUILD:-0}" == 1 ]]; then compose up -d; else compose up -d --build; fi
for attempt in $(seq 1 60); do
  curl --silent --fail "$base_url/api/healthz" >/dev/null 2>&1 && break
  [[ "$attempt" == 60 ]] && { compose logs --no-color; fail "OpenManage did not become healthy"; }
  sleep 2
done

network="${project}_default"
echo "Starting OpenWrt container ($openwrt_image) on $network ..."
docker run -d --name "$router" --network "$network" "$openwrt_image" /sbin/init >/dev/null
for attempt in $(seq 1 30); do
  docker exec "$router" sh -c 'pgrep dropbear >/dev/null && ubus call system board >/dev/null' 2>/dev/null && break
  [[ "$attempt" == 30 ]] && { docker logs "$router" | tail -20; fail "OpenWrt did not start dropbear and ubus"; }
  sleep 1
done
# docker cp keeps the host file owner (uid 1001 on GitHub runners); dropbear ignores
# an authorized_keys file that root does not own.
docker cp "$work/mgmt_key.pub" "$router:/etc/dropbear/authorized_keys"
docker exec "$router" sh -c "chown root:root /etc/dropbear/authorized_keys && chmod 600 /etc/dropbear/authorized_keys"
router_ip="$(docker inspect -f "{{(index .NetworkSettings.Networks \"$network\").IPAddress}}" "$router")"
[[ -n "$router_ip" ]] || fail "could not determine OpenWrt container address"

# Fingerprint taken from the router itself (the trusted-console path), not from the network.
host_fp="$(docker exec "$router" dropbearkey -y -f /etc/dropbear/dropbear_ed25519_host_key | grep '^ssh-ed25519' | ssh-keygen -lf - | awk '{print $2}')"
[[ "$host_fp" == SHA256:* ]] || fail "could not read OpenWrt host-key fingerprint"
echo "PASS: OpenWrt $(docker exec "$router" sh -c '. /etc/openwrt_release; echo $DISTRIB_RELEASE') running at $router_ip"

status="$(curl --silent -o "$body" -w '%{http_code}' -c "$cookie_jar" -H 'Content-Type: application/json' \
  --data '{"username":"admin","password":"lab-admin-password-123"}' "$base_url/api/auth/login")"
[[ "$status" == 200 ]] || fail "login returned HTTP $status"
csrf="$(json 'print(d["csrf_token"])' < "$body")"

api() { # method path [json-body] -> sets $status, response in $body
  local args=(--silent -o "$body" -w '%{http_code}' -X "$1" -b "$cookie_jar" -H "X-CSRF-Token: $csrf")
  [[ $# -ge 3 ]] && args+=(-H 'Content-Type: application/json' --data "$3")
  status="$(curl "${args[@]}" "$base_url$2")"
}
enroll_body() { # key-file fingerprint
  python3 -c 'import json,sys; print(json.dumps({"username":"root","private_key":open(sys.argv[1]).read(),"expected_fingerprint":sys.argv[2]}))' "$1" "$2"
}
expect_code() { # http-status error-code label
  [[ "$status" == "$1" ]] || { cat "$body" >&2; fail "$3: HTTP $status (expected $1)"; }
  json 'assert d.get("code")==sys.argv[1], d' "$2" < "$body"
  ! grep -q "PRIVATE KEY" "$body" || fail "$3: response echoed key material"
  echo "PASS: $3"
}

api POST /api/devices "{\"name\":\"lab-router\",\"address\":\"$router_ip\",\"model\":\"x86-64\",\"firmware\":\"OpenWrt\"}"
[[ "$status" == 201 ]] || fail "device creation returned HTTP $status"
device_id="$(json 'assert d["enrollable"] is True, d; print(d["id"])' < "$body")"

# Fresh OpenWrt has no root password, so dropbear accepts SSH "none" authentication.
# That must not be mistaken for proof that a key works.
api POST "/api/devices/$device_id/enroll" "$(enroll_body "$work/other_key" "$host_fp")"
expect_code 422 key_not_verified "enrollment refuses a router that skips key authentication (blank root password)"
root_password="$(python3 -c 'import secrets; print(secrets.token_urlsafe(18))')"
docker exec "$router" sh -c "printf '%s\n%s\n' '$root_password' '$root_password' | passwd root >/dev/null 2>&1"

api POST "/api/devices/$device_id/enroll" "$(enroll_body "$work/mgmt_key" "SHA256:$(printf 'A%.0s' $(seq 43))")"
expect_code 422 host_key_mismatch "enrollment rejects a wrong host-key fingerprint"
api POST "/api/devices/$device_id/enroll" "$(enroll_body "$work/other_key" "$host_fp")"
expect_code 422 ssh_auth_failed "enrollment rejects a key the router does not authorize"

api POST "/api/devices/$device_id/enroll" "$(enroll_body "$work/mgmt_key" "$host_fp")"
[[ "$status" == 200 ]] || { cat "$body" >&2; fail "enrollment returned HTTP $status"; }
json 'assert d["host_key_fingerprint"]==sys.argv[1], d' "$host_fp" < "$body"
! grep -q "PRIVATE KEY" "$body" || fail "enrollment response echoed key material"
echo "PASS: enrollment with pinned host key and authorized key"

api POST "/api/devices/$device_id/health-check"
[[ "$status" == 200 ]] || { cat "$body" >&2; fail "health check returned HTTP $status"; }
json '
assert d["status"]=="online", d
failed=[r["name"] for r in d["results"] if r["status"]!="ok"]
assert not failed, ("failed diagnostics", failed, d)
board=[r for r in d["results"] if r["name"]=="system_board"][0]
assert "OpenWrt" in board["output"], board' < "$body"
echo "PASS: read-only health checks succeed on real OpenWrt"
api GET "/api/devices/$device_id"
json 'assert d["firmware"].startswith("OpenWrt 24.10"), d' < "$body"
echo "PASS: health check records the firmware the router reports"

api POST "/api/devices/$device_id/config-snapshots"
[[ "$status" == 200 ]] || { cat "$body" >&2; fail "first config snapshot returned HTTP $status"; }
first_snapshot="$(json 'assert d["first"] and not d["changed"] and d["snapshot"]["option_count"]>50, d; print(d["snapshot"]["id"])' < "$body")"
api POST "/api/devices/$device_id/config-snapshots"
json 'assert not d["first"] and not d["changed"] and d["snapshot"]["id"]==int(sys.argv[1]), d' "$first_snapshot" < "$body"
echo "PASS: config snapshot captured; an unchanged config is not stored twice"
snapshot_secret="lab-secret-$RANDOM$RANDOM"
docker exec "$router" sh -c "uci set system.@system[0].hostname=lab-router && uci set system.@system[0].api_key='$snapshot_secret' && uci commit system"
api POST "/api/devices/$device_id/config-snapshots"
second_snapshot="$(json 'assert d["changed"] and d["change_count"]==2, d; print(d["snapshot"]["id"])' < "$body")"
api GET "/api/devices/$device_id/config-snapshots/$second_snapshot/changes"
[[ "$status" == 200 ]] || { cat "$body" >&2; fail "snapshot changes returned HTTP $status"; }
! grep -q "$snapshot_secret" "$body" || fail "snapshot diff leaked a secret value"
json '
c={x["path"]:x for x in d["changes"]}
assert c["system.@system[0].hostname"]=={"path":"system.@system[0].hostname","kind":"changed","old":"OpenWrt","new":"lab-router"}, c
assert c["system.@system[0].api_key"]["redacted"] and "new" not in c["system.@system[0].api_key"], c
assert len(c)==2, c' < "$body"
api GET "/api/devices/$device_id/config-snapshots"
json 'assert len(d)==2, d' < "$body"
leaks="$(compose exec -T db psql -U openmanage -d openmanage -tAc "SELECT count(*) FROM config_snapshots WHERE position(convert_to('$snapshot_secret','UTF8') in ciphertext)>0 OR position(convert_to('lab-router','UTF8') in ciphertext)>0")"
[[ "$leaks" == 0 ]] || fail "config snapshot plaintext found in PostgreSQL"
echo "PASS: config drift is detected, secrets are redacted, and snapshots are encrypted at rest"
api GET "/api/devices/$device_id"; json 'assert d["config_drift"], d' < "$body"
api POST "/api/devices/$device_id/config-snapshots/$second_snapshot/acknowledge"
json 'assert d["acknowledged"]==1, d' < "$body"
api GET "/api/devices/$device_id"; json 'assert not d["config_drift"], d' < "$body"
echo "PASS: drift counts for the device until an administrator acknowledges it"

# Scheduled snapshots: backdating the newest snapshot makes the device due on the
# next 15-second scheduler poll.
api PUT /api/monitoring/settings '{"enabled":false,"interval_seconds":300,"snapshots_enabled":true,"snapshot_interval_seconds":900}'
json 'assert d["snapshots_enabled"] and d["snapshot_interval_seconds"]==900 and not d["enabled"], d' < "$body"
docker exec "$router" sh -c "uci set system.@system[0].hostname=lab-scheduled && uci commit system"
compose exec -T db psql -q -U openmanage -d openmanage -c "UPDATE config_snapshots SET last_verified_at=NOW()-INTERVAL '1 hour' WHERE device_id='$device_id'" >/dev/null
for attempt in $(seq 1 30); do
  api GET "/api/devices/$device_id/config-snapshots"
  json 'assert len(d)==3 and d[0]["drift"] and d[0]["acknowledged_at"] is None, d' < "$body" 2>/dev/null && break
  [[ "$attempt" == 30 ]] && { cat "$body" >&2; fail "scheduled snapshot did not record drift"; }
  sleep 2
done
api GET "/api/devices/$device_id"; json 'assert d["config_drift"], d' < "$body"
scheduled_audit="$(compose exec -T db psql -U openmanage -d openmanage -tAc "SELECT count(*) FROM audit_events WHERE action='device.config_snapshot' AND user_id IS NULL AND details->>'scheduled'='true' AND details->>'changed'='true'")"
[[ "$scheduled_audit" == 1 ]] || fail "scheduled drift was not audited as a system action"
echo "PASS: scheduled snapshots notice drift without a manual capture"

# A spare address in the same private subnet has no SSH listener.
unreachable_ip="$(python3 -c 'import ipaddress,sys; n=ipaddress.ip_interface(sys.argv[1]+"/16").network; print(n.broadcast_address-2)' "$router_ip")"
api POST /api/devices "{\"name\":\"lab-missing\",\"address\":\"$unreachable_ip\"}"
missing_id="$(json 'print(d["id"])' < "$body")"
api POST "/api/devices/$missing_id/enroll" "$(enroll_body "$work/mgmt_key" "$host_fp")"
expect_code 502 device_unreachable "enrollment reports an unreachable device"
api DELETE "/api/devices/$missing_id"

# User management: a second user, role changes and disabling revoke sessions, and the
# last active administrator cannot be removed.
login_as() { # username password jar -> sets $status, $csrf_other
  status="$(curl --silent -o "$body" -w '%{http_code}' -c "$3" -H 'Content-Type: application/json' \
    --data "{\"username\":\"$1\",\"password\":\"$2\"}" "$base_url/api/auth/login")"
  [[ "$status" != 200 ]] || csrf_other="$(json 'print(d["csrf_token"])' < "$body")"
}
api_as() { # jar csrf method path [json-body]
  local args=(--silent -o "$body" -w '%{http_code}' -X "$3" -b "$1" -H "X-CSRF-Token: $2")
  [[ $# -ge 5 ]] && args+=(-H 'Content-Type: application/json' --data "$5")
  status="$(curl "${args[@]}" "$base_url$4")"
}
op_jar="$work/operator.cookies"
api POST /api/users '{"username":"lab-operator","role":"operator","password":"lab-operator-pass-1"}'
[[ "$status" == 201 ]] || { cat "$body" >&2; fail "creating a user returned HTTP $status"; }
op_id="$(json 'assert d["role"]=="operator" and not d["disabled"], d; print(d["id"])' < "$body")"
api POST /api/users '{"username":"lab-operator","role":"readonly","password":"lab-operator-pass-1"}'
[[ "$status" == 409 ]] || fail "duplicate username returned HTTP $status"
login_as lab-operator lab-operator-pass-1 "$op_jar"; [[ "$status" == 200 ]] || fail "new operator could not log in"
api_as "$op_jar" "$csrf_other" GET /api/users; [[ "$status" == 403 ]] || fail "operator listed users (HTTP $status)"
api_as "$op_jar" "$csrf_other" POST /api/auth/password '{"current_password":"wrong-password-000","new_password":"lab-operator-pass-2"}'
[[ "$status" == 403 ]] || fail "wrong current password returned HTTP $status"
api_as "$op_jar" "$csrf_other" POST /api/auth/password '{"current_password":"lab-operator-pass-1","new_password":"lab-operator-pass-2"}'
[[ "$status" == 200 ]] || fail "changing own password returned HTTP $status"
api_as "$op_jar" "$csrf_other" GET /api/auth/me; [[ "$status" == 200 ]] || fail "password change ended the caller's own session"
api PATCH "/api/users/$op_id" '{"role":"readonly"}'
json 'assert d["role"]=="readonly", d' < "$body"
api_as "$op_jar" "$csrf_other" GET /api/auth/me; [[ "$status" == 401 ]] || fail "role change did not revoke sessions (HTTP $status)"
login_as lab-operator lab-operator-pass-2 "$op_jar"; [[ "$status" == 200 ]] || fail "login with the new password failed"
api PATCH "/api/users/$op_id" '{"disabled":true}'
json 'assert d["disabled"], d' < "$body"
api_as "$op_jar" "$csrf_other" GET /api/auth/me; [[ "$status" == 401 ]] || fail "disabling did not revoke sessions"
login_as lab-operator lab-operator-pass-2 "$op_jar"; [[ "$status" == 401 ]] || fail "disabled user could log in (HTTP $status)"
api GET /api/users
admin_id="$(json 'print([u["id"] for u in d if u["username"]=="admin"][0])' < "$body")"
api PATCH "/api/users/$admin_id" '{"role":"operator"}'
[[ "$status" == 409 ]] || fail "last administrator could be demoted (HTTP $status)"
api PATCH "/api/users/$admin_id" '{"disabled":true}'
[[ "$status" == 409 ]] || fail "last administrator could be disabled (HTTP $status)"
api GET /api/auth/me; [[ "$status" == 200 ]] || fail "administrator session was lost"
users_audit="$(compose exec -T db psql -U openmanage -d openmanage -tAc "SELECT count(*) FROM audit_events WHERE action IN ('user.create','user.update','user.password_change')")"
[[ "$users_audit" == 4 ]] || fail "user management audit events: $users_audit (expected 4)"
! compose exec -T db psql -U openmanage -d openmanage -tAc "SELECT details::text FROM audit_events WHERE resource_type='user'" | grep -q "lab-operator-pass" \
  || fail "a password reached the audit log"
echo "PASS: user management creates, demotes and disables users, revokes their sessions, and keeps one administrator"

# Monitoring scenarios. Clearing this device's history makes it due on the next
# 15-second scheduler poll instead of waiting a full interval.
make_due() { compose exec -T db psql -q -U openmanage -d openmanage -c "DELETE FROM monitoring_checks WHERE device_id='$device_id'" >/dev/null; }
wait_check() { # expected-status label [reason-substring]
  for attempt in $(seq 1 45); do
    api GET "/api/monitoring/history?limit=20"
    if json 'cs=[c for c in d if c["device_id"]==sys.argv[1]]; assert cs and cs[0]["status"]==sys.argv[2] and sys.argv[3].lower() in cs[0]["reason"].lower()' \
        "$device_id" "$1" "${3:-}" < "$body" 2>/dev/null; then
      echo "PASS: monitoring records $2"; return 0
    fi
    sleep 2
  done
  cat "$body" >&2; fail "monitoring did not record '$1' for: $2"
}
api PUT /api/monitoring/settings '{"enabled":true,"interval_seconds":3600}'
[[ "$status" == 200 ]] || fail "enabling monitoring returned HTTP $status"
make_due; wait_check online "a healthy device as online"

docker compose --project-name "$project" --env-file "$env_file" restart app >/dev/null
for attempt in $(seq 1 30); do curl --silent --fail "$base_url/api/healthz" >/dev/null 2>&1 && break; sleep 1; done
make_due; wait_check online "checks again after an app restart"

docker exec "$router" rm /etc/dropbear/authorized_keys
make_due; wait_check error "a revoked credential as an error, not offline"
api GET "/api/devices/$device_id"; json 'assert d["status"]!="offline", d' < "$body"
docker cp "$work/mgmt_key.pub" "$router:/etc/dropbear/authorized_keys"
docker exec "$router" sh -c "chown root:root /etc/dropbear/authorized_keys && chmod 600 /etc/dropbear/authorized_keys"

docker exec "$router" sh -c 'rm -f /etc/dropbear/dropbear_ed25519_host_key && dropbearkey -t ed25519 -f /etc/dropbear/dropbear_ed25519_host_key >/dev/null && /etc/init.d/dropbear restart'
sleep 2
make_due; wait_check error "a changed host key as an error" "host-key"

docker stop "$router" >/dev/null
make_due; wait_check offline "an outage as offline"

echo "All OpenWrt lab tests passed."
