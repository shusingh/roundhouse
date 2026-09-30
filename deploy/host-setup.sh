#!/usr/bin/env bash
# Prepares a Linux host (or WSL2 with nested KVM) to run Roundhouse:
# system packages, the Go toolchain, KVM access for the invoking user,
# Firecracker and the jailer, and the data directory.
#
# Run as root: `sudo bash deploy/host-setup.sh`, or `make host-setup`.
# Every download is verified against a pinned SHA-256 checksum. The script is
# idempotent; rerunning it only repairs what is missing.
set -euo pipefail

FIRECRACKER_VERSION=v1.17.0
FIRECRACKER_SHA256=06094a1108ae9e82aa4c23a775aa92758f53f1175d422270d9d6162cb9ade558
GO_VERSION=1.27.1
GO_SHA256=63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445
GOLANGCI_LINT_VERSION=v2.14.0
DATA_DIR=/var/lib/roundhouse

if [[ $EUID -ne 0 ]]; then
  echo "host-setup: run as root (sudo bash $0)" >&2
  exit 1
fi
if [[ $(uname -m) != x86_64 ]]; then
  echo "host-setup: only x86_64 hosts are supported" >&2
  exit 1
fi
if [[ ! -c /dev/kvm ]]; then
  echo "host-setup: /dev/kvm is missing; enable virtualization (nested, on WSL2) first" >&2
  exit 1
fi

target_user=${SUDO_USER:-}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fetch() {
  local url=$1 out=$2 sum=$3
  curl -fsSL --retry 3 -o "$out" "$url"
  echo "$sum  $out" | sha256sum --check --quiet -
}

echo "==> system packages"
apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
  nftables iproute2 e2fsprogs squashfs-tools curl ca-certificates acl \
  build-essential >/dev/null # build-essential: a C compiler for the race detector

echo "==> Go ${GO_VERSION}"
if [[ "$(/usr/local/go/bin/go env GOVERSION 2>/dev/null)" != "go${GO_VERSION}" ]]; then
  fetch "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" "$tmp/go.tgz" "$GO_SHA256"
  rm -rf /usr/local/go
  tar -C /usr/local -xzf "$tmp/go.tgz"
fi
cat >/etc/profile.d/go.sh <<'PROFILE'
export PATH=/usr/local/go/bin:$HOME/go/bin:$PATH
PROFILE

echo "==> golangci-lint ${GOLANGCI_LINT_VERSION}"
if [[ -n "$target_user" ]]; then
  sudo -u "$target_user" -H env PATH=/usr/local/go/bin:"$PATH" \
    go install "github.com/golangci/golangci-lint/v2/cmd/golangci-lint@${GOLANGCI_LINT_VERSION}"
fi

echo "==> KVM access"
if [[ -n "$target_user" ]] && ! id -nG "$target_user" | grep -qw kvm; then
  usermod -aG kvm "$target_user"
  echo "    added $target_user to kvm (takes effect in new login sessions)"
fi

echo "==> Firecracker ${FIRECRACKER_VERSION}"
if [[ "$(/usr/local/bin/firecracker --version 2>/dev/null | head -n 1)" != "Firecracker ${FIRECRACKER_VERSION}" ]]; then
  fetch "https://github.com/firecracker-microvm/firecracker/releases/download/${FIRECRACKER_VERSION}/firecracker-${FIRECRACKER_VERSION}-x86_64.tgz" \
    "$tmp/fc.tgz" "$FIRECRACKER_SHA256"
  tar -C "$tmp" -xzf "$tmp/fc.tgz"
  release_dir="$tmp/release-${FIRECRACKER_VERSION}-x86_64"
  install -m 0755 "$release_dir/firecracker-${FIRECRACKER_VERSION}-x86_64" /usr/local/bin/firecracker
  install -m 0755 "$release_dir/jailer-${FIRECRACKER_VERSION}-x86_64" /usr/local/bin/jailer
fi

echo "==> kernel settings"
sysctl -q -w net.ipv4.ip_forward=1
echo 'net.ipv4.ip_forward = 1' >/etc/sysctl.d/90-roundhouse.conf

echo "==> data directory ${DATA_DIR}"
install -d -m 0755 "$DATA_DIR" "$DATA_DIR/images" "$DATA_DIR/jail" "$DATA_DIR/sandboxes"

echo "==> done"
/usr/local/bin/firecracker --version | head -n 1
/usr/local/go/bin/go version
