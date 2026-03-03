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
	"sync"
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
	drainDone        sync.Map           // machineID → chan struct{}, closed when drain goroutine exits
	lastRebalanceMigration time.Time    // cooldown after rebalancer-triggered migration
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

	// Live migration (Layer 5.1)
	mux.HandleFunc("POST /api/users/{id}/migrate", coord.handleMigrateUser)
	mux.HandleFunc("GET /api/migrations", coord.handleGetMigrations)

	// Machine drain (Layer 5.2)
	mux.HandleFunc("POST /api/fleet/{machine_id}/drain", coord.handleDrainMachine)
	mux.HandleFunc("POST /api/fleet/{machine_id}/undrain", coord.handleUndrainMachine)

	// Events & Operations (Layer 4.6)
	mux.HandleFunc("GET /api/events", coord.handleGetEvents)
	mux.HandleFunc("GET /api/operations", coord.handleGetOperations)

	// Event query & system health (Layer 5.2 — event-log testing)
	mux.HandleFunc("GET /api/events/query", coord.handleQueryEvents)
	mux.HandleFunc("GET /api/events/count", coord.handleCountEvents)
	mux.HandleFunc("GET /api/system/stable", coord.handleSystemStable)
	mux.HandleFunc("GET /api/system/consistency", coord.handleSystemConsistency)
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

func (coord *Coordinator) handleMigrateUser(w http.ResponseWriter, r *http.Request) {
	userID := r.PathValue("id")
	var req shared.MigrateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SourceMachine == "" || req.TargetMachine == "" {
		writeError(w, http.StatusBadRequest, "source_machine and target_machine are required")
		return
	}

	u := coord.store.GetUser(userID)
	if u == nil {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if u.Status != "running" {
		writeError(w, http.StatusConflict, "user must be in running state to migrate (current: "+u.Status+")")
		return
	}

	// Validate source is in bipod
	bipods := coord.store.GetBipods(userID)
	sourceInBipod := false
	targetInBipod := false
	for _, b := range bipods {
		if b.Role == "stale" {
			continue
		}
		if b.MachineID == req.SourceMachine {
			sourceInBipod = true
		}
		if b.MachineID == req.TargetMachine {
			targetInBipod = true
		}
	}
	if !sourceInBipod {
		writeError(w, http.StatusBadRequest, "source_machine is not in user's bipod")
		return
	}
	if targetInBipod {
		writeError(w, http.StatusBadRequest, "target_machine is already in user's bipod")
		return
	}

	// Validate target machine exists and is active
	targetMachine := coord.store.GetMachine(req.TargetMachine)
	if targetMachine == nil {
		writeError(w, http.StatusBadRequest, "target_machine not found")
		return
	}
	if targetMachine.Status != "active" {
		writeError(w, http.StatusBadRequest, "target_machine is not active (status: "+targetMachine.Status+")")
		return
	}

	coord.store.SetUserStatus(userID, "migrating", "")
	go coord.MigrateUser(userID, req.SourceMachine, req.TargetMachine, "manual")

	slog.Info("Migration started", "user_id", userID, "source", req.SourceMachine, "target", req.TargetMachine)
	writeJSON(w, http.StatusAccepted, shared.MigrateUserResponse{
		UserID: userID,
		Status: "migrating",
	})
}

func (coord *Coordinator) handleGetMigrations(w http.ResponseWriter, r *http.Request) {
	events := coord.store.GetMigrationEvents()
	if events == nil {
		events = []MigrationEvent{}
	}
	writeJSON(w, http.StatusOK, events)
}

func (coord *Coordinator) handleDrainMachine(w http.ResponseWriter, r *http.Request) {
	machineID := r.PathValue("machine_id")

	machine := coord.store.GetMachine(machineID)
	if machine == nil {
		writeError(w, http.StatusNotFound, "machine not found")
		return
	}
	if machine.Status == "draining" {
		writeError(w, http.StatusConflict, "machine is already draining")
		return
	}
	if machine.Status != "active" {
		writeError(w, http.StatusConflict, "machine must be active to drain (current: "+machine.Status+")")
		return
	}

	// Check minimum fleet size — need at least 2 other active machines for bipod placement
	activeMachines := coord.store.GetActiveNonDrainingMachines()
	otherActive := 0
	for _, m := range activeMachines {
		if m.MachineID != machineID {
			otherActive++
		}
	}
	if otherActive < 2 {
		writeError(w, http.StatusConflict, "cannot drain: need at least 2 other active machines for bipod placement")
		return
	}

	// Count users to migrate
	userIDs := coord.store.GetUsersOnMachine(machineID)
	runningCount := 0
	for _, uid := range userIDs {
		u := coord.store.GetUser(uid)
		if u != nil && u.Status == "running" {
			runningCount++
		}
	}

	coord.store.SetMachineStatus(machineID, "draining")
	coord.startDrainGoroutine(machineID)

	slog.Info("Drain started", "machine_id", machineID, "users_to_migrate", runningCount)
	writeJSON(w, http.StatusAccepted, map[string]interface{}{
		"machine_id":       machineID,
		"status":           "draining",
		"users_to_migrate": runningCount,
	})
}

func (coord *Coordinator) handleUndrainMachine(w http.ResponseWriter, r *http.Request) {
	machineID := r.PathValue("machine_id")

	machine := coord.store.GetMachine(machineID)
	if machine == nil {
		writeError(w, http.StatusNotFound, "machine not found")
		return
	}
	if machine.Status != "draining" {
		writeError(w, http.StatusConflict, "machine is not draining (current: "+machine.Status+")")
		return
	}

	// Set status to active — the drain goroutine will see this and stop picking new users
	coord.store.SetMachineStatus(machineID, "active")

	// Wait for the drain goroutine to finish (including any in-flight migration)
	if doneCh, ok := coord.drainDone.Load(machineID); ok {
		slog.Info("Undrain: waiting for drain goroutine to finish", "machine_id", machineID)
		select {
		case <-doneCh.(chan struct{}):
			slog.Info("Undrain: drain goroutine finished", "machine_id", machineID)
		case <-time.After(10 * time.Minute):
			slog.Warn("Undrain: timed out waiting for drain goroutine", "machine_id", machineID)
		}
	}

	slog.Info("Undrain: machine returned to active", "machine_id", machineID)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"machine_id": machineID,
		"status":     "active",
	})
}

