package vmm

// Request bodies for Firecracker's API. Field names and optionality follow
// the API's swagger definition (src/firecracker/swagger/firecracker.yaml at
// v1.17.0); only the fields Roundhouse uses are declared.

// MachineConfig sizes the VM. Firecracker accepts 1 or an even number of vCPUs.
type MachineConfig struct {
	VCPUCount  int  `json:"vcpu_count"`
	MemSizeMiB int  `json:"mem_size_mib"`
	SMT        bool `json:"smt"`
	// TrackDirtyPages enables diff snapshots; it costs performance, so it is
	// off unless a caller needs diffs.
	TrackDirtyPages bool `json:"track_dirty_pages,omitempty"`
}

// BootSource is the guest kernel and its command line. Paths are relative to
// the jail root when the VMM runs under the jailer.
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
}

// Drive is a virtio block device.
type Drive struct {
	DriveID      string       `json:"drive_id"`
	PathOnHost   string       `json:"path_on_host"`
	IsRootDevice bool         `json:"is_root_device"`
	IsReadOnly   bool         `json:"is_read_only"`
	RateLimiter  *RateLimiter `json:"rate_limiter,omitempty"`
}

// NetworkInterface is a virtio net device backed by a tap device in the VMM's
// network namespace.
type NetworkInterface struct {
	IfaceID       string       `json:"iface_id"`
	HostDevName   string       `json:"host_dev_name"`
	GuestMAC      string       `json:"guest_mac,omitempty"`
	RxRateLimiter *RateLimiter `json:"rx_rate_limiter,omitempty"`
	TxRateLimiter *RateLimiter `json:"tx_rate_limiter,omitempty"`
}

// Vsock is the virtio vsock device. The host side is the Unix socket at
// UDSPath; see docs/adr/0004-http-over-vsock.md.
type Vsock struct {
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// Serial routes the guest's serial console to a host file. The rate limiter
// bounds how fast a guest can grow that file.
type Serial struct {
	OutputPath  string       `json:"serial_out_path"`
	RateLimiter *TokenBucket `json:"rate_limiter,omitempty"`
}

// Logger directs the VMM's own log to a file in the jail.
type Logger struct {
	LogPath       string `json:"log_path"`
	Level         string `json:"level,omitempty"`
	ShowLevel     bool   `json:"show_level,omitempty"`
	ShowLogOrigin bool   `json:"show_log_origin,omitempty"`
}

// Entropy is a virtio-rng device that feeds the guest kernel's random number
// generator from the host, including after a snapshot restore.
type Entropy struct {
	RateLimiter *RateLimiter `json:"rate_limiter,omitempty"`
}

// RateLimiter limits a device by bytes, operations, or both.
type RateLimiter struct {
	Bandwidth *TokenBucket `json:"bandwidth,omitempty"`
	Ops       *TokenBucket `json:"ops,omitempty"`
}

// TokenBucket holds Size tokens and refills completely every RefillTimeMS
// milliseconds. OneTimeBurst adds tokens available once, at start.
type TokenBucket struct {
	Size         int64 `json:"size"`
	OneTimeBurst int64 `json:"one_time_burst,omitempty"`
	RefillTimeMS int64 `json:"refill_time"`
}

// SnapshotCreate writes a paused VM's state and memory to files.
type SnapshotCreate struct {
	SnapshotType string `json:"snapshot_type,omitempty"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

// Snapshot types for SnapshotCreate.
const (
	SnapshotFull = "Full"
	SnapshotDiff = "Diff"
)

// SnapshotLoad restores a VM from snapshot files into a fresh VMM process.
type SnapshotLoad struct {
	SnapshotPath string        `json:"snapshot_path"`
	MemBackend   MemoryBackend `json:"mem_backend"`
	ResumeVM     bool          `json:"resume_vm"`
	// ClockRealtime advances the guest's wall clock by the time the snapshot
	// spent on disk (x86_64 only), so restored guests have the correct time.
	ClockRealtime bool           `json:"clock_realtime,omitempty"`
	VsockOverride *VsockOverride `json:"vsock_override,omitempty"`
}

// MemoryBackend says how guest memory is loaded on restore: mapped from a
// file, or served by a userfaultfd handler.
type MemoryBackend struct {
	BackendType string `json:"backend_type"`
	BackendPath string `json:"backend_path"`
}

// Memory backend types for MemoryBackend.
const (
	BackendFile = "File"
	BackendUffd = "Uffd"
)

// VsockOverride points a restored VM's vsock device at a new host socket.
type VsockOverride struct {
	UDSPath string `json:"uds_path"`
}

// vmState is the body of PATCH /vm.
type vmState struct {
	State string `json:"state"`
}

// instanceAction is the body of PUT /actions.
type instanceAction struct {
	ActionType string `json:"action_type"`
}

// apiFault is the body of an error response.
type apiFault struct {
	FaultMessage string `json:"fault_message"`
}
