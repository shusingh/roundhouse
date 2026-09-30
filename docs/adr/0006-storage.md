# ADR 0006: Shared read-only rootfs and a per-sandbox scratch disk

- **Status:** Accepted
- **Date:** 2026-09-29

## Context

Every sandbox needs a writable root file system, and creating one must be fast.
Copying a full root image per sandbox costs time and disk. Copy-on-write at the
host file system level (reflinks) is not available on the development host's
ext4 volume. Device-mapper snapshots work but add a privileged subsystem to
manage and clean up.

## Decision

- Each template has one read-only ext4 root image, shared by its sandboxes
  through hard links into each jail. The guest mounts it read-only.
- Each sandbox gets a writable ext4 scratch image, created by sparse-copying a
  pre-formatted pristine image (fast, since the file is mostly holes). The
  guest mounts it and overlays it on the root with overlayfs.
- The scratch image's size is the sandbox's disk limit.

## Consequences

- Create cost for storage is a sparse copy and hard links, independent of the
  root image's size.
- Snapshots reference jail-relative paths, which are identical in every jail,
  so restored sandboxes find their disks.
- The data directory and jail root must be on one file system for hard links;
  checked at startup.
- A sandbox cannot modify the shared root: writes land in its own scratch disk,
  which is deleted with the sandbox.
