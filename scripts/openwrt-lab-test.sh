#!/usr/bin/env bash
set -Eeuo pipefail

# Starts a disposable OpenManage + PostgreSQL stack next to a real OpenWrt rootfs
# container (procd, ubus, dropbear), then exercises SSH enrollment, health checks,
# and persistent monitoring against it, including failure scenarios. Everything,
# including generated keys, is removed on exit. Nothing here touches real devices.
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

for tool in docker curl python3 ssh-keygen go openssl eapol_test; do
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
OPENMANAGE_RADIUS_LISTEN=:1812
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

# Wi-Fi client tracking. The OpenWrt rootfs container has no radio, so a ubus shim
# answers only the hostapd calls with a fixture file; every other call reaches the real
# ubus. This exercises the SSH operation, the parser and the PostgreSQL paths, but not
# real hostapd output.
fake_mac="aa:bb:cc:11:22:33"
docker exec "$router" sh -c 'ubus_bin="$(command -v ubus)" && mv "$ubus_bin" "$ubus_bin.real" && cat > "$ubus_bin" <<SHIM
#!/bin/sh
if [ "\$1" = list ] && [ "\$2" = "hostapd.*" ]; then echo hostapd.phy0-ap0; exit 0; fi
if [ "\$1" = call ] && [ "\$2" = hostapd.phy0-ap0 ] && [ "\$3" = get_clients ]; then cat /tmp/fake-clients.json; exit 0; fi
exec "$ubus_bin.real" "\$@"
SHIM
chmod +x "$ubus_bin"'
set_fake_clients() { docker exec -i "$router" sh -c "cat > /tmp/fake-clients.json" <<< "$1"; }
set_fake_clients "{\"freq\":5180,\"clients\":{\"$fake_mac\":{\"auth\":true,\"assoc\":true,\"authorized\":true,\"signal\":-58}}}"
api PUT /api/monitoring/settings '{"enabled":false,"interval_seconds":3600,"client_tracking_enabled":true,"client_poll_interval_seconds":30}'
[[ "$status" == 400 ]] || fail "a 30 s client poll interval was accepted (HTTP $status)"
api PUT /api/monitoring/settings '{"enabled":false,"interval_seconds":3600,"client_tracking_enabled":true,"client_poll_interval_seconds":60}'
[[ "$status" == 200 ]] || { cat "$body" >&2; fail "enabling client tracking returned HTTP $status"; }
for attempt in $(seq 1 40); do
  api GET /api/wifi-clients
  [[ "$(json 'print(len(d))' < "$body")" == 1 ]] && break
  [[ "$attempt" == 40 ]] && { cat "$body" >&2; fail "client tracking did not record the fixture client"; }
  sleep 1
done
json 'c=d[0]; assert c["connected"] and c["signal_dbm"]==-58 and c["interface"]=="phy0-ap0" and c["device_id"]==sys.argv[1], c; assert c["mac_hint"]=="aa:bb:cc:••:••:••" and c["label"]=="" and not c["hidden"], c' "$device_id" < "$body"
! grep -q "11:22:33" "$body" || fail "client list leaked the full MAC address"
client_id="$(json 'print(d[0]["id"])' < "$body")"
mac_leaks="$(compose exec -T db psql -U openmanage -d openmanage -tAc "SELECT count(*) FROM wifi_clients WHERE position(convert_to('$fake_mac','UTF8') in mac_ciphertext)>0 OR mac_hint LIKE '%11:22:33%'")"
[[ "$mac_leaks" == 0 ]] || fail "client MAC found in plaintext in PostgreSQL"
echo "PASS: a client associated to the AP is tracked, with its MAC masked and encrypted at rest"
api PATCH "/api/wifi-clients/$client_id" '{"label":"lab phone"}'
[[ "$status" == 204 ]] || fail "labelling a client returned HTTP $status"
# The client leaves: the next poll (one interval later) records a disconnect.
set_fake_clients '{"freq":5180,"clients":{}}'
for attempt in $(seq 1 100); do
  api GET /api/wifi-clients
  [[ "$(json 'print(d[0]["connected"])' < "$body")" == False ]] && break
  [[ "$attempt" == 100 ]] && { cat "$body" >&2; fail "client tracking did not notice the client leaving"; }
  sleep 1
