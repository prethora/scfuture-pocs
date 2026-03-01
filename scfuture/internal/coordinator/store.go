package coordinator

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	_ "github.com/lib/pq"
	"scfuture/internal/shared"
)

// ─── Event types (unified into single events table) ───

type FailoverEvent struct {
	UserID      string    `json:"user_id"`
	FromMachine string    `json:"from_machine"`
	ToMachine   string    `json:"to_machine"`
	Type        string    `json:"type"`
	Success     bool      `json:"success"`
	Error       string    `json:"error,omitempty"`
	DurationMS  int64     `json:"duration_ms"`
	Timestamp   time.Time `json:"timestamp"`
}

type ReformationEvent struct {
	UserID       string    `json:"user_id"`
	OldSecondary string    `json:"old_secondary"`
	NewSecondary string    `json:"new_secondary"`
	Success      bool      `json:"success"`
	Error        string    `json:"error,omitempty"`
	Method       string    `json:"method,omitempty"`
	DurationMS   int64     `json:"duration_ms"`
	Timestamp    time.Time `json:"timestamp"`
}

type LifecycleEvent struct {
	UserID     string    `json:"user_id"`
	Type       string    `json:"type"`
	Success    bool      `json:"success"`
	Error      string    `json:"error,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	Timestamp  time.Time `json:"timestamp"`
}

// ─── Operation tracking (crash recovery) ───

type Operation struct {
	OperationID string
	Type        string
	UserID      string
	Status      string // "in_progress", "complete", "failed", "cancelled"
	CurrentStep string
	Metadata    map[string]interface{}
	StartedAt   time.Time
	CompletedAt *time.Time
	Error       string
}

// ─── Core data types (unchanged public interface) ───

type Machine struct {
	MachineID       string    `json:"machine_id"`
	Address         string    `json:"address"`
	PublicAddress   string    `json:"public_address"`
	Status          string    `json:"status"`
	StatusChangedAt time.Time `json:"status_changed_at"`
	DiskTotalMB     int64     `json:"disk_total_mb"`
	DiskUsedMB      int64     `json:"disk_used_mb"`
	RAMTotalMB      int64     `json:"ram_total_mb"`
	RAMUsedMB       int64     `json:"ram_used_mb"`
	ActiveAgents    int       `json:"active_agents"`
	MaxAgents       int       `json:"max_agents"`
	RunningAgents   []string  `json:"running_agents"` // ephemeral, NOT in Postgres
	LastHeartbeat   time.Time `json:"last_heartbeat"`
}

type User struct {
	UserID           string    `json:"user_id"`
	Status           string    `json:"status"`
	StatusChangedAt  time.Time `json:"status_changed_at"`
	PrimaryMachine   string    `json:"primary_machine"`
	DRBDPort         int       `json:"drbd_port"`
	ImageSizeMB      int       `json:"image_size_mb"`
	Error            string    `json:"error"`
	CreatedAt        time.Time `json:"created_at"`
	BackupExists     bool      `json:"backup_exists"`
	BackupPath       string    `json:"backup_path,omitempty"`
	BackupBucket     string    `json:"backup_bucket,omitempty"`
	BackupTimestamp  time.Time `json:"backup_timestamp,omitempty"`
	DRBDDisconnected bool      `json:"drbd_disconnected"`
}

type Bipod struct {
	UserID     string `json:"user_id"`
	MachineID  string `json:"machine_id"`
	Role       string `json:"role"`
	DRBDMinor  int    `json:"drbd_minor"`
	LoopDevice string `json:"loop_device"`
}

type Store struct {
	db *sql.DB
	mu sync.RWMutex

	// In-memory cache (populated from Postgres on startup)
	machines map[string]*Machine
	users    map[string]*User
	bipods   map[string]*Bipod // keyed by "{userID}:{machineID}"
}

func NewStore(databaseURL string) (*Store, error) {
	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}

	s := &Store{
		db:       db,
		machines: make(map[string]*Machine),
		users:    make(map[string]*User),
		bipods:   make(map[string]*Bipod),
	}

	if err := s.migrate(); err != nil {
		return nil, fmt.Errorf("migrate schema: %w", err)
	}

	if err := s.loadCache(); err != nil {
		return nil, fmt.Errorf("load cache: %w", err)
	}

	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// AcquireAdvisoryLock attempts to acquire a Postgres advisory lock.
// Retries for up to 30 seconds to handle stale locks from crashed instances
// (especially when using connection poolers like pgbouncer/Supabase).
func (s *Store) AcquireAdvisoryLock() error {
	// First attempt
	var acquired bool
	err := s.db.QueryRow("SELECT pg_try_advisory_lock(12345)").Scan(&acquired)
	if err != nil {
		return fmt.Errorf("advisory lock query failed: %w", err)
	}
	if acquired {
		slog.Info("Advisory lock acquired — this coordinator is the active instance")
		return nil
	}

	// Lock is held — likely a stale session from a previous crash (pgbouncer keeps
	// the old backend alive). Terminate the backend holding the lock and retry.
	slog.Warn("Advisory lock held by another session, attempting to terminate stale backend...")
	var stalePID sql.NullInt64
	err = s.db.QueryRow(`
		SELECT pid FROM pg_locks
		WHERE locktype = 'advisory' AND objid = 12345 AND granted = true
		LIMIT 1
	`).Scan(&stalePID)
	if err == nil && stalePID.Valid {
		slog.Warn("Found stale backend holding advisory lock", "pid", stalePID.Int64)
		_, _ = s.db.Exec("SELECT pg_terminate_backend($1)", stalePID.Int64)
		time.Sleep(2 * time.Second)
	}

	// Retry after termination
	deadline := time.Now().Add(30 * time.Second)
	for {
		err = s.db.QueryRow("SELECT pg_try_advisory_lock(12345)").Scan(&acquired)
		if err != nil {
			return fmt.Errorf("advisory lock query failed: %w", err)
		}
		if acquired {
			slog.Info("Advisory lock acquired after terminating stale backend")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("another coordinator is already running (advisory lock held after 30s)")
		}
		slog.Warn("Advisory lock still held, retrying in 2s...")
		time.Sleep(2 * time.Second)
	}
}

// migrate runs the schema DDL (CREATE TABLE IF NOT EXISTS).
func (s *Store) migrate() error {
	schema := `
	CREATE TABLE IF NOT EXISTS machines (
		machine_id TEXT PRIMARY KEY, address TEXT NOT NULL DEFAULT '',
		public_address TEXT DEFAULT '', status TEXT NOT NULL DEFAULT 'active',
		status_changed_at TIMESTAMPTZ DEFAULT NOW(),
		disk_total_mb BIGINT DEFAULT 0, disk_used_mb BIGINT DEFAULT 0,
		ram_total_mb BIGINT DEFAULT 0, ram_used_mb BIGINT DEFAULT 0,
		active_agents INTEGER DEFAULT 0, max_agents INTEGER DEFAULT 10,
		last_heartbeat TIMESTAMPTZ DEFAULT NOW()
	);
	CREATE TABLE IF NOT EXISTS users (
		user_id TEXT PRIMARY KEY, status TEXT NOT NULL DEFAULT 'registered',
		status_changed_at TIMESTAMPTZ DEFAULT NOW(), primary_machine TEXT DEFAULT '',
		drbd_port INTEGER UNIQUE, image_size_mb INTEGER DEFAULT 512,
		error TEXT DEFAULT '', created_at TIMESTAMPTZ DEFAULT NOW(),
		backup_exists BOOLEAN DEFAULT FALSE, backup_path TEXT DEFAULT '',
		backup_bucket TEXT DEFAULT '', backup_timestamp TIMESTAMPTZ,
		drbd_disconnected BOOLEAN DEFAULT FALSE
	);
	CREATE TABLE IF NOT EXISTS bipods (
		user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
		machine_id TEXT NOT NULL, role TEXT NOT NULL DEFAULT 'primary',
		drbd_minor INTEGER DEFAULT 0, loop_device TEXT DEFAULT '',
		created_at TIMESTAMPTZ DEFAULT NOW(),
		PRIMARY KEY (user_id, machine_id)
	);
	CREATE TABLE IF NOT EXISTS operations (
		operation_id TEXT PRIMARY KEY, type TEXT NOT NULL,
		user_id TEXT NOT NULL REFERENCES users(user_id) ON DELETE CASCADE,
		status TEXT NOT NULL DEFAULT 'in_progress', current_step TEXT DEFAULT '',
		metadata JSONB DEFAULT '{}', started_at TIMESTAMPTZ DEFAULT NOW(),
		completed_at TIMESTAMPTZ, error TEXT DEFAULT ''
	);
	CREATE TABLE IF NOT EXISTS events (
		event_id SERIAL PRIMARY KEY, timestamp TIMESTAMPTZ DEFAULT NOW(),
		event_type TEXT NOT NULL, machine_id TEXT, user_id TEXT,
		operation_id TEXT, details JSONB DEFAULT '{}'
	);
	CREATE INDEX IF NOT EXISTS idx_bipods_machine ON bipods(machine_id);
	CREATE INDEX IF NOT EXISTS idx_bipods_user ON bipods(user_id);
	CREATE INDEX IF NOT EXISTS idx_operations_status ON operations(status);
	CREATE INDEX IF NOT EXISTS idx_operations_user ON operations(user_id);
	CREATE INDEX IF NOT EXISTS idx_events_type ON events(event_type);
	CREATE INDEX IF NOT EXISTS idx_events_timestamp ON events(timestamp);
	`
	_, err := s.db.Exec(schema)
	return err
}

// loadCache populates in-memory maps from Postgres.
func (s *Store) loadCache() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Load machines
	rows, err := s.db.Query(`SELECT machine_id, address, public_address, status, status_changed_at, disk_total_mb, disk_used_mb, ram_total_mb, ram_used_mb, active_agents, max_agents, last_heartbeat FROM machines`)
	if err != nil {
		return fmt.Errorf("load machines: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		m := &Machine{}
		if err := rows.Scan(&m.MachineID, &m.Address, &m.PublicAddress, &m.Status, &m.StatusChangedAt, &m.DiskTotalMB, &m.DiskUsedMB, &m.RAMTotalMB, &m.RAMUsedMB, &m.ActiveAgents, &m.MaxAgents, &m.LastHeartbeat); err != nil {
			return fmt.Errorf("scan machine: %w", err)
		}
		s.machines[m.MachineID] = m
	}

	// Load users
	rows2, err := s.db.Query(`SELECT user_id, status, status_changed_at, primary_machine, drbd_port, image_size_mb, error, created_at, backup_exists, backup_path, backup_bucket, backup_timestamp, drbd_disconnected FROM users`)
	if err != nil {
		return fmt.Errorf("load users: %w", err)
	}
	defer rows2.Close()
	for rows2.Next() {
		u := &User{}
		var backupTimestamp sql.NullTime
		var drbdPort sql.NullInt64
		if err := rows2.Scan(&u.UserID, &u.Status, &u.StatusChangedAt, &u.PrimaryMachine, &drbdPort, &u.ImageSizeMB, &u.Error, &u.CreatedAt, &u.BackupExists, &u.BackupPath, &u.BackupBucket, &backupTimestamp, &u.DRBDDisconnected); err != nil {
			return fmt.Errorf("scan user: %w", err)
		}
		if drbdPort.Valid {
			u.DRBDPort = int(drbdPort.Int64)
		}
		if backupTimestamp.Valid {
			u.BackupTimestamp = backupTimestamp.Time
		}
		s.users[u.UserID] = u
	}

	// Load bipods
	rows3, err := s.db.Query(`SELECT user_id, machine_id, role, drbd_minor, loop_device FROM bipods`)
	if err != nil {
		return fmt.Errorf("load bipods: %w", err)
	}
	defer rows3.Close()
	for rows3.Next() {
		b := &Bipod{}
		if err := rows3.Scan(&b.UserID, &b.MachineID, &b.Role, &b.DRBDMinor, &b.LoopDevice); err != nil {
			return fmt.Errorf("scan bipod: %w", err)
		}
		key := b.UserID + ":" + b.MachineID
		s.bipods[key] = b
	}

	slog.Info("Cache loaded from Postgres",
		"machines", len(s.machines),
		"users", len(s.users),
		"bipods", len(s.bipods),
	)
	return nil
}

// ─── Machine methods (write to Postgres, update cache) ───

func (s *Store) RegisterMachine(req *shared.FleetRegisterRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	_, err := s.db.Exec(`
		INSERT INTO machines (machine_id, address, status, disk_total_mb, disk_used_mb, ram_total_mb, ram_used_mb, max_agents, last_heartbeat)
		VALUES ($1, $2, 'active', $3, $4, $5, $6, $7, $8)
		ON CONFLICT (machine_id) DO UPDATE SET
			address = EXCLUDED.address, disk_total_mb = EXCLUDED.disk_total_mb,
			disk_used_mb = EXCLUDED.disk_used_mb, ram_total_mb = EXCLUDED.ram_total_mb,
			ram_used_mb = EXCLUDED.ram_used_mb, max_agents = EXCLUDED.max_agents,
			last_heartbeat = EXCLUDED.last_heartbeat, status = 'active'`,
		req.MachineID, req.Address, req.DiskTotalMB, req.DiskUsedMB, req.RAMTotalMB, req.RAMUsedMB, req.MaxAgents, now)
	if err != nil {
		slog.Error("Failed to register machine in DB", "error", err)
	}

	m, exists := s.machines[req.MachineID]
	if !exists {
		m = &Machine{MachineID: req.MachineID, Status: "active"}
		s.machines[req.MachineID] = m
	}
	m.Address = req.Address
	m.Status = "active"
	m.DiskTotalMB = req.DiskTotalMB
	m.DiskUsedMB = req.DiskUsedMB
	m.RAMTotalMB = req.RAMTotalMB
	m.RAMUsedMB = req.RAMUsedMB
	m.MaxAgents = req.MaxAgents
	m.LastHeartbeat = now

	slog.Info("Machine registered", "machine_id", req.MachineID, "address", req.Address)
}

func (s *Store) UpdateHeartbeat(req *shared.FleetHeartbeatRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()

	m, ok := s.machines[req.MachineID]
	if !ok {
		slog.Warn("Heartbeat from unknown machine", "machine_id", req.MachineID)
		return
	}

	now := time.Now()

	// Resurrection: machine came back from dead/suspect
	if m.Status == "dead" || m.Status == "suspect" {
		slog.Info("[HEALTH] Machine resurrected", "machine_id", req.MachineID, "was", m.Status)
		m.Status = "active"
		m.StatusChangedAt = now
		s.db.Exec(`UPDATE machines SET status='active', status_changed_at=$1 WHERE machine_id=$2`, now, req.MachineID)
	}

	m.DiskTotalMB = req.DiskTotalMB
	m.DiskUsedMB = req.DiskUsedMB
	m.RAMTotalMB = req.RAMTotalMB
	m.RAMUsedMB = req.RAMUsedMB
	m.ActiveAgents = req.ActiveAgents
	m.RunningAgents = req.RunningAgents
	m.LastHeartbeat = now

	s.db.Exec(`UPDATE machines SET disk_total_mb=$1, disk_used_mb=$2, ram_total_mb=$3, ram_used_mb=$4, active_agents=$5, last_heartbeat=$6 WHERE machine_id=$7`,
		req.DiskTotalMB, req.DiskUsedMB, req.RAMTotalMB, req.RAMUsedMB, req.ActiveAgents, now, req.MachineID)
}

func (s *Store) GetMachine(id string) *Machine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.machines[id]
}

func (s *Store) AllMachines() []*Machine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*Machine, 0, len(s.machines))
	for _, m := range s.machines {
		clone := *m
		result = append(result, &clone)
	}
	return result
}

func (s *Store) SetMachineStatus(machineID, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	m, ok := s.machines[machineID]
	if !ok {
		return
	}
	m.Status = status
	m.StatusChangedAt = now
	s.db.Exec(`UPDATE machines SET status=$1, status_changed_at=$2 WHERE machine_id=$3`, status, now, machineID)
}

// ─── User methods ───

func (s *Store) CreateUser(userID string, imageSizeMB int) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.users[userID]; exists {
		return nil, fmt.Errorf("user %q already exists", userID)
	}
	if imageSizeMB <= 0 {
		imageSizeMB = 512
	}

	now := time.Now()
	_, err := s.db.Exec(`INSERT INTO users (user_id, status, image_size_mb, created_at, status_changed_at) VALUES ($1, 'registered', $2, $3, $3)`,
		userID, imageSizeMB, now)
	if err != nil {
		return nil, fmt.Errorf("insert user: %w", err)
	}

	u := &User{UserID: userID, Status: "registered", ImageSizeMB: imageSizeMB, CreatedAt: now, StatusChangedAt: now}
	s.users[userID] = u
	clone := *u
	return &clone, nil
}

func (s *Store) GetUser(userID string) *User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[userID]
	if !ok {
		return nil
	}
	clone := *u
	return &clone
}

func (s *Store) AllUsers() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		clone := *u
		result = append(result, &clone)
	}
	return result
}

func (s *Store) SetUserStatus(userID, status, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return
	}
	now := time.Now()
	u.Status = status
	u.StatusChangedAt = now
	u.Error = errMsg
	s.db.Exec(`UPDATE users SET status=$1, status_changed_at=$2, error=$3 WHERE user_id=$4`, status, now, errMsg, userID)
}

func (s *Store) SetUserPrimary(userID, machineID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return
	}
	u.PrimaryMachine = machineID
	s.db.Exec(`UPDATE users SET primary_machine=$1 WHERE user_id=$2`, machineID, userID)
}

func (s *Store) SetUserPort(userID string, port int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return
	}
	u.DRBDPort = port
	s.db.Exec(`UPDATE users SET drbd_port=$1 WHERE user_id=$2`, port, userID)
}

func (s *Store) SetUserBackup(userID, b2Path, bucketName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return
	}
	now := time.Now()
	u.BackupExists = true
	u.BackupPath = b2Path
	u.BackupBucket = bucketName
	u.BackupTimestamp = now
	s.db.Exec(`UPDATE users SET backup_exists=true, backup_path=$1, backup_bucket=$2, backup_timestamp=$3 WHERE user_id=$4`, b2Path, bucketName, now, userID)
}

func (s *Store) SetUserDRBDDisconnected(userID string, disconnected bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return
	}
	u.DRBDDisconnected = disconnected
	s.db.Exec(`UPDATE users SET drbd_disconnected=$1 WHERE user_id=$2`, disconnected, userID)
}

// ─── Bipod methods ───

func (s *Store) CreateBipod(userID, machineID, role string, minor int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := userID + ":" + machineID
	s.bipods[key] = &Bipod{UserID: userID, MachineID: machineID, Role: role, DRBDMinor: minor}
	s.db.Exec(`INSERT INTO bipods (user_id, machine_id, role, drbd_minor) VALUES ($1, $2, $3, $4) ON CONFLICT (user_id, machine_id) DO UPDATE SET role=EXCLUDED.role, drbd_minor=EXCLUDED.drbd_minor`,
		userID, machineID, role, minor)
}

func (s *Store) SetBipodLoopDevice(userID, machineID, loopDev string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := userID + ":" + machineID
	b, ok := s.bipods[key]
	if !ok {
		return
	}
	b.LoopDevice = loopDev
	s.db.Exec(`UPDATE bipods SET loop_device=$1 WHERE user_id=$2 AND machine_id=$3`, loopDev, userID, machineID)
}

func (s *Store) SetBipodRole(userID, machineID, role string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := userID + ":" + machineID
	b, ok := s.bipods[key]
	if !ok {
		return
	}
	b.Role = role
	s.db.Exec(`UPDATE bipods SET role=$1 WHERE user_id=$2 AND machine_id=$3`, role, userID, machineID)
}

func (s *Store) GetBipods(userID string) []*Bipod {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Bipod
	for _, b := range s.bipods {
		if b.UserID == userID {
			clone := *b
			result = append(result, &clone)
		}
	}
	return result
}

func (s *Store) RemoveBipod(userID, machineID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := userID + ":" + machineID
	delete(s.bipods, key)
	s.db.Exec(`DELETE FROM bipods WHERE user_id=$1 AND machine_id=$2`, userID, machineID)
}

func (s *Store) ClearUserBipods(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, b := range s.bipods {
		if b.UserID == userID {
			delete(s.bipods, key)
		}
	}
	if u, ok := s.users[userID]; ok {
		u.PrimaryMachine = ""
	}
	s.db.Exec(`DELETE FROM bipods WHERE user_id=$1`, userID)
	s.db.Exec(`UPDATE users SET primary_machine='' WHERE user_id=$1`, userID)
}

// ─── Port and minor allocation (derived from DB) ───

func (s *Store) AllocatePort() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	var maxPort sql.NullInt64
	s.db.QueryRow(`SELECT MAX(drbd_port) FROM users WHERE drbd_port IS NOT NULL`).Scan(&maxPort)
	nextPort := 7900
	if maxPort.Valid && int(maxPort.Int64) >= nextPort {
		nextPort = int(maxPort.Int64) + 1
	}
	return nextPort
}

func (s *Store) AllocateMinor(machineID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	var maxMinor sql.NullInt64
	s.db.QueryRow(`SELECT MAX(drbd_minor) FROM bipods WHERE machine_id=$1 AND role != 'stale'`, machineID).Scan(&maxMinor)
	nextMinor := 0
	if maxMinor.Valid {
		nextMinor = int(maxMinor.Int64) + 1
	}
	return nextMinor
}

func (s *Store) SelectMachines() (primary *Machine, secondary *Machine, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var candidates []*Machine
	for _, m := range s.machines {
		if m.Status != "active" {
			continue
		}
		if m.DiskTotalMB > 0 && m.DiskUsedMB > int64(float64(m.DiskTotalMB)*0.85) {
			continue
		}
		candidates = append(candidates, m)
	}
	if len(candidates) < 2 {
		return nil, nil, fmt.Errorf("need at least 2 active machines, have %d", len(candidates))
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ActiveAgents < candidates[j].ActiveAgents
	})

	candidates[0].ActiveAgents++
	candidates[1].ActiveAgents++
	s.db.Exec(`UPDATE machines SET active_agents=$1 WHERE machine_id=$2`, candidates[0].ActiveAgents, candidates[0].MachineID)
	s.db.Exec(`UPDATE machines SET active_agents=$1 WHERE machine_id=$2`, candidates[1].ActiveAgents, candidates[1].MachineID)

	p := *candidates[0]
	sec := *candidates[1]
	return &p, &sec, nil
}

func (s *Store) SelectOneSecondary(excludeMachineIDs []string) (*Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	exclude := make(map[string]bool)
	for _, id := range excludeMachineIDs {
		exclude[id] = true
	}

	var candidates []*Machine
	for _, m := range s.machines {
		if m.Status != "active" || exclude[m.MachineID] {
			continue
		}
		if m.DiskTotalMB > 0 && m.DiskUsedMB > int64(float64(m.DiskTotalMB)*0.85) {
			continue
		}
		candidates = append(candidates, m)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no available active machine (excluding %v)", excludeMachineIDs)
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ActiveAgents < candidates[j].ActiveAgents
	})

	candidates[0].ActiveAgents++
	s.db.Exec(`UPDATE machines SET active_agents=$1 WHERE machine_id=$2`, candidates[0].ActiveAgents, candidates[0].MachineID)
	result := *candidates[0]
	return &result, nil
}

// ─── Health and query methods ───

func (s *Store) CheckMachineHealth(suspectThreshold, deadThreshold time.Duration) []string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var newlyDead []string
	now := time.Now()

	for id, m := range s.machines {
		elapsed := now.Sub(m.LastHeartbeat)
		var newStatus string

		switch {
		case elapsed > deadThreshold:
			newStatus = "dead"
		case elapsed > suspectThreshold:
			newStatus = "suspect"
		default:
			newStatus = "active"
		}

		if newStatus != m.Status {
			oldStatus := m.Status
			m.Status = newStatus
			m.StatusChangedAt = now
			slog.Info("[HEALTH] Machine status changed", "machine_id", id, "from", oldStatus, "to", newStatus, "last_heartbeat_ago", elapsed.String())
			s.db.Exec(`UPDATE machines SET status=$1, status_changed_at=$2 WHERE machine_id=$3`, newStatus, now, id)
			if newStatus == "dead" {
				newlyDead = append(newlyDead, id)
			}
		}
	}
	return newlyDead
}

func (s *Store) GetUsersOnMachine(machineID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := make(map[string]bool)
	var userIDs []string
	for _, b := range s.bipods {
		if b.MachineID == machineID && !seen[b.UserID] {
			seen[b.UserID] = true
			userIDs = append(userIDs, b.UserID)
		}
	}
	return userIDs
}

func (s *Store) GetSurvivingBipod(userID, deadMachineID string) *Bipod {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.bipods {
		if b.UserID == userID && b.MachineID != deadMachineID && b.Role != "stale" {
			clone := *b
			return &clone
		}
	}
	return nil
}

func (s *Store) GetDegradedUsers(stabilizationPeriod time.Duration) []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*User
	now := time.Now()
	for _, u := range s.users {
		if u.Status == "running_degraded" && now.Sub(u.StatusChangedAt) > stabilizationPeriod {
			clone := *u
			result = append(result, &clone)
		}
	}
	return result
}

func (s *Store) GetStaleBipodsOnActiveMachines(userID string) []*Bipod {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Bipod
	for _, b := range s.bipods {
		if b.UserID == userID && b.Role == "stale" {
			m, ok := s.machines[b.MachineID]
			if ok && m.Status == "active" {
				clone := *b
				result = append(result, &clone)
			}
		}
	}
	return result
}

func (s *Store) GetAllStaleBipodsOnActiveMachines() []*Bipod {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*Bipod
	for _, b := range s.bipods {
		if b.Role == "stale" {
			m, ok := s.machines[b.MachineID]
			if ok && m.Status == "active" {
				clone := *b
				result = append(result, &clone)
			}
		}
	}
	return result
}

func (s *Store) GetSuspendedUsers() []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []*User
	for _, u := range s.users {
		if u.Status == "suspended" {
			clone := *u
			result = append(result, &clone)
		}
	}
	return result
}

// ─── Operation tracking methods (new for Layer 4.6) ───

func (s *Store) CreateOperation(opID, opType, userID string, metadata map[string]interface{}) error {
	metaJSON, _ := json.Marshal(metadata)
	_, err := s.db.Exec(`INSERT INTO operations (operation_id, type, user_id, status, metadata, started_at) VALUES ($1, $2, $3, 'in_progress', $4, NOW())`,
		opID, opType, userID, metaJSON)
	return err
}

func (s *Store) UpdateOperationStep(opID, step string) {
	s.db.Exec(`UPDATE operations SET current_step=$1 WHERE operation_id=$2`, step, opID)
}

func (s *Store) CompleteOperation(opID string) {
	s.db.Exec(`UPDATE operations SET status='complete', completed_at=NOW() WHERE operation_id=$1`, opID)
}

func (s *Store) FailOperation(opID, errMsg string) {
	s.db.Exec(`UPDATE operations SET status='failed', error=$1, completed_at=NOW() WHERE operation_id=$2`, errMsg, opID)
}

func (s *Store) CancelOperation(opID string) {
	s.db.Exec(`UPDATE operations SET status='cancelled', completed_at=NOW() WHERE operation_id=$1`, opID)
}

func (s *Store) GetIncompleteOperations() ([]*Operation, error) {
	rows, err := s.db.Query(`SELECT operation_id, type, user_id, status, current_step, metadata, started_at, completed_at, error FROM operations WHERE status IN ('in_progress', 'pending') ORDER BY started_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ops []*Operation
	for rows.Next() {
		op := &Operation{}
		var metaJSON []byte
		var completedAt sql.NullTime
		if err := rows.Scan(&op.OperationID, &op.Type, &op.UserID, &op.Status, &op.CurrentStep, &metaJSON, &op.StartedAt, &completedAt, &op.Error); err != nil {
			return nil, err
		}
		if completedAt.Valid {
			op.CompletedAt = &completedAt.Time
		}
		op.Metadata = make(map[string]interface{})
		json.Unmarshal(metaJSON, &op.Metadata)
		ops = append(ops, op)
	}
	return ops, nil
}

