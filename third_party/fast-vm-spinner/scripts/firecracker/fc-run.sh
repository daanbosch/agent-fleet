#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-/opt/fastvm-microvm}"
ID="${ID:-${1:-fcbench}}"
VCPUS="${VCPUS:-16}"
MEM_MIB="${MEM_MIB:-65536}"
FC_SLICE="${FC_SLICE:-qemu.slice}"
HOST_CPUS="${HOST_CPUS:-}"
FIRECRACKER_TAP_MATCH="${FIRECRACKER_TAP_MATCH:-fc+}"
FIRECRACKER_GUEST_CIDR="${FIRECRACKER_GUEST_CIDR:-192.168.0.0/16}"

if [ "$(id -u)" -ne 0 ]; then
  echo "fc-run must run as root" >&2
  exit 1
fi

FC_BIN="${INSTALL_DIR}/bin/firecracker"
KERNEL="$(find "${INSTALL_DIR}/images" -maxdepth 1 -name 'vmlinux-*' | sort -V | tail -1)"
BASE_ROOTFS="${INSTALL_DIR}/images/ubuntu.ext4"
SSH_KEY="${INSTALL_DIR}/images/id_rsa"

for path in "$FC_BIN" "$KERNEL" "$BASE_ROOTFS" "$SSH_KEY"; do
  if [ ! -e "$path" ]; then
    echo "missing required asset: $path" >&2
    exit 1
  fi
done

safe_id="$(echo "$ID" | tr -cd '[:alnum:]-' | cut -c1-32)"
if [ -z "$safe_id" ]; then
  echo "invalid ID: $ID" >&2
  exit 1
fi

instance_dir="${INSTALL_DIR}/instances/${safe_id}"
api_socket="${instance_dir}/firecracker.socket"
log_file="${instance_dir}/firecracker.log"
rootfs="${instance_dir}/rootfs.ext4"
pid_file="${instance_dir}/firecracker.pid"
state_file="${instance_dir}/state.env"
tap_dev="fc$(echo "$safe_id" | tr -cd '[:alnum:]' | cut -c1-10)"
net_octet="$(( ($(printf '%s' "$safe_id" | cksum | awk '{print $1}') % 200) + 16 ))"
tap_ip="192.168.${net_octet}.1"
guest_ip="192.168.${net_octet}.2"
mac_octet="$(printf '%02x' "$net_octet")"
guest_mac="06:00:c0:a8:${mac_octet}:02"
host_iface="$(ip -j route list default | jq -r '.[0].dev')"
unit="fastvm-microvm-${safe_id}.service"

mkdir -p "$instance_dir"
if [ -f "$pid_file" ] && kill -0 "$(cat "$pid_file")" 2>/dev/null; then
  echo "instance ${safe_id} already running with pid $(cat "$pid_file")" >&2
  exit 1
fi

rm -f "$api_socket"
cp --reflink=auto --sparse=always "$BASE_ROOTFS" "$rootfs"

ip link del "$tap_dev" 2>/dev/null || true
ip tuntap add dev "$tap_dev" mode tap
ip addr add "${tap_ip}/30" dev "$tap_dev"
ip link set dev "$tap_dev" up
echo 1 >/proc/sys/net/ipv4/ip_forward
/usr/sbin/iptables -C FORWARD -i "$FIRECRACKER_TAP_MATCH" -j ACCEPT 2>/dev/null ||
  /usr/sbin/iptables -A FORWARD -i "$FIRECRACKER_TAP_MATCH" -j ACCEPT
/usr/sbin/iptables -C FORWARD -o "$FIRECRACKER_TAP_MATCH" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT 2>/dev/null ||
  /usr/sbin/iptables -A FORWARD -o "$FIRECRACKER_TAP_MATCH" -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
/usr/sbin/iptables -t nat -C POSTROUTING -s "$FIRECRACKER_GUEST_CIDR" -o "$host_iface" -j MASQUERADE 2>/dev/null ||
  /usr/sbin/iptables -t nat -A POSTROUTING -s "$FIRECRACKER_GUEST_CIDR" -o "$host_iface" -j MASQUERADE

stdout_log="${instance_dir}/stdout.log"
stderr_log="${instance_dir}/stderr.log"
if command -v systemd-run >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  run_args=(--unit "$unit" --slice "$FC_SLICE" --collect)
  if [ -n "$HOST_CPUS" ]; then
    run_args+=(--property "AllowedCPUs=${HOST_CPUS}")
  fi
  run_args+=(--property "StandardOutput=append:${stdout_log}" --property "StandardError=append:${stderr_log}")
  systemd-run "${run_args[@]}" "$FC_BIN" --api-sock "$api_socket" --enable-pci >/dev/null
  fc_pid=""
  for _ in $(seq 1 100); do
    fc_pid="$(systemctl show "$unit" -p MainPID --value 2>/dev/null || true)"
    if [ -n "$fc_pid" ] && [ "$fc_pid" != "0" ]; then
      break
    fi
    sleep 0.05
  done
  if [ -z "$fc_pid" ] || [ "$fc_pid" = "0" ]; then
    echo "Firecracker systemd unit did not report a MainPID" >&2
    exit 1
  fi