done
json 'assert d[0]["label"]=="lab phone", d' < "$body"
api GET "/api/wifi-client-events?client=$client_id"
json 'assert [e["kind"] for e in d]==["disconnect","connect"] and d[1]["signal_dbm"]==-58 and d[0]["from_device_id"]==sys.argv[1], d' "$device_id" < "$body"
! grep -q "11:22:33" "$body" || fail "client history leaked the full MAC address"
echo "PASS: a client leaving the AP shows as disconnected (red) and the history records connect and disconnect"
api DELETE /api/wifi-clients
[[ "$status" == 204 ]] || fail "clearing client data returned HTTP $status"
api GET /api/wifi-clients
json 'assert d==[], d' < "$body"
api GET /api/wifi-client-events
json 'assert d==[], d' < "$body"
api PUT /api/monitoring/settings '{"enabled":false,"interval_seconds":3600,"client_tracking_enabled":false}'
[[ "$status" == 200 ]] || fail "disabling client tracking returned HTTP $status"
echo "PASS: clearing client data removes clients and history"
# Remove the shim so later sections (the forced-rollback wrapper) see the real ubus.
docker exec "$router" sh -c 'ubus_bin="$(command -v ubus)" && mv "$ubus_bin.real" "$ubus_bin" && rm -f /tmp/fake-clients.json'

# Controlled configuration change: preview, apply with rpcd rollback armed, verify
# over a fresh connection, confirm.
router_hostname() { docker exec "$router" uci get system.@system[0].hostname; }
api GET /api/config-settings
json 'assert any(x["key"]=="system.hostname" for x in d), d' < "$body"
api POST "/api/devices/$device_id/config-changes" '{"setting":"system.hostname","value":"bad name"}'
[[ "$status" == 400 ]] || fail "invalid hostname preview returned HTTP $status"
api POST "/api/devices/$device_id/config-changes" '{"setting":"system.hostname","value":"lab-applied"}'
[[ "$status" == 201 ]] || { cat "$body" >&2; fail "change preview returned HTTP $status"; }
change_id="$(json 'assert d["status"]=="previewed" and d["old_value"]=="lab-scheduled" and d["new_value"]=="lab-applied", d; print(d["id"])' < "$body")"
[[ "$(router_hostname)" == lab-scheduled ]] || fail "preview changed the device"
api POST "/api/devices/$device_id/config-changes/$change_id/apply"
[[ "$status" == 200 ]] || { cat "$body" >&2; fail "change apply returned HTTP $status"; }
json 'assert d["status"]=="applied" and "confirmed" in d["detail"], d' < "$body"
sleep 1
[[ "$(router_hostname)" == lab-applied ]] || fail "applied hostname is not on the device"
api POST "/api/devices/$device_id/config-changes/$change_id/apply"
[[ "$status" == 409 ]] || fail "a used preview was applied twice (HTTP $status)"
api GET "/api/devices/$device_id/config-snapshots"
json 'assert not d[0]["drift"], d[0]' < "$body"
echo "PASS: a controlled change is previewed, applied, verified and confirmed, and is not reported as drift"

# The confirm must arrive: after the rollback window the change is still there.
sleep 62
[[ "$(router_hostname)" == lab-applied ]] || fail "confirmed change was rolled back by the rpcd timer"
echo "PASS: a confirmed change survives the rollback window"

api POST "/api/devices/$device_id/config-changes" '{"setting":"system.hostname","value":"lab-stale"}'
stale_id="$(json 'print(d["id"])' < "$body")"
docker exec "$router" sh -c "uci set system.@system[0].hostname=lab-outside && uci commit system"
api POST "/api/devices/$device_id/config-changes/$stale_id/apply"
json 'assert d["status"]=="failed" and "changed since the preview" in d["detail"], d' < "$body"
[[ "$(router_hostname)" == lab-outside ]] || fail "a stale change touched the device"
echo "PASS: a change whose preview is stale is refused without touching the device"

