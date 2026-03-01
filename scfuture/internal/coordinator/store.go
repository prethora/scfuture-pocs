package coordinator

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"scfuture/internal/shared"
)

type Store struct {
	mu       sync.RWMutex
	machines map[string]*Machine
	users    map[string]*User
	bipods   map[string]*Bipod // keyed by "{userID}:{machineID}"

	nextPort  int            // next DRBD port to allocate (starts at 7900)
	nextMinor map[string]int // per-machine next DRBD minor (starts at 0)

	dataDir string
}

type Machine struct {
	MachineID     string    `json:"machine_id"`
	Address       string    `json:"address"`
	PublicAddress string    `json:"public_address"`
	Status        string    `json:"status"`
	DiskTotalMB   int64     `json:"disk_total_mb"`
	DiskUsedMB    int64     `json:"disk_used_mb"`
	RAMTotalMB    int64     `json:"ram_total_mb"`
	RAMUsedMB     int64     `json:"ram_used_mb"`
	ActiveAgents  int       `json:"active_agents"`
	MaxAgents     int       `json:"max_agents"`
	RunningAgents []string  `json:"running_agents"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

type User struct {
	UserID         string    `json:"user_id"`
	Status         string    `json:"status"`
	PrimaryMachine string    `json:"primary_machine"`
	DRBDPort       int       `json:"drbd_port"`
	ImageSizeMB    int       `json:"image_size_mb"`
	Error          string    `json:"error"`
	CreatedAt      time.Time `json:"created_at"`
}

type Bipod struct {
	UserID     string `json:"user_id"`
	MachineID  string `json:"machine_id"`
	Role       string `json:"role"`
	DRBDMinor  int    `json:"drbd_minor"`
	LoopDevice string `json:"loop_device"`
}

func NewStore(dataDir string) *Store {
	return &Store{
		machines:  make(map[string]*Machine),
		users:     make(map[string]*User),
		bipods:    make(map[string]*Bipod),
		nextPort:  7900,
		nextMinor: make(map[string]int),
		dataDir:   dataDir,
	}
}

func (s *Store) RegisterMachine(req *shared.FleetRegisterRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()

	m, exists := s.machines[req.MachineID]
	if !exists {
		m = &Machine{
			MachineID: req.MachineID,
			Status:    "active",
		}
		s.machines[req.MachineID] = m
	}
	m.Address = req.Address
	m.DiskTotalMB = req.DiskTotalMB
	m.DiskUsedMB = req.DiskUsedMB
	m.RAMTotalMB = req.RAMTotalMB
	m.RAMUsedMB = req.RAMUsedMB
	m.MaxAgents = req.MaxAgents
	m.LastHeartbeat = time.Now()

	slog.Info("Machine registered", "machine_id", req.MachineID, "address", req.Address)
	s.persist()
}

func (s *Store) UpdateHeartbeat(req *shared.FleetHeartbeatRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()

	m, ok := s.machines[req.MachineID]
	if !ok {
		slog.Warn("Heartbeat from unknown machine", "machine_id", req.MachineID)
		return
	}
	m.DiskTotalMB = req.DiskTotalMB
	m.DiskUsedMB = req.DiskUsedMB
	m.RAMTotalMB = req.RAMTotalMB
	m.RAMUsedMB = req.RAMUsedMB
	m.ActiveAgents = req.ActiveAgents
	m.RunningAgents = req.RunningAgents
	m.LastHeartbeat = time.Now()

	s.persist()
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

func (s *Store) CreateUser(userID string, imageSizeMB int) (*User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.users[userID]; exists {
		return nil, fmt.Errorf("user %q already exists", userID)
	}

	if imageSizeMB <= 0 {
		imageSizeMB = 512
	}

	u := &User{
		UserID:      userID,
		Status:      "registered",
		ImageSizeMB: imageSizeMB,
		CreatedAt:   time.Now(),
	}
	s.users[userID] = u
	s.persist()
	return u, nil
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
	u.Status = status
	u.Error = errMsg
	s.persist()
}

func (s *Store) SetUserPrimary(userID, machineID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return
	}
	u.PrimaryMachine = machineID
	s.persist()
}

func (s *Store) SetUserPort(userID string, port int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return
	}
	u.DRBDPort = port
	s.persist()
}

func (s *Store) CreateBipod(userID, machineID, role string, minor int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := userID + ":" + machineID
	s.bipods[key] = &Bipod{
		UserID:    userID,
		MachineID: machineID,
		Role:      role,
		DRBDMinor: minor,
	}
	s.persist()
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
	s.persist()
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

func (s *Store) AllocatePort() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	port := s.nextPort
	s.nextPort++
	s.persist()
	return port
}

func (s *Store) AllocateMinor(machineID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	minor := s.nextMinor[machineID]
	s.nextMinor[machineID] = minor + 1
	s.persist()
	return minor
}

// SelectMachines picks the 2 least-loaded active machines.
// Holds the write lock to prevent concurrent placements from seeing stale counts.
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

	// Increment active_agents now to prevent double-placement
	candidates[0].ActiveAgents++
	candidates[1].ActiveAgents++

	p := *candidates[0]
	sec := *candidates[1]
	s.persist()
	return &p, &sec, nil
}

type persistState struct {
	Machines  map[string]*Machine `json:"machines"`
	Users     map[string]*User    `json:"users"`
	Bipods    map[string]*Bipod   `json:"bipods"`
	NextPort  int                 `json:"next_port"`
	NextMinor map[string]int      `json:"next_minor"`
}

func (s *Store) persist() {
	if s.dataDir == "" {
		return
	}
	state := persistState{
		Machines:  s.machines,
		Users:     s.users,
		Bipods:    s.bipods,
		NextPort:  s.nextPort,
		NextMinor: s.nextMinor,
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		slog.Warn("Failed to marshal state", "error", err)
		return
	}
	path := filepath.Join(s.dataDir, "state.json")
	os.MkdirAll(s.dataDir, 0755)
	if err := os.WriteFile(path, data, 0644); err != nil {
		slog.Warn("Failed to persist state", "error", err)
	}
}
