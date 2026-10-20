#!/usr/bin/env bash
set -euo pipefail

default_proxmox_api="http://atl1-pc1-025.hosts.telnyx.io:8090"
default_firecracker_api="http://atl1-pc1-027.hosts.telnyx.io:8090"

proxmox_api="${PROXMOX_FASTVM_API:-$default_proxmox_api}"
firecracker_api="${FIRECRACKER_FASTVM_API:-$default_firecracker_api}"
backends="${FASTVM_BENCH_BACKENDS:-proxmox,firecracker}"
clone_count="${FASTVM_BENCH_CLONES:-2}"
cores="${FASTVM_BENCH_CORES:-16}"
memory_mb="${FASTVM_BENCH_MEMORY_MB:-65536}"
jobs="${FASTVM_BENCH_JOBS:-$cores}"
compile_memory_mb="${FASTVM_BENCH_COMPILE_MEMORY_MB:-60000}"
compile_timeout="${FASTVM_BENCH_COMPILE_TIMEOUT:-7200}"
results_dir="${FASTVM_BENCH_RESULTS_DIR:-}"
source_tgz="${FASTVM_BENCH_SOURCE_TGZ:-}"
source_dir="${FASTVM_BENCH_SOURCE_DIR:-}"
bazel_bin="${FASTVM_BENCH_BAZEL_BIN:-}"
ssh_private_key="${FASTVM_BENCH_SSH_KEY:-}"
ssh_public_key="${FASTVM_BENCH_SSH_PUBKEY:-}"
parallel_compile=1
skip_compile=0
keep_vms=0
keep_templates=0
dry_run=0
declare -A cleanup_vms=()
declare -A cleanup_templates=()

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
workload_script="${script_dir}/run-ffproxy-build.sh"

usage() {
  cat <<'USAGE'
Usage:
  scripts/bench/compare-fastvm-backends.sh [options]

Compares Proxmox and Firecracker FastVM backends through the same fastvm-api
contract. For each backend it measures:

  create base VM -> stage/setup workload -> snapshot -> clone batch ->
  compile on clones -> converge/remove losers -> destroy winner -> cleanup template

Options:
  --proxmox-api URL         Proxmox API URL.
  --firecracker-api URL     Firecracker API URL.
  --backends LIST           Comma list: proxmox,firecracker. Default: both.
  --source-tgz PATH         feature-flag-proxy-cpp source tarball.
  --source-dir PATH         Source tree to tar as feature-flag-proxy-cpp.
  --bazel-bin PATH          Bazel binary copied into the VM.
  --clone-count N           Clone count from the benchmark snapshot. Default: 2.
  --cores N                 vCPUs per VM. Default: 16.
  --memory-mb N             RAM per VM. Default: 65536.
  --jobs N                  Bazel jobs. Default: cores.
  --compile-memory-mb N     Bazel local_resources memory. Default: 60000.
  --compile-timeout SEC     Per-clone compile timeout. Default: 7200.
  --results-dir PATH        Output directory. Default: bench-results/<timestamp>.
  --ssh-key PATH            Private key used to SSH into created VMs.
  --ssh-pubkey PATH         Public key injected into created VMs.
  --sequential-compile      Compile on clones one at a time.
  --skip-compile            Measure lifecycle only.
  --keep-vms                Leave VMs/templates in place for debugging.
  --keep-template           Destroy VMs but leave the snapshot template.
  --dry-run                 Print resolved config and exit.
  -h, --help                Show this help.

Required for compile workload:
  --source-tgz or --source-dir, and --bazel-bin.

Environment mirrors the long flags with FASTVM_BENCH_* variables.
USAGE
}

log() {
  printf '[%s] %s\n' "$(date -u +%H:%M:%S)" "$*" >&2
}

