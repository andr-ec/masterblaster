package vmhostcmder

import (
	"context"
	"log"
	"time"

	"github.com/papercomputeco/masterblaster/pkg/config"
	"github.com/papercomputeco/masterblaster/pkg/vm"
	"github.com/papercomputeco/masterblaster/pkg/vmhost"
)

// proxmoxController adapts the ProxmoxBackend to the VMController interface.
// The hypervisor runs on the Proxmox host, not as a local child process.
type proxmoxController struct {
	backend *vm.ProxmoxBackend
	inst    *vm.Instance
	logger  *log.Logger
}

func (c *proxmoxController) State() string {
	state, _ := c.backend.Status(context.Background(), c.inst)
	return string(state)
}

func (c *proxmoxController) Stop(ctx context.Context, timeout time.Duration) error {
	return c.backend.Down(ctx, c.inst, timeout)
}

func (c *proxmoxController) ForceStop(ctx context.Context) error {
	return c.backend.ForceDown(ctx, c.inst)
}

func (c *proxmoxController) SSHPort() int {
	return c.inst.SSHPort
}

func (c *proxmoxController) Backend() string {
	return "proxmox"
}

func (c *proxmoxController) Wait() error {
	// No local hypervisor process to wait on. The VM runs on the Proxmox
	// host. Block forever — the vmhost server will handle shutdown signals.
	select {}
}

func bootProxmox(ctx context.Context, baseDir string, inst *vm.Instance, logger *log.Logger) (vmhost.VMController, error) {
	// Load config to get Proxmox connection settings
	cfg, err := config.Load(inst.JcardPath())
	if err != nil {
		return nil, err
	}
	inst.Config = cfg

	backend := vm.NewProxmoxBackend(baseDir, &cfg.Proxmox)

	logger.Printf("provisioning Proxmox VM %q", inst.Name)
	if err := backend.Boot(ctx, inst); err != nil {
		return nil, err
	}

	return &proxmoxController{
		backend: backend,
		inst:    inst,
		logger:  logger,
	}, nil
}