# A pending apply from someone else (for example LuCI) blocks ours; nothing is applied.
api POST "/api/devices/$device_id/config-changes" '{"setting":"system.hostname","value":"lab-blocked"}'
blocked_id="$(json 'print(d["id"])' < "$body")"
docker exec "$router" sh -c '
  sid=$(ubus call session create "{\"timeout\":300}" | jsonfilter -e @.ubus_rpc_session)
  ubus call session grant "{\"ubus_rpc_session\":\"$sid\",\"scope\":\"uci\",\"objects\":[[\"system\",\"read\"],[\"system\",\"write\"]]}"
  ubus call uci set "{\"ubus_rpc_session\":\"$sid\",\"config\":\"system\",\"section\":\"@system[0]\",\"values\":{\"description\":\"lab-pending\"}}"
  ubus call uci apply "{\"ubus_rpc_session\":\"$sid\",\"rollback\":true,\"timeout\":15}"' >/dev/null
api POST "/api/devices/$device_id/config-changes/$blocked_id/apply"
json 'assert d["status"]=="failed" and "refused to apply" in d["detail"], d' < "$body"
[[ "$(router_hostname)" == lab-outside ]] || fail "a blocked change touched the device"
sleep 17
echo "PASS: a change is refused while another apply is pending on the device"

# Forced rollback: a lab-only ubus wrapper reports a wrong value once a change has been
# applied, so verification fails and OpenManage must roll the device back.
docker exec "$router" sh -c 'mv /bin/ubus /bin/ubus.real && cat > /bin/ubus <<"W"
#!/bin/sh
[ "$1 $2 $3" = "call uci apply" ] && touch /tmp/om-lab-applied
if [ "$1 $2 $3" = "call uci get" ] && [ -f /tmp/om-lab-applied ]; then echo "{\"value\":\"tampered\"}"; exit 0; fi
exec /bin/ubus.real "$@"
W
chmod +x /bin/ubus'
api POST "/api/devices/$device_id/config-changes" '{"setting":"system.hostname","value":"lab-rollback"}'
rollback_id="$(json 'print(d["id"])' < "$body")"
api POST "/api/devices/$device_id/config-changes/$rollback_id/apply"
docker exec "$router" sh -c 'mv /bin/ubus.real /bin/ubus && rm -f /tmp/om-lab-applied'
json 'assert d["status"]=="rolled_back" and "previous configuration was restored" in d["detail"], d' < "$body"
sleep 1
[[ "$(router_hostname)" == lab-outside ]] || fail "a failed verification did not roll the device back"
echo "PASS: a change that fails verification is rolled back"
applied_audit="$(compose exec -T db psql -U openmanage -d openmanage -tAc "SELECT count(*) FROM audit_events WHERE action IN ('device.config_change.applied','device.config_change.failed','device.config_change.rolled_back')")"
[[ "$applied_audit" == 4 ]] || fail "config change audit events: $applied_audit (expected 4)"

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

