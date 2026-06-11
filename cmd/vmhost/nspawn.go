package vmhostcmder

import (
	"context"
	"log"
	"time"

	"github.com/papercomputeco/masterblaster/pkg/vm"
	"github.com/papercomputeco/masterblaster/pkg/vmhost"
)

// nspawnController adapts the NspawnBackend to the VMController interface.
// Like the native backend there is no hypervisor child process to wait on:
// the container runs as a transient systemd unit (mb-<name>) supervised by
// systemd, so Wait() blocks forever and lifecycle ops shell out to
// machinectl/systemctl.
type nspawnController struct {
	backend *vm.NspawnBackend
	inst    *vm.Instance
	logger  *log.Logger
}

func (c *nspawnController) State() string {
	state, _ := c.backend.Status(context.Background(), c.inst)
	return string(state)
}

func (c *nspawnController) Stop(ctx context.Context, timeout time.Duration) error {
	return c.backend.Down(ctx, c.inst, timeout)
}

func (c *nspawnController) ForceStop(ctx context.Context) error {
	return c.backend.ForceDown(ctx, c.inst)
}

func (c *nspawnController) SSHPort() int {
	return c.inst.SSHPort
}

func (c *nspawnController) Backend() string {
	return "nspawn"
}

func (c *nspawnController) Wait() error {
	// No child process to reap — the transient systemd unit supervises the
	// container. Block forever; the daemon stops us via the control socket.
	select {}
}

func bootNspawn(ctx context.Context, baseDir string, inst *vm.Instance, logger *log.Logger) (vmhost.VMController, error) {
	backend := vm.NewNspawnBackend(baseDir)

	logger.Printf("launching nspawn sandbox %q", inst.Name)
	if err := backend.Boot(ctx, inst); err != nil {
		return nil, err
	}

	return &nspawnController{
		backend: backend,
		inst:    inst,
		logger:  logger,
	}, nil
}
