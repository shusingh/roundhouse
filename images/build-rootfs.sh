#!/usr/bin/env bash
# Builds a Roundhouse guest image from the base root filesystem: installs the
# guest agent as the guest's init and creates a pristine scratch disk.
#
# Output, in OUT_DIR:
#   rootfs.ext4   read-only root filesystem with /sbin/roundhouse-agent
#   scratch.ext4  empty ext4 image, sparse; each sandbox gets a copy as its
#                 writable layer (docs/adr/0006-storage.md)
#   vmlinux       hard link to the base kernel
#
# Usage: sudo bash images/build-rootfs.sh AGENT_BINARY [OUT_DIR] [SCRATCH_SIZE]
#   AGENT_BINARY  static roundhouse-agent (make build puts it in bin/)
#   OUT_DIR       defaults to /var/lib/roundhouse/images/default
#   SCRATCH_SIZE  defaults to 2G; the per-sandbox disk limit
set -euo pipefail

base=/var/lib/roundhouse/images/base
agent=${1:?usage: build-rootfs.sh AGENT_BINARY [OUT_DIR] [SCRATCH_SIZE]}
out=${2:-/var/lib/roundhouse/images/default}
scratch_size=${3:-2G}

if [[ $EUID -ne 0 ]]; then
  echo "build-rootfs: run as root; the image is loop-mounted to install the agent" >&2
  exit 1
fi
if ! file "$agent" | grep -q 'statically linked'; then
  echo "build-rootfs: $agent must be a static binary (build with CGO_ENABLED=0)" >&2
  exit 1
fi

install -d -m 0755 "$out"
work=$(mktemp -d)
mnt="$work/mnt"
mkdir "$mnt"

# shellcheck disable=SC2317 # invoked through the EXIT trap
cleanup() {
  if mountpoint -q "$mnt"; then umount "$mnt" || true; fi
  rm -rf "$work"
}
trap cleanup EXIT

cp --sparse=always "$base/rootfs.ext4" "$work/rootfs.ext4"
mount -o loop "$work/rootfs.ext4" "$mnt"
install -m 0755 -o root -g root "$agent" "$mnt/sbin/roundhouse-agent"
# The agent mounts the scratch disk on /mnt and builds the overlay under it.
install -d -m 0755 "$mnt/mnt" "$mnt/root"
umount "$mnt"
e2fsck -fy "$work/rootfs.ext4" >/dev/null || [[ $? -le 1 ]]
chmod 0444 "$work/rootfs.ext4"
mv -f "$work/rootfs.ext4" "$out/rootfs.ext4"

truncate -s "$scratch_size" "$work/scratch.ext4"
mkfs.ext4 -q -F -L rh-scratch "$work/scratch.ext4"
chmod 0444 "$work/scratch.ext4"
mv -f "$work/scratch.ext4" "$out/scratch.ext4"

ln -f "$(readlink -f "$base/vmlinux")" "$out/vmlinux"
ls -ls "$out"
