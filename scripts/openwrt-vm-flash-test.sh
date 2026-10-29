#!/usr/bin/env bash
set -Eeuo pipefail

# Flashes a real OpenWrt x86-64 VM (QEMU) through OpenManage's firmware upgrade and checks
# the post-reboot results, which a container cannot show. OpenManage and PostgreSQL run in
# Docker as in scripts/openwrt-lab-test.sh. The VM boots OLD_VERSION and is upgraded to
# NEW_VERSION with the official image from downloads.openwrt.org; scripts/openwrt-vm/asu.py
# stands in for the ASU build server and hands that image out. Needs sudo (tap device),
# qemu-system-x86_64, docker and internet access. Nothing here touches real devices.
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

for tool in docker curl python3 ssh-keygen ssh-keyscan qemu-system-x86_64 sudo gunzip sha256sum; do
  command -v "$tool" >/dev/null || { echo "ERROR: $tool is required" >&2; exit 2; }
done
docker compose version >/dev/null

old_version="${OPENWRT_OLD_VERSION:-24.10.3}"
new_version="${OPENWRT_NEW_VERSION:-24.10.4}"
mirror="${OPENWRT_MIRROR:-https://downloads.openwrt.org}"
image_suffix="x86-64-generic-squashfs-combined.img.gz"
project="openmanage-vm-${GITHUB_RUN_ID:-local}-$$"
port="${OPENMANAGE_LAB_PORT:-18082}"
asu_port=18090
serial_port=14555
tap="om-vmtap"
vm_ip="192.168.1.1"
host_ip="192.168.1.2"
base_url="http://127.0.0.1:${port}"
work="$(mktemp -d)"
env_file="$work/env"
cookie_jar="$work/cookies"
body="$work/body"
qemu_pid=""
asu_pid=""
compose() { docker compose --project-name "$project" --env-file "$env_file" -f compose.yaml "$@"; }
fail() { echo "ERROR: $*" >&2; exit 1; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); $1" "${@:2}"; }
cleanup() {
  status=$?
  if [[ $status != 0 ]]; then
    echo "---- VM console ----"; tail -n 60 "$work/serial.log" 2>/dev/null || true
    echo "---- stand-in build server ----"; tail -n 30 "$work/asu.log" 2>/dev/null || true
    echo "---- OpenManage ----"; compose logs --no-color --tail 80 app 2>/dev/null || true
  fi
  [[ -n "$qemu_pid" ]] && kill "$qemu_pid" 2>/dev/null || true
  [[ -n "$asu_pid" ]] && kill "$asu_pid" 2>/dev/null || true
  compose down --volumes --remove-orphans >/dev/null 2>&1 || true
  sudo iptables -D FORWARD -i "$tap" -j ACCEPT 2>/dev/null || true
  sudo iptables -D FORWARD -o "$tap" -j ACCEPT 2>/dev/null || true
  sudo iptables -t nat -D POSTROUTING -o "$tap" -j MASQUERADE 2>/dev/null || true
  sudo ip link del "$tap" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

# --- Images -------------------------------------------------------------------------------
fetch_image() { # version -> path of the verified .img.gz
  local version="$1" dir="$work/dl-$1" name="openwrt-$1-$image_suffix"
  mkdir -p "$dir"
  local url="$mirror/releases/$version/targets/x86/64"
  curl --silent --show-error --fail --location -o "$dir/$name" "$url/$name" || fail "could not download $url/$name"
  curl --silent --show-error --fail --location -o "$dir/sha256sums" "$url/sha256sums" || fail "could not download $url/sha256sums"
  (cd "$dir" && grep " \*\?$name\$" sha256sums | sed 's/ \*/  /' | sha256sum -c --status) || fail "$name does not match the published sha256sums"
  echo "$dir/$name"
}
echo "Downloading OpenWrt $old_version and $new_version ..."
old_gz="$(fetch_image "$old_version")"
new_gz="$(fetch_image "$new_version")"
disk="$work/disk.img"
gunzip -c "$old_gz" > "$disk" || [[ $? == 2 ]] # gzip warns about trailing zeros in OpenWrt images
echo "PASS: both images match the published checksums"

# --- Network and VM -----------------------------------------------------------------------
sudo ip tuntap add dev "$tap" mode tap user "$(id -un)"
sudo ip addr add "$host_ip/24" dev "$tap"
sudo ip link set "$tap" up
sudo sysctl -q -w net.ipv4.ip_forward=1
# Docker drops forwarded traffic it does not know; let the app container reach the VM.
sudo iptables -I FORWARD -i "$tap" -j ACCEPT
sudo iptables -I FORWARD -o "$tap" -j ACCEPT
sudo iptables -t nat -I POSTROUTING -o "$tap" -j MASQUERADE

accel=(-machine accel=tcg -cpu max)
if [[ -e /dev/kvm ]]; then
  sudo chmod 666 /dev/kvm 2>/dev/null || true
  [[ -w /dev/kvm ]] && accel=(-machine accel=kvm -cpu host)
fi
echo "Booting OpenWrt $old_version (${accel[*]}) ..."
qemu-system-x86_64 "${accel[@]}" -m 1024 -smp 2 -display none \
  -drive "file=$disk,format=raw,if=virtio" \
  -netdev "tap,id=n0,ifname=$tap,script=no,downscript=no" -device virtio-net-pci,netdev=n0 \
  -serial "tcp:127.0.0.1:$serial_port,server=on,wait=off" > "$work/qemu.log" 2>&1 &
qemu_pid=$!

ssh-keygen -q -t ed25519 -N '' -C openmanage-vm -f "$work/mgmt_key"
root_password="$(python3 -c 'import secrets; print(secrets.token_urlsafe(18))')"
cat > "$work/setup.sh" <<EOF
printf '%s\n%s\n' '$root_password' '$root_password' | passwd root
mkdir -p /etc/dropbear && echo '$(cat "$work/mgmt_key.pub")' > /etc/dropbear/authorized_keys && chmod 600 /etc/dropbear/authorized_keys
EOF
python3 scripts/openwrt-vm/console.py "$serial_port" "$work/setup.sh" > "$work/serial.log" || { tail -n 40 "$work/serial.log"; fail "could not set up the VM through its console"; }
for attempt in $(seq 1 60); do
  host_fp="$(ssh-keyscan -T 3 -t ed25519 "$vm_ip" 2>/dev/null | ssh-keygen -lf - 2>/dev/null | awk '{print $2}')" || true
  [[ "$host_fp" == SHA256:* ]] && break
  [[ "$attempt" == 60 ]] && fail "the VM does not answer SSH at $vm_ip"
  sleep 2
done
vm_ssh() { ssh -i "$work/mgmt_key" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=5 "root@$vm_ip" "$@"; }
[[ "$(vm_ssh '. /etc/openwrt_release; echo $DISTRIB_RELEASE')" == "$old_version" ]] || fail "the VM does not run OpenWrt $old_version"
echo "PASS: OpenWrt $old_version is running at $vm_ip and reachable over SSH"

# --- Stand-in build server ----------------------------------------------------------------
# The image handed out is the official one. Reachable from the app container through the
# gateway of the compose network, which only exists once the stack is up.
cat > "$env_file" <<EOF
POSTGRES_PASSWORD=lab-db-password
OPENMANAGE_ADMIN_USERNAME=admin
OPENMANAGE_ADMIN_PASSWORD=lab-admin-password-123
OPENMANAGE_COOKIE_SECURE=false
OPENMANAGE_ENCRYPTION_KEY=$(python3 -c 'import base64,secrets; print(base64.b64encode(secrets.token_bytes(32)).decode())')
OPENMANAGE_PORT=127.0.0.1:$port
EOF
echo "Starting disposable OpenManage stack on 127.0.0.1:$port ..."
if [[ "${OPENMANAGE_LAB_SKIP_BUILD:-0}" == 1 ]]; then compose up -d; else compose up -d --build; fi
for attempt in $(seq 1 60); do
  curl --silent --fail "$base_url/api/healthz" >/dev/null 2>&1 && break
  [[ "$attempt" == 60 ]] && fail "OpenManage did not become healthy"
  sleep 2
done
gateway="$(docker network inspect "${project}_default" -f '{{(index .IPAM.Config 0).Gateway}}')"
[[ -n "$gateway" ]] || fail "could not determine the compose network gateway"
python3 scripts/openwrt-vm/asu.py "$asu_port" "$new_gz" "$new_version" "$old_version" "http://$gateway:$asu_port" 2> "$work/asu.log" &
asu_pid=$!
for attempt in $(seq 1 20); do curl --silent --fail "http://127.0.0.1:$asu_port/json/v1/overview.json" >/dev/null && break; sleep 0.5; done

# --- OpenManage ---------------------------------------------------------------------------
status="$(curl --silent -o "$body" -w '%{http_code}' -c "$cookie_jar" -H 'Content-Type: application/json' \
  --data '{"username":"admin","password":"lab-admin-password-123"}' "$base_url/api/auth/login")"
[[ "$status" == 200 ]] || fail "login returned HTTP $status"
csrf="$(json 'print(d["csrf_token"])' < "$body")"
api() { # method path [json-body] -> sets $status, response in $body
  local args=(--silent -o "$body" -w '%{http_code}' -X "$1" -b "$cookie_jar" -H "X-CSRF-Token: $csrf")
  [[ $# -ge 3 ]] && args+=(-H 'Content-Type: application/json' --data "$3")
  status="$(curl "${args[@]}" "$base_url$2")"
}
expect() { # http-status label
  [[ "$status" == "$1" ]] || { cat "$body" >&2; fail "$2: HTTP $status (expected $1)"; }
}
pinned_backups() { compose exec -T db psql -U openmanage -d openmanage -tAc "SELECT count(*) FROM device_backups WHERE pinned"; }

api POST /api/devices "{\"name\":\"vm-ap\",\"address\":\"$vm_ip\",\"model\":\"x86-64\",\"firmware\":\"OpenWrt\"}"
expect 201 "device creation"
device_id="$(json 'print(d["id"])' < "$body")"
api POST "/api/devices/$device_id/enroll" "$(python3 -c 'import json,sys; print(json.dumps({"username":"root","private_key":open(sys.argv[1]).read(),"expected_fingerprint":sys.argv[2]}))' "$work/mgmt_key" "$host_fp")"
expect 200 "enrollment"
api POST "/api/devices/$device_id/firmware-inventory"
expect 200 "firmware inventory"
json 'm=d["inventory"]["manifest"]; assert m["release"]==sys.argv[1] and m["rom_known"] and m["package_manager"]=="opkg", m' "$old_version" < "$body"
echo "PASS: the VM is enrolled and its inventory reads OpenWrt $old_version"

api PUT /api/firmware/settings "{\"build_server_enabled\":true,\"build_server_url\":\"http://$gateway:$asu_port\"}"
expect 200 "firmware settings"
api GET "/api/devices/$device_id/firmware-plan?version=$new_version"
expect 200 "upgrade plan"
json 'p=d["plan"]; assert p["profile"]=="generic" and not p["blockers"], p' < "$body"

wait_upgrade() { # [rounds]: waits until the newest job is neither preparing, flashing nor verifying; leaves the list in $body
  for _ in $(seq 1 "${1:-60}"); do
    api GET "/api/devices/$device_id/firmware-upgrades"
    [[ "$status" == 200 ]] || { cat "$body" >&2; fail "listing firmware upgrades returned HTTP $status"; }
    case "$(json 'print(d[0]["status"])' < "$body")" in preparing|flashing|verifying) ;; *) return 0 ;; esac
    sleep 5
  done
  cat "$body" >&2; fail "the firmware upgrade did not settle"
}

api POST "/api/devices/$device_id/firmware-upgrades" "{\"version\":\"$new_version\"}"
expect 202 "preparing the upgrade"
upgrade_id="$(json 'print(d["id"])' < "$body")"
wait_upgrade 120
json 'u=d[0]; assert u["status"]=="ready" and u["backup_id"] and u["image_size"]>1000000, u' < "$body" || { cat "$body" >&2; fail "the upgrade did not become ready"; }
[[ "$(pinned_backups)" == 1 ]] || fail "a ready upgrade must hold exactly one pinned backup"
vm_ssh 'sysupgrade -T /tmp/.om-firmware/image.bin' || fail "the VM's own sysupgrade -T rejects the staged official image"
echo "PASS: the official $new_version image is built, hash-checked, staged, and accepted by the VM's sysupgrade -T"

# --- Flash --------------------------------------------------------------------------------
boot_before="$(vm_ssh 'cat /proc/sys/kernel/random/boot_id')"
api POST "/api/devices/$device_id/firmware-upgrades/$upgrade_id/flash" '{"confirm_name":"vm-ap","acknowledge_warnings":true}'
expect 202 "starting the flash"
echo "Flash sent; waiting for the VM to restart and OpenManage to verify it ..."
wait_upgrade 240
echo "---- result ----"; json 'import pprint; u=d[0]; pprint.pprint({k: u[k] for k in ("status","detail","result")})' < "$body"
json 'u=d[0]; assert u["status"]=="done" and u["flash_sent_at"], u' < "$body" || fail "the upgraded VM did not pass every check"
json 'u=d[0]; assert all(c["status"]=="ok" for c in u["result"]), u["result"]' < "$body"
boot_after="$(vm_ssh 'cat /proc/sys/kernel/random/boot_id')"
[[ "$boot_before" != "$boot_after" ]] || fail "the VM did not restart"
[[ "$(vm_ssh '. /etc/openwrt_release; echo $DISTRIB_RELEASE')" == "$new_version" ]] || fail "the VM does not run OpenWrt $new_version"
vm_ssh 'test ! -e /tmp/.om-firmware' || fail "the staged image survived the restart"
echo "PASS: the VM restarted into OpenWrt $new_version and every post-reboot check passed"

# --- After ---------------------------------------------------------------------------------
[[ "$(pinned_backups)" == 1 ]] || fail "the backup must stay pinned until the result is acknowledged"
api POST "/api/devices/$device_id/firmware-upgrades/$upgrade_id/acknowledge"
expect 200 "acknowledge"
[[ "$(pinned_backups)" == 0 ]] || fail "acknowledging left the backup pinned"
api POST "/api/devices/$device_id/firmware-inventory"
expect 200 "firmware inventory after the upgrade"
json 'assert d["inventory"]["manifest"]["release"]==sys.argv[1], d["inventory"]["manifest"]["release"]' "$new_version" < "$body"
echo "PASS: the backup is released on acknowledge and the device reads as OpenWrt $new_version"
echo "All VM flash checks passed."
