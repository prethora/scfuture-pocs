package coordinator

import (
	"log/slog"
	"os"
	"strconv"
	"time"
)

var (
	RetentionEnforcerInterval = 60 * time.Second
	WarmRetentionPeriod       = 7 * 24 * time.Hour // 7 days — DRBD disconnected after this
	EvictionPeriod            = 30 * 24 * time.Hour // 30 days — evicted after this
)

func init() {
	// Allow override via environment variables (for testing with short durations)
	if v := os.Getenv("WARM_RETENTION_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			WarmRetentionPeriod = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("EVICTION_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			EvictionPeriod = time.Duration(n) * time.Second
		}
	}
}

// StartRetentionEnforcer launches a background goroutine that enforces
// time-based retention transitions for suspended users.
func StartRetentionEnforcer(store *Store, coord *Coordinator) {
	go func() {
		slog.Info("[RETENTION] Retention enforcer started",
			"interval", RetentionEnforcerInterval.String(),
			"warm_retention", WarmRetentionPeriod.String(),
			"eviction_period", EvictionPeriod.String(),
		)

		ticker := time.NewTicker(RetentionEnforcerInterval)
		defer ticker.Stop()
		for range ticker.C {
			coord.enforceRetention()
		}
	}()
}

// enforceRetention scans suspended users and enforces time-based transitions.
func (coord *Coordinator) enforceRetention() {
	suspended := coord.store.GetSuspendedUsers()
	if len(suspended) == 0 {
		return
	}

	now := time.Now()

	for _, user := range suspended {
		elapsed := now.Sub(user.StatusChangedAt)

		// Check for eviction first (longer threshold)
		if elapsed > EvictionPeriod {
			slog.Info("[RETENTION] Auto-evicting user",
				"user_id", user.UserID,
				"suspended_for", elapsed.String(),
			)
			coord.evictUser(user.UserID)
			continue
		}

		// Check for DRBD disconnect (shorter threshold)
		if elapsed > WarmRetentionPeriod && !user.DRBDDisconnected {
			slog.Info("[RETENTION] Disconnecting DRBD for suspended user",
				"user_id", user.UserID,
				"suspended_for", elapsed.String(),
			)
			coord.disconnectSuspendedDRBD(user.UserID)
		}
	}
}

// disconnectSuspendedDRBD disconnects DRBD on both machines for a suspended user.
func (coord *Coordinator) disconnectSuspendedDRBD(userID string) {
	start := time.Now()
	logger := slog.With("component", "retention", "user_id", userID)

	bipods := coord.store.GetBipods(userID)
	for _, b := range bipods {
		if b.Role == "stale" {
			continue
		}
		m := coord.store.GetMachine(b.MachineID)
		if m == nil || m.Status != "active" {
			continue
		}
		client := NewMachineClient(m.Address)
		if _, err := client.DRBDDisconnect(userID); err != nil {
			logger.Warn("DRBD disconnect failed on machine (non-fatal)", "machine", b.MachineID, "error", err)
		} else {
			logger.Info("DRBD disconnected on machine", "machine", b.MachineID)
		}
	}

	coord.store.SetUserDRBDDisconnected(userID, true)
	coord.store.RecordLifecycleEvent(LifecycleEvent{
		UserID: userID, Type: "drbd_disconnect", Success: true,
		DurationMS: time.Since(start).Milliseconds(), Timestamp: time.Now(),
	})
}
