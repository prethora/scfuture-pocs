package machineagent

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

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

type DRBDStatusResponse struct {
	Resource        string  `json:"resource"`
	Role            string  `json:"role"`
	ConnectionState string  `json:"connection_state"`
	DiskState       string  `json:"disk_state"`
	PeerDiskState   string  `json:"peer_disk_state"`
	SyncProgress    *string `json:"sync_progress"`
	Exists          bool    `json:"exists"`
}

type DRBDInfo struct {
	Role            string
	ConnectionState string
	DiskState       string
	PeerDiskState   string
	SyncProgress    *string
}

func (a *Agent) DRBDCreate(userID string, req *DRBDCreateRequest) (*DRBDCreateResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	if len(req.Nodes) != 2 {
		return nil, fmt.Errorf("exactly 2 nodes required")
	}

	resName := req.ResourceName

	// Check if already exists
	_, err := runCmd("drbdadm", "status", resName)
	if err == nil {
		slog.Info("DRBD resource already exists", "component", "drbd", "user", userID, "resource", resName)
		return &DRBDCreateResponse{AlreadyExisted: true}, nil
	}

	// Write config file
	configPath := fmt.Sprintf("/etc/drbd.d/%s.res", resName)
	config := fmt.Sprintf(`resource %s {
    net {
        protocol A;
        max-buffers 8000;
        max-epoch-size 8000;
        sndbuf-size 0;
        rcvbuf-size 0;
    }
    disk {
        on-io-error detach;
    }
    on %s {
        device /dev/drbd%d minor %d;
        disk %s;
        address %s:%d;
        meta-disk internal;
    }
    on %s {
        device /dev/drbd%d minor %d;
        disk %s;
        address %s:%d;
        meta-disk internal;
    }
}
`, resName,
		req.Nodes[0].Hostname, req.Nodes[0].Minor, req.Nodes[0].Minor, req.Nodes[0].Disk, req.Nodes[0].Address, req.Port,
		req.Nodes[1].Hostname, req.Nodes[1].Minor, req.Nodes[1].Minor, req.Nodes[1].Disk, req.Nodes[1].Address, req.Port,
	)

	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		return nil, fmt.Errorf("write DRBD config: %w", err)
	}

	// Create metadata
	result, err := runCmd("drbdadm", "create-md", "--force", resName)
	if err != nil {
		return nil, cmdError("drbdadm create-md failed", cmdString("drbdadm", "create-md", "--force", resName), result)
	}

	// Bring up
	result, err = runCmd("drbdadm", "up", resName)
	if err != nil {
		return nil, cmdError("drbdadm up failed", cmdString("drbdadm", "up", resName), result)
	}

	// Update in-memory state
	hostname, _ := os.Hostname()
	u := a.getUser(userID)
	if u == nil {
		u = &UserResources{}
	}
	u.DRBDResource = resName
	for _, n := range req.Nodes {
		if n.Hostname == hostname {
			u.DRBDMinor = n.Minor
			u.DRBDDevice = fmt.Sprintf("/dev/drbd%d", n.Minor)
		}
	}
	a.setUser(userID, u)

	slog.Info("DRBD resource created", "component", "drbd", "user", userID, "resource", resName)
	return &DRBDCreateResponse{}, nil
}

func (a *Agent) DRBDPromote(userID string) (map[string]interface{}, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	resName := "user-" + userID

	// Check current role
	info := a.getDRBDStatus(resName)
	if info != nil && info.Role == "Primary" {
		slog.Info("DRBD already Primary", "component", "drbd", "user", userID)
		return map[string]interface{}{"already_existed": true}, nil
	}

	result, err := runCmd("drbdadm", "primary", "--force", resName)
	if err != nil {
		return nil, cmdError("drbdadm primary failed", cmdString("drbdadm", "primary", "--force", resName), result)
	}

	slog.Info("DRBD promoted to Primary", "component", "drbd", "user", userID)
	return map[string]interface{}{"ok": true}, nil
}

func (a *Agent) DRBDDemote(userID string) (map[string]interface{}, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	resName := "user-" + userID

	// Check current role
	info := a.getDRBDStatus(resName)
	if info != nil && info.Role == "Secondary" {
		slog.Info("DRBD already Secondary", "component", "drbd", "user", userID)
		return map[string]interface{}{"already_existed": true}, nil
	}

	// Safety: unmount host if mounted
	mountPath := a.mountPath(userID)
	if isMounted(mountPath) {
		result, err := runCmd("umount", mountPath)
		if err != nil {
			return nil, cmdError("umount failed before demote", cmdString("umount", mountPath), result)
		}
	}

	result, err := runCmd("drbdadm", "secondary", resName)
	if err != nil {
		return nil, cmdError("drbdadm secondary failed", cmdString("drbdadm", "secondary", resName), result)
	}

	slog.Info("DRBD demoted to Secondary", "component", "drbd", "user", userID)
	return map[string]interface{}{"ok": true}, nil
}