die() {
  printf 'error: %s\n' "$*" >&2
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

now_ms() {
  date +%s%3N
}

elapsed_seconds() {
  local start_ms="$1"
  local end_ms="$2"
  awk -v start="$start_ms" -v end="$end_ms" 'BEGIN { printf "%.3f", (end - start) / 1000 }'
}

api_host_from_url() {
  local url="$1"
  url="${url#http://}"
  url="${url#https://}"
  url="${url%%/*}"
  printf '%s\n' "${url%%:*}"
}

cleanup_key() {
  printf '%s|%s|%s' "$1" "$2" "$3"
}

register_vm_cleanup() {
  cleanup_vms["$(cleanup_key "$1" "$2" "$3")"]=1
}

unregister_vm_cleanup() {
  local key
  key="$(cleanup_key "$1" "$2" "$3")"
  unset "cleanup_vms[$key]"
}

register_template_cleanup() {
  cleanup_templates["$(cleanup_key "$1" "$2" "$3")"]=1
}

unregister_template_cleanup() {
  local key
  key="$(cleanup_key "$1" "$2" "$3")"
  unset "cleanup_templates[$key]"
}

cleanup_on_exit() {
  local status="$?"
  local key backend api vmid

  if [ "$status" -ne 0 ] && [ "$keep_vms" -eq 0 ]; then
    log "Benchmark failed; cleaning registered VMs/templates"
    for key in "${!cleanup_vms[@]}"; do
      IFS='|' read -r backend api vmid <<<"$key"
      [ -n "$vmid" ] || continue
      curl -sS -X DELETE "${api}/api/v1/vms/${vmid}" >/dev/null || true
    done
    if [ "$keep_templates" -eq 0 ]; then
      for key in "${!cleanup_templates[@]}"; do
        IFS='|' read -r backend api vmid <<<"$key"
        [ -n "$vmid" ] || continue
        cleanup_template "$backend" "$api" "$vmid" >/dev/null 2>&1 || true
      done
    fi
  fi
}

record_event() {
  local backend="$1"
  local phase="$2"
  local seconds="$3"
  local status="${4:-ok}"
  local vmid="${5:-}"
  local details="${6:-}"

  jq -nc \
    --arg backend "$backend" \
    --arg phase "$phase" \
    --arg status "$status" \
    --arg vmid "$vmid" \
    --arg details "$details" \
    --argjson seconds "$seconds" \
    --arg timestamp "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{timestamp:$timestamp,backend:$backend,phase:$phase,seconds:$seconds,status:$status,vmid:$vmid,details:$details}' \
    >>"${events_path}"
}

api_request() {
  local backend="$1"
  local method="$2"
  local api="$3"
  local path="$4"
  local body="${5:-}"
  local out="$6"
  local code

  if [ -n "$body" ]; then
    code="$(curl -sS -w '%{http_code}' -o "$out" -X "$method" -H 'Content-Type: application/json' --data "$body" "${api}${path}")"
  else
    code="$(curl -sS -w '%{http_code}' -o "$out" -X "$method" "${api}${path}")"
  fi

  if [ "$code" -lt 200 ] || [ "$code" -ge 300 ]; then
    printf 'API %s %s returned HTTP %s\n' "$method" "${api}${path}" "$code" >&2
    cat "$out" >&2 || true
    return 1
  fi

  if [ "$(jq -r '.ok // false' "$out")" != "true" ]; then
    printf 'API %s %s returned ok=false\n' "$method" "${api}${path}" >&2
    cat "$out" >&2 || true
    return 1
  fi

  jq --arg backend "$backend" '.data' "$out"
}

autodetect_ssh_keys() {
  if [ -z "$ssh_public_key" ]; then
    for candidate in "$HOME/.ssh/id_ed25519.pub" "$HOME/.ssh/id_rsa.pub"; do
      if [ -f "$candidate" ]; then
        ssh_public_key="$candidate"
        break
      fi
    done
  fi

  if [ -z "$ssh_private_key" ] && [ -n "$ssh_public_key" ]; then
    local candidate="${ssh_public_key%.pub}"
    if [ -f "$candidate" ]; then
      ssh_private_key="$candidate"
    fi
  fi
}

parse_ssh_command() {
  local ssh_command="$1"
  local __port_var="$2"
  local __target_var="$3"
  local parsed_port parsed_target

  parsed_port="$(awk '{for (i = 1; i <= NF; i++) if ($i == "-p") {print $(i + 1); exit}}' <<<"$ssh_command")"
  parsed_target="${ssh_command##* }"

  [ -n "$parsed_port" ] || return 1
  [[ "$parsed_target" == *@* ]] || return 1

  printf -v "$__port_var" '%s' "$parsed_port"
  printf -v "$__target_var" '%s' "$parsed_target"
}

ssh_base_args() {
  local args=(
    -o StrictHostKeyChecking=no
    -o UserKnownHostsFile=/dev/null
    -o LogLevel=ERROR
    -o ConnectTimeout=5
    -o ServerAliveInterval=30
    -o ServerAliveCountMax=3
  )
  if [ -n "$ssh_private_key" ]; then
    args+=(-i "$ssh_private_key")
  fi
  printf '%s\0' "${args[@]}"
}

ssh_run() {
  local port="$1"
  local target="$2"
  local command="$3"
  local -a args=()
  while IFS= read -r -d '' arg; do
    args+=("$arg")
  done < <(ssh_base_args)
  ssh "${args[@]}" -p "$port" "$target" "$command"
}

ssh_run_timeout() {
  local timeout_s="$1"
  local port="$2"
  local target="$3"
  local command="$4"
  local -a args=()
  while IFS= read -r -d '' arg; do
    args+=("$arg")
  done < <(ssh_base_args)
  timeout "$timeout_s" ssh "${args[@]}" -p "$port" "$target" "$command"
}

scp_to_vm() {
  local port="$1"
  local target="$2"
  local src="$3"
  local dst="$4"
  local -a args=()
  while IFS= read -r -d '' arg; do
    args+=("$arg")
  done < <(ssh_base_args)
  scp "${args[@]}" -P "$port" "$src" "${target}:${dst}"
}

scp_from_vm() {
  local port="$1"
  local target="$2"
  local src="$3"
  local dst="$4"
  local -a args=()
  while IFS= read -r -d '' arg; do
    args+=("$arg")
  done < <(ssh_base_args)
  scp "${args[@]}" -P "$port" "${target}:${src}" "$dst"
}

wait_for_ssh() {
  local backend="$1"
  local port="$2"
  local target="$3"
  local timeout_s="${4:-180}"
  local start_ms end_ms deadline

  start_ms="$(now_ms)"
  deadline=$((SECONDS + timeout_s))
  until ssh_run "$port" "$target" 'true' >/dev/null 2>&1; do
    if [ "$SECONDS" -ge "$deadline" ]; then
      end_ms="$(now_ms)"
      record_event "$backend" "ssh_ready" "$(elapsed_seconds "$start_ms" "$end_ms")" "failed" "" "$target:$port"
      return 1
    fi
    sleep 2
  done
  end_ms="$(now_ms)"
  record_event "$backend" "ssh_ready" "$(elapsed_seconds "$start_ms" "$end_ms")" "ok" "" "$target:$port"
}

remote_env_command() {
  local mode="$1"
  local label="$2"
  local remote_dir="$3"
  local workdir="$4"
  local log_path="$5"
  local result_path="$6"
  local env_prefix

  env_prefix="SRC_TGZ=${remote_dir}/feature-flag-proxy-cpp-src.tgz BAZEL_BIN=${remote_dir}/bazel JOBS=${jobs} MEMORY_MB=${compile_memory_mb} WORKDIR=${workdir} LOG_PATH=${log_path} RESULT_PATH=${result_path}"
  printf 'env %s bash %s/run-ffproxy-build.sh %s %s' \
    "$env_prefix" "$remote_dir" "$label" "$mode"
}

stage_workload() {
  local backend="$1"
  local port="$2"
  local target="$3"
  local remote_dir="$4"
  local label="$5"
  local workdir="$6"
  local start_ms end_ms

  start_ms="$(now_ms)"
  ssh_run "$port" "$target" "mkdir -p '$remote_dir'"
  scp_to_vm "$port" "$target" "$workload_script" "${remote_dir}/run-ffproxy-build.sh"
  scp_to_vm "$port" "$target" "$source_tgz" "${remote_dir}/feature-flag-proxy-cpp-src.tgz"
  scp_to_vm "$port" "$target" "$bazel_bin" "${remote_dir}/bazel"
  ssh_run "$port" "$target" "chmod +x '${remote_dir}/run-ffproxy-build.sh' '${remote_dir}/bazel'"

  ssh_run "$port" "$target" "$(remote_env_command setup "$label" "$remote_dir" "$workdir" "${remote_dir}/setup.log" "${remote_dir}/setup.env")" \
    >"${results_dir}/${backend}-setup.stdout" 2>"${results_dir}/${backend}-setup.stderr"
  scp_from_vm "$port" "$target" "${remote_dir}/setup.env" "${results_dir}/${backend}-setup.env" || true

  end_ms="$(now_ms)"
  record_event "$backend" "stage_and_setup" "$(elapsed_seconds "$start_ms" "$end_ms")" "ok" "" "$target"
}

run_compile_one() {
  local backend="$1"
  local vmid="$2"
  local ssh_command="$3"
  local remote_dir="$4"
  local workdir="$5"
  local label="${backend}-${vmid}"
  local port target start_ms end_ms status detail

  parse_ssh_command "$ssh_command" port target || die "cannot parse SSH command for VM ${vmid}: ${ssh_command}"
  wait_for_ssh "$backend" "$port" "$target" "$compile_timeout"

  start_ms="$(now_ms)"
  status="ok"
  detail="$target"
  if ! ssh_run_timeout "$compile_timeout" "$port" "$target" "$(remote_env_command build "$label" "$remote_dir" "$workdir" "${remote_dir}/${label}.log" "${remote_dir}/${label}.env")" \
      >"${results_dir}/${backend}-${vmid}-build.stdout" 2>"${results_dir}/${backend}-${vmid}-build.stderr"; then
    status="failed"
    detail="compile command failed or timed out"
  fi
  end_ms="$(now_ms)"

  scp_from_vm "$port" "$target" "${remote_dir}/${label}.env" "${results_dir}/${backend}-${vmid}-build.env" || true
  scp_from_vm "$port" "$target" "${remote_dir}/${label}.log" "${results_dir}/${backend}-${vmid}-build.log" || true
  record_event "$backend" "compile" "$(elapsed_seconds "$start_ms" "$end_ms")" "$status" "$vmid" "$detail"

  [ "$status" = "ok" ]
}

cleanup_template() {
  local backend="$1"
  local api="$2"
  local template_vmid="$3"
  local host

  [ "$keep_templates" -eq 0 ] || return 0
  [[ "$template_vmid" =~ ^[0-9]+$ ]] || die "refusing to clean non-numeric template id: ${template_vmid}"

  host="$(api_host_from_url "$api")"
  log "Cleaning ${backend} benchmark template ${template_vmid} on ${host}"
  case "$backend" in
    proxmox)
      ssh "$host" "sudo qm destroy '${template_vmid}' --purge 1" >/dev/null
      ;;
    firecracker)
      ssh "$host" "sudo rm -f '/opt/fastvm-microvm/templates/${template_vmid}.ext4' '/opt/fastvm-microvm/templates/${template_vmid}.env'" >/dev/null
      ;;
  esac
}

