package coordinator

import (
	"scfuture/internal/shared"
)

func (coord *Coordinator) HandleRegister(req *shared.FleetRegisterRequest) {
	coord.store.RegisterMachine(req)
}

func (coord *Coordinator) HandleHeartbeat(req *shared.FleetHeartbeatRequest) {
	coord.store.UpdateHeartbeat(req)
}
