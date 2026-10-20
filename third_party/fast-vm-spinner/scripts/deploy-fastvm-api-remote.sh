#!/usr/bin/env bash
set -euo pipefail

binary_src="${FASTVM_API_BINARY_SRC:-/tmp/fastvm-api-linux-amd64}"
firecrackerctl_src="${FASTVM_FIRECRACKERCTL_SRC:-/tmp/firecrackerctl-linux-amd64}"
service_src="${FASTVM_API_SERVICE_SRC:-/tmp/fastvm-api.service}"
env_src="${FASTVM_API_ENV_SRC:-/tmp/fastvm-api.env}"
expected_version="${FASTVM_EXPECTED_VERSION:-}"

install -m 0755 "$binary_src" /usr/local/bin/fastvm-api
if [ -f "$firecrackerctl_src" ]; then
  install_dir="$(grep -E '^FASTVM_FIRECRACKER_INSTALL_DIR=' "$env_src" | cut -d= -f2-)"
  install_dir="${install_dir:-/opt/fastvm-microvm}"
  install -d -m 0755 "${install_dir}/bin"
  install -m 0755 "$firecrackerctl_src" "${install_dir}/bin/firecrackerctl"
fi
install -m 0644 "$service_src" /etc/systemd/system/fastvm-api.service
install -m 0600 "$env_src" /etc/default/fastvm-api

systemctl daemon-reload
systemctl enable fastvm-api
systemctl restart fastvm-api

for i in $(seq 1 30); do
  if curl -sf http://localhost:8090/api/v1/health >/dev/null; then
    break
  fi
  sleep 2
done

if ! version_json="$(curl -sf http://localhost:8090/api/v1/version)"; then
  echo "fastvm-api did not become healthy on localhost:8090" >&2
  journalctl -u fastvm-api -n 100 --no-pager >&2 || true
  exit 1
fi
echo "fastvm-api version response: ${version_json}"

if [ -n "$expected_version" ] && ! grep -q "\"version\":\"${expected_version}\"" <<<"$version_json"; then
  echo "Expected fastvm-api version ${expected_version}" >&2
  journalctl -u fastvm-api -n 100 --no-pager >&2 || true
  exit 1
fi

rm -f "$binary_src" "$firecrackerctl_src" "$service_src" "$env_src" /tmp/deploy-fastvm-api-remote.sh
