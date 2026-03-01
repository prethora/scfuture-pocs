package machineagent

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// DeleteUser tears down ALL resources for a user in reverse order.
func (a *Agent) DeleteUser(userID string) error {
	slog.Info("Deleting user resources", "component", "cleanup", "user", userID)

	containerName := userID + "-agent"

	// 1. Stop container if running
	if a.containerExists(containerName) {
		runCmd("docker", "stop", "--time", "10", containerName)
		runCmd("docker", "rm", "-f", containerName)
	}

	// 2. Unmount if mounted
	mountPath := a.mountPath(userID)
	if isMounted(mountPath) {
		runCmd("umount", mountPath)
	}

	// 3. DRBD down
	resName := "user-" + userID
	runCmd("drbdadm", "down", resName)

	// 4. Remove DRBD config
	os.Remove("/etc/drbd.d/" + resName + ".res")

	// 5. Detach loop device
	imgPath := a.imagePath(userID)
	loop := a.findLoopDevice(imgPath)
	if loop != "" {
		runCmd("losetup", "-d", loop)
	}

	// 6. Remove image file
	os.Remove(imgPath)

	// 7. Remove mount directory
	os.Remove(mountPath)

	// 8. Clear state
	a.deleteUser(userID)
	a.locks.Delete(userID)

	slog.Info("User resources deleted", "component", "cleanup", "user", userID)
	return nil
}

// Cleanup tears down ALL user resources on this machine.
func (a *Agent) Cleanup() error {
	slog.Info("Full machine cleanup", "component", "cleanup")

	// 1. Stop all *-agent containers
	result, _ := runCmd("docker", "ps", "-a", "--filter", "name=-agent", "--format", "{{.Names}}")
	for _, name := range strings.Split(result.Stdout, "\n") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		runCmd("docker", "stop", "--time", "10", name)
		runCmd("docker", "rm", "-f", name)
	}

	// 2. Unmount all /mnt/users/*
	mResult, _ := runCmd("mount")
	for _, line := range strings.Split(mResult.Stdout, "\n") {
		if !strings.Contains(line, "/mnt/users/") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 3 {
			runCmd("umount", parts[2])
		}
	}

	// 3. Down all DRBD resources
	configs, _ := filepath.Glob("/etc/drbd.d/user-*.res")
	for _, cfg := range configs {
		base := filepath.Base(cfg)
		resName := strings.TrimSuffix(base, ".res")
		runCmd("drbdadm", "down", resName)
	}

	// 4. Remove DRBD configs
	for _, cfg := range configs {
		os.Remove(cfg)
	}

	// 5. Detach all loop devices for our images
	loopResult, _ := runCmd("losetup", "-a")
	imgDir := filepath.Join(a.dataDir, "images")
	for _, line := range strings.Split(loopResult.Stdout, "\n") {
		if !strings.Contains(line, imgDir) {
			continue
		}
		m := loopRe.FindStringSubmatch(line)
		if m != nil {
			runCmd("losetup", "-d", m[1])
		}
	}

	// 6. Remove all images
	imgs, _ := filepath.Glob(filepath.Join(imgDir, "*.img"))
	for _, img := range imgs {
		os.Remove(img)
	}

	// 7. Remove mount dirs
	entries, _ := os.ReadDir("/mnt/users")
	for _, e := range entries {
		os.RemoveAll(filepath.Join("/mnt/users", e.Name()))
	}

	// 8. Clear state
	a.usersMu.Lock()
	a.users = make(map[string]*UserResources)
	a.usersMu.Unlock()
	a.locks = sync.Map{}

	slog.Info("Machine cleanup complete", "component", "cleanup")
	return nil
}
