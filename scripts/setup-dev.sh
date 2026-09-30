#!/usr/bin/env bash
# Prepares a fresh clone for development: installs the repository's git hooks
# and reports whether the required tools are available.
set -euo pipefail

repo_root=$(git rev-parse --show-toplevel)
cd "$repo_root"

git config core.hooksPath scripts/githooks
chmod +x scripts/githooks/*
echo "git hooks: installed (core.hooksPath=scripts/githooks)"

required_go="1.27"
status=0

check() {
  local name=$1
  shift
  if command -v "$name" >/dev/null 2>&1; then
    echo "$name: $("$@" 2>&1 | head -n 1)"
  else
    echo "$name: MISSING" >&2
    status=1
  fi
}

check go go version
check golangci-lint golangci-lint version
check make make --version

if command -v go >/dev/null 2>&1; then
  have=$(go env GOVERSION | sed 's/^go//')
  if [[ "$(printf '%s\n%s\n' "$required_go" "$have" | sort -V | head -n 1)" != "$required_go" ]]; then
    echo "go: version $have is older than required $required_go" >&2
    status=1
  fi
fi

exit $status
