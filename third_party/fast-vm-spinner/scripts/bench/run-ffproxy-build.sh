#!/usr/bin/env bash
set -euo pipefail

label="${1:-ffproxy}"
mode="${MODE:-${2:-all}}"
src_tgz="${SRC_TGZ:-/tmp/feature-flag-proxy-cpp-src.tgz}"
bazel_bin="${BAZEL_BIN:-/tmp/bazel}"
jobs="${JOBS:-16}"
memory_mb="${MEMORY_MB:-60000}"
workdir="${WORKDIR:-/tmp/ffproxy-bench-${label}}"
log_path="${LOG_PATH:-/tmp/ffproxy-${label}.log}"
result_path="${RESULT_PATH:-/tmp/ffproxy-${label}.env}"

require_file() {
  if [ ! -f "$1" ]; then
    echo "missing required file: $1" >&2
    exit 1
  fi
}

require_file "$bazel_bin"
if [ "$mode" = "setup" ] || [ "$mode" = "all" ]; then
  require_file "$src_tgz"
fi

apt_get() {
  if [ "$(id -u)" -eq 0 ]; then
    DEBIAN_FRONTEND=noninteractive apt-get "$@"
  else
    sudo -n env DEBIAN_FRONTEND=noninteractive apt-get "$@"
  fi
}

case "$mode" in
  setup|build|all) ;;
  *)
    echo "usage: $0 [label] [setup|build|all]" >&2
    exit 2
    ;;
esac

start_epoch="$(date +%s)"
setup_start="$(date +%s)"

if [ "$mode" = "setup" ] || [ "$mode" = "all" ]; then
  if command -v apt-get >/dev/null 2>&1; then
    apt_get update -qq
    apt_get install -y -qq \
      build-essential \
      ca-certificates \
      curl \
      git \
      libgoogle-perftools-dev \
      libmaxminddb-dev \
      libpq-dev \
      libssl-dev \
      pkg-config \
      python3 \
      time \
      zlib1g-dev
  fi

  chmod +x "$bazel_bin"
  rm -rf "$workdir"
  mkdir -p "$workdir"
  tar -xzf "$src_tgz" -C "$workdir"
fi

setup_end="$(date +%s)"
cd "$workdir/feature-flag-proxy-cpp"

build_start="$(date +%s)"
build_exit=0
if [ "$mode" = "build" ] || [ "$mode" = "all" ]; then
  set +e
  /usr/bin/time -v "$bazel_bin" build -c opt --jobs="$jobs" --local_resources="memory=${memory_mb}" //:feature-flag-proxy >"$log_path" 2>&1
  build_exit="$?"
  set -e
fi
build_end="$(date +%s)"
end_epoch="$(date +%s)"

cat >"$result_path" <<RESULT
LABEL=${label}
MODE=${mode}
HOSTNAME=$(hostname)
START_UTC=$(date -u -d "@${start_epoch}" +"%Y-%m-%dT%H:%M:%SZ")
END_UTC=$(date -u -d "@${end_epoch}" +"%Y-%m-%dT%H:%M:%SZ")
NPROC=$(nproc)
MEM_TOTAL_KB=$(awk '/MemTotal:/ {print $2}' /proc/meminfo)
SETUP_SECONDS=$((setup_end - setup_start))
BUILD_SECONDS=$((build_end - build_start))
TOTAL_SECONDS=$((end_epoch - start_epoch))
BUILD_EXIT=${build_exit}
LOG_PATH=${log_path}
RESULT

cat "$result_path"
exit "$build_exit"
