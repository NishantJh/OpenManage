#!/usr/bin/env bash
set -Eeuo pipefail

# Backup/restore drill (OM-SAF-06). Seeds a disposable OpenManage + PostgreSQL stack,
# takes a pg_dump, restores it into a second fresh stack, and verifies inventory,
# audit rows, and that a stored credential decrypts with the restored
# OPENMANAGE_ENCRYPTION_KEY (and not with a wrong key). Uses generated keys and fake
# credentials only; both stacks and their volumes are removed on exit.
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

for tool in docker curl python3 openssl go; do
  command -v "$tool" >/dev/null || { echo "ERROR: $tool is required" >&2; exit 2; }
done
docker compose version >/dev/null

run_id="${GITHUB_RUN_ID:-local}-$$"
src_project="openmanage-drill-src-$run_id"
dst_project="openmanage-drill-dst-$run_id"
src_port="${OPENMANAGE_DRILL_SRC_PORT:-18081}"
dst_port="${OPENMANAGE_DRILL_DST_PORT:-18082}"
db_password="drill-db-password"
admin_password="drill-admin-password-123"
encryption_key="$(openssl rand -base64 32)"
work_dir="$(mktemp -d)"
chmod 700 "$work_dir"
src_env="$work_dir/src.env"
dst_env="$work_dir/dst.env"
dump_file="$work_dir/openmanage.dump"

# The Go helpers compile the app package, which embeds web/dist. Use a placeholder
# when the frontend has not been built; CI and normal builds provide the real assets.
dist_dir="cmd/openmanage/web/dist"
placeholder=0
if [[ ! -d "$dist_dir" ]]; then
  mkdir -p "$dist_dir"
  : > "$dist_dir/.drill-placeholder"
  placeholder=1
fi

cleanup() {
  if [[ "$placeholder" == 1 ]]; then rm -rf cmd/openmanage/web; fi
  docker compose --project-name "$src_project" --env-file "$src_env" down --volumes --remove-orphans >/dev/null 2>&1 || true
  docker compose --project-name "$dst_project" --env-file "$dst_env" down --volumes --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$work_dir"
}
trap cleanup EXIT

write_env() { # file port
  cat > "$1" <<EOT
POSTGRES_PASSWORD=$db_password
OPENMANAGE_ADMIN_USERNAME=admin
OPENMANAGE_ADMIN_PASSWORD=$admin_password
OPENMANAGE_COOKIE_SECURE=false
OPENMANAGE_ENCRYPTION_KEY=$encryption_key
OPENMANAGE_PORT=127.0.0.1:$2
EOT
}
write_env "$src_env" "$src_port"
write_env "$dst_env" "$dst_port"

dc() { local project="$1" envf="$2"; shift 2; docker compose --project-name "$project" --env-file "$envf" "$@"; }
psql_src() { dc "$src_project" "$src_env" exec -T db psql -U openmanage -d openmanage -v ON_ERROR_STOP=1 -At "$@"; }
psql_dst() { dc "$dst_project" "$dst_env" exec -T db psql -U openmanage -d openmanage -v ON_ERROR_STOP=1 -At "$@"; }

wait_healthy() { # port
  local i
  for i in $(seq 1 60); do
    if curl --silent --fail "http://127.0.0.1:$1/api/healthz" >/dev/null 2>&1; then return 0; fi
    sleep 2
  done
  echo "ERROR: app on port $1 did not become healthy" >&2
  return 1
}

login() { # port cookie_jar -> prints csrf token
  curl --silent --show-error --fail -c "$2" -H 'Content-Type: application/json' \
    --data "{\"username\":\"admin\",\"password\":\"$admin_password\"}" \
    "http://127.0.0.1:$1/api/auth/login" | python3 -c 'import json,sys; print(json.load(sys.stdin)["csrf_token"])'
}

now() { date +%s; }

echo "== Source stack: seed data =="
dc "$src_project" "$src_env" up -d --build
wait_healthy "$src_port"
cookie="$work_dir/src.cookies"
csrf="$(login "$src_port" "$cookie")"
device="$(curl --silent --show-error --fail -b "$cookie" -H "X-CSRF-Token: $csrf" -H 'Content-Type: application/json' \
  --data '{"name":"drill-router","address":"192.0.2.20","model":"drill-model","firmware":"drill"}' \
  "http://127.0.0.1:$src_port/api/devices")"
device_id="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' <<< "$device")"

# SSH enrollment needs a live router, so store a credential the way the app does:
# encrypt with the production code path and insert the ciphertext directly.
ct_file="$work_dir/ciphertext.hex"
OPENMANAGE_ENCRYPTION_KEY="$encryption_key" OPENMANAGE_DRILL_ENCRYPT_OUT="$ct_file" \
  go test ./cmd/openmanage -run '^TestRestoreDrillEncrypt$' -count=1 >/dev/null
