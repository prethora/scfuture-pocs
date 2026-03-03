package machineagent

import (
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"

	"scfuture/internal/shared"
)

type DRBDInfo struct {
	Role            string
	ConnectionState string
	DiskState       string
	PeerDiskState   string
	SyncProgress    *string
	Peers           []shared.DRBDPeerInfo
}

func generateDRBDConfig(resName string, nodes []shared.DRBDNode, port int) string {
	// Sort nodes by hostname for consistent connection-mesh ordering
	sorted := make([]shared.DRBDNode, len(nodes))
	copy(sorted, nodes)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Hostname < sorted[j].Hostname
	})

	var b strings.Builder
	fmt.Fprintf(&b, "resource %s {\n", resName)
	b.WriteString("    net {\n")
	b.WriteString("        protocol A;\n")
	b.WriteString("        max-buffers 8000;\n")
	b.WriteString("        max-epoch-size 8000;\n")
	b.WriteString("        sndbuf-size 0;\n")
	b.WriteString("        rcvbuf-size 0;\n")
	b.WriteString("    }\n")
	b.WriteString("    disk {\n")
	b.WriteString("        on-io-error detach;\n")
	b.WriteString("    }\n")
	for _, node := range sorted {
		fmt.Fprintf(&b, "    on %s {\n", node.Hostname)
		fmt.Fprintf(&b, "        node-id %d;\n", stableNodeID(node.Hostname))
		fmt.Fprintf(&b, "        device /dev/drbd%d minor %d;\n", node.Minor, node.Minor)
		fmt.Fprintf(&b, "        disk %s;\n", node.Disk)
		fmt.Fprintf(&b, "        address %s:%d;\n", node.Address, port)
		b.WriteString("        meta-disk internal;\n")
		b.WriteString("    }\n")
	}
	// connection-mesh is required for 3+ nodes and also works for 2 nodes
	b.WriteString("    connection-mesh {\n")
	b.WriteString("        hosts")
	for _, node := range sorted {
		fmt.Fprintf(&b, " %s", node.Hostname)
	}
	b.WriteString(";\n")
	b.WriteString("    }\n")
	b.WriteString("}\n")
	return b.String()
}

// stableNodeID derives a stable DRBD node-id from a hostname like "fleet-1".
// This ensures node-ids are consistent across reconfigurations (2→3→2 nodes),
// preventing metadata/config mismatches that cause drbdadm adjust/up failures.
func stableNodeID(hostname string) int {
	parts := strings.Split(hostname, "-")
	if len(parts) >= 2 {
		if n, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
			return n - 1 // fleet-1→0, fleet-2→1, fleet-3→2
		}
	}
	return 0
}

func worstDiskState(states []string) string {
	priority := map[string]int{
		"Inconsistent": 4,
		"Outdated":     3,
		"DUnknown":     2,
		"UpToDate":     1,
	}
	worst := ""
	worstPri := 0
	for _, s := range states {
		if p, ok := priority[s]; ok && p > worstPri {
			worst = s
			worstPri = p
		} else if !ok && worst == "" {
			worst = s
		}
	}
	return worst
}

func (a *Agent) DRBDCreate(userID string, req *shared.DRBDCreateRequest) (*shared.DRBDCreateResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	if len(req.Nodes) < 2 || len(req.Nodes) > 3 {
		return nil, fmt.Errorf("2 or 3 nodes required, got %d", len(req.Nodes))
	}

	resName := req.ResourceName

	// Check if already exists
	_, err := runCmd("drbdadm", "status", resName)
	if err == nil {
		slog.Info("DRBD resource already exists", "component", "drbd", "user", userID, "resource", resName)
		return &shared.DRBDCreateResponse{AlreadyExisted: true}, nil
	}

	// Write config file
	configPath := fmt.Sprintf("/etc/drbd.d/%s.res", resName)
	config := generateDRBDConfig(resName, req.Nodes, req.Port)

	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		return nil, fmt.Errorf("write DRBD config: %w", err)
	}

	// Create metadata (--max-peers 2 allows up to 3 nodes for tripod migration)
	result, err := runCmd("drbdadm", "create-md", "--max-peers", "2", "--force", resName)
	if err != nil {
		return nil, cmdError("drbdadm create-md failed", cmdString("drbdadm", "create-md", "--max-peers", "2", "--force", resName), result)
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
	return &shared.DRBDCreateResponse{}, nil
}

func (a *Agent) DRBDPromote(userID string) (*shared.DRBDPromoteResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	resName := "user-" + userID

	// Check current role
	info := a.getDRBDStatus(resName)
	if info != nil && info.Role == "Primary" {
		slog.Info("DRBD already Primary", "component", "drbd", "user", userID)
		return &shared.DRBDPromoteResponse{AlreadyExisted: true}, nil
	}

	result, err := runCmd("drbdadm", "primary", "--force", resName)
	if err != nil {
		return nil, cmdError("drbdadm primary failed", cmdString("drbdadm", "primary", "--force", resName), result)
	}

	slog.Info("DRBD promoted to Primary", "component", "drbd", "user", userID)
	return &shared.DRBDPromoteResponse{OK: true}, nil
}