// startDrainGoroutine launches a drain goroutine with a done channel for synchronization.
func (coord *Coordinator) startDrainGoroutine(machineID string) {
	done := make(chan struct{})
	coord.drainDone.Store(machineID, done)
	go func() {
		defer func() {
			close(done)
			coord.drainDone.Delete(machineID)
		}()
		coord.DrainMachine(machineID)
	}()
}

// ─── Event query & system health endpoints (Layer 5.2) ───

// handleQueryEvents returns filtered events.
// Query params: type, since, user_id, machine_id, trigger, success, limit
func (coord *Coordinator) handleQueryEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := `SELECT event_id, timestamp, event_type, machine_id, user_id, operation_id, details FROM events WHERE 1=1`
	var args []interface{}
	argN := 1

	if v := q.Get("type"); v != "" {
		query += fmt.Sprintf(` AND event_type = $%d`, argN)
		args = append(args, v)
		argN++
	}
	if v := q.Get("since"); v != "" {
		query += fmt.Sprintf(` AND timestamp > $%d`, argN)
		args = append(args, v)
		argN++
	}
	if v := q.Get("user_id"); v != "" {
		query += fmt.Sprintf(` AND user_id = $%d`, argN)
		args = append(args, v)
		argN++
	}
	if v := q.Get("machine_id"); v != "" {
		query += fmt.Sprintf(` AND machine_id = $%d`, argN)
		args = append(args, v)
		argN++
	}
	if v := q.Get("trigger"); v != "" {
		query += fmt.Sprintf(` AND details->>'trigger' = $%d`, argN)
		args = append(args, v)
		argN++
	}
	if v := q.Get("success"); v != "" {
		query += fmt.Sprintf(` AND details->>'success' = $%d`, argN)
		args = append(args, v)
		argN++
	}

	query += ` ORDER BY timestamp ASC`

	limit := 500
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	query += fmt.Sprintf(` LIMIT $%d`, argN)
	args = append(args, limit)

	rows, err := coord.store.DB().Query(query, args...)
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

