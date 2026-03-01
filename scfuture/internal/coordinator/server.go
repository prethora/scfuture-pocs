package coordinator

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"scfuture/internal/shared"
)

type Coordinator struct {
	store *Store
}

func NewCoordinator(dataDir string) *Coordinator {
	return &Coordinator{
		store: NewStore(dataDir),
	}
}

func (coord *Coordinator) RegisterRoutes(mux *http.ServeMux) {
	// Fleet management (called by machine agents)
	mux.HandleFunc("POST /api/fleet/register", coord.handleFleetRegister)
	mux.HandleFunc("POST /api/fleet/heartbeat", coord.handleFleetHeartbeat)
	mux.HandleFunc("GET /api/fleet", coord.handleFleetStatus)

	// User management (called by test harness / external systems)
	mux.HandleFunc("POST /api/users", coord.handleCreateUser)
	mux.HandleFunc("GET /api/users", coord.handleListUsers)
	mux.HandleFunc("GET /api/users/{id}", coord.handleGetUser)
	mux.HandleFunc("POST /api/users/{id}/provision", coord.handleProvisionUser)
	mux.HandleFunc("GET /api/users/{id}/bipod", coord.handleGetBipod)
}

func (coord *Coordinator) handleFleetRegister(w http.ResponseWriter, r *http.Request) {
	var req shared.FleetRegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	coord.store.RegisterMachine(&req)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (coord *Coordinator) handleFleetHeartbeat(w http.ResponseWriter, r *http.Request) {
	var req shared.FleetHeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	coord.store.UpdateHeartbeat(&req)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (coord *Coordinator) handleFleetStatus(w http.ResponseWriter, r *http.Request) {
	machines := coord.store.AllMachines()
	var statuses []shared.MachineStatus
	for _, m := range machines {
		statuses = append(statuses, shared.MachineStatus{
			MachineID:     m.MachineID,
			Address:       m.Address,
			Status:        m.Status,
			DiskTotalMB:   m.DiskTotalMB,
			DiskUsedMB:    m.DiskUsedMB,
			RAMTotalMB:    m.RAMTotalMB,
			RAMUsedMB:     m.RAMUsedMB,
			ActiveAgents:  m.ActiveAgents,
			MaxAgents:     m.MaxAgents,
			RunningAgents: m.RunningAgents,
			LastHeartbeat: m.LastHeartbeat.Format("2006-01-02T15:04:05Z"),
		})
	}
	if statuses == nil {
		statuses = []shared.MachineStatus{}
	}
	writeJSON(w, http.StatusOK, shared.FleetStatusResponse{Machines: statuses})
}

func (coord *Coordinator) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req shared.CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.UserID == "" {
		writeError(w, http.StatusBadRequest, "user_id is required")
		return
	}

	u, err := coord.store.CreateUser(req.UserID, req.ImageSizeMB)
	if err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, shared.CreateUserResponse{
		UserID: u.UserID,
		Status: u.Status,
	})
}

func (coord *Coordinator) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users := coord.store.AllUsers()
	var result []shared.UserDetailResponse
	for _, u := range users {
		bipods := coord.store.GetBipods(u.UserID)
		var entries []shared.BipodEntry
		for _, b := range bipods {
			entries = append(entries, shared.BipodEntry{
				MachineID:  b.MachineID,
				Role:       b.Role,
				DRBDMinor:  b.DRBDMinor,
				LoopDevice: b.LoopDevice,
			})
		}
		if entries == nil {
			entries = []shared.BipodEntry{}
		}
		result = append(result, shared.UserDetailResponse{
			UserID:         u.UserID,
			Status:         u.Status,
			PrimaryMachine: u.PrimaryMachine,
			DRBDPort:       u.DRBDPort,
			Error:          u.Error,
			Bipod:          entries,
		})
	}
	if result == nil {
		result = []shared.UserDetailResponse{}
	}
	writeJSON(w, http.StatusOK, result)
}

func (coord *Coordinator) handleGetUser(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	u := coord.store.GetUser(userID)
	if u == nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}

	bipods := coord.store.GetBipods(userID)
	var entries []shared.BipodEntry
	for _, b := range bipods {
		entries = append(entries, shared.BipodEntry{
			MachineID:  b.MachineID,
			Role:       b.Role,
			DRBDMinor:  b.DRBDMinor,
			LoopDevice: b.LoopDevice,
		})
	}
	if entries == nil {
		entries = []shared.BipodEntry{}
	}

	writeJSON(w, http.StatusOK, shared.UserDetailResponse{
		UserID:         u.UserID,
		Status:         u.Status,
		PrimaryMachine: u.PrimaryMachine,
		DRBDPort:       u.DRBDPort,
		Error:          u.Error,
		Bipod:          entries,
	})
}

func (coord *Coordinator) handleProvisionUser(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	u := coord.store.GetUser(userID)
	if u == nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if u.Status != "registered" {
		writeError(w, http.StatusConflict, "user is not in registered state (current: "+u.Status+")")
		return
	}

	coord.store.SetUserStatus(userID, "provisioning", "")
	go coord.ProvisionUser(userID)

	slog.Info("Provisioning started", "user_id", userID)
	writeJSON(w, http.StatusOK, shared.CreateUserResponse{
		UserID: userID,
		Status: "provisioning",
	})
}

func (coord *Coordinator) handleGetBipod(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	bipods := coord.store.GetBipods(userID)
	var entries []shared.BipodEntry
	for _, b := range bipods {
		entries = append(entries, shared.BipodEntry{
			MachineID:  b.MachineID,
			Role:       b.Role,
			DRBDMinor:  b.DRBDMinor,
			LoopDevice: b.LoopDevice,
		})
	}
	if entries == nil {
		entries = []shared.BipodEntry{}
	}
	writeJSON(w, http.StatusOK, entries)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, errMsg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": errMsg})
}