create_source_tgz_from_dir() {
  local src="$1"
  local out="$2"
  local parent base
  parent="$(cd "$(dirname "$src")" && pwd)"
  base="$(basename "$src")"
  tar -C "$parent" \
    --exclude='.git' \
    --exclude='bazel-*' \
    --exclude='*.o' \
    -czf "$out" \
    --transform "s#^${base}#feature-flag-proxy-cpp#" \
    "$base"
}

run_backend() {
  local backend="$1"
  local api="$2"
  local prefix="$3"
  local user_base="fb${run_id}${prefix}b"
  local user_clone="fb${run_id}${prefix}c"
  local base_vmid=""
  local base_ssh=""
  local template_vmid=""
  local clone_vmids=()
  local clone_ssh=()
  local keep_vmid=""
  local tmp_json start_ms end_ms body data remote_user remote_home remote_dir workdir

  tmp_json="${results_dir}/${backend}-response.json"
  log "Benchmarking ${backend} via ${api}"
  api_request "$backend" GET "$api" "/api/v1/health" "" "$tmp_json" >/dev/null
  data="$(api_request "$backend" GET "$api" "/api/v1/status" "" "$tmp_json")"
  record_event "$backend" "status" "0" "ok" "" "$(jq -r '.backend // "unknown"' <<<"$data")"

  body="$(jq -nc \
    --arg user "$user_base" \
    --arg name "bench-${backend}-${run_id}-base" \
    --arg ssh_key_content "$(cat "$ssh_public_key")" \
    --argjson count 1 \
    --argjson memory "$memory_mb" \
    --argjson cores "$cores" \
    '{user:$user,count:$count,name:$name,memory:$memory,cores:$cores,ssh_key_content:$ssh_key_content}')"

  start_ms="$(now_ms)"
  data="$(api_request "$backend" POST "$api" "/api/v1/vms" "$body" "$tmp_json")"
  end_ms="$(now_ms)"
  base_vmid="$(jq -r '.vms[0].vmid' <<<"$data")"
  base_ssh="$(jq -r '.vms[0].ssh_command' <<<"$data")"
  register_vm_cleanup "$backend" "$api" "$base_vmid"
  record_event "$backend" "create_base" "$(elapsed_seconds "$start_ms" "$end_ms")" "ok" "$base_vmid" "$base_ssh"

  parse_ssh_command "$base_ssh" base_port base_target || die "cannot parse base SSH command: ${base_ssh}"
  remote_user="${base_target%@*}"
  if [ "$remote_user" = "root" ]; then
    remote_home="/root"
  else
    remote_home="/home/${remote_user}"
  fi
  remote_dir="${remote_home}/fastvm-bench-${run_id}/stage"
  workdir="${remote_home}/fastvm-bench-${run_id}/work"
  wait_for_ssh "$backend" "$base_port" "$base_target" 240

  if [ "$skip_compile" -eq 0 ]; then
    stage_workload "$backend" "$base_port" "$base_target" "$remote_dir" "${backend}-base" "$workdir"
  fi

  start_ms="$(now_ms)"
  api_request "$backend" POST "$api" "/api/v1/vms/${base_vmid}/snapshot" '{}' "$tmp_json" >/dev/null
  end_ms="$(now_ms)"
  template_vmid="$base_vmid"
  unregister_vm_cleanup "$backend" "$api" "$base_vmid"
  register_template_cleanup "$backend" "$api" "$template_vmid"
  base_vmid=""
  record_event "$backend" "snapshot" "$(elapsed_seconds "$start_ms" "$end_ms")" "ok" "$template_vmid" "template=${template_vmid}"

  body="$(jq -nc \
    --arg user "$user_clone" \
    --arg name "bench-${backend}-${run_id}-clone" \
    --arg ssh_key_content "$(cat "$ssh_public_key")" \
    --argjson count "$clone_count" \
    --argjson memory "$memory_mb" \
    --argjson cores "$cores" \
    --argjson template "$template_vmid" \
    '{user:$user,count:$count,name:$name,memory:$memory,cores:$cores,template:$template,ssh_key_content:$ssh_key_content}')"

  start_ms="$(now_ms)"
  data="$(api_request "$backend" POST "$api" "/api/v1/vms" "$body" "$tmp_json")"
  end_ms="$(now_ms)"
  mapfile -t clone_vmids < <(jq -r '.vms[].vmid' <<<"$data")
  mapfile -t clone_ssh < <(jq -r '.vms[].ssh_command' <<<"$data")
  for vmid in "${clone_vmids[@]}"; do
    register_vm_cleanup "$backend" "$api" "$vmid"
  done
  keep_vmid="${clone_vmids[0]:-}"
  record_event "$backend" "clone_batch" "$(elapsed_seconds "$start_ms" "$end_ms")" "ok" "" "count=${#clone_vmids[@]},template=${template_vmid}"

  if [ "$skip_compile" -eq 0 ]; then
    start_ms="$(now_ms)"
    if [ "$parallel_compile" -eq 1 ]; then
      local pids=()
      for idx in "${!clone_vmids[@]}"; do
        run_compile_one "$backend" "${clone_vmids[$idx]}" "${clone_ssh[$idx]}" "$remote_dir" "$workdir" &
        pids+=("$!")
      done
      local failed=0
      for pid in "${pids[@]}"; do
        if ! wait "$pid"; then
          failed=1
        fi
      done
      [ "$failed" -eq 0 ] || die "${backend} compile failed; see ${results_dir}"
    else
      for idx in "${!clone_vmids[@]}"; do
        run_compile_one "$backend" "${clone_vmids[$idx]}" "${clone_ssh[$idx]}" "$remote_dir" "$workdir"
      done
    fi
    end_ms="$(now_ms)"
    record_event "$backend" "compile_wall" "$(elapsed_seconds "$start_ms" "$end_ms")" "ok" "" "count=${#clone_vmids[@]}"
  fi

  if [ "${#clone_vmids[@]}" -gt 1 ]; then
    body="$(jq -nc --arg user "$user_clone" --arg keep "$keep_vmid" '{user:$user,keep_vmid:$keep}')"
    start_ms="$(now_ms)"
    api_request "$backend" POST "$api" "/api/v1/converge" "$body" "$tmp_json" >/dev/null
    end_ms="$(now_ms)"
    record_event "$backend" "converge_remove" "$(elapsed_seconds "$start_ms" "$end_ms")" "ok" "$keep_vmid" "removed=$((${#clone_vmids[@]} - 1))"
    for vmid in "${clone_vmids[@]}"; do
      if [ "$vmid" != "$keep_vmid" ]; then
        unregister_vm_cleanup "$backend" "$api" "$vmid"
      fi
    done
    clone_vmids=("$keep_vmid")
  fi

  if [ "$keep_vms" -eq 0 ] && [ -n "$keep_vmid" ]; then
    start_ms="$(now_ms)"
    api_request "$backend" DELETE "$api" "/api/v1/vms/${keep_vmid}" "" "$tmp_json" >/dev/null
    end_ms="$(now_ms)"
    record_event "$backend" "destroy_winner" "$(elapsed_seconds "$start_ms" "$end_ms")" "ok" "$keep_vmid" ""
    unregister_vm_cleanup "$backend" "$api" "$keep_vmid"
    clone_vmids=()
    keep_vmid=""
  fi

  if [ "$keep_vms" -eq 0 ] && [ -n "$template_vmid" ]; then
    start_ms="$(now_ms)"
    cleanup_template "$backend" "$api" "$template_vmid"
    end_ms="$(now_ms)"
    record_event "$backend" "cleanup_template" "$(elapsed_seconds "$start_ms" "$end_ms")" "ok" "$template_vmid" ""
    unregister_template_cleanup "$backend" "$api" "$template_vmid"
    template_vmid=""
  fi
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --proxmox-api) proxmox_api="$2"; shift 2 ;;
    --firecracker-api) firecracker_api="$2"; shift 2 ;;
    --backends) backends="$2"; shift 2 ;;
    --source-tgz) source_tgz="$2"; shift 2 ;;
    --source-dir) source_dir="$2"; shift 2 ;;
    --bazel-bin) bazel_bin="$2"; shift 2 ;;
    --clone-count) clone_count="$2"; shift 2 ;;
    --cores) cores="$2"; shift 2 ;;
    --memory-mb) memory_mb="$2"; shift 2 ;;
    --jobs) jobs="$2"; shift 2 ;;
    --compile-memory-mb) compile_memory_mb="$2"; shift 2 ;;
    --compile-timeout) compile_timeout="$2"; shift 2 ;;
    --results-dir) results_dir="$2"; shift 2 ;;
    --ssh-key) ssh_private_key="$2"; shift 2 ;;
    --ssh-pubkey) ssh_public_key="$2"; shift 2 ;;
    --sequential-compile) parallel_compile=0; shift ;;
    --skip-compile) skip_compile=1; shift ;;
    --keep-vms) keep_vms=1; keep_templates=1; shift ;;
    --keep-template) keep_templates=1; shift ;;
    --dry-run) dry_run=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown option: $1" ;;
  esac