ct_hex="$(cat "$ct_file")"
psql_src -c "INSERT INTO device_credentials(device_id, username, private_key_ciphertext, host_key_fingerprint) VALUES ('$device_id', 'drill', decode('$ct_hex','hex'), 'SHA256:drill-fingerprint')"

src_devices="$(psql_src -c "SELECT count(*) FROM devices")"
src_audit="$(psql_src -c "SELECT count(*) FROM audit_events WHERE action='device.create'")"
[[ "$src_devices" -ge 1 && "$src_audit" -ge 1 ]] || { echo "ERROR: seed data missing" >&2; exit 1; }
src_pg="$(psql_src -c "SHOW server_version")"
src_app="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
echo "PASS: seeded $src_devices device(s), $src_audit device.create audit row(s), 1 encrypted credential"

echo "== Backup =="
t0="$(now)"
dc "$src_project" "$src_env" stop app >/dev/null
dc "$src_project" "$src_env" exec -T db pg_dump -U openmanage -d openmanage --format=custom > "$dump_file"
[[ -s "$dump_file" ]] || { echo "ERROR: dump is empty" >&2; exit 1; }
backup_secs=$(( $(now) - t0 ))
dump_bytes="$(wc -c < "$dump_file")"
echo "PASS: pg_dump completed (${dump_bytes} bytes, ${backup_secs}s)"

echo "== Restore into a fresh stack =="
t1="$(now)"
dc "$dst_project" "$dst_env" up -d --build
wait_healthy "$dst_port"
dc "$dst_project" "$dst_env" stop app >/dev/null
dc "$dst_project" "$dst_env" exec -T db pg_restore --clean --if-exists --no-owner --no-privileges \
  -U openmanage -d openmanage < "$dump_file"
dc "$dst_project" "$dst_env" start app >/dev/null
wait_healthy "$dst_port"
restore_secs=$(( $(now) - t1 ))
echo "PASS: pg_restore completed and app healthy (${restore_secs}s)"

echo "== Verify =="
dst_devices="$(psql_dst -c "SELECT count(*) FROM devices")"
dst_audit="$(psql_dst -c "SELECT count(*) FROM audit_events WHERE action='device.create'")"
[[ "$dst_devices" == "$src_devices" ]] || { echo "ERROR: device count $dst_devices != $src_devices" >&2; exit 1; }
[[ "$dst_audit" == "$src_audit" ]] || { echo "ERROR: audit row count $dst_audit != $src_audit" >&2; exit 1; }
cookie="$work_dir/dst.cookies"
login "$dst_port" "$cookie" >/dev/null
curl --silent --show-error --fail -b "$cookie" "http://127.0.0.1:$dst_port/api/devices" \
  | python3 -c 'import json,sys; ds=json.load(sys.stdin); assert any(d["id"]==sys.argv[1] and d["name"]=="drill-router" for d in ds), ds' "$device_id"
curl --silent --show-error --fail -b "$cookie" "http://127.0.0.1:$dst_port/api/audit" \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); a=d if isinstance(d,list) else d.get("events",d); assert "device.create" in json.dumps(a), "device.create audit row not visible via API"'
echo "PASS: restored inventory and audit history are visible through the API"

restored_hex="$(psql_dst -c "SELECT encode(private_key_ciphertext,'hex') FROM device_credentials WHERE device_id='$device_id'")"
[[ -n "$restored_hex" ]] || { echo "ERROR: credential row missing after restore" >&2; exit 1; }
[[ "$restored_hex" == "$ct_hex" ]] || { echo "ERROR: restored ciphertext differs from the original" >&2; exit 1; }
OPENMANAGE_ENCRYPTION_KEY="$encryption_key" OPENMANAGE_DRILL_CIPHERTEXT_HEX="$restored_hex" \
  go test ./cmd/openmanage -run '^TestRestoreDrillDecrypt$' -count=1 >/dev/null
echo "PASS: restored credential decrypts with the original key and fails with a wrong key"

dst_pg="$(psql_dst -c "SHOW server_version")"
cat <<EOT

Restore drill passed.
  application revision : $src_app
  PostgreSQL (source)  : $src_pg
  PostgreSQL (restore) : $dst_pg
  docker compose       : $(docker compose version --short)
  dump size            : ${dump_bytes} bytes
  backup time          : ${backup_secs}s (app stop + pg_dump)
  restore time         : ${restore_secs}s (fresh stack up, pg_restore, app healthy)
EOT
