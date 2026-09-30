// Package vmm launches and drives Firecracker microVMs.
//
// It has two halves. Client speaks Firecracker's HTTP API over the Unix socket
// the VMM listens on, with request types that mirror the API's JSON schema
// (Firecracker v1.17). Jail prepares a jailer chroot, launches Firecracker
// through the jailer with cgroup, namespace, and uid isolation, watches the
// process, and tears everything down again.
//
// This package and package netns are the only places Roundhouse performs
// privileged host operations; see docs/engineering/coding-standards.md
// section 15.
package vmm
