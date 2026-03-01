package machineagent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

type UserResources struct {
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

type Agent struct {
	nodeID  string
	dataDir string
	users   map[string]*UserResources
	usersMu sync.RWMutex
	locks   sync.Map // map[string]*sync.Mutex — per-user operation lock
}

func NewAgent(nodeID, dataDir string) *Agent {
	return &Agent{
		nodeID:  nodeID,
		dataDir: dataDir,
		users:   make(map[string]*UserResources),
	}
}

func (a *Agent) getUserLock(userID string) *sync.Mutex {
	val, _ := a.locks.LoadOrStore(userID, &sync.Mutex{})
	return val.(*sync.Mutex)
}

func (a *Agent) getUser(userID string) *UserResources {
	a.usersMu.RLock()
	defer a.usersMu.RUnlock()
	return a.users[userID]
}

func (a *Agent) setUser(userID string, u *UserResources) {
	a.usersMu.Lock()
	defer a.usersMu.Unlock()
	a.users[userID] = u
}

func (a *Agent) deleteUser(userID string) {
	a.usersMu.Lock()
	defer a.usersMu.Unlock()
	delete(a.users, userID)
}

func (a *Agent) allUsers() map[string]*UserResources {
	a.usersMu.RLock()
	defer a.usersMu.RUnlock()
	cp := make(map[string]*UserResources, len(a.users))
	for k, v := range a.users {
		clone := *v
		cp[k] = &clone
	}
	return cp
}

func (a *Agent) imagePath(userID string) string {
	return filepath.Join(a.dataDir, "images", userID+".img")
}

func (a *Agent) mountPath(userID string) string {
	return filepath.Join("/mnt/users", userID)
}

// Discover rebuilds in-memory state from system reality.
func (a *Agent) Discover() {
	slog.Info("Discovering existing state", "component", "state")

	a.usersMu.Lock()
	defer a.usersMu.Unlock()

	// ensure map is fresh
	a.users = make(map[string]*UserResources)

	// 1. Scan losetup -a for active loop devices
	a.discoverLoopDevices()

	// 2. Scan DRBD config files
	a.discoverDRBDConfigs()

	// 3. Parse DRBD status
	a.discoverDRBDStatus()

	// 4. Scan mounts
	a.discoverMounts()

	// 5. Scan docker containers
	a.discoverContainers()

	slog.Info("Discovery complete", "component", "state", "users", len(a.users))
}

func (a *Agent) ensureUser(userID string) *UserResources {
	u, ok := a.users[userID]
	if !ok {
		u = &UserResources{}
		a.users[userID] = u
	}
	return u
}

var loopRe = regexp.MustCompile(`^(/dev/loop\d+):\s+\[\d+\]:\d+\s+\((.+)\)`)

func (a *Agent) discoverLoopDevices() {
	result, err := runCmd("losetup", "-a")
	if err != nil {
		slog.Warn("losetup -a failed", "component", "state", "error", err)
		return
	}
	imgDir := filepath.Join(a.dataDir, "images")
	for _, line := range strings.Split(result.Stdout, "\n") {
		m := loopRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		loopDev := m[1]
		imgPath := m[2]
		if !strings.HasPrefix(imgPath, imgDir) {
			continue
		}
		base := filepath.Base(imgPath)
		if !strings.HasSuffix(base, ".img") {
			continue
		}
		userID := strings.TrimSuffix(base, ".img")
		u := a.ensureUser(userID)
		u.ImagePath = imgPath
		u.LoopDevice = loopDev
		slog.Info("Discovered loop device", "component", "state", "user", userID, "loop", loopDev)
	}

	// Also check for image files without loop devices
	entries, _ := os.ReadDir(filepath.Join(a.dataDir, "images"))
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".img") {
			continue
		}
		userID := strings.TrimSuffix(e.Name(), ".img")
		u := a.ensureUser(userID)
		if u.ImagePath == "" {
			u.ImagePath = filepath.Join(imgDir, e.Name())
		}
	}
}

var minorRe = regexp.MustCompile(`minor\s+(\d+)`)

func (a *Agent) discoverDRBDConfigs() {
	entries, err := filepath.Glob("/etc/drbd.d/user-*.res")
	if err != nil {
		return
	}
	for _, path := range entries {
		base := filepath.Base(path)
		// user-alice.res → alice
		userID := strings.TrimPrefix(strings.TrimSuffix(base, ".res"), "user-")
		u := a.ensureUser(userID)
		u.DRBDResource = "user-" + userID

		// Parse minor from config
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		content := string(data)

		// Find the minor for the local node by matching hostname
		hostname, _ := os.Hostname()
		sections := strings.Split(content, "on ")
		for _, sec := range sections {
			if strings.HasPrefix(strings.TrimSpace(sec), hostname) {
				m := minorRe.FindStringSubmatch(sec)
				if m != nil {
					minor, _ := strconv.Atoi(m[1])
					u.DRBDMinor = minor
					u.DRBDDevice = fmt.Sprintf("/dev/drbd%d", minor)
				}
				break
			}
		}
		slog.Info("Discovered DRBD config", "component", "state", "user", userID, "resource", u.DRBDResource, "minor", u.DRBDMinor)
	}
}

func (a *Agent) discoverDRBDStatus() {
	result, err := runCmd("drbdadm", "status", "all")
	if err != nil {
		slog.Warn("drbdadm status all failed", "component", "state", "error", err)
		return
	}

	resources := parseDRBDStatusAll(result.Stdout)
	for resName, info := range resources {
		userID := strings.TrimPrefix(resName, "user-")
		u := a.ensureUser(userID)
		u.DRBDRole = info.Role
		u.DRBDConnection = info.ConnectionState
		u.DRBDDiskState = info.DiskState
		u.DRBDPeerDisk = info.PeerDiskState
		slog.Info("Discovered DRBD status", "component", "state", "user", userID,
			"role", info.Role, "disk", info.DiskState)
	}
}

func (a *Agent) discoverMounts() {
	result, err := runCmd("mount")
	if err != nil {
		return
	}
	for _, line := range strings.Split(result.Stdout, "\n") {
		if !strings.Contains(line, "/mnt/users/") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		mountPoint := parts[2]
		userID := filepath.Base(mountPoint)
		u := a.ensureUser(userID)
		u.HostMounted = true
		slog.Info("Discovered host mount", "component", "state", "user", userID, "mount", mountPoint)
	}
}

func (a *Agent) discoverContainers() {
	result, err := runCmd("docker", "ps", "--format", "{{.Names}}")
	if err != nil {
		return
	}
	for _, name := range strings.Split(result.Stdout, "\n") {
		name = strings.TrimSpace(name)
		if !strings.HasSuffix(name, "-agent") {
			continue
		}
		userID := strings.TrimSuffix(name, "-agent")
		u := a.ensureUser(userID)
		u.ContainerRunning = true
		u.ContainerName = name
		slog.Info("Discovered running container", "component", "state", "user", userID, "container", name)
	}
}
