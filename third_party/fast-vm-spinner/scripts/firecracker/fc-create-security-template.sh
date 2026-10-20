#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-/opt/fastvm-microvm}"
SOURCE_TEMPLATE="${SOURCE_TEMPLATE:-plain-ubuntu}"
TARGET_TEMPLATE="${TARGET_TEMPLATE:-9013}"
FORCE="${FORCE:-0}"
FASTVM_ENV="${FASTVM_ENV:-dev}"
FASTVM_CONSUL_DATACENTER="${FASTVM_CONSUL_DATACENTER:-at1-dev}"
FASTVM_SQUAD="${FASTVM_SQUAD:-infra-bare-metal-squad}"
LACEWORK_GPG_KEY_ID="${LACEWORK_GPG_KEY_ID:-360D55D76727556814078E25FF3E1D4DEE0CC692}"
FLUENTBIT_REPO_RELEASE="${FLUENTBIT_REPO_RELEASE:-noble}"
LACEWORK_REPO_RELEASE="${LACEWORK_REPO_RELEASE:-noble}"

if [ "$(id -u)" -ne 0 ]; then
  echo "fc-create-security-template must run as root" >&2
  exit 1
fi

for cmd in cp date grep install mktemp mv rm sed virt-customize; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "missing required command: $cmd" >&2
    exit 1
  fi
done

if ! [[ "$TARGET_TEMPLATE" =~ ^[0-9]+$ ]]; then
  echo "TARGET_TEMPLATE must be numeric so the existing fastvm CLI can select it with -t" >&2
  exit 1
fi

source_path="${INSTALL_DIR}/templates/${SOURCE_TEMPLATE}.ext4"
source_env_path="${INSTALL_DIR}/templates/${SOURCE_TEMPLATE}.env"
target_path="${INSTALL_DIR}/templates/${TARGET_TEMPLATE}.ext4"
target_env_path="${INSTALL_DIR}/templates/${TARGET_TEMPLATE}.env"

if [ ! -f "$source_path" ]; then
  echo "source template not found: $source_path" >&2
  exit 1
fi

if [ -e "$target_path" ] && [ "$FORCE" != "1" ]; then
  echo "target template already exists: $target_path (set FORCE=1 to replace)" >&2
  exit 1
fi

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
work_path="${INSTALL_DIR}/templates/${TARGET_TEMPLATE}.security.${stamp}.ext4"
backup_path="${INSTALL_DIR}/templates/${TARGET_TEMPLATE}.pre-security.${stamp}.ext4"
backup_env_path="${INSTALL_DIR}/templates/${TARGET_TEMPLATE}.pre-security.${stamp}.env"
tmp_dir="$(mktemp -d)"

cleanup() {
  rm -rf "$tmp_dir"
  if [ -f "$work_path" ]; then
    rm -f "$work_path"
  fi
}
trap cleanup EXIT

guest_script="${tmp_dir}/guest-security-template.sh"
fluentbit_conf="${tmp_dir}/fluent-bit.conf"
audit_rules="${tmp_dir}/telnyx.rules"
lacework_logrotate="${tmp_dir}/lacework-logrotate"
tmp_env="${tmp_dir}/template.env"

cat > "$guest_script" <<'GUEST_SCRIPT'
#!/usr/bin/env bash
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
export NEEDRESTART_MODE=a

apt_install() {
  apt-get -q -y \
    -o Dpkg::Options::=--force-confdef \
    -o Dpkg::Options::=--force-confold \
    install "$@"
}

: "${FASTVM_TEMPLATE:?}"
: "${FASTVM_ENV:?}"
: "${FASTVM_CONSUL_DATACENTER:?}"
: "${FASTVM_SQUAD:?}"
: "${LACEWORK_GPG_KEY_ID:?}"
: "${LACEWORK_REPO_RELEASE:?}"
: "${FLUENTBIT_REPO_RELEASE:?}"

chmod 1777 /tmp
apt-get -q update
apt_install ca-certificates curl gnupg apt-transport-https auditd iptables

# Firecracker's current CI kernel does not expose nf_tables, while Ubuntu
# defaults iptables to the nft frontend. Docker's default bridge/NAT setup works
# with the legacy xtables frontend on this kernel, so keep templates pinned to
# legacy before users install Docker into custom snapshots.
if command -v update-alternatives >/dev/null 2>&1; then
  for tool in iptables ip6tables arptables ebtables; do
    legacy="/usr/sbin/${tool}-legacy"
    if [ -x "$legacy" ]; then
      update-alternatives --set "$tool" "$legacy" || true
    fi
  done
fi

install -d -m 0755 /etc/apt/keyrings
curl -fsSL "https://keyserver.ubuntu.com/pks/lookup?op=get&search=0x${LACEWORK_GPG_KEY_ID}" \
  | gpg --dearmor --yes -o /etc/apt/keyrings/lacework-agent.gpg
