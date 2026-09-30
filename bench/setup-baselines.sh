#!/usr/bin/env bash
# Installs the container runtimes the benchmarks compare against, pinned and
# checksum-verified, and builds an OCI bundle from the same Ubuntu root file
# system the guests use, so baseline and sandbox run identical user space.
#
# Usage: sudo bash bench/setup-baselines.sh
set -euo pipefail

runc_version=v1.5.2
runc_sha256=599f6f94ff8c5057241eff0d54c3c74f95c34935b6457b33fe545defc61e9488
gvisor_release=20260921.0
gvisor_sha512=7c899979bed334f0987888c41e545cede8e1f7e67a257978c8de244c3867e32bc30d08491e9af36720273c502acd01d8d86c9844ebcbe125790c43fe6bdf563e

base_rootfs=/var/lib/roundhouse/images/base/rootfs.ext4
bundle=/var/lib/roundhouse/bench/bundle

if [[ $EUID -ne 0 ]]; then
  echo "setup-baselines: run as root" >&2
  exit 1
fi

work=$(mktemp -d)
mnt="$work/mnt"
# shellcheck disable=SC2317 # invoked through the EXIT trap
cleanup() {
  if mountpoint -q "$mnt"; then umount "$mnt" || true; fi
  rm -rf "$work"
}
trap cleanup EXIT

if ! /usr/local/bin/runc --version 2>/dev/null | grep -q "runc version ${runc_version#v}"; then
  curl -fsSL -o "$work/runc" "https://github.com/opencontainers/runc/releases/download/${runc_version}/runc.amd64"
  echo "${runc_sha256}  $work/runc" | sha256sum -c --quiet
  install -m 0755 "$work/runc" /usr/local/bin/runc
fi
echo "runc: $(/usr/local/bin/runc --version | head -1)"

# runsc runs its sentry and helpers from gvisor-bin/ beside it.
if ! /usr/local/bin/runsc --version 2>/dev/null | grep -q "release-${gvisor_release}" || [[ ! -x /usr/local/bin/gvisor-bin/gvisor_sentry ]]; then
  curl -fsSL -o "$work/gvisor.tar.bz2" "https://storage.googleapis.com/gvisor/releases/release/${gvisor_release}/x86_64/gvisor.tar.bz2"
  echo "${gvisor_sha512}  $work/gvisor.tar.bz2" | sha512sum -c --quiet
  mkdir "$work/gvisor"
  tar -xjf "$work/gvisor.tar.bz2" -C "$work/gvisor"
  install -m 0755 "$work/gvisor/runsc" /usr/local/bin/runsc
  install -d -m 0755 /usr/local/bin/gvisor-bin
  install -m 0755 "$work"/gvisor/gvisor-bin/* /usr/local/bin/gvisor-bin/
fi
echo "runsc: $(/usr/local/bin/runsc --version | head -1)"

if [[ ! -f $bundle/config.json ]]; then
  mkdir -p "$mnt" "$bundle/rootfs"
  mount -o loop,ro "$base_rootfs" "$mnt"
  cp -a "$mnt/." "$bundle/rootfs/"
  umount "$mnt"
  (cd "$bundle" && /usr/local/bin/runc spec)
fi
echo "bundle: $bundle"
