# Architecture decision records

Each record states one decision, the context that forced it, and its
consequences. Records are immutable once accepted; a changed decision gets a
new record that supersedes the old one.

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-firecracker-microvms.md) | Firecracker microVMs for isolation | Accepted |
| [0002](0002-go.md) | Go for the control plane and the guest agent | Accepted |
| [0003](0003-snapshot-restore-and-warm-pool.md) | Snapshot restore and a warm pool for starts | Accepted |
| [0004](0004-http-over-vsock.md) | HTTP/1.1 over vsock for the agent protocol | Accepted |
| [0005](0005-network-namespace-per-sandbox.md) | One network namespace per sandbox, identical guest addressing | Accepted |
| [0006](0006-storage.md) | Shared read-only rootfs and a per-sandbox scratch disk | Accepted |
| [0007](0007-single-host.md) | Single host in v1, with a node boundary kept | Accepted |
| [0008](0008-wsl2-development-host.md) | Develop on WSL2 with nested KVM | Accepted |
