#!/usr/bin/env bash
# Downloads the base guest kernel and root filesystem published by the
# Firecracker project's CI, verifies their checksums, and converts the root
# filesystem from squashfs to an ext4 image.
#
# The kernel is Firecracker's CI build of Linux 6.1, which has VMGenID,
# virtio-vsock, virtio-blk, ext4, and overlayfs built in (checked against its
# published .config). See docs/engineering/design.md section 7.3.
#
# Usage: sudo bash images/fetch-base.sh [OUT_DIR]
#   OUT_DIR defaults to /var/lib/roundhouse/images/base.
set -euo pipefail

CI_BASE=https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/x86_64
KERNEL=vmlinux-6.1.155
KERNEL_SHA256=e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2
ROOTFS=ubuntu-24.04.squashfs
ROOTFS_SHA256=68321e0482baeb3844dafe8a6b08a6902401a7afc41fbfd8c3d9ea08aadd244f
ROOTFS_SIZE=1G

out=${1:-/var/lib/roundhouse/images/base}

if [[ $EUID -ne 0 ]]; then
  echo "fetch-base: run as root; unpacking the root filesystem preserves ownership" >&2
  exit 1
fi

install -d -m 0755 "$out"
cd "$out"

fetch() {
  local name=$1 sum=$2
  if [[ -f "$name" ]] && echo "$sum  $name" | sha256sum --check --quiet - 2>/dev/null; then
    return
  fi
  curl -fsSL --retry 3 -o "$name.part" "$CI_BASE/$name"
  echo "$sum  $name.part" | sha256sum --check --quiet -
  mv "$name.part" "$name"
}

fetch "$KERNEL" "$KERNEL_SHA256"
fetch "$ROOTFS" "$ROOTFS_SHA256"
ln -sf "$KERNEL" vmlinux

if [[ ! -f rootfs.ext4 || rootfs.ext4 -ot "$ROOTFS" ]]; then
  work=$(mktemp -d)
  trap 'rm -rf "$work"' EXIT
  unsquashfs -q -d "$work/root" "$ROOTFS" >/dev/null
  truncate -s "$ROOTFS_SIZE" rootfs.ext4.part
  mkfs.ext4 -q -F -d "$work/root" rootfs.ext4.part
  mv rootfs.ext4.part rootfs.ext4
fi

ls -l "$out"
