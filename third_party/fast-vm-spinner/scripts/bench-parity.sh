#!/usr/bin/env bash
# bench-parity.sh — benchmark of the fastvm warm-clone stack (sandbox-parity feature set).
#
# Run ON the API host (localhost API), single user, cleans up after itself.
#   curl -s http://localhost:8090/api/v1/health   # prerequisite
#   bash scripts/bench-parity.sh <user> [warm-template]
#
# Measures (host-side API wall clock; in-guest clocks are invalid on restored
# clones because KVM clock payloads are frozen):
#   1. cold create            (x3)
#   2. warm launch            (x5 sequential)
#   3. warm launch 8x concurrent
#   4. warm snapshot          (x3, of a fresh clone; new template removed after)
#   5. pause / resume         (x3 each)
#   6. exec round-trip        (x3)
#   7. port expose cycle      (x1: expose + HTTP fetch + unexpose)
#   8. isolation acceptance   (probe suite inside a clone)
set -uo pipefail

API="${FASTVM_API:-http://localhost:8090}"
USER="${1:?user required}"
TPL="${2:-warm-ubuntu-dev}"
TS="$(date -u +%Y%m%dT%H%M%SZ)"

jget() { python3 -c "import sys,json;d=json.load(sys.stdin);print(d$1)" 2>/dev/null; }
apitime() { # method path body -> seconds (single curl run, time_total)
  local m="$1" p="$2" b="${3:-}"
  if [ -n "$b" ]; then
    curl -s -o /tmp/.bench-resp -w '%{time_total}' -X "$m" -H 'Content-Type: application/json' \
      -d "$b" "$API$p"
  else
    curl -s -o /tmp/.bench-resp -w '%{time_total}' -X "$m" "$API$p"
  fi
}
stat_of() { jget '["status"]' </tmp/.bench-resp; }

R() { printf '%s\n' "$*"; }
med() { python3 - "$@" <<'PY'
import sys
xs = sorted(float(x) for x in sys.argv[1:])
print(f"{xs[len(xs)//2]:.3f}")
PY
}

# fresh sequenced VM name helper; list keys used by cleanup
CLEANUP_VMIDS=()
cleanup_on_exit() {
  for v in "${CLEANUP_VMIDS[@]:-}"; do
    [ -n "$v" ] && curl -s -X DELETE "$API/api/v1/vms/$v" >/dev/null 2>&1
  done
  # remove benchmark snapshot template if left behind
  curl -s -X DELETE "$API/api/v1/warm-templates/bench-parity-$TS" >/dev/null 2>&1
}
trap cleanup_on_exit EXIT

R "# fastvm warm-stack benchmark — $TS"
R
R "\`\`\`"
R "api          : $API"
R "user         : $USER"
R "template     : $TPL"
R "cpu          : $(lscpu 2>/dev/null | grep 'Model name' | cut -c21- || true)"
R "kernel       : $(uname -rm)"
R "firecracker  : $(cat /opt/fastvm-microvm/bin/FIRECRACKER_VERSION 2>/dev/null || /opt/fastvm-microvm/bin/firecracker --version 2>/dev/null | head -1 || echo n/a)"
R "mem          : $(free -h | awk '/^Mem/{print $2}')"
R "filesystem   : $(findmnt -no FSTYPE,SIZE $(df /opt/fastvm-microvm 2>/dev/null | tail -1 | awk '{print $1}') 2>/dev/null || df -h / | tail -1 | awk '{print $1,$2,$6}')"
R "\`\`\`"
R

# ---- 1. cold create ---------------------------------------------------------
times=()
for i in 1 2 3; do
  t=$(apitime POST /api/v1/vms "{\"user\":\"$USER\",\"count\":1,\"name\":\"bench-cold-$TS-$i\"}")
  sleep 1.5
  vmid=$(curl -s "$API/api/v1/vms/by-name/bench-cold-$TS-$i" | jget '["data"]["vmid"]')
  CLEANUP_VMIDS+=("$vmid"); times+=("$t")
done
cold_med=$(med "${times[@]}")
R "## Results"
R
R "| operation | samples | wall time (median) |"
R "|---|---|---|"
R "| cold create (boot, disk clone, sshd) | 3 | ${cold_med}s |"