done

require_cmd curl
require_cmd jq
require_cmd ssh
require_cmd scp
require_cmd tar
require_cmd awk
require_cmd timeout

autodetect_ssh_keys
[ -n "$ssh_public_key" ] && [ -f "$ssh_public_key" ] || die "SSH public key is required; pass --ssh-pubkey"
[ -n "$ssh_private_key" ] && [ -f "$ssh_private_key" ] || die "SSH private key is required; pass --ssh-key"

if [ "$skip_compile" -eq 0 ]; then
  [ -x "$workload_script" ] || die "missing executable workload script: $workload_script"
  if [ -n "$source_dir" ]; then
    [ -d "$source_dir" ] || die "source dir does not exist: $source_dir"
  elif [ -n "$source_tgz" ]; then
    [ -f "$source_tgz" ] || die "source tarball does not exist: $source_tgz"
  else
    die "compile workload requires --source-dir or --source-tgz"
  fi
  [ -n "$bazel_bin" ] && [ -f "$bazel_bin" ] || die "compile workload requires --bazel-bin"
fi

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
run_id="$(date -u +%d%H%M%S)$$"
if [ -z "$results_dir" ]; then
  results_dir="bench-results/${timestamp}"
fi
events_path="${results_dir}/events.jsonl"

if [ "$dry_run" -eq 1 ]; then
  cat <<DRYRUN
