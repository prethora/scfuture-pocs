package machineagent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"scfuture/internal/shared"
)

func (a *Agent) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /status", a.handleStatus)
	mux.HandleFunc("POST /images/{user_id}/create", a.handleImageCreate)
	mux.HandleFunc("DELETE /images/{user_id}", a.handleImageDelete)
	mux.HandleFunc("POST /images/{user_id}/drbd/create", a.handleDRBDCreate)
	mux.HandleFunc("POST /images/{user_id}/drbd/promote", a.handleDRBDPromote)
	mux.HandleFunc("POST /images/{user_id}/drbd/demote", a.handleDRBDDemote)
	mux.HandleFunc("GET /images/{user_id}/drbd/status", a.handleDRBDStatus)
	mux.HandleFunc("POST /images/{user_id}/drbd/disconnect", a.handleDRBDDisconnect)
	mux.HandleFunc("POST /images/{user_id}/drbd/reconfigure", a.handleDRBDReconfigure)
	mux.HandleFunc("DELETE /images/{user_id}/drbd", a.handleDRBDDestroy)
	mux.HandleFunc("POST /images/{user_id}/format-btrfs", a.handleFormatBtrfs)
	mux.HandleFunc("POST /containers/{user_id}/start", a.handleContainerStart)
	mux.HandleFunc("POST /containers/{user_id}/stop", a.handleContainerStop)
	mux.HandleFunc("GET /containers/{user_id}/status", a.handleContainerStatus)
	mux.HandleFunc("POST /cleanup", a.handleCleanup)
}

func (a *Agent) handleStatus(w http.ResponseWriter, r *http.Request) {
	// Refresh state from system
	a.Discover()

	users := a.allUsers()
	usersDTO := make(map[string]*shared.UserStatusDTO, len(users))
	for id, u := range users {
		usersDTO[id] = &shared.UserStatusDTO{
			ImageExists:      u.ImagePath != "",
			ImagePath:        u.ImagePath,
			LoopDevice:       u.LoopDevice,
			DRBDResource:     u.DRBDResource,
			DRBDMinor:        u.DRBDMinor,
			DRBDDevice:       u.DRBDDevice,
			DRBDRole:         u.DRBDRole,
			DRBDConnection:   u.DRBDConnection,
			DRBDDiskState:    u.DRBDDiskState,
			DRBDPeerDisk:     u.DRBDPeerDisk,
			HostMounted:      u.HostMounted,
			ContainerRunning: u.ContainerRunning,
			ContainerName:    u.ContainerName,
		}
	}

	resp := shared.StatusResponse{
		MachineID:   a.nodeID,
		DiskTotalMB: getDiskTotalMB(),
		DiskUsedMB:  getDiskUsedMB(),
		RAMTotalMB:  getRAMTotalMB(),
		RAMUsedMB:   getRAMUsedMB(),
		Users:       usersDTO,
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleImageCreate(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	var req shared.ImageCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}
	if req.ImageSizeMB <= 0 {
		writeError(w, http.StatusBadRequest, "image_size_mb must be positive", "")
		return
	}

	resp, err := a.CreateImage(userID, req.ImageSizeMB)
	if err != nil {
		if strings.Contains(err.Error(), "invalid user_id") {
			writeError(w, http.StatusBadRequest, err.Error(), "")
		} else {
			writeError(w, http.StatusInternalServerError, err.Error(), "")
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleImageDelete(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	if err := a.DeleteUser(userID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *Agent) handleDRBDCreate(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	var req shared.DRBDCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	resp, err := a.DRBDCreate(userID, &req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleDRBDPromote(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	resp, err := a.DRBDPromote(userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleDRBDDemote(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	resp, err := a.DRBDDemote(userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleDRBDStatus(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")

	resp, err := a.DRBDStatus(userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleDRBDDestroy(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	if err := a.DRBDDestroy(userID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *Agent) handleDRBDDisconnect(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	resp, err := a.DRBDDisconnect(userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleDRBDReconfigure(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	var req shared.DRBDReconfigureRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", err.Error())
		return
	}

	resp, err := a.DRBDReconfigure(userID, &req)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleFormatBtrfs(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	resp, err := a.FormatBtrfs(userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleContainerStart(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	resp, err := a.ContainerStart(userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleContainerStop(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")
	lock := a.getUserLock(userID)
	lock.Lock()
	defer lock.Unlock()

	if err := a.ContainerStop(userID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *Agent) handleContainerStatus(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("user_id")

	resp, err := a.ContainerStatus(userID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *Agent) handleCleanup(w http.ResponseWriter, r *http.Request) {
	if err := a.Cleanup(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// EnsureContainerImage checks if platform/app-container exists and builds it if not.
func (a *Agent) EnsureContainerImage() error {
	result, _ := runCmd("docker", "images", "platform/app-container", "--format", "{{.Repository}}")
	if strings.TrimSpace(result.Stdout) == "platform/app-container" {
		slog.Info("Container image already built", "component", "server")
		return nil
	}

	slog.Info("Building container image", "component", "server")
	result, err := runCmd("docker", "build", "-t", "platform/app-container", "/opt/platform/container/")
	if err != nil {
		return fmt.Errorf("docker build failed: %s %s", result.Stderr, result.Stdout)
	}
	slog.Info("Container image built", "component", "server")
	return nil
}

// Helper functions

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, errMsg, details string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	resp := map[string]string{"error": errMsg}
	if details != "" {
		resp["details"] = details
	}
	json.NewEncoder(w).Encode(resp)
}

// System info helpers

func getDiskTotalMB() int64 {
	result, err := runCmd("df", "--output=size", "-BM", "/")
	if err != nil {
		return 0
	}
	return parseDfMB(result.Stdout)
}

func getDiskUsedMB() int64 {
	result, err := runCmd("df", "--output=used", "-BM", "/")
	if err != nil {
		return 0
	}
	return parseDfMB(result.Stdout)
}

func parseDfMB(output string) int64 {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		return 0
	}
	val := strings.TrimSpace(lines[1])
	val = strings.TrimSuffix(val, "M")
	n, _ := strconv.ParseInt(val, 10, 64)
	return n
}

func getRAMTotalMB() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return parseMemInfoKB(string(data), "MemTotal") / 1024
}

func getRAMUsedMB() int64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	total := parseMemInfoKB(string(data), "MemTotal")
	available := parseMemInfoKB(string(data), "MemAvailable")
	return (total - available) / 1024
}

func parseMemInfoKB(content, key string) int64 {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, key+":") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				n, _ := strconv.ParseInt(fields[1], 10, 64)
				return n
			}
		}
	}
	return 0
}