# ---- 2. warm launch sequential ---------------------------------------------
vmid_cold_for_snap="${CLEANUP_VMIDS[0]}"
times=()
for i in 1 2 3 4 5; do
  t=$(apitime POST /api/v1/warm-launch "{\"user\":\"$USER\",\"template\":\"$TPL\",\"name\":\"bench-warm-$TS-$i\"}")
  sleep 1; vmid=$(curl -s "$API/api/v1/vms/by-name/bench-warm-$TS-$i" | jget '["data"]["vmid"]')
  CLEANUP_VMIDS+=("$vmid"); times+=("$t")
  curl -s -X DELETE "$API/api/v1/vms/$vmid" >/dev/null 2>&1 \
    || CLEANUP_VMIDS=("${CLEANUP_VMIDS[@]:0:${#CLEANUP_VMIDS[@]}-1}")
done
warm_med=$(med "${times[@]}")
R "| warm launch (restore 4 GiB memory-resident clone) | 5 | ${warm_med}s |"

# ---- 3. warm launch 8x concurrent ------------------------------------------
t0=$(date +%s.%N)
for i in 1 2 3 4 5 6 7 8; do
  apitime POST /api/v1/warm-launch "{\"user\":\"$USER\",\"template\":\"$TPL\",\"name\":\"bench-conc-$TS-$i\"}" >/dev/null &
done
wait
for check in $(seq 1 75); do
  allok=1
  for i in 1 2 3 4 5 6 7 8; do
    ip=$(curl -s "$API/api/v1/vms/by-name/bench-conc-$TS-$i" | jget '["data"]["ip"]' 2>/dev/null)
    [ -z "${ip:-}" ] && allok=0
  done
  [ "$allok" = "1" ] && break
  sleep 0.2
done
t1=$(date +%s.%N)
conc_total=$(python3 -c "print(f'{$t1-$t0:.2f}')")
conc_alive=$(for i in 1 2 3 4 5 6 7 8; do curl -s "$API/api/v1/vms/by-name/bench-conc-$TS-$i" | jget '["data"]["ip"]' 2>/dev/null | grep -c .; done | grep -c 1)
for i in 1 2 3 4 5 6 7 8; do
  vmid=$(curl -s "$API/api/v1/vms/by-name/bench-conc-$TS-$i" | jget '["data"]["vmid"]')
  [ -n "$vmid" ] && curl -s -X DELETE "$API/api/v1/vms/$vmid" >/dev/null 2>&1
done
R "| warm launch x8 (concurrent, total wall time) | 1 | ${conc_total}s ($conc_alive/8 served at once) |"

# ---- 4. warm snapshot (the 3 cold bench VMs are the sources) ---------------
times=()
for i in 1 2 3; do
  t=$(apitime POST "/api/v1/vms/${CLEANUP_VMIDS[$((i-1))]}/warmsnap" "{\"name\":\"bench-parity-$TS-$i\"}")
  times+=("$t")
  sleep 1.5
  curl -s -X DELETE "$API/api/v1/warm-templates/bench-parity-$TS-$i" >/dev/null 2>&1
done
snap_med=$(med "${times[@]}")
R "| warm snapshot (memory-dump 4 GiB cold VM, consumes source) | 3 | ${snap_med}s |"

# ---- 5. pause / resume + exec ----------------------------------------------
apitime POST /api/v1/warm-launch "{\"user\":\"$USER\",\"template\":\"$TPL\",\"name\":\"bench-ops-$TS\"}" >/dev/null
cvmid=$(curl -s "$API/api/v1/vms/by-name/bench-ops-$TS" | jget '["data"]["vmid"]')
sleep 30   # steady-state intent: skip the restored-guest first-connect window
CLEANUP_VMIDS+=("$cvmid")
ptimes=(); rtimes=()
for i in 1 2 3; do
  t=$(apitime POST /api/v1/vms/$cvmid/pause ''); ptimes+=("$t")
  t=$(apitime POST /api/v1/vms/$cvmid/resume ''); rtimes+=("$t")
done
R "| pause (VMM freeze, memory resident) | 3 | $(med "${ptimes[@]}")s |"
R "| resume (instant continue) | 3 | $(med "${rtimes[@]}")s |"

# ---- 6. exec ----------------------------------------------------------------
# first connect after restore can retry; use a warmed VM for the channel
etimes=()
for i in 1 2 3; do
  t=$(apitime POST /api/v1/vms/$cvmid/exec '{"command":"uptime","timeout_s":30}' '')
  etimes+=("$t")
done
R "| exec round-trip (SSH channel) | 3 | $(med "${etimes[@]}")s |"
curl -s -X DELETE "$API/api/v1/vms/$cvmid" >/dev/null 2>&1

