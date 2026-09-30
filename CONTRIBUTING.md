# Contributing to Roundhouse

## Prerequisites

| Tool | Version | Notes |
|---|---|---|
| Go | 1.27 or newer | Matches the `go` directive in `go.mod` |
| golangci-lint | v2 | `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest` |
| GNU Make | 4.x | On Windows, install with `scoop install make` and use Git Bash |
| Linux with KVM | cgroup v2 | Only for integration and containment tests; WSL2 with nested virtualization works |

Unit tests run anywhere. Code that needs Linux lives in `*_linux.go` files,
and tests that boot microVMs carry the `integration` or `containment` build
tag and skip with a message when KVM or root is unavailable.

## First-time setup

```bash
git clone https://github.com/shusingh/roundhouse.git
cd roundhouse
make setup
make host-setup                 # Linux or WSL2: packages, KVM access, Firecracker
sudo bash images/fetch-base.sh  # guest kernel and root file system
```

`make setup` points git at the repository's hooks (`scripts/githooks/`) and
checks that the toolchain is present. `deploy/smoke-boot.sh` boots one microVM
directly through Firecracker to confirm the host works.

## Everyday commands

| Command | What it does |
|---|---|
| `make build` | Build the Linux binaries into `./bin` |
| `make test` | Portable unit tests with the race detector |
| `make test-linux` | All unit tests, including Linux-only code (in WSL on Windows) |
| `make integration` | Tests that boot real microVMs |
| `make containment` | The hostile-workload containment suite |
| `make lint` | golangci-lint for this platform and for Linux |
| `make check` | Everything CI runs; run before every push |
| `make help` | List all targets |

## Repository layout

See [design.md section 17](docs/engineering/design.md#17-repository-layout).
Binaries live in `cmd/`, implementation in `internal/`, tests next to the code
they test, and real-VM suites in `test/`.

## Engineering documents

| Document | Purpose |
|---|---|
| [Design](docs/engineering/design.md) | Architecture, threat model, and benchmark method |
| [Coding standards](docs/engineering/coding-standards.md) | How code in this repo is written and reviewed |
| [ADRs](docs/adr/README.md) | Architecture decisions and their rationale |

## Making a change

1. Branch from `main`: `git switch -c feat/short-topic`.
2. Keep each commit to one logical change that builds and passes tests.
3. Follow the [coding standards](docs/engineering/coding-standards.md); section
   17 is the self-review checklist.
4. Write commit messages in the Conventional Commits style described in
   [coding standards section 16](docs/engineering/coding-standards.md#16-git-and-commits).
5. Run `make check` before pushing.
6. If a change alters architecture or behavior, update the design doc in the
   same commit, and record significant decisions as a new ADR.
