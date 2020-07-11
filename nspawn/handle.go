package nspawn

import (
	"context"
	"sync"
	"time"

	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-plugin"
	"github.com/hashicorp/nomad/drivers/shared/executor"
	"github.com/hashicorp/nomad/plugins/drivers"
)

var (
	NspawnMeasuredCpuStats = []string{"System Mode", "User Mode", "Percent"}

	NspawnMeasuredMemStats = []string{"RSS", "Cache"}
)

const (
// startup timeouts
// machinePropertiesTimeout = 30 * time.Second
)

type taskHandle struct {
	machineName string
	logger      hclog.Logger

	// stateLock syncs access to all fields below
	stateLock sync.RWMutex

	exec         executor.Executor
	pluginClient *plugin.Client
	taskConfig   *drivers.TaskConfig
	procState    drivers.TaskState
	startedAt    time.Time
	completedAt  time.Time
	exitResult   *drivers.ExitResult
}

/*func (h *taskHandle) DescribeMachine() (*MachineProps, error) {
  if h.machine == nil {
    machine, err := DescribeMachine(h.machineName, machinePropertiesTimeout)
    if err == nil {
      h.machine = machine
    } else {
      return nil, err
    }
  }
  return h.machine, nil
}*/
func (h *taskHandle) TaskStatus() *drivers.TaskStatus {
	h.stateLock.RLock()
	defer h.stateLock.RUnlock()

	return &drivers.TaskStatus{
		ID:          h.taskConfig.ID,
		Name:        h.taskConfig.Name,
		State:       h.procState,
		StartedAt:   h.startedAt,
		CompletedAt: h.completedAt,
		ExitResult:  h.exitResult,
		// TODO: Maybe return machine config later
		// DriverAttributes: map[string]string{
		//	"pid": strconv.FormatUint(uint64(h.machine.Leader), 10),
		// },
	}
}

func (h *taskHandle) IsRunning() bool {
	h.stateLock.RLock()
	defer h.stateLock.RUnlock()
	return h.procState == drivers.TaskStateRunning
}

func (h *taskHandle) run() {
	h.stateLock.Lock()
	if h.exitResult == nil {
		h.exitResult = &drivers.ExitResult{}
	}
	h.stateLock.Unlock()

	ps, err := h.exec.Wait(context.Background())
	h.stateLock.Lock()
	defer h.stateLock.Unlock()

	if err != nil {
		h.exitResult.Err = err
		h.procState = drivers.TaskStateUnknown
		h.completedAt = time.Now()
		return
	}
	h.procState = drivers.TaskStateExited
	h.exitResult.ExitCode = ps.ExitCode
	h.exitResult.Signal = ps.Signal
	h.completedAt = ps.Time
	h.logger.Debug("run() exited successful")
}