curl -fsSL https://packages.fluentbit.io/fluentbit.key \
  | gpg --dearmor --yes -o /etc/apt/keyrings/fluent-bit.gpg

cat > /etc/apt/sources.list.d/lacework.list <<EOF
deb [arch=amd64 signed-by=/etc/apt/keyrings/lacework-agent.gpg] https://packages.lacework.net/latest/DEB/ubuntu ${LACEWORK_REPO_RELEASE} main
deb [arch=amd64 signed-by=/etc/apt/keyrings/lacework-agent.gpg] https://packages.lacework.net/established/DEB/ubuntu ${LACEWORK_REPO_RELEASE} main
deb [arch=amd64 signed-by=/etc/apt/keyrings/lacework-agent.gpg] https://packages.lacework.net/archived/DEB/ubuntu ${LACEWORK_REPO_RELEASE} main
EOF

cat > /etc/apt/sources.list.d/fluent-bit.list <<EOF
deb [arch=amd64 signed-by=/etc/apt/keyrings/fluent-bit.gpg] https://packages.fluentbit.io/ubuntu/${FLUENTBIT_REPO_RELEASE} ${FLUENTBIT_REPO_RELEASE} main
EOF

apt-get -q update
if ! getent passwd lacework-user >/dev/null; then
  useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin --uid 200 --comment "Lacework user" lacework-user
fi
apt_install lacework fluent-bit

install -d -m 0770 /etc/lacework
printf '{}\n' > /etc/lacework/config.json
chmod 0600 /etc/lacework/config.json
install -d -m 0755 /var/lib/lacework/config
printf 'LWDC_OPTS="-f /etc/lacework/config.json"\n' > /var/lib/lacework/config/commandline.conf
chmod 0644 /var/lib/lacework/config/commandline.conf
install -m 0644 /tmp/lacework-logrotate /etc/logrotate.d/lacework

install -d -m 0755 /etc/audit/rules.d
install -m 0640 /tmp/telnyx.rules /etc/audit/rules.d/telnyx.rules

install -d -m 0755 /etc/fluent-bit /data/fluent-bit/storage /data/journal
if [ -d /var/log/journal ] && [ ! -L /var/log/journal ]; then
  rm -rf /var/log/journal
fi
ln -sfn /data/journal /var/log/journal
sed \
  -e "s/__FASTVM_TEMPLATE__/${FASTVM_TEMPLATE}/g" \
  -e "s/__FASTVM_ENV__/${FASTVM_ENV}/g" \
  -e "s/__FASTVM_CONSUL_DATACENTER__/${FASTVM_CONSUL_DATACENTER}/g" \
  -e "s/__FASTVM_SQUAD__/${FASTVM_SQUAD}/g" \
  /tmp/fluent-bit.conf > /etc/fluent-bit/fluent-bit.conf
chmod 0644 /etc/fluent-bit/fluent-bit.conf

systemctl disable lacework datacollector fluent-bit auditd falcon-sensor 2>/dev/null || true
if [ -f /lib/systemd/system/datacollector.service ]; then
  install -m 0644 /lib/systemd/system/datacollector.service /etc/systemd/system/lacework.service