func (a *Agent) DRBDDemote(userID string) (*shared.DRBDDemoteResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	resName := "user-" + userID

	// Check current role
	info := a.getDRBDStatus(resName)
	if info != nil && info.Role == "Secondary" {
		slog.Info("DRBD already Secondary", "component", "drbd", "user", userID)
		return &shared.DRBDDemoteResponse{AlreadyExisted: true}, nil
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
	return &shared.DRBDDemoteResponse{OK: true}, nil
}

func (a *Agent) DRBDStatus(userID string) (*shared.DRBDStatusResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	resName := "user-" + userID
	info := a.getDRBDStatus(resName)
	if info == nil {
		return &shared.DRBDStatusResponse{Exists: false}, nil
	}

	return &shared.DRBDStatusResponse{
		Resource:        resName,
		Role:            info.Role,
		ConnectionState: info.ConnectionState,
		DiskState:       info.DiskState,
		PeerDiskState:   info.PeerDiskState,
		SyncProgress:    info.SyncProgress,
		Peers:           info.Peers,
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

func (a *Agent) DRBDDisconnect(userID string) (*shared.DRBDDisconnectResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	resName := "user-" + userID

	// Check current status
	info := a.getDRBDStatus(resName)
	if info == nil {
		return nil, fmt.Errorf("DRBD resource %s does not exist", resName)
	}

	// If already StandAlone or no peer connection, return success
	if info.ConnectionState == "StandAlone" {
		slog.Info("DRBD already disconnected (StandAlone)", "component", "drbd", "user", userID)
		return &shared.DRBDDisconnectResponse{Status: "disconnected", WasConnected: false}, nil
	}

	result, err := runCmd("drbdadm", "disconnect", resName)
	if err != nil {
		return nil, cmdError("drbdadm disconnect failed", cmdString("drbdadm", "disconnect", resName), result)
	}

	slog.Info("DRBD disconnected from peer", "component", "drbd", "user", userID)
	return &shared.DRBDDisconnectResponse{Status: "disconnected", WasConnected: true}, nil
}

func (a *Agent) DRBDConnect(userID string) (*shared.DRBDConnectResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}

	resName := "user-" + userID

	// Check current status
	info := a.getDRBDStatus(resName)
	if info == nil {
		return nil, fmt.Errorf("DRBD resource %s does not exist", resName)
	}

	// If already connected, return success
	if info.ConnectionState == "Connected" {
		slog.Info("DRBD already connected", "component", "drbd", "user", userID)
		return &shared.DRBDConnectResponse{Status: "connected", WasConnected: true}, nil
	}

	result, err := runCmd("drbdadm", "connect", resName)
	if err != nil {
		return nil, cmdError("drbdadm connect failed", cmdString("drbdadm", "connect", resName), result)
	}

	slog.Info("DRBD connected to peer", "component", "drbd", "user", userID)
	return &shared.DRBDConnectResponse{Status: "connected", WasConnected: false}, nil
}

func (a *Agent) DRBDReconfigure(userID string, req *shared.DRBDReconfigureRequest) (*shared.DRBDReconfigureResponse, error) {
	if err := validateUserID(userID); err != nil {
		return nil, err
	}
	if len(req.Nodes) < 2 || len(req.Nodes) > 3 {
		return nil, fmt.Errorf("2 or 3 nodes required, got %d", len(req.Nodes))
	}

	resName := "user-" + userID
	configPath := fmt.Sprintf("/etc/drbd.d/%s.res", resName)

	// Write new config file
	config := generateDRBDConfig(resName, req.Nodes, req.Port)

	if err := os.WriteFile(configPath, []byte(config), 0644); err != nil {
		return nil, fmt.Errorf("write DRBD config: %w", err)
	}

	if !req.Force {
		// Try adjust
		result, err := runCmd("drbdadm", "adjust", resName)
		if err == nil {
			// Explicitly connect to any new peers — adjust may not initiate connections
			// to peers that were added to the config but are in StandAlone state.
			if connResult, connErr := runCmd("drbdadm", "connect", resName); connErr != nil {
				slog.Info("drbdadm connect after adjust (non-fatal)", "component", "drbd", "user", userID, "stderr", connResult.Stderr)
			}
			slog.Info("DRBD reconfigured via adjust", "component", "drbd", "user", userID)

			// Update in-memory state with new peer info
			hostname, _ := os.Hostname()
			u := a.getUser(userID)
			if u != nil {
				for _, n := range req.Nodes {
					if n.Hostname == hostname {
						u.DRBDMinor = n.Minor
						u.DRBDDevice = fmt.Sprintf("/dev/drbd%d", n.Minor)
					}
				}
				a.setUser(userID, u)
			}

			return &shared.DRBDReconfigureResponse{Status: "reconfigured", Method: "adjust"}, nil
		}
		slog.Warn("drbdadm adjust failed", "component", "drbd", "user", userID, "error", err, "output", result.Stderr)
		// Return error — coordinator will handle fallback
		return nil, fmt.Errorf("adjust failed (stderr: %s), coordinator should retry with force=true", result.Stderr)
	}

	// Force path: full down/up cycle, then promote only if role=primary
	// The coordinator is responsible for stopping/starting the container around this call
	slog.Info("DRBD reconfigure via down/up (force)", "component", "drbd", "user", userID, "role", req.Role)

	// Unmount host if mounted (safety)
	mountPath := a.mountPath(userID)
	if isMounted(mountPath) {
		runCmd("umount", mountPath)
	}

	// Down — must succeed before up, otherwise minor is still in use
	result, err := runCmd("drbdadm", "down", resName)
	if err != nil {
		slog.Warn("drbdadm down failed, retrying after disconnect", "component", "drbd", "user", userID, "error", err)
		// Try disconnecting peers first, then down again
		runCmd("drbdadm", "disconnect", resName)
		result, err = runCmd("drbdadm", "down", resName)
		if err != nil {
			return nil, cmdError("drbdadm down failed after reconfigure (minor still in use)", cmdString("drbdadm", "down", resName), result)
		}
	}

	// Up (uses new config)
	result, err = runCmd("drbdadm", "up", resName)
	if err != nil {
		return nil, cmdError("drbdadm up failed after reconfigure", cmdString("drbdadm", "up", resName), result)
	}

	// Only promote to primary if the role demands it (secondary nodes stay secondary)
	if req.Role != "secondary" {
		result, err = runCmd("drbdadm", "primary", "--force", resName)
		if err != nil {
			return nil, cmdError("drbdadm primary failed after reconfigure", cmdString("drbdadm", "primary", "--force", resName), result)
		}
	}

	// Update in-memory state
	hostname, _ := os.Hostname()
	u := a.getUser(userID)
	if u != nil {
		for _, n := range req.Nodes {
			if n.Hostname == hostname {
				u.DRBDMinor = n.Minor
				u.DRBDDevice = fmt.Sprintf("/dev/drbd%d", n.Minor)
			}
		}
		a.setUser(userID, u)
	}

	return &shared.DRBDReconfigureResponse{Status: "reconfigured", Method: "down_up"}, nil
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
// Supports multiple peers (2-node and 3-node DRBD configurations).
//
// DRBD 9 status lines contain space-separated key:value tokens, e.g.:
//
//	user-alice role:Primary
//	  disk:UpToDate open:no
//	  machine-2 role:Secondary
//	    peer-disk:UpToDate
//	  machine-3 role:Secondary
//	    replication:SyncSource peer-disk:Inconsistent done:45.20
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

		// Parse remaining lines — track multiple peers
		var currentPeer *shared.DRBDPeerInfo
		inPeerSection := false

		for i := 1; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			tokens := strings.Fields(line)

			// Detect peer section: line has a hostname token followed by role: token
			// but does NOT start with disk:, peer-disk:, or replication:
			isPeerLine := false
			if !strings.HasPrefix(line, "disk:") &&
				!strings.HasPrefix(line, "peer-disk:") &&
				!strings.HasPrefix(line, "replication:") {
				for _, t := range tokens {
					if strings.HasPrefix(t, "role:") {
						isPeerLine = true
						break
					}
				}
			}

			if isPeerLine {
				// Start new peer
				inPeerSection = true
				info.ConnectionState = "Connected"
				peer := shared.DRBDPeerInfo{}
				// First token is hostname, rest are key:value
				if len(tokens) > 0 && !strings.Contains(tokens[0], ":") {
					peer.Hostname = tokens[0]
				}
				for _, t := range tokens {
					if strings.HasPrefix(t, "role:") {
						peer.Role = strings.TrimPrefix(t, "role:")
					}
				}
				info.Peers = append(info.Peers, peer)
				currentPeer = &info.Peers[len(info.Peers)-1]
				continue
			}

			// Extract key:value tokens
			for _, t := range tokens {
				if strings.HasPrefix(t, "disk:") && !inPeerSection {
					info.DiskState = strings.TrimPrefix(t, "disk:")
				}
				if strings.HasPrefix(t, "peer-disk:") && currentPeer != nil {
					currentPeer.DiskState = strings.TrimPrefix(t, "peer-disk:")
				} else if strings.HasPrefix(t, "peer-disk:") {
					// Fallback for no peer context
					info.PeerDiskState = strings.TrimPrefix(t, "peer-disk:")
				}
				if strings.HasPrefix(t, "replication:") {
					info.ConnectionState = "Connected"
				}
				if strings.HasPrefix(t, "done:") {
					progress := strings.TrimPrefix(t, "done:")
					if currentPeer != nil {
						currentPeer.SyncProgress = &progress
					}
				}
			}
		}

		// Compute backward-compatible fields from peers
		if len(info.Peers) > 0 {
			var diskStates []string
			for _, p := range info.Peers {
				if p.DiskState != "" {
					diskStates = append(diskStates, p.DiskState)
				}
				if p.SyncProgress != nil && info.SyncProgress == nil {
					info.SyncProgress = p.SyncProgress
				}
			}
			if len(diskStates) > 0 {
				info.PeerDiskState = worstDiskState(diskStates)
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