// ─── Unified event recording ───

func (s *Store) RecordEvent(eventType string, machineID, userID, operationID string, details map[string]interface{}) {
	detailsJSON, _ := json.Marshal(details)
	s.db.Exec(`INSERT INTO events (event_type, machine_id, user_id, operation_id, details) VALUES ($1, $2, $3, $4, $5)`,
		eventType, machineID, userID, operationID, detailsJSON)
}

func (s *Store) RecordFailoverEvent(event FailoverEvent) {
	details := map[string]interface{}{
		"from_machine": event.FromMachine,
		"to_machine":   event.ToMachine,
		"type":         event.Type,
		"success":      event.Success,
		"error":        event.Error,
		"duration_ms":  event.DurationMS,
	}
	s.RecordEvent("failover", event.FromMachine, event.UserID, "", details)
}

func (s *Store) GetFailoverEvents() []FailoverEvent {
	rows, err := s.db.Query(`SELECT user_id, details, timestamp FROM events WHERE event_type='failover' ORDER BY timestamp`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []FailoverEvent
	for rows.Next() {
		var userID string
		var detailsJSON []byte
		var ts time.Time
		rows.Scan(&userID, &detailsJSON, &ts)
		var details map[string]interface{}
		json.Unmarshal(detailsJSON, &details)
		fe := FailoverEvent{UserID: userID, Timestamp: ts}
		if v, ok := details["from_machine"].(string); ok {
			fe.FromMachine = v
		}
		if v, ok := details["to_machine"].(string); ok {
			fe.ToMachine = v
		}
		if v, ok := details["type"].(string); ok {
			fe.Type = v
		}
		if v, ok := details["success"].(bool); ok {
			fe.Success = v
		}
		if v, ok := details["error"].(string); ok {
			fe.Error = v
		}
		if v, ok := details["duration_ms"].(float64); ok {
			fe.DurationMS = int64(v)
		}
		result = append(result, fe)
	}
	return result
}

func (s *Store) RecordReformationEvent(event ReformationEvent) {
	details := map[string]interface{}{
		"old_secondary": event.OldSecondary,
		"new_secondary": event.NewSecondary,
		"success":       event.Success,
		"error":         event.Error,
		"method":        event.Method,
		"duration_ms":   event.DurationMS,
	}
	s.RecordEvent("reformation", "", event.UserID, "", details)
}

func (s *Store) GetReformationEvents() []ReformationEvent {
	rows, err := s.db.Query(`SELECT user_id, details, timestamp FROM events WHERE event_type='reformation' ORDER BY timestamp`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []ReformationEvent
	for rows.Next() {
		var userID string
		var detailsJSON []byte
		var ts time.Time
		rows.Scan(&userID, &detailsJSON, &ts)
		var details map[string]interface{}
		json.Unmarshal(detailsJSON, &details)
		re := ReformationEvent{UserID: userID, Timestamp: ts}
		if v, ok := details["old_secondary"].(string); ok {
			re.OldSecondary = v
		}
		if v, ok := details["new_secondary"].(string); ok {
			re.NewSecondary = v
		}
		if v, ok := details["success"].(bool); ok {
			re.Success = v
		}
		if v, ok := details["error"].(string); ok {
			re.Error = v
		}
		if v, ok := details["method"].(string); ok {
			re.Method = v
		}
		if v, ok := details["duration_ms"].(float64); ok {
			re.DurationMS = int64(v)
		}
		result = append(result, re)
	}
	return result
}

func (s *Store) RecordLifecycleEvent(event LifecycleEvent) {
	details := map[string]interface{}{
		"type":        event.Type,
		"success":     event.Success,
		"error":       event.Error,
		"duration_ms": event.DurationMS,
	}
	s.RecordEvent("lifecycle_"+event.Type, "", event.UserID, "", details)
}

func (s *Store) GetLifecycleEvents() []LifecycleEvent {
	rows, err := s.db.Query(`SELECT user_id, event_type, details, timestamp FROM events WHERE event_type LIKE 'lifecycle_%' ORDER BY timestamp`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []LifecycleEvent
	for rows.Next() {
		var userID, eventType string
		var detailsJSON []byte
		var ts time.Time
		rows.Scan(&userID, &eventType, &detailsJSON, &ts)
		var details map[string]interface{}
		json.Unmarshal(detailsJSON, &details)
		le := LifecycleEvent{UserID: userID, Timestamp: ts}
		if v, ok := details["type"].(string); ok {
			le.Type = v
		}
		if v, ok := details["success"].(bool); ok {
			le.Success = v
		}
		if v, ok := details["error"].(string); ok {
			le.Error = v
		}
		if v, ok := details["duration_ms"].(float64); ok {
			le.DurationMS = int64(v)
		}
		result = append(result, le)
	}
	return result
}

// ─── Direct DB access for reconciliation ───

func (s *Store) DB() *sql.DB {
	return s.db
}

// ReloadCache refreshes the in-memory cache from Postgres.
// Used by reconciliation after making direct DB changes.
func (s *Store) ReloadCache() error {
	s.mu.Lock()
	s.machines = make(map[string]*Machine)
	s.users = make(map[string]*User)
	s.bipods = make(map[string]*Bipod)
	s.mu.Unlock()
	return s.loadCache()
}