func (a *Agent) DRBDStatus(userID string) (*DRBDStatusResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	resName := "user-" + userID
	info := a.getDRBDStatus(resName)
	if info == nil {
		return &DRBDStatusResponse{Exists: false}, nil
	}

	return &DRBDStatusResponse{
		Resource:        resName,
		Role:            info.Role,
		ConnectionState: info.ConnectionState,
		DiskState:       info.DiskState,
		PeerDiskState:   info.PeerDiskState,
		SyncProgress:    info.SyncProgress,
		Exists:          true,
	}, nil
}

func (a *Agent) DRBDDestroy(userID string) error {
	resName := "user-" + userID

	// Down (ignore errors — may already be down)
	runCmd("drbdadm", "down", resName)

	// Remove config
	configPath := fmt.Sprintf("/etc/drbd.d/%s.res", resName)
	os.Remove(configPath)

	// Update state
	u := a.getUser(userID)
	if u != nil {
		u.DRBDResource = ""
		u.DRBDMinor = 0
		u.DRBDDevice = ""
		u.DRBDRole = ""
		u.DRBDConnection = ""
		u.DRBDDiskState = ""
		u.DRBDPeerDisk = ""
		a.setUser(userID, u)
	}

	slog.Info("DRBD resource destroyed", "component", "drbd", "user", userID)
	return nil
}

func (a *Agent) getDRBDStatus(resName string) *DRBDInfo {
	result, err := runCmd("drbdadm", "status", resName)
	if err != nil {
		return nil
	}
	resources := parseDRBDStatusAll(result.Stdout)
	return resources[resName]
}

// parseDRBDStatusAll parses multi-resource drbdadm status output.
// Handles Connected, Syncing, Disconnected, and StandAlone formats.
//
// DRBD 9 status lines contain space-separated key:value tokens, e.g.:
//   user-alice role:Primary
//     disk:UpToDate open:no
//     machine-2 role:Secondary
//       peer-disk:UpToDate
//   or: replication:SyncSource peer-disk:Inconsistent done:45.20
func parseDRBDStatusAll(output string) map[string]*DRBDInfo {
	results := make(map[string]*DRBDInfo)

	blocks := splitResourceBlocks(output)

	for _, block := range blocks {
		lines := strings.Split(strings.TrimSpace(block), "\n")
		if len(lines) == 0 {
			continue
		}

		// First line: "user-alice role:Primary"
		firstLine := strings.TrimSpace(lines[0])
		fields := strings.Fields(firstLine)
		if len(fields) < 2 {
			continue
		}
		resName := fields[0]
		info := &DRBDInfo{
			ConnectionState: "StandAlone",
		}

		// Parse all key:value tokens from first line
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "role:") {
				info.Role = strings.TrimPrefix(f, "role:")
			}
		}

		// Parse remaining lines — each line has space-separated key:value tokens
		inPeerSection := false
		for i := 1; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			tokens := strings.Fields(line)

			// Detect peer section: line starts with a hostname followed by role:
			for _, t := range tokens {
				if strings.HasPrefix(t, "role:") && !strings.HasPrefix(line, "disk:") &&
					!strings.HasPrefix(line, "peer-disk:") && !strings.HasPrefix(line, "replication:") {
					// This is a peer line (e.g. "machine-2 role:Secondary")
					inPeerSection = true
					info.ConnectionState = "Connected"
				}
			}

			// Extract key:value tokens
			for _, t := range tokens {
				if strings.HasPrefix(t, "disk:") && !inPeerSection {
					info.DiskState = strings.TrimPrefix(t, "disk:")
				}
				if strings.HasPrefix(t, "peer-disk:") {
					info.PeerDiskState = strings.TrimPrefix(t, "peer-disk:")
				}
				if strings.HasPrefix(t, "replication:") {
					info.ConnectionState = "Connected"
				}
				if strings.HasPrefix(t, "done:") {
					progress := strings.TrimPrefix(t, "done:")
					info.SyncProgress = &progress
				}
			}
		}

		results[resName] = info
	}

	return results
}

func splitResourceBlocks(output string) []string {
	var blocks []string
	var current strings.Builder
	for _, line := range strings.Split(output, "\n") {
		if line == "" && current.Len() > 0 {
			blocks = append(blocks, current.String())
			current.Reset()
			continue
		}
		if current.Len() > 0 {
			current.WriteString("\n")
		}
		current.WriteString(line)
	}
	if current.Len() > 0 {
		blocks = append(blocks, current.String())
	}
	return blocks
}

func isMounted(path string) bool {
	result, err := runCmd("mountpoint", "-q", path)
	return err == nil && result.ExitCode == 0
}