// handleCountEvents returns count of matching events.
// Query params: type, since, trigger, success, user_id, machine_id
func (coord *Coordinator) handleCountEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := `SELECT COUNT(*) FROM events WHERE 1=1`
	var args []interface{}
	argN := 1

	if v := q.Get("type"); v != "" {
		query += fmt.Sprintf(` AND event_type = $%d`, argN)
		args = append(args, v)
		argN++
	}
	if v := q.Get("since"); v != "" {
		query += fmt.Sprintf(` AND timestamp > $%d`, argN)
		args = append(args, v)
		argN++
	}
	if v := q.Get("trigger"); v != "" {
		query += fmt.Sprintf(` AND details->>'trigger' = $%d`, argN)
		args = append(args, v)
		argN++
	}
	if v := q.Get("success"); v != "" {
		query += fmt.Sprintf(` AND details->>'success' = $%d`, argN)
		args = append(args, v)
		argN++
	}
	if v := q.Get("user_id"); v != "" {
		query += fmt.Sprintf(` AND user_id = $%d`, argN)
		args = append(args, v)
		argN++
	}
	if v := q.Get("machine_id"); v != "" {
		query += fmt.Sprintf(` AND machine_id = $%d`, argN)
		args = append(args, v)
		argN++
	}

	var count int
	if err := coord.store.DB().QueryRow(query, args...).Scan(&count); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"count": count})
}

// handleSystemStable returns whether the system is in a stable state.
func (coord *Coordinator) handleSystemStable(w http.ResponseWriter, r *http.Request) {
	// Count in-progress operations
	var inProgressOps int
	coord.store.DB().QueryRow(`SELECT COUNT(*) FROM operations WHERE status = 'in_progress'`).Scan(&inProgressOps)

	// Count users in transient states
	transientStates := []string{"provisioning", "failing_over", "reforming", "suspending", "reactivating", "evicting", "migrating"}
	var transientUsers int
	for _, s := range transientStates {
		transientUsers += coord.store.CountUsersByStatus(s)
	}

	// Count draining machines
	drainingCount := len(coord.store.GetDrainingMachines())

	stable := inProgressOps == 0 && transientUsers == 0
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"stable":            stable,
		"in_progress_ops":   inProgressOps,
		"transient_users":   transientUsers,
		"draining_machines": drainingCount,
	})
}

