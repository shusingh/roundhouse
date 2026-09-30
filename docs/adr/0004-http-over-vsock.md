# ADR 0004: HTTP/1.1 over vsock for the agent protocol

- **Status:** Accepted
- **Date:** 2026-09-29

## Context

The host must run commands in the guest, stream their output, and move files.
The channel should not depend on guest networking, which may be disabled by
policy. Options:

- **Serial console**: no multiplexing, fragile framing, slow.
- **SSH over the guest network**: requires networking and key management in
  every guest, and a much larger daemon.
- **A custom framed protocol over vsock**: small, but everything (streaming,
  cancellation, large bodies, errors) is reinvented and needs its own tools.
- **gRPC or Connect over vsock** (what E2B uses): strong typing and streaming,
  at the cost of code generation and extra dependencies in a minimal agent.
- **HTTP/1.1 over vsock**: standard library on both sides, chunked streaming,
  bodies of any size, cancellation by closing the connection, and testable
  with ordinary HTTP tests.

## Decision

The agent serves HTTP/1.1 on a vsock port. The host dials Firecracker's vsock
Unix socket, sends `CONNECT <port>\n`, waits for `OK`, and uses the connection
as an HTTP transport. Streaming responses (exec output) are newline-delimited
JSON frames with a bounded size. Wire types live in `internal/agentproto`.

## Consequences

- No generated code and no extra dependencies in the guest.
- Every connection pays one handshake; the host keeps connections alive per
  sandbox and redials after a restore, because Firecracker resets vsock
  connections across snapshots.
- Frames are JSON with base64 output chunks: simple and debuggable, at some
  encoding overhead that the exec benchmark measures.