# Built-in RADIUS (PAP lab listener). The probe runs inside the router container so
# requests come from the enrolled device's address.
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$work/radius-probe" ./scripts/radius-probe
docker cp "$work/radius-probe" "$router:/tmp/radius-probe"
app_ip="$(docker inspect -f "{{(index .NetworkSettings.Networks \"$network\").IPAddress}}" "$(compose ps -q app)")"
probe() { # secret user password [flags...] -> prints accept/reject/timeout
  docker exec "$router" /tmp/radius-probe -server "$app_ip:1812" -secret "$1" -user "$2" -password "$3" "${@:4}"
}
expect_probe() { # expected label secret user password [flags...]
  local got; got="$(probe "${@:3}")"
  [[ "$got" == "$1" ]] || fail "$2: RADIUS probe got '$got' (expected $1)"
  echo "PASS: RADIUS $2"
}
api GET /api/radius
json 'assert d["enabled"] and d["listen_addr"]==":1812" and d["clients"]==[], d' < "$body"
ca_fp="$(json 'c=d["certificate"]; assert c["server_name"]=="radius.openmanage.home.arpa", c; print(c["ca_fingerprint_sha256"])' < "$body")"
curl --silent --fail -o "$work/radius-ca.pem" -b "$cookie_jar" "$base_url/api/radius/ca.pem" || fail "CA certificate download failed"
[[ "$(openssl x509 -in "$work/radius-ca.pem" -noout -fingerprint -sha256 | cut -d= -f2)" == "$ca_fp" ]] \
  || fail "downloaded CA certificate does not match the reported fingerprint"
! compose exec -T db psql -U openmanage -d openmanage -tAc "SELECT encode(ca_key_ciphertext,'escape') || encode(server_key_ciphertext,'escape') FROM radius_certificate" | grep -q "PRIVATE KEY" \
  || fail "RADIUS certificate keys are stored unencrypted"
echo "PASS: RADIUS generates a CA and server certificate and offers the CA for download"
curl --silent --fail -o "$work/wifi.mobileconfig" -b "$cookie_jar" "$base_url/api/radius/apple-profile?ssid=Lab%20%26%20Home" \
  || fail "Apple profile download failed"
python3 - "$work/wifi.mobileconfig" "$work/radius-ca.pem" <<'PY' || fail "Apple profile is malformed"
import base64, plistlib, ssl, sys
p = plistlib.load(open(sys.argv[1], "rb"))
ca, wifi = p["PayloadContent"]
assert ca["PayloadContent"] == ssl.PEM_cert_to_DER_cert(open(sys.argv[2]).read())
eap = wifi["EAPClientConfiguration"]
assert wifi["SSID_STR"] == "Lab & Home" and eap["AcceptEAPTypes"] == [21] and eap["TTLSInnerAuthentication"] == "PAP"
assert eap["TLSTrustedServerNames"] == ["radius.openmanage.home.arpa"] and eap["PayloadCertificateAnchorUUID"] == [ca["PayloadUUID"]]
PY
echo "PASS: RADIUS offers an Apple setup profile that trusts only this CA and server name"
expect_probe timeout "ignores a device without a secret" not-a-secret lab-wifi lab-wifi-password-1
api POST /api/network-users '{"username":"lab-wifi","password":"lab-wifi-password-1"}'
[[ "$status" == 201 ]] || fail "creating a network user returned HTTP $status"
wifi_id="$(json 'print(d["id"])' < "$body")"
api POST /api/network-users '{"username":"lab-locked","password":"lab-locked-password-1"}'
[[ "$status" == 201 ]] || fail "creating a second network user returned HTTP $status"
locked_id="$(json 'print(d["id"])' < "$body")"
api POST "/api/auth/login" '{"username":"lab-wifi","password":"lab-wifi-password-1"}'
[[ "$status" == 401 ]] || fail "a network user signed in to OpenManage (HTTP $status)"
api POST "/api/devices/$device_id/radius-secret"
[[ "$status" == 200 ]] || fail "creating a RADIUS secret returned HTTP $status"
radius_secret="$(json 'print(d["secret"])' < "$body")"
[[ ${#radius_secret} == 64 ]] || fail "unexpected RADIUS secret length"
expect_probe accept "accepts a valid network user" "$radius_secret" lab-wifi lab-wifi-password-1
expect_probe accept "answers a retransmission with the same reply" "$radius_secret" lab-wifi lab-wifi-password-1 -repeat 3
expect_probe reject "rejects a wrong password" "$radius_secret" lab-wifi wrong-password-123
expect_probe reject "rejects an unknown user" "$radius_secret" lab-nobody lab-wifi-password-1
expect_probe challenge "answers an EAP identity with an EAP-TTLS challenge" "$radius_secret" lab-wifi lab-wifi-password-1 -eap
expect_probe timeout "drops a request without Message-Authenticator" "$radius_secret" lab-wifi lab-wifi-password-1 -no-message-authenticator
expect_probe timeout "drops a request signed with the wrong secret" "wrong-$radius_secret" lab-wifi lab-wifi-password-1
api PATCH "/api/network-users/$wifi_id" '{"disabled":true}'
[[ "$status" == 200 ]] || fail "disabling a network user returned HTTP $status"
expect_probe reject "rejects a disabled network user" "$radius_secret" lab-wifi lab-wifi-password-1
# A phone keeps retrying while its user is disabled; re-enabling must let it back in.
for attempt in $(seq 1 10); do probe "$radius_secret" lab-wifi lab-wifi-password-1 >/dev/null; done
api PATCH "/api/network-users/$wifi_id" '{"disabled":false}'
expect_probe accept "accepts a re-enabled user after rejections while disabled" "$radius_secret" lab-wifi lab-wifi-password-1
for attempt in $(seq 1 10); do probe "$radius_secret" lab-locked wrong-password-123 >/dev/null; done
expect_probe reject "blocks a user after repeated failures" "$radius_secret" lab-locked lab-locked-password-1
api POST "/api/network-users/$locked_id/password" '{"password":"lab-locked-password-2"}'
[[ "$status" == 200 ]] || fail "resetting a network user's password returned HTTP $status"
expect_probe accept "accepts a blocked user right after a password reset" "$radius_secret" lab-locked lab-locked-password-2
# Phones of disabled users retrying must not lock the AP out for everyone: 50
# rejections of disabled users would reach the per-AP limit if they counted there.
for n in 1 2 3 4 5; do
  api POST /api/network-users "{\"username\":\"lab-off-$n\",\"password\":\"lab-off-password-$n\"}"
  off_id="$(json 'print(d["id"])' < "$body")"
  api PATCH "/api/network-users/$off_id" '{"disabled":true}'
  for attempt in $(seq 1 10); do probe "$radius_secret" "lab-off-$n" "lab-off-password-$n" >/dev/null; done
done
expect_probe accept "keeps the AP open after many rejections of disabled users" "$radius_secret" lab-wifi lab-wifi-password-1
api POST "/api/devices/$device_id/radius-secret"
new_secret="$(json 'print(d["secret"])' < "$body")"
expect_probe timeout "stops accepting a rotated-out secret" "$radius_secret" lab-wifi lab-wifi-password-1
expect_probe accept "accepts the rotated secret" "$new_secret" lab-wifi lab-wifi-password-1
api GET /api/radius
json 'assert len(d["clients"])==1 and d["dropped_packets"]>=4, d' < "$body"
! grep -q "$new_secret" "$body" || fail "RADIUS status returned the shared secret"
# Run the AP commands from docs/radius-client-setup.md step 3 as written, so a guide
# step that uci rejects fails here first. The lab router has no radio, so the
# section is only checked, not brought up (no wifi reload).
guide_ap_cmds="$(python3 - docs/radius-client-setup.md <<'PY'
import re, sys
text = open(sys.argv[1]).read()
section = text.split("## 3. Configure the AP", 1)[1].split("\n## ", 1)[0]
block = re.search(r"```sh\n(.*?)```", section, re.S).group(1)
print("\n".join(l for l in block.splitlines() if l.strip() != "wifi reload"))
PY
)"
[[ "$guide_ap_cmds" == *"uci commit wireless"* ]] || fail "could not read the AP commands from the setup guide"
guide_ap_cmds="${guide_ap_cmds//<secret from step 2>/$new_secret}"
docker exec "$router" sh -c "set -e; touch /etc/config/wireless; $guide_ap_cmds" || fail "the setup guide's AP commands failed on OpenWrt"
[[ "$(docker exec "$router" uci get wireless.omtest.encryption)" == wpa2 ]] || fail "the setup guide's AP commands did not configure WPA2-Enterprise"
docker exec "$router" sh -c "uci delete wireless.omtest && uci commit wireless" || fail "the setup guide's undo command failed"
echo "PASS: the setup guide's AP commands run on OpenWrt"
radius_audit="$(compose exec -T db psql -U openmanage -d openmanage -tAc "SELECT count(*) FROM audit_events WHERE action='radius.auth_accept'")"
[[ "$radius_audit" == 6 ]] || fail "RADIUS accept audit events: $radius_audit (expected 6)"
leaks="$(compose exec -T db psql -U openmanage -d openmanage -tAc "SELECT details::text FROM audit_events" | grep -c -e lab-wifi-password -e lab-locked-password -e "$new_secret" -e "$radius_secret" || true)"
[[ "$leaks" == 0 ]] || fail "a RADIUS password or secret reached the audit log"
! compose logs --no-color app | grep -q -e lab-wifi-password -e "$new_secret" || fail "a RADIUS password or secret reached the app log"

# EAP-TTLS/PAP with wpa_supplicant's eapol_test, run on the host: requests come from
# the Docker network gateway, so that address is enrolled as a second RADIUS client.
gateway="$(docker network inspect "$network" -f '{{(index .IPAM.Config 0).Gateway}}')"
api POST /api/devices "{\"name\":\"lab-eapol\",\"address\":\"$gateway\"}"
[[ "$status" == 201 ]] || fail "adding the eapol_test client returned HTTP $status"
eapol_device="$(json 'print(d["id"])' < "$body")"
api POST "/api/devices/$eapol_device/radius-secret"
eapol_secret="$(json 'print(d["secret"])' < "$body")"
ttls_conf() { # identity password phase2 domain
  cat > "$work/ttls.conf" <<CONF
network={
  key_mgmt=WPA-EAP
  eap=TTLS
  identity="$1"
  anonymous_identity="anonymous"
  password="$2"
  phase2="$3"
  ca_cert="$work/radius-ca.pem"
  domain_match="$4"
}
CONF
}
expect_ttls() { # success|failure label identity password phase2 [domain]
  ttls_conf "$3" "$4" "$5" "${6:-radius.openmanage.home.arpa}"
  if eapol_test -c "$work/ttls.conf" -a "$app_ip" -s "$eapol_secret" -t 15 > "$work/eapol.log" 2>&1; then
    [[ "$1" == success ]] || fail "EAP-TTLS $2: authentication succeeded unexpectedly"
    grep -q "MPPE keys OK: 1  mismatch: 0" "$work/eapol.log" || fail "EAP-TTLS $2: MPPE keys missing or wrong"
  else
    [[ "$1" == failure ]] || { tail -30 "$work/eapol.log" >&2; fail "EAP-TTLS $2: authentication failed"; }
    grep -q "CTRL-EVENT-EAP-FAILURE" "$work/eapol.log" || fail "EAP-TTLS $2: no EAP failure (timeout?)"
  fi
  echo "PASS: EAP-TTLS $2"
}
expect_ttls success "authenticates a network user and delivers matching MPPE keys" lab-wifi lab-wifi-password-1 "auth=PAP"
expect_ttls failure "rejects a wrong inner password" lab-wifi wrong-password-123 "auth=PAP"
expect_ttls failure "rejects an inner method other than PAP" lab-wifi lab-wifi-password-1 "autheap=MSCHAPV2"
expect_ttls failure "is refused by a client expecting another server name" lab-wifi lab-wifi-password-1 "auth=PAP" other.example
ttls_audit="$(compose exec -T db psql -U openmanage -d openmanage -tAc "SELECT string_agg(details->>'reason', ',' ORDER BY id) FROM audit_events WHERE action='radius.auth_reject' AND details->>'method'='eap-ttls'")"
[[ "$ttls_audit" == "bad_credentials,inner_method_not_pap,tls_failed" ]] || fail "EAP-TTLS reject audit: $ttls_audit"
! compose exec -T db psql -U openmanage -d openmanage -tAc "SELECT details::text FROM audit_events" | grep -q -e lab-wifi-password -e "$eapol_secret" \
  || fail "an EAP-TTLS password or secret reached the audit log"
echo "PASS: EAP-TTLS failures are audited with a reason and no secrets"

api DELETE "/api/devices/$device_id/radius-secret"
[[ "$status" == 200 ]] || fail "removing the RADIUS secret returned HTTP $status"
expect_probe timeout "ignores a device whose secret was removed" "$new_secret" lab-wifi lab-wifi-password-1

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
api GET /api/radius
json 'assert d["certificate"]["ca_fingerprint_sha256"]==sys.argv[1], d' "$ca_fp" < "$body"
echo "PASS: RADIUS keeps its certificate across an app restart"

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
