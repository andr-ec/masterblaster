package vmhostcmder

import (
	"context"
	"log"
	"time"

	"github.com/papercomputeco/masterblaster/pkg/vm"
	"github.com/papercomputeco/masterblaster/pkg/vmhost"
)

// nativeController adapts the NativeBackend to the VMController interface.
// Unlike QEMU or Apple Virt, there is no hypervisor process to manage.
// The agent runs in a gVisor sandbox managed by the local agentd.
type nativeController struct {
	backend *vm.NativeBackend
	inst    *vm.Instance
	logger  *log.Logger
}

func (c *nativeController) State() string {
	state, _ := c.backend.Status(context.Background(), c.inst)
	return string(state)
}

func (c *nativeController) Stop(ctx context.Context, timeout time.Duration) error {
	return c.backend.Down(ctx, c.inst, timeout)
}

func (c *nativeController) ForceStop(ctx context.Context) error {
	return c.backend.ForceDown(ctx, c.inst)
}

func (c *nativeController) SSHPort() int {
	return c.inst.SSHPort
}

func (c *nativeController) Backend() string {
	return "native"
}

func (c *nativeController) Wait() error {
	// No hypervisor process to wait on. Block forever — stereosd is a
	// systemd service, not our child process.
	select {}
}

func bootNative(ctx context.Context, baseDir string, inst *vm.Instance, logger *log.Logger) (vmhost.VMController, error) {
	backend := vm.NewNativeBackend(baseDir)

	logger.Printf("provisioning native sandbox %q", inst.Name)
	if err := backend.Boot(ctx, inst); err != nil {
		return nil, err
	}

	return &nativeController{
		backend: backend,
		inst:    inst,
		logger:  logger,
	}, nil
}
