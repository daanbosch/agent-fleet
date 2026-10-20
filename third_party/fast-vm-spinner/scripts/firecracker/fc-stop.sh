#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-/opt/fastvm-microvm}"
ID="${ID:-${1:-fcbench}}"
safe_id="$(echo "$ID" | tr -cd '[:alnum:]-' | cut -c1-32)"
instance_dir="${INSTALL_DIR}/instances/${safe_id}"
state_file="${instance_dir}/state.env"

if [ "$(id -u)" -ne 0 ]; then
  echo "fc-stop must run as root" >&2
  exit 1
fi

if [ ! -f "$state_file" ]; then
  echo "state not found for ${safe_id}" >&2
  exit 1
fi

# shellcheck disable=SC1090
. "$state_file"

if [ -n "${UNIT:-}" ]; then
  systemctl stop "$UNIT" 2>/dev/null || true
fi

if [ -n "${PID:-}" ] && kill -0 "$PID" 2>/dev/null; then
  kill "$PID" 2>/dev/null || true
  for _ in $(seq 1 50); do
    kill -0 "$PID" 2>/dev/null || break
    sleep 0.1
  done
  kill -9 "$PID" 2>/dev/null || true
fi

if [ -n "${TAP_DEV:-}" ]; then
  # Remove legacy per-VM rules if present. Shared fc+/CIDR firewall rules are
  # intentionally kept because other Firecracker VMs may still be running.
  if [ -n "${GUEST_IP:-}" ] && [ -n "${HOST_IFACE:-}" ]; then
    /usr/sbin/iptables -t nat -D POSTROUTING -s "${GUEST_IP}/32" -o "$HOST_IFACE" -j MASQUERADE 2>/dev/null || true
  fi
  /usr/sbin/iptables -D FORWARD -i "$TAP_DEV" -j ACCEPT 2>/dev/null || true
  /usr/sbin/iptables -D FORWARD -o "$TAP_DEV" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null || true
  ip link del "$TAP_DEV" 2>/dev/null || true
fi

rm -f "${API_SOCKET:-}" "${instance_dir}/firecracker.pid" "$state_file"
echo "stopped ${safe_id}"
