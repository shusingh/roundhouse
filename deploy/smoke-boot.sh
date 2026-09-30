#!/usr/bin/env bash
# Boots one microVM directly through Firecracker's API socket, without
# Roundhouse, to check that a host can run Firecracker. Prints the time from
# InstanceStart until the guest's first user-space process writes to the
# serial console.
#
# Usage: bash deploy/smoke-boot.sh [IMAGE_DIR]
#   IMAGE_DIR defaults to /var/lib/roundhouse/images/base (see
#   images/fetch-base.sh). Needs read/write access to /dev/kvm.
set -euo pipefail

images=${1:-/var/lib/roundhouse/images/base}
work=$(mktemp -d)
sock="$work/api.sock"
fc_pid=""

# shellcheck disable=SC2317 # invoked through the EXIT trap
cleanup() {
  if [[ -n "$fc_pid" ]]; then
    kill "$fc_pid" 2>/dev/null || true
    wait "$fc_pid" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

api() {
  local method=$1 path=$2 body=$3
  local code
  code=$(curl -s -o "$work/resp" -w '%{http_code}' --unix-socket "$sock" \
    -X "$method" "http://localhost$path" -H 'Content-Type: application/json' -d "$body")
  if [[ $code != 2* ]]; then
    echo "smoke-boot: $method $path -> $code: $(cat "$work/resp")" >&2
    exit 1
  fi
}

# A private copy, so the image is never modified.
cp --sparse=always "$images/rootfs.ext4" "$work/rootfs.ext4"

firecracker --api-sock "$sock" --level Warning >"$work/console.log" 2>&1 &
fc_pid=$!
for _ in $(seq 100); do [[ -S "$sock" ]] && break; sleep 0.01; done

api PUT /machine-config '{"vcpu_count": 1, "mem_size_mib": 256}'
api PUT /boot-source "{\"kernel_image_path\": \"$images/vmlinux\", \"boot_args\": \"console=ttyS0 reboot=k panic=1 pci=off quiet init=/bin/sh -- -c 'echo RH_USERSPACE_UP; exec sleep 3600'\"}"
api PUT /drives/rootfs "{\"drive_id\": \"rootfs\", \"path_on_host\": \"$work/rootfs.ext4\", \"is_root_device\": true, \"is_read_only\": false}"

start=$(date +%s%N)
api PUT /actions '{"action_type": "InstanceStart"}'
for _ in $(seq 1000); do
  if grep -q RH_USERSPACE_UP "$work/console.log"; then
    end=$(date +%s%N)
    echo "boot to user space: $(( (end - start) / 1000000 )) ms"
    exit 0
  fi
  sleep 0.005
done

echo "smoke-boot: guest did not reach user space within 5s; console:" >&2
tail -n 20 "$work/console.log" >&2
exit 1
