#!/bin/sh
# Root-owned, fixed-purpose network setup for OpenManage's disposable QEMU test.
# This helper deliberately accepts no interface, address, command, or firewall arguments.
set -eu

PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH

RUNNER_USER=runner
TAP=om-vmtap
HOST_CIDR=192.168.1.2/24
STATE_FILE=/run/openmanage-vm-net.ip_forward

fail() {
  echo "openmanage-vm-net: $*" >&2
  exit 1
}

[ "$(id -u)" -eq 0 ] || fail "must be invoked through sudo"
[ "${SUDO_USER:-}" = "$RUNNER_USER" ] || fail "only the '$RUNNER_USER' runner account may invoke this helper"
[ "$#" -eq 1 ] || fail "usage: openmanage-vm-net {setup|cleanup}"

cleanup() {
  iptables -D FORWARD -i "$TAP" -j ACCEPT 2>/dev/null || true
  iptables -D FORWARD -o "$TAP" -j ACCEPT 2>/dev/null || true
  iptables -t nat -D POSTROUTING -o "$TAP" -j MASQUERADE 2>/dev/null || true
  ip link del "$TAP" 2>/dev/null || true

  if [ -f "$STATE_FILE" ]; then
    previous="$(cat "$STATE_FILE")"
    case "$previous" in
      0|1) sysctl -q -w "net.ipv4.ip_forward=$previous" >/dev/null || true ;;
      *) echo "openmanage-vm-net: invalid saved forwarding state; leaving it unchanged" >&2 ;;
    esac
    rm -f "$STATE_FILE"
  fi
}

case "$1" in
  setup)
    [ ! -e "$STATE_FILE" ] || fail "stale state exists; run cleanup first"
    if ip link show "$TAP" >/dev/null 2>&1; then
      fail "reserved interface $TAP already exists; refusing to modify it"
    fi

    previous="$(sysctl -n net.ipv4.ip_forward)"
    case "$previous" in 0|1) ;; *) fail "unexpected IPv4 forwarding value: $previous" ;; esac
    printf '%s\n' "$previous" > "$STATE_FILE"
    trap cleanup EXIT

    ip tuntap add dev "$TAP" mode tap user "$RUNNER_USER"
    ip addr add "$HOST_CIDR" dev "$TAP"
    ip link set "$TAP" up
    sysctl -q -w net.ipv4.ip_forward=1 >/dev/null
    iptables -I FORWARD -i "$TAP" -j ACCEPT
    iptables -I FORWARD -o "$TAP" -j ACCEPT
    iptables -t nat -I POSTROUTING -o "$TAP" -j MASQUERADE

    trap - EXIT
    echo "openmanage-vm-net: QEMU test network is ready"
    ;;
  cleanup)
    cleanup
    echo "openmanage-vm-net: QEMU test network cleaned up"
    ;;
  *)
    fail "usage: openmanage-vm-net {setup|cleanup}"
    ;;
esac