fi
rm -f /run/lacework* /tmp/lacework* /tmp/fluent-bit.conf /tmp/telnyx.rules /tmp/lacework-logrotate
apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/*
truncate -s 0 /etc/machine-id
rm -f /var/lib/dbus/machine-id
GUEST_SCRIPT

cat > "$fluentbit_conf" <<'FLUENTBIT_CONF'
[SERVICE]
    Flush                               1
    Daemon                              Off
    Log_Level                           info
    Parsers_File                        parsers.conf
    Plugins_File                        plugins.conf
    storage.type                        filesystem
    storage.sync                        normal
    storage.metrics                     off
    storage.path                        /data/fluent-bit/storage
    storage.delete_irrecoverable_chunks true
    storage.backlog.mem_limit           256M
    storage.max_chunks_up               100
    HTTP_Server                         On
    HTTP_Listen                         0.0.0.0
    HTTP_Port                           2020

[INPUT]
    Name            systemd
    Tag             fastvm.systemd
    storage.type    filesystem
    Read_From_Tail  On
    Mem_Buf_Limit   100MB

[INPUT]
    Name              tail
    Tag               fastvm.auditd
    Path              /var/log/audit/audit.log
    Path_Key          filename
    Read_from_Head    false
    Rotate_Wait       30
    Skip_Long_Lines   On
    Buffer_Chunk_Size 64KB
    Buffer_Max_Size   1MB
    Mem_Buf_Limit     100MB
    storage.type      filesystem

[FILTER]
    Name record_modifier
    Match fastvm.systemd
    Record service_name systemd

[FILTER]
    Name record_modifier
    Match fastvm.auditd
    Record service_name auditd

[FILTER]
    Name record_modifier
    Match *
    Record squad __FASTVM_SQUAD__
    Record consul_datacenter __FASTVM_CONSUL_DATACENTER__
    Record fastvm_backend firecracker
    Record fastvm_template __FASTVM_TEMPLATE__
    Record fastvm_env __FASTVM_ENV__
    Remove_key _AUDIT_FIELD_SUBJ_APPARMOR
    Remove_key _AUDIT_ID
    Remove_key _AUDIT_TYPE
    Remove_key _BOOT_ID
    Remove_key _CAP_EFFECTIVE
    Remove_key _CMDLINE
    Remove_key _COMM
    Remove_key _EXE
    Remove_key _GID
    Remove_key _HOSTNAME
    Remove_key _MACHINE_ID
    Remove_key _SYSLOG_IDENTIFIER
    Remove_key _PID
    Remove_key _SELINUX_CONTEXT
    Remove_key _SOURCE_REALTIME_TIMESTAMP
    Remove_key _STREAM_ID
    Remove_key _SYSTEMD_CGROUP
    Remove_key _SYSTEMD_INVOCATION_ID
    Remove_key _SYSTEMD_SLICE
    Remove_key _SYSTEMD_UNIT
    Remove_key _TRANSPORT
    Remove_key _UID
    Remove_key PRIORITY
    Remove_key SYSLOG_FACILITY
    Remove_key _RUNTIME_SCOPE

[OUTPUT]
    Name                     loki
    Match                    *
    Host                     grafana-alloy-vms.query.__FASTVM_ENV__.telnyx.io
    Port                     12900
    Retry_Limit              False
    storage.total_limit_size 1000M
    Labels                   source=${HOSTNAME},service_name=$service_name,squad=$squad,consul_datacenter=$consul_datacenter,backend=$fastvm_backend,template=$fastvm_template,env=$fastvm_env
FLUENTBIT_CONF

cat > "$audit_rules" <<'AUDIT_RULES'
# Telnyx auditd baseline for FastVM microVM templates.
-D
-b 16384
-f 1
-i

-w /var/log/audit/ -k auditlog
-w /etc/audit/ -p wa -k auditconfig
-w /etc/libaudit.conf -p wa -k auditconfig
-w /etc/audisp/ -p wa -k audispconfig
-w /sbin/auditctl -p x -k audittools
-w /sbin/auditd -p x -k audittools
-w /usr/sbin/augenrules -p x -k audittools

-a always,exclude -F msgtype=AVC
-a always,exclude -F msgtype=CWD
-a always,exclude -F msgtype=CRYPTO_KEY_USER
-a never,exit -F arch=b32 -F dir=/dev/shm -k sharedmemaccess
-a never,exit -F arch=b64 -F dir=/dev/shm -k sharedmemaccess
-a never,exit -F arch=b32 -F dir=/var/lock/lvm -k locklvm
-a never,exit -F arch=b64 -F dir=/var/lock/lvm -k locklvm

-w /etc/sysctl.conf -p wa -k sysctl
-w /etc/sysctl.d -p wa -k sysctl
-a always,exit -F perm=x -F auid!=-1 -F path=/sbin/insmod -k modules
-a always,exit -F perm=x -F auid!=-1 -F path=/sbin/modprobe -k modules
-a always,exit -F perm=x -F auid!=-1 -F path=/sbin/rmmod -k modules
-a always,exit -F arch=b64 -S finit_module -S init_module -S delete_module -F auid!=-1 -k modules
-a always,exit -F arch=b32 -S finit_module -S init_module -S delete_module -F auid!=-1 -k modules
-w /etc/modprobe.conf -p wa -k modprobe
-w /etc/modprobe.d -p wa -k modprobe

-w /etc/passwd -p wa -k identity
-w /etc/group -p wa -k identity
-w /etc/shadow -p wa -k identity
-w /etc/gshadow -p wa -k identity
-w /etc/security/opasswd -p wa -k identity
-w /etc/sudoers -p wa -k scope
-w /etc/sudoers.d/ -p wa -k scope
-w /etc/ssh/sshd_config -p wa -k sshd
-w /etc/ssh/sshd_config.d/ -p wa -k sshd
-w /etc/systemd/ -p wa -k systemd
-w /usr/lib/systemd/ -p wa -k systemd
-w /lib/systemd/ -p wa -k systemd

-a always,exit -F arch=b64 -S execve -F euid=0 -F auid>=1000 -F auid!=-1 -k rootcmd
-a always,exit -F arch=b32 -S execve -F euid=0 -F auid>=1000 -F auid!=-1 -k rootcmd
-a always,exit -F arch=b64 -S mount -S umount2 -F auid>=1000 -F auid!=-1 -k mount
-a always,exit -F arch=b32 -S mount -S umount2 -F auid>=1000 -F auid!=-1 -k mount
-a always,exit -F arch=b64 -S adjtimex -S settimeofday -S clock_settime -k time-change
-a always,exit -F arch=b32 -S adjtimex -S settimeofday -S clock_settime -k time-change

-e 2
AUDIT_RULES

cat > "$lacework_logrotate" <<'LACEWORK_LOGROTATE'
/var/log/lacework/datacollector.log {
  daily
  rotate 7
  compress
  missingok
  notifempty
  copytruncate
}
LACEWORK_LOGROTATE

cp --reflink=auto --sparse=always "$source_path" "$work_path"

virt-customize -a "$work_path" \
  --upload "${guest_script}:/tmp/fastvm-security-template.sh" \
  --upload "${fluentbit_conf}:/tmp/fluent-bit.conf" \
  --upload "${audit_rules}:/tmp/telnyx.rules" \
  --upload "${lacework_logrotate}:/tmp/lacework-logrotate" \
  --run-command "chmod 0755 /tmp/fastvm-security-template.sh && FASTVM_TEMPLATE='${TARGET_TEMPLATE}' FASTVM_ENV='${FASTVM_ENV}' FASTVM_CONSUL_DATACENTER='${FASTVM_CONSUL_DATACENTER}' FASTVM_SQUAD='${FASTVM_SQUAD}' LACEWORK_GPG_KEY_ID='${LACEWORK_GPG_KEY_ID}' LACEWORK_REPO_RELEASE='${LACEWORK_REPO_RELEASE}' FLUENTBIT_REPO_RELEASE='${FLUENTBIT_REPO_RELEASE}' /tmp/fastvm-security-template.sh"

virt-customize -a "$work_path" \
  --run-command "dpkg-query -W falcon-sensor auditd lacework fluent-bit >/dev/null" \
  --run-command "test -x /opt/CrowdStrike/falconctl" \
  --run-command "test -f /etc/lacework/config.json && test -f /var/lib/lacework/config/commandline.conf && test -f /lib/systemd/system/datacollector.service && test -f /etc/systemd/system/lacework.service" \
  --run-command "test -f /etc/audit/rules.d/telnyx.rules && test -f /lib/systemd/system/auditd.service" \
  --run-command "test -f /etc/fluent-bit/fluent-bit.conf && test -f /lib/systemd/system/fluent-bit.service"

# virt-customize seeds machine-id before running commands. Sanitize it after
# the verification pass so cloned VMs generate their own identity at boot.
virt-customize -a "$work_path" \
  --run-command "truncate -s 0 /etc/machine-id; rm -f /var/lib/dbus/machine-id"

if [ -f "$target_path" ]; then
  cp --reflink=auto --sparse=always "$target_path" "$backup_path"
  if [ -f "$target_env_path" ]; then
    cp --reflink=auto --sparse=always "$target_env_path" "$backup_env_path"
  fi
fi

if [ -f "$source_env_path" ]; then
  grep -Ev '^(TEMPLATE|CREATED_FROM|CREATED_AT|SOURCE_ROOTFS|SECURITY_|FASTVM_|LACEWORK_|FLUENTBIT_)=' "$source_env_path" > "$tmp_env" || true
else
  : > "$tmp_env"
fi

{
  printf "TEMPLATE=%s\n" "$TARGET_TEMPLATE"
  printf "CREATED_FROM=%s\n" "$SOURCE_TEMPLATE"
  printf "CREATED_AT=%s\n" "$stamp"
  printf "SECURITY_TEMPLATE=1\n"
  printf "SECURITY_FEATURES=crowdstrike,lacework,auditd,fluent-bit\n"
  printf "FASTVM_ENV=%s\n" "$FASTVM_ENV"
  printf "FASTVM_CONSUL_DATACENTER=%s\n" "$FASTVM_CONSUL_DATACENTER"
  printf "FASTVM_SQUAD=%s\n" "$FASTVM_SQUAD"
  printf "LACEWORK_REPO_RELEASE=%s\n" "$LACEWORK_REPO_RELEASE"
  printf "FLUENTBIT_REPO_RELEASE=%s\n" "$FLUENTBIT_REPO_RELEASE"
} >> "$tmp_env"

mv "$work_path" "$target_path"
chmod 0644 "$target_path"
install -m 0644 "$tmp_env" "$target_env_path"

trap - EXIT
rm -rf "$tmp_dir"

echo "template=${target_path}"
if [ -f "$backup_path" ]; then
  echo "backup=${backup_path}"
fi
if [ -f "$backup_env_path" ]; then
  echo "env_backup=${backup_env_path}"
fi
echo "source=${source_path}"
echo "features=crowdstrike,lacework,auditd,fluent-bit"