proxmox_api=${proxmox_api}
firecracker_api=${firecracker_api}
backends=${backends}
clone_count=${clone_count}
cores=${cores}
memory_mb=${memory_mb}
jobs=${jobs}
compile_memory_mb=${compile_memory_mb}
skip_compile=${skip_compile}
source_tgz=${source_tgz}
source_dir=${source_dir}
bazel_bin=${bazel_bin}
ssh_public_key=${ssh_public_key}
ssh_private_key=${ssh_private_key}
results_dir=${results_dir}
DRYRUN
  exit 0
fi

mkdir -p "$results_dir"
: >"$events_path"
trap cleanup_on_exit EXIT

if [ "$skip_compile" -eq 0 ] && [ -n "$source_dir" ]; then
  source_tgz="${results_dir}/feature-flag-proxy-cpp-src.tgz"
  log "Creating source tarball from ${source_dir}"
  create_source_tgz_from_dir "$source_dir" "$source_tgz"
fi

IFS=',' read -r -a backend_list <<<"$backends"
for backend in "${backend_list[@]}"; do
  case "$backend" in
    proxmox) run_backend proxmox "$proxmox_api" p ;;
    firecracker) run_backend firecracker "$firecracker_api" f ;;
    *) die "unknown backend in --backends: $backend" ;;
  esac
done

jq -r '[.backend,.phase,.seconds,.status,.vmid,.details] | @tsv' "$events_path" >"${results_dir}/events.tsv"

{
  echo "# FastVM Backend Benchmark"
  echo
  echo "- Timestamp: ${timestamp}"
  echo "- Backends: ${backends}"
  echo "- Clone count: ${clone_count}"
  echo "- VM shape: ${cores} cores / ${memory_mb} MiB"
  echo "- Compile: $([ "$skip_compile" -eq 1 ] && echo skipped || echo "feature-flag-proxy-cpp, jobs=${jobs}")"
  echo
  echo "## Events"
  echo
  echo "| Backend | Phase | Seconds | Status | VMID | Details |"
  echo "|---|---:|---:|---|---|---|"
  jq -r '"| \(.backend) | \(.phase) | \(.seconds) | \(.status) | \(.vmid) | \(.details) |"' "$events_path"
} >"${results_dir}/summary.md"

log "Benchmark complete: ${results_dir}/summary.md"
