package shared

// ─── Image types (from images.go) ───

type ImageCreateRequest struct {
	ImageSizeMB int `json:"image_size_mb"`
}

type ImageCreateResponse struct {
	LoopDevice     string `json:"loop_device"`
	ImagePath      string `json:"image_path"`
	AlreadyExisted bool   `json:"already_existed"`
}

// ─── DRBD types (from drbd.go) ───

type DRBDNode struct {
	Hostname string `json:"hostname"`
	Minor    int    `json:"minor"`
	Disk     string `json:"disk"`
	Address  string `json:"address"`
}

type DRBDCreateRequest struct {
	ResourceName string     `json:"resource_name"`
	Nodes        []DRBDNode `json:"nodes"`
	Port         int        `json:"port"`
}

type DRBDCreateResponse struct {
	AlreadyExisted bool `json:"already_existed"`
}

type DRBDPromoteResponse struct {
	OK             bool `json:"ok,omitempty"`
	AlreadyExisted bool `json:"already_existed,omitempty"`
}

type DRBDDemoteResponse struct {
	OK             bool `json:"ok,omitempty"`
	AlreadyExisted bool `json:"already_existed,omitempty"`
}

type DRBDStatusResponse struct {
	Resource        string  `json:"resource"`
	Role            string  `json:"role"`
	ConnectionState string  `json:"connection_state"`
	DiskState       string  `json:"disk_state"`
	PeerDiskState   string  `json:"peer_disk_state"`
	SyncProgress    *string `json:"sync_progress"`
	Exists          bool    `json:"exists"`
}

// ─── Btrfs types (from btrfs.go) ───

type FormatBtrfsResponse struct {
	AlreadyFormatted bool `json:"already_formatted"`
}

// ─── Container types (from containers.go) ───

type ContainerStartResponse struct {
	ContainerName  string `json:"container_name"`
	AlreadyExisted bool   `json:"already_existed"`
}

type ContainerStatusResponse struct {
	Exists        bool   `json:"exists"`
	Running       bool   `json:"running"`
	ContainerName string `json:"container_name,omitempty"`
	StartedAt     string `json:"started_at,omitempty"`
}

// ─── Status types (from server.go) ───

type StatusResponse struct {
	MachineID   string                    `json:"machine_id"`
	DiskTotalMB int64                     `json:"disk_total_mb"`
	DiskUsedMB  int64                     `json:"disk_used_mb"`
	RAMTotalMB  int64                     `json:"ram_total_mb"`
	RAMUsedMB   int64                     `json:"ram_used_mb"`
	Users       map[string]*UserStatusDTO `json:"users"`
}

// ─── Fleet types (from heartbeat.go / coordinator) ───

type FleetRegisterRequest struct {
	MachineID   string `json:"machine_id"`
	Address     string `json:"address"`
	DiskTotalMB int64  `json:"disk_total_mb"`
	DiskUsedMB  int64  `json:"disk_used_mb"`
	RAMTotalMB  int64  `json:"ram_total_mb"`
	RAMUsedMB   int64  `json:"ram_used_mb"`
	MaxAgents   int    `json:"max_agents"`
}

type FleetHeartbeatRequest struct {
	MachineID     string   `json:"machine_id"`
	DiskTotalMB   int64    `json:"disk_total_mb"`
	DiskUsedMB    int64    `json:"disk_used_mb"`
	RAMTotalMB    int64    `json:"ram_total_mb"`
	RAMUsedMB     int64    `json:"ram_used_mb"`
	ActiveAgents  int      `json:"active_agents"`
	RunningAgents []string `json:"running_agents"`
}

// ─── Coordinator API types ───

type CreateUserRequest struct {
	UserID      string `json:"user_id"`
	ImageSizeMB int    `json:"image_size_mb,omitempty"`
}

type CreateUserResponse struct {
	UserID string `json:"user_id"`
	Status string `json:"status"`
}

type UserDetailResponse struct {
	UserID         string       `json:"user_id"`
	Status         string       `json:"status"`
	PrimaryMachine string       `json:"primary_machine"`
	DRBDPort       int          `json:"drbd_port"`
	Error          string       `json:"error,omitempty"`
	Bipod          []BipodEntry `json:"bipod"`
}

type BipodEntry struct {
	MachineID  string `json:"machine_id"`
	Role       string `json:"role"`
	DRBDMinor  int    `json:"drbd_minor"`
	LoopDevice string `json:"loop_device"`
}

type FleetStatusResponse struct {
	Machines []MachineStatus `json:"machines"`
}

type MachineStatus struct {
	MachineID     string   `json:"machine_id"`
	Address       string   `json:"address"`
	Status        string   `json:"status"`
	DiskTotalMB   int64    `json:"disk_total_mb"`
	DiskUsedMB    int64    `json:"disk_used_mb"`
	RAMTotalMB    int64    `json:"ram_total_mb"`
	RAMUsedMB     int64    `json:"ram_used_mb"`
	ActiveAgents  int      `json:"active_agents"`
	MaxAgents     int      `json:"max_agents"`
	RunningAgents []string `json:"running_agents"`
	LastHeartbeat string   `json:"last_heartbeat"`
}

// ─── DRBD disconnect/reconfigure types (from drbd.go, Layer 4.4) ───

type DRBDDisconnectResponse struct {
	Status       string `json:"status"`
	WasConnected bool   `json:"was_connected"`
}

type DRBDReconfigureRequest struct {
	Nodes []DRBDNode `json:"nodes"`
	Port  int        `json:"port"`
	Force bool       `json:"force"` // false=adjust only, true=down/up/promote
}

type DRBDReconfigureResponse struct {
	Status string `json:"status"` // "reconfigured"
	Method string `json:"method"` // "adjust" or "down_up"
}

// ─── Reformation types (from coordinator reformer) ───

type ReformationEventResponse struct {
	UserID       string `json:"user_id"`
	OldSecondary string `json:"old_secondary"`
	NewSecondary string `json:"new_secondary"`
	Success      bool   `json:"success"`
	Error        string `json:"error,omitempty"`
	Method       string `json:"method,omitempty"` // "adjust" or "down_up"
	DurationMS   int64  `json:"duration_ms"`
	Timestamp    string `json:"timestamp"`
}

// ─── Failover types (from coordinator healthcheck) ───

type FailoverEventResponse struct {
	UserID      string `json:"user_id"`
	FromMachine string `json:"from_machine"`
	ToMachine   string `json:"to_machine"`
	Type        string `json:"type"`
	Success     bool   `json:"success"`
	Error       string `json:"error,omitempty"`
	DurationMS  int64  `json:"duration_ms"`
	Timestamp   string `json:"timestamp"`
}

type UserStatusDTO struct {
	ImageExists      bool   `json:"image_exists"`
	ImagePath        string `json:"image_path"`
	LoopDevice       string `json:"loop_device"`
	DRBDResource     string `json:"drbd_resource"`
	DRBDMinor        int    `json:"drbd_minor"`
	DRBDDevice       string `json:"drbd_device"`
	DRBDRole         string `json:"drbd_role"`
	DRBDConnection   string `json:"drbd_connection"`
	DRBDDiskState    string `json:"drbd_disk_state"`
	DRBDPeerDisk     string `json:"drbd_peer_disk_state"`
	HostMounted      bool   `json:"host_mounted"`
	ContainerRunning bool   `json:"container_running"`
	ContainerName    string `json:"container_name"`
}
