package coordinator

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	mathrand "math/rand"
	"net/http"
	"os"
	"strconv"
	"time"

	"scfuture/internal/shared"
)

type Coordinator struct {
	store            *Store
	B2BucketName     string
	failAt           string
	chaosMode        bool
	chaosProbability float64
	cancelFunc       context.CancelFunc // for graceful shutdown
}

func NewCoordinator(databaseURL, b2BucketName string) (*Coordinator, error) {
	store, err := NewStore(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("create store: %w", err)
	}

	failAt := os.Getenv("FAIL_AT")
	chaosMode := os.Getenv("CHAOS_MODE") == "true"
	chaosProbability := 0.05
	if v := os.Getenv("CHAOS_PROBABILITY"); v != "" {
		if p, err := strconv.ParseFloat(v, 64); err == nil {
			chaosProbability = p
		}
	}

	return &Coordinator{
		store:            store,
		B2BucketName:     b2BucketName,
		failAt:           failAt,
		chaosMode:        chaosMode,
		chaosProbability: chaosProbability,
	}, nil
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

	// Failover events
	mux.HandleFunc("GET /api/failovers", coord.handleGetFailovers)

	// Reformation events
	mux.HandleFunc("GET /api/reformations", coord.handleGetReformations)

	// Lifecycle management (Layer 4.5)
	mux.HandleFunc("POST /api/users/{id}/suspend", coord.handleSuspendUser)
	mux.HandleFunc("POST /api/users/{id}/reactivate", coord.handleReactivateUser)
	mux.HandleFunc("POST /api/users/{id}/evict", coord.handleEvictUser)
	mux.HandleFunc("GET /api/lifecycle-events", coord.handleGetLifecycleEvents)

	// Events & Operations (Layer 4.6)
	mux.HandleFunc("GET /api/events", coord.handleGetEvents)
	mux.HandleFunc("GET /api/operations", coord.handleGetOperations)
}

func (coord *Coordinator) GetStore() *Store {
	return coord.store
}

func (coord *Coordinator) SetCancelFunc(cancel context.CancelFunc) {
	coord.cancelFunc = cancel
}

// ─── Fault injection ───

func (coord *Coordinator) checkFault(name string) {
	if coord.failAt == name {
		slog.Warn("FAULT INJECTION: crashing at checkpoint", "checkpoint", name)
		os.Exit(1)
	}
	if coord.chaosMode && mathrand.Float64() < coord.chaosProbability {
		slog.Warn("CHAOS: random crash at checkpoint", "checkpoint", name)
		os.Exit(1)
	}
}

func (coord *Coordinator) step(opID, stepName string) {
	coord.store.UpdateOperationStep(opID, stepName)
	coord.checkFault(stepName)
}

// ─── Operation ID generation ───

func generateOpID() string {
	return fmt.Sprintf("op-%d-%s", time.Now().UnixNano(), randomSuffix(6))
}

func randomSuffix(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// ─── Metadata helpers for operation resumption ───

func metaString(meta map[string]interface{}, key string) string {
	if v, ok := meta[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func metaInt(meta map[string]interface{}, key string) int {
	if v, ok := meta[key]; ok {
		switch n := v.(type) {
		case float64:
			return int(math.Round(n))
		case int:
			return n
		}
	}
	return 0
}

// ─── Handlers ───

func (coord *Coordinator) handleGetFailovers(w http.ResponseWriter, r *http.Request) {
	events := coord.store.GetFailoverEvents()
	if events == nil {
		events = []FailoverEvent{}
	}
	writeJSON(w, http.StatusOK, events)
}

func (coord *Coordinator) handleGetReformations(w http.ResponseWriter, r *http.Request) {
	events := coord.store.GetReformationEvents()
	if events == nil {
		events = []ReformationEvent{}
	}
	writeJSON(w, http.StatusOK, events)
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
			UserID:           u.UserID,
			Status:           u.Status,
			PrimaryMachine:   u.PrimaryMachine,
			DRBDPort:         u.DRBDPort,
			Error:            u.Error,
			Bipod:            entries,
			BackupExists:     u.BackupExists,
			BackupPath:       u.BackupPath,
			BackupBucket:     u.BackupBucket,
			DRBDDisconnected: u.DRBDDisconnected,
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
		UserID:           u.UserID,
		Status:           u.Status,
		PrimaryMachine:   u.PrimaryMachine,
		DRBDPort:         u.DRBDPort,
		Error:            u.Error,
		Bipod:            entries,
		BackupExists:     u.BackupExists,
		BackupPath:       u.BackupPath,
		BackupBucket:     u.BackupBucket,
		DRBDDisconnected: u.DRBDDisconnected,
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

func (coord *Coordinator) handleSuspendUser(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	go coord.suspendUser(userID)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "suspending", "user_id": userID})
}

func (coord *Coordinator) handleReactivateUser(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	go coord.reactivateUser(userID)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "reactivating", "user_id": userID})
}

func (coord *Coordinator) handleEvictUser(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	go coord.evictUser(userID)
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "evicting", "user_id": userID})
}

func (coord *Coordinator) handleGetLifecycleEvents(w http.ResponseWriter, r *http.Request) {
	events := coord.store.GetLifecycleEvents()
	if events == nil {
		events = []LifecycleEvent{}
	}
	writeJSON(w, http.StatusOK, events)
}

func (coord *Coordinator) handleGetEvents(w http.ResponseWriter, r *http.Request) {
	rows, err := coord.store.DB().Query(`SELECT event_id, timestamp, event_type, machine_id, user_id, operation_id, details FROM events ORDER BY timestamp DESC LIMIT 100`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var events []map[string]interface{}
	for rows.Next() {
		var eventID int
		var ts time.Time
		var eventType string
		var machineID, userID, operationID sql.NullString
		var detailsJSON []byte
		rows.Scan(&eventID, &ts, &eventType, &machineID, &userID, &operationID, &detailsJSON)

		var details map[string]interface{}
		json.Unmarshal(detailsJSON, &details)

		events = append(events, map[string]interface{}{
			"event_id":     eventID,
			"timestamp":    ts,
			"event_type":   eventType,
			"machine_id":   machineID.String,
			"user_id":      userID.String,
			"operation_id": operationID.String,
			"details":      details,
		})
	}
	if events == nil {
		events = []map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, events)
}

func (coord *Coordinator) handleGetOperations(w http.ResponseWriter, r *http.Request) {
	rows, err := coord.store.DB().Query(`SELECT operation_id, type, user_id, status, current_step, metadata, started_at, completed_at, error FROM operations ORDER BY started_at DESC LIMIT 100`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	var ops []map[string]interface{}
	for rows.Next() {
		var opID, opType, userID, status, currentStep, errStr string
		var metaJSON []byte
		var startedAt time.Time
		var completedAt sql.NullTime
		rows.Scan(&opID, &opType, &userID, &status, &currentStep, &metaJSON, &startedAt, &completedAt, &errStr)

		var meta map[string]interface{}
		json.Unmarshal(metaJSON, &meta)

		entry := map[string]interface{}{
			"operation_id": opID,
			"type":         opType,
			"user_id":      userID,
			"status":       status,
			"current_step": currentStep,
			"metadata":     meta,
			"started_at":   startedAt,
			"error":        errStr,
		}
		if completedAt.Valid {
			entry["completed_at"] = completedAt.Time
		}
		ops = append(ops, entry)
	}
	if ops == nil {
		ops = []map[string]interface{}{}
	}
	writeJSON(w, http.StatusOK, ops)
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
