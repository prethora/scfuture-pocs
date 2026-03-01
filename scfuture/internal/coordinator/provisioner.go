package coordinator

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"scfuture/internal/shared"
)

// ProvisionUser drives the full provisioning state machine for a user.
// Runs in its own goroutine.
func (coord *Coordinator) ProvisionUser(userID string) {
	logger := slog.With("user_id", userID, "component", "provisioner")

	// Helper to fail provisioning
	fail := func(step string, err error) {
		msg := fmt.Sprintf("%s: %v", step, err)
		logger.Error("Provisioning failed", "step", step, "error", err)
		coord.store.SetUserStatus(userID, "failed", msg)
	}

	// Helper to retry an operation once
	retry := func(step string, fn func() error) error {
		if err := fn(); err != nil {
			logger.Warn("Step failed, retrying in 2s", "step", step, "error", err)
			time.Sleep(2 * time.Second)
			return fn()
		}
		return nil
	}

	// ── Step 1: Select machines ──
	u := coord.store.GetUser(userID)
	if u == nil {
		fail("lookup", fmt.Errorf("user not found"))
		return
	}

	primary, secondary, err := coord.store.SelectMachines()
	if err != nil {
		fail("select_machines", err)
		return
	}

	port := coord.store.AllocatePort()
	primaryMinor := coord.store.AllocateMinor(primary.MachineID)
	secondaryMinor := coord.store.AllocateMinor(secondary.MachineID)

	coord.store.SetUserPrimary(userID, primary.MachineID)
	coord.store.SetUserPort(userID, port)
	coord.store.CreateBipod(userID, primary.MachineID, "primary", primaryMinor)
	coord.store.CreateBipod(userID, secondary.MachineID, "secondary", secondaryMinor)

	logger.Info("Machines selected",
		"primary", primary.MachineID,
		"secondary", secondary.MachineID,
		"port", port,
		"primary_minor", primaryMinor,
		"secondary_minor", secondaryMinor,
	)

	primaryClient := NewMachineClient(primary.Address)
	secondaryClient := NewMachineClient(secondary.Address)

	// ── Step 2: Create images ──
	var primaryLoop, secondaryLoop string

	err = retry("images_primary", func() error {
		resp, e := primaryClient.CreateImage(userID, u.ImageSizeMB)
		if e != nil {
			return e
		}
		primaryLoop = resp.LoopDevice
		return nil
	})
	if err != nil {
		fail("images_primary", err)
		return
	}

	err = retry("images_secondary", func() error {
		resp, e := secondaryClient.CreateImage(userID, u.ImageSizeMB)
		if e != nil {
			return e
		}
		secondaryLoop = resp.LoopDevice
		return nil
	})
	if err != nil {
		fail("images_secondary", err)
		return
	}

	coord.store.SetBipodLoopDevice(userID, primary.MachineID, primaryLoop)
	coord.store.SetBipodLoopDevice(userID, secondary.MachineID, secondaryLoop)
	logger.Info("Images created", "primary_loop", primaryLoop, "secondary_loop", secondaryLoop)

	// ── Step 3: Configure DRBD ──
	// Extract private IP (without port) for DRBD addresses
	primaryAddr := stripPort(primary.Address)
	secondaryAddr := stripPort(secondary.Address)

	drbdReq := &shared.DRBDCreateRequest{
		ResourceName: "user-" + userID,
		Nodes: []shared.DRBDNode{
			{
				Hostname: primary.MachineID,
				Minor:    primaryMinor,
				Disk:     primaryLoop,
				Address:  primaryAddr,
			},
			{
				Hostname: secondary.MachineID,
				Minor:    secondaryMinor,
				Disk:     secondaryLoop,
				Address:  secondaryAddr,
			},
		},
		Port: port,
	}

	err = retry("drbd_primary", func() error {
		_, e := primaryClient.DRBDCreate(userID, drbdReq)
		return e
	})
	if err != nil {
		fail("drbd_primary", err)
		return
	}

	err = retry("drbd_secondary", func() error {
		_, e := secondaryClient.DRBDCreate(userID, drbdReq)
		return e
	})
	if err != nil {
		fail("drbd_secondary", err)
		return
	}
	logger.Info("DRBD configured")

	// ── Step 4: Promote primary ──
	// CRITICAL: Promote BEFORE waiting for sync. Sync does not begin until one side is Primary.
	err = retry("drbd_promote", func() error {
		_, e := primaryClient.DRBDPromote(userID)
		return e
	})
	if err != nil {
		fail("drbd_promote", err)
		return
	}
	logger.Info("Primary promoted")

	// ── Step 5: Wait for DRBD sync ──
	syncTimeout := 120 * time.Second
	syncStart := time.Now()
	for {
		if time.Since(syncStart) > syncTimeout {
			fail("drbd_sync", fmt.Errorf("sync timeout after %v", syncTimeout))
			return
		}

		status, err := primaryClient.DRBDStatus(userID)
		if err != nil {
			logger.Warn("DRBD status check failed", "error", err)
			time.Sleep(2 * time.Second)
			continue
		}

		if status.PeerDiskState == "UpToDate" {
			logger.Info("DRBD sync complete")
			break
		}

		progress := "unknown"
		if status.SyncProgress != nil {
			progress = *status.SyncProgress
		}
		logger.Info("DRBD syncing", "peer_disk", status.PeerDiskState, "progress", progress)
		time.Sleep(2 * time.Second)
	}

	// ── Step 6: Format Btrfs ──
	err = retry("format_btrfs", func() error {
		_, e := primaryClient.FormatBtrfs(userID)
		return e
	})
	if err != nil {
		fail("format_btrfs", err)
		return
	}
	logger.Info("Btrfs formatted")

	// ── Step 7: Start container ──
	err = retry("container_start", func() error {
		_, e := primaryClient.ContainerStart(userID)
		return e
	})
	if err != nil {
		fail("container_start", err)
		return
	}
	logger.Info("Container started")

	// ── Step 8: Mark running ──
	coord.store.SetUserStatus(userID, "running", "")
	logger.Info("Provisioning complete — user is running")
}

func stripPort(address string) string {
	idx := strings.LastIndex(address, ":")
	if idx == -1 {
		return address
	}
	return address[:idx]
}
