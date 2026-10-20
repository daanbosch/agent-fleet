#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-/opt/fastvm-microvm}"
TEMPLATE="${TEMPLATE:-plain-ubuntu}"
FALCON_SENSOR_DEB="${FALCON_SENSOR_DEB:-}"
FALCON_SENSOR_SHA256="${FALCON_SENSOR_SHA256:-}"
FALCON_DEPS="${FALCON_DEPS:-openssl libnl-3-200 libnl-genl-3-200 zlib1g}"

if [ "$(id -u)" -ne 0 ]; then
  echo "fc-install-falcon-template must run as root" >&2
  exit 1
fi

for cmd in cp date grep install mv rm sha256sum virt-customize; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "missing required command: $cmd" >&2
    exit 1
  fi
done

if [ -z "$FALCON_SENSOR_DEB" ]; then
  echo "FALCON_SENSOR_DEB must point to the falcon-sensor .deb package" >&2
  exit 1
fi

if [ ! -f "$FALCON_SENSOR_DEB" ]; then
  echo "falcon-sensor package not found: $FALCON_SENSOR_DEB" >&2
  exit 1
fi

if [ -n "$FALCON_SENSOR_SHA256" ]; then
  printf "%s  %s\n" "$FALCON_SENSOR_SHA256" "$FALCON_SENSOR_DEB" | sha256sum -c -
else
  FALCON_SENSOR_SHA256="$(sha256sum "$FALCON_SENSOR_DEB" | awk '{print $1}')"
fi

template_path="${INSTALL_DIR}/templates/${TEMPLATE}.ext4"
env_path="${INSTALL_DIR}/templates/${TEMPLATE}.env"
if [ ! -f "$template_path" ]; then
  echo "template not found: $template_path" >&2
  exit 1
fi

package_name="$(basename "$FALCON_SENSOR_DEB")"
stamp="$(date -u +%Y%m%dT%H%M%SZ)"
work_path="${INSTALL_DIR}/templates/${TEMPLATE}.with-falcon.${stamp}.ext4"
backup_path="${INSTALL_DIR}/templates/${TEMPLATE}.pre-falcon.${stamp}.ext4"
env_backup_path="${INSTALL_DIR}/templates/${TEMPLATE}.pre-falcon.${stamp}.env"
tmp_env="$(mktemp)"

cleanup() {
  rm -f "$tmp_env"
  if [ -f "$work_path" ]; then
    rm -f "$work_path"
  fi
}
trap cleanup EXIT

cp --reflink=auto --sparse=always "$template_path" "$work_path"

virt-customize -a "$work_path" \
  --upload "${FALCON_SENSOR_DEB}:/tmp/falcon-sensor.deb" \
  --run-command "chmod 1777 /tmp" \
  --run-command "DEBIAN_FRONTEND=noninteractive apt-get -q update" \
  --run-command "DEBIAN_FRONTEND=noninteractive apt-get -q -y install ${FALCON_DEPS}" \
  --run-command "DEBIAN_FRONTEND=noninteractive dpkg -i /tmp/falcon-sensor.deb" \
  --run-command "rm -f /tmp/falcon-sensor.deb" \
  --run-command "apt-get clean; rm -rf /var/lib/apt/lists/*" \
  --run-command "truncate -s 0 /etc/machine-id" \
  --run-command "rm -f /var/lib/dbus/machine-id" \
  --run-command "rm -f /opt/CrowdStrike/falconstore* /opt/CrowdStrike/Registry*.bin* 2>/dev/null || true" \
  --run-command "systemctl disable falcon-sensor 2>/dev/null || true"

virt-customize -a "$work_path" \
  --run-command "test -x /opt/CrowdStrike/falconctl && test -f /lib/systemd/system/falcon-sensor.service && dpkg-query -W falcon-sensor >/dev/null"

# virt-customize seeds machine-id before running commands. Sanitize it after
# the verification pass so cloned VMs generate their own identity at boot.
virt-customize -a "$work_path" \
  --run-command "truncate -s 0 /etc/machine-id; rm -f /var/lib/dbus/machine-id"

cp --reflink=auto --sparse=always "$template_path" "$backup_path"
if [ -f "$env_path" ]; then
  cp --reflink=auto --sparse=always "$env_path" "$env_backup_path"
  grep -Ev '^(FALCON_SENSOR_PACKAGE|FALCON_SENSOR_SHA256|FALCON_INSTALLED_AT)=' "$env_path" > "$tmp_env" || true
else
  : > "$tmp_env"
fi

{
  printf "FALCON_SENSOR_PACKAGE=%s\n" "$package_name"
  printf "FALCON_SENSOR_SHA256=%s\n" "$FALCON_SENSOR_SHA256"
  printf "FALCON_INSTALLED_AT=%s\n" "$stamp"
} >> "$tmp_env"

mv "$work_path" "$template_path"
chmod 0644 "$template_path"
install -m 0644 "$tmp_env" "$env_path"

trap - EXIT
rm -f "$tmp_env"

echo "template=${template_path}"
echo "backup=${backup_path}"
if [ -f "$env_backup_path" ]; then
  echo "env_backup=${env_backup_path}"
fi
echo "falcon_sensor_package=${package_name}"
echo "falcon_sensor_sha256=${FALCON_SENSOR_SHA256}"
