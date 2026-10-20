#!/usr/bin/env bash
set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-/opt/fastvm-microvm}"
ROOTFS_SIZE="${ROOTFS_SIZE:-32G}"
RELEASE_URL="${RELEASE_URL:-https://github.com/firecracker-microvm/firecracker/releases}"

if [ "$(id -u)" -ne 0 ]; then
  echo "fc-prepare-host must run as root" >&2
  exit 1
fi

for cmd in curl wget tar grep sort unsquashfs ssh-keygen truncate chown; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "missing required command: $cmd" >&2
    exit 1
  fi
done

MKFS_EXT4="${MKFS_EXT4:-$(command -v mkfs.ext4 || command -v /sbin/mkfs.ext4 || true)}"
E2FSCK="${E2FSCK:-$(command -v e2fsck || command -v /sbin/e2fsck || true)}"
if [ -z "$MKFS_EXT4" ] || [ -z "$E2FSCK" ]; then
  echo "missing mkfs.ext4/e2fsck" >&2
  exit 1
fi

ARCH="$(uname -m)"
latest="$(basename "$(curl -fsSLI -o /dev/null -w '%{url_effective}' "${RELEASE_URL}/latest")")"
ci_version="${latest%.*}"

mkdir -p "${INSTALL_DIR}/bin" "${INSTALL_DIR}/images" "${INSTALL_DIR}/instances"
workdir="$(mktemp -d)"
cleanup() {
  rm -rf "$workdir"
}
trap cleanup EXIT

echo "Installing Firecracker ${latest} for ${ARCH}"
curl -fsSL "${RELEASE_URL}/download/${latest}/firecracker-${latest}-${ARCH}.tgz" -o "${workdir}/firecracker.tgz"
tar -xzf "${workdir}/firecracker.tgz" -C "$workdir"
release_dir="${workdir}/release-${latest}-${ARCH}"
install -m 0755 "${release_dir}/firecracker-${latest}-${ARCH}" "${INSTALL_DIR}/bin/firecracker"
if [ -f "${release_dir}/jailer-${latest}-${ARCH}" ]; then
  install -m 0755 "${release_dir}/jailer-${latest}-${ARCH}" "${INSTALL_DIR}/bin/jailer"
fi

kernel="$(find "${INSTALL_DIR}/images" -maxdepth 1 -name 'vmlinux-*' | sort -V | tail -1 || true)"
rootfs="${INSTALL_DIR}/images/ubuntu.ext4"
ssh_key="${INSTALL_DIR}/images/id_rsa"

if [ -z "$kernel" ]; then
  echo "Downloading Firecracker CI kernel for ${ci_version}/${ARCH}"
  kernel_key="$(
    curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/?prefix=firecracker-ci/${ci_version}/${ARCH}/vmlinux-&list-type=2" |
      grep -oE "firecracker-ci/${ci_version}/${ARCH}/vmlinux-[0-9]+\\.[0-9]+\\.[0-9]{1,3}" |
      sort -V |
      tail -1
  )"
  if [ -z "$kernel_key" ]; then
    echo "could not find Firecracker CI kernel for ${ci_version}/${ARCH}" >&2
    exit 1
  fi
  wget -q -O "${INSTALL_DIR}/images/$(basename "$kernel_key")" "https://s3.amazonaws.com/spec.ccfc.min/${kernel_key}"
  kernel="${INSTALL_DIR}/images/$(basename "$kernel_key")"
fi

if [ ! -f "$rootfs" ]; then
  echo "Downloading and preparing Ubuntu rootfs"
  ubuntu_key="$(
    curl -fsSL "https://s3.amazonaws.com/spec.ccfc.min/?prefix=firecracker-ci/${ci_version}/${ARCH}/ubuntu-&list-type=2" |
      grep -oE "firecracker-ci/${ci_version}/${ARCH}/ubuntu-[0-9]+\\.[0-9]+\\.squashfs" |
      sort -V |
      tail -1
  )"
  if [ -z "$ubuntu_key" ]; then
    echo "could not find Firecracker CI Ubuntu rootfs for ${ci_version}/${ARCH}" >&2
    exit 1
  fi

  wget -q -O "${workdir}/ubuntu.squashfs" "https://s3.amazonaws.com/spec.ccfc.min/${ubuntu_key}"
  unsquashfs -q -d "${workdir}/squashfs-root" "${workdir}/ubuntu.squashfs"
  mkdir -p "${workdir}/squashfs-root/root/.ssh"
  ssh-keygen -q -f "${workdir}/id_rsa" -N ""
  cp "${workdir}/id_rsa.pub" "${workdir}/squashfs-root/root/.ssh/authorized_keys"
  chmod 700 "${workdir}/squashfs-root/root/.ssh"
  chmod 600 "${workdir}/squashfs-root/root/.ssh/authorized_keys"

  chown -R root:root "${workdir}/squashfs-root"
  truncate -s "$ROOTFS_SIZE" "$rootfs"
  "$MKFS_EXT4" -q -d "${workdir}/squashfs-root" -F "$rootfs"
  mv "${workdir}/id_rsa" "$ssh_key"
  chmod 600 "$ssh_key"
fi

"$E2FSCK" -fn "$rootfs" >/dev/null

echo "Firecracker: ${INSTALL_DIR}/bin/firecracker"
echo "Jailer: ${INSTALL_DIR}/bin/jailer"
echo "Kernel: ${kernel}"
echo "Rootfs: ${rootfs}"
echo "SSH key: ${ssh_key}"