// handleSystemConsistency runs server-side consistency checks.
func (coord *Coordinator) handleSystemConsistency(w http.ResponseWriter, r *http.Request) {
	type checkResult struct {
		Name   string `json:"name"`
		Pass   bool   `json:"pass"`
		Detail string `json:"detail,omitempty"`
	}
	var results []checkResult
	allPass := true

	addCheck := func(name string, pass bool, detail string) {
		results = append(results, checkResult{Name: name, Pass: pass, Detail: detail})
		if !pass {
			allPass = false
		}
	}

	db := coord.store.DB()

	// Check 1: Running users have exactly 2 non-stale bipods
	rows, _ := db.Query(`
		SELECT u.user_id, COUNT(b.machine_id)
		FROM users u
		LEFT JOIN bipods b ON u.user_id = b.user_id AND b.role != 'stale'
		WHERE u.status = 'running'
		GROUP BY u.user_id
		HAVING COUNT(b.machine_id) != 2
	`)
	var badBipodUsers []string
	if rows != nil {
		for rows.Next() {
			var uid string
			var cnt int
			rows.Scan(&uid, &cnt)
			badBipodUsers = append(badBipodUsers, fmt.Sprintf("%s(%d)", uid, cnt))
		}
		rows.Close()
	}
	if len(badBipodUsers) == 0 {
		addCheck("running_users_have_2_bipods", true, "")
	} else {
		addCheck("running_users_have_2_bipods", false, fmt.Sprintf("bad: %v", badBipodUsers))
	}

	// Check 2: No same-machine bipod pairs
	var dupCount int
	db.QueryRow(`SELECT COUNT(*) FROM (SELECT user_id, machine_id FROM bipods WHERE role != 'stale' GROUP BY user_id, machine_id HAVING COUNT(*) > 1) t`).Scan(&dupCount)
	addCheck("no_same_machine_bipods", dupCount == 0, fmt.Sprintf("duplicates: %d", dupCount))

	// Check 3: No duplicate DRBD ports
	var dupPorts int
	db.QueryRow(`SELECT COUNT(*) FROM (SELECT port FROM users WHERE status != 'evicted' AND port > 0 GROUP BY port HAVING COUNT(*) > 1) t`).Scan(&dupPorts)
	addCheck("no_duplicate_ports", dupPorts == 0, fmt.Sprintf("duplicate_ports: %d", dupPorts))

	// Check 4: No duplicate DRBD minors per machine
	var dupMinors int
	db.QueryRow(`SELECT COUNT(*) FROM (SELECT machine_id, minor FROM bipods WHERE role != 'stale' AND minor > 0 GROUP BY machine_id, minor HAVING COUNT(*) > 1) t`).Scan(&dupMinors)
	addCheck("no_duplicate_minors", dupMinors == 0, fmt.Sprintf("duplicate_minors: %d", dupMinors))

	// Check 5: No users stuck in transient states
	var stuckCount int
	db.QueryRow(`SELECT COUNT(*) FROM users WHERE status IN ('provisioning','failing_over','reforming','suspending','reactivating','evicting','migrating')`).Scan(&stuckCount)
	addCheck("no_stuck_transient_users", stuckCount == 0, fmt.Sprintf("stuck: %d", stuckCount))

	// Check 6: No operations stuck in_progress
	var stuckOps int
	db.QueryRow(`SELECT COUNT(*) FROM operations WHERE status = 'in_progress'`).Scan(&stuckOps)
	addCheck("no_stuck_operations", stuckOps == 0, fmt.Sprintf("stuck_ops: %d", stuckOps))

	// Check 7: Suspended users have 0 running containers (check via machine agents)
	var suspendedWithContainers []string
	suspendedUsers := coord.store.GetUsersByStatus("suspended")
	for _, u := range suspendedUsers {
		bipods := coord.store.GetBipods(u.UserID)
		for _, b := range bipods {
			if b.Role == "stale" {
				continue
			}
			m := coord.store.GetMachine(b.MachineID)
			if m == nil {
				continue
			}
			client := NewMachineClient(m.Address)
			status, err := client.ContainerStatus(u.UserID)
			if err == nil && status.Running {
				suspendedWithContainers = append(suspendedWithContainers, u.UserID)
				break
			}
		}
	}
	addCheck("suspended_no_containers", len(suspendedWithContainers) == 0,
		fmt.Sprintf("suspended_with_containers: %v", suspendedWithContainers))

	// Check 8: Evicted users have 0 non-stale bipods
	var evictedWithBipods int
	db.QueryRow(`SELECT COUNT(DISTINCT u.user_id) FROM users u JOIN bipods b ON u.user_id = b.user_id WHERE u.status = 'evicted' AND b.role != 'stale'`).Scan(&evictedWithBipods)
	addCheck("evicted_no_bipods", evictedWithBipods == 0, fmt.Sprintf("evicted_with_bipods: %d", evictedWithBipods))

	// Check 9: Running users have a running container on exactly one machine
	var runningNoContainer []string
	runningUsers := coord.store.GetUsersByStatus("running")
	for _, u := range runningUsers {
		bipods := coord.store.GetBipods(u.UserID)
		containerCount := 0
		for _, b := range bipods {
			if b.Role == "stale" {
				continue
			}
			m := coord.store.GetMachine(b.MachineID)
			if m == nil {
				continue
			}
			client := NewMachineClient(m.Address)
			status, err := client.ContainerStatus(u.UserID)
			if err == nil && status.Running {
				containerCount++
			}
		}
		if containerCount != 1 {
			runningNoContainer = append(runningNoContainer, fmt.Sprintf("%s(%d)", u.UserID, containerCount))
		}
	}
	addCheck("running_users_have_1_container", len(runningNoContainer) == 0,
		fmt.Sprintf("bad: %v", runningNoContainer))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"pass":   allPass,
		"checks": results,
	})
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