else
  "$FC_BIN" --api-sock "$api_socket" --enable-pci >"$stdout_log" 2>"$stderr_log" &
  fc_pid="$!"
  unit=""
fi
echo "$fc_pid" >"$pid_file"

for _ in $(seq 1 100); do
  [ -S "$api_socket" ] && break
  sleep 0.05
done
if [ ! -S "$api_socket" ]; then
  echo "Firecracker API socket did not appear" >&2
  exit 1
fi

fc_put() {
  local endpoint="$1"
  local body="$2"
  curl -fsS -X PUT --unix-socket "$api_socket" --data "$body" "http://localhost${endpoint}" >/dev/null
}

expand_cpu_list() {
  local list="$1"
  local part start end cpu
  IFS=, read -ra parts <<<"$list"
  for part in "${parts[@]}"; do
    if [[ "$part" == *-* ]]; then
      start="${part%-*}"
      end="${part#*-}"
      for ((cpu = start; cpu <= end; cpu++)); do
        echo "$cpu"
      done
    elif [ -n "$part" ]; then
      echo "$part"
    fi
  done
}

pin_vcpus() {
  [ -n "$HOST_CPUS" ] || return 0
  mapfile -t cpus < <(expand_cpu_list "$HOST_CPUS")
  [ "${#cpus[@]}" -gt 0 ] || return 0

  for _ in $(seq 1 100); do
    count=0
    for task in "/proc/${fc_pid}/task/"*; do
      [ -e "$task/comm" ] || continue
      comm="$(cat "$task/comm")"
      case "$comm" in
        fc_vcpu*) count=$((count + 1)) ;;
      esac
    done
    [ "$count" -ge "$VCPUS" ] && break
    sleep 0.05
  done

  for task in "/proc/${fc_pid}/task/"*; do
    [ -e "$task/comm" ] || continue
    tid="${task##*/}"
    comm="$(cat "$task/comm")"
    case "$comm" in
      fc_vcpu*)
        idx="${comm#fc_vcpu }"
        cpu="${cpus[$((idx % ${#cpus[@]}))]}"
        taskset -pc "$cpu" "$tid" >/dev/null
        ;;
    esac
  done
}

fc_put /logger "{
  \"log_path\": \"${log_file}\",
  \"level\": \"Info\",
  \"show_level\": true,
  \"show_log_origin\": true
}"

fc_put /machine-config "{
  \"vcpu_count\": ${VCPUS},
  \"mem_size_mib\": ${MEM_MIB},
  \"smt\": false
}"

fc_put /boot-source "{
  \"kernel_image_path\": \"${KERNEL}\",
  \"boot_args\": \"console=ttyS0 reboot=k panic=1 root=/dev/vda rw\"
}"

fc_put /drives/rootfs "{
  \"drive_id\": \"rootfs\",
  \"path_on_host\": \"${rootfs}\",
  \"is_root_device\": true,
  \"is_read_only\": false
}"

fc_put /network-interfaces/net1 "{
  \"iface_id\": \"net1\",
  \"guest_mac\": \"${guest_mac}\",
  \"host_dev_name\": \"${tap_dev}\"
}"

sleep 0.05
fc_put /actions '{"action_type":"InstanceStart"}'
pin_vcpus

cat >"$state_file" <<STATE
ID=${safe_id}
PID=${fc_pid}
TAP_DEV=${tap_dev}
TAP_IP=${tap_ip}
GUEST_IP=${guest_ip}
GUEST_MAC=${guest_mac}
API_SOCKET=${api_socket}
ROOTFS=${rootfs}
VCPUS=${VCPUS}
MEM_MIB=${MEM_MIB}
SSH_KEY=${SSH_KEY}
UNIT=${unit}
HOST_CPUS=${HOST_CPUS}
HOST_IFACE=${host_iface}
FIREWALL_MODE=shared
FIRECRACKER_TAP_MATCH=${FIRECRACKER_TAP_MATCH}
FIRECRACKER_GUEST_CIDR=${FIRECRACKER_GUEST_CIDR}
STATE

for _ in $(seq 1 60); do
  if ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=1 -i "$SSH_KEY" "root@${guest_ip}" true >/dev/null 2>&1; then
    ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -i "$SSH_KEY" "root@${guest_ip}" \
      "ip route replace default via ${tap_ip} dev eth0; printf 'nameserver 10.255.0.100\nnameserver 8.8.8.8\n' > /etc/resolv.conf" >/dev/null
    break
  fi
  sleep 1
done

echo "ID=${safe_id}"
echo "PID=${fc_pid}"
echo "TAP=${tap_dev}"
echo "GUEST_IP=${guest_ip}"
echo "SSH=ssh -i ${SSH_KEY} root@${guest_ip}"