# ---- 7. expose cycle --------------------------------------------------------
apitime POST /api/v1/warm-launch "{\"user\":\"$USER\",\"template\":\"$TPL\",\"name\":\"bench-xpose-$TS\"}" >/dev/null
xvmid=$(curl -s "$API/api/v1/vms/by-name/bench-xpose-$TS" | jget '["data"]["vmid"]')
sleep 45   # steady-state intent: skip the restored-guest first-connect window
t_expose=$(apitime POST /api/v1/vms/$xvmid/expose '{"guest_port":8080}')
hostport=$(jget '["data"]["host_port"]' </tmp/.bench-resp)
curl -s -X POST "$API/api/v1/vms/$xvmid/exec" -d '{"command":"setsid nohup timeout 30 python3 -m http.server 8080 >/dev/null 2>&1 < /dev/null & sleep 1","timeout_s":15}' >/dev/null
listening=$(curl -s -X POST "$API/api/v1/vms/$xvmid/exec" -d '{"command":"ss -ltn | grep -c :8080","timeout_s":15}' | tr -d '' | python3 -c "import sys,json;print(json.load(sys.stdin)['data']['output'].strip().splitlines()[-1])" 2>/dev/null)
[ "${listening:-}" = "1" ] || listening="0"
t0=$(date +%s.%N)
code=$(timeout 8 curl -4 -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:$hostport/"); t1=$(date +%s.%N)
t_fetch=$(python3 -c "print(f'{$t1-$t0:.3f}')")
[ -n "$hostport" ] && apitime POST /api/v1/vms/$xvmid/unexpose "{\"host_port\":$hostport}" >/dev/null
R "| expose (allocate host port + socat up) | 1 | ${t_expose}s (port ${hostport:-none}) |"
R "| exposed HTTP fetch (host -> guest ${listening:+service via }port $hostport) | 1 | ${t_fetch}s (HTTP ${code:-fail}) |"
R
curl -s -X DELETE "$API/api/v1/vms/$xvmid" >/dev/null 2>&1

# ---- 8. isolation probes ----------------------------------------------------
t0=$(date +%s.%N)
probe_script="$(dirname "$0")/isolation-probe.sh"
if [ -r "$probe_script" ]; then
  apitime POST /api/v1/warm-launch "{\"user\":\"$USER\",\"template\":\"$TPL\",\"name\":\"bench-probe-$TS\"}" >/dev/null
  pvmid=$(curl -s "$API/api/v1/vms/by-name/bench-probe-$TS" | jget '["data"]["vmid"]')
  CLEANUP_VMIDS+=("$pvmid")
  sleep 45   # steady-state probes: let the restored-guest first-connect settle
  # ship the probe script into the clone over the engine SSH channel
  sshport=$(curl -s "$API/api/v1/vms/$pvmid" | jget '["data"]["ssh_port"]')
  key=/opt/fastvm-microvm/images/id_rsa
  for try in $(seq 1 15); do
    res=$(timeout 40 ssh -p "$sshport" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
          -o BatchMode=yes -o IdentitiesOnly=yes -i "$key" -o ConnectTimeout=15 \
          "root@localhost" 'bash -s' < "$probe_script" 2>/dev/null)
    [ -n "$res" ] && break
    sleep 4
  done
  t1=$(date +%s.%N)
  probe_t=$(python3 -c "print(f'{$t1-$t0:.1f}')")
  if [ -z "${res:-}" ]; then
    R "| isolation acceptance | FAILED to reach guest via engine SSH (first-connect caveat) | n/a |"
  else
    R "\`\`\`"
    printf '%s\n' "$res"
    R "\`\`\`"
    failcount=$(printf '%s\n' "$res" | grep -c '  FAIL' || true)
    passcount=$(printf '%s\n' "$res" | grep -c '  PASS' || true)
    R
    R "| isolation acceptance (DNS allow, host/metadata/tenant drops, internet allow) | $passcount pass / $failcount fail | ${probe_t}s |"
  fi
  R

  curl -s -X DELETE "$API/api/v1/vms/$pvmid" >/dev/null 2>&1
else
  R "| isolation probe | skipped (isolation-probe.sh not found) | n/a |"
fi

R
leftover=$(for v in "${CLEANUP_VMIDS[@]}"; do curl -s "$API/api/v1/vms/$v" | jget '["data"]["vmid"]' 2>/dev/null; done | grep -c .)
R
R "VMs left behind: $leftover (target 0)"
