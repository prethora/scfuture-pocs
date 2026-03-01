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
