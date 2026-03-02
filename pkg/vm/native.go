package vm

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/papercomputeco/masterblaster/pkg/config"
	"github.com/papercomputeco/masterblaster/pkg/ssh"
	"github.com/papercomputeco/masterblaster/pkg/vsock"
)

const (
	// DefaultStereosdSocket is the default path to the stereosd unix socket.
	DefaultStereosdSocket = "/run/stereos/stereosd.sock"
)

// NativeBackend implements the Backend interface for running on a host that
// already has stereosd and agentd running (e.g., an existing NixOS system
// with stereOS modules imported). No VM is created — the agent runs in a
// gVisor sandbox managed by the local agentd.
type NativeBackend struct {
	baseDir    string
	socketPath string
}

// NewNativeBackend creates a new native backend.
func NewNativeBackend(baseDir string) *NativeBackend {
	socketPath := os.Getenv("MB_STEREOSD_SOCKET")
	if socketPath == "" {
		socketPath = DefaultStereosdSocket
	}
	return &NativeBackend{
		baseDir:    baseDir,
		socketPath: socketPath,
	}
}

// Up configures and starts an agent sandbox on the local system via stereosd.
// No VM is created — the agent runs in a gVisor sandbox managed by agentd.
func (n *NativeBackend) Up(ctx context.Context, inst *Instance) error {
	if inst.Config == nil {
		return fmt.Errorf("instance %q has no configuration", inst.Name)
	}
	cfg := inst.Config

	// Create instance directory
	vmDir := filepath.Join(VMsDir(n.baseDir), inst.Name)
	if _, err := os.Stat(vmDir); err == nil {
		return fmt.Errorf("sandbox %q already exists at %s", inst.Name, vmDir)
	}
	if err := os.MkdirAll(vmDir, 0755); err != nil {
		return fmt.Errorf("creating instance directory: %w", err)
	}
	inst.Dir = vmDir

	// Save jcard.toml
	if err := saveJcard(vmDir, cfg); err != nil {
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("saving jcard config: %w", err)
	}

	// Generate ephemeral SSH keypair for this sandbox
	sshKeyPath, sshPubKey, err := ssh.GenerateKeyPair(inst.Dir, fmt.Sprintf("mb-%s", inst.Name))
	if err != nil {
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("generating SSH keypair: %w", err)
	}
	inst.SSHKeyPath = sshKeyPath
	inst.sshPublicKey = sshPubKey
	inst.SSHPort = 22

	// Save state
	stateFile := &StateFile{
		Name:        inst.Name,
		CreatedAt:   time.Now().UTC(),
		Mixtape:     cfg.Mixtape,
		CPUs:        cfg.Resources.CPUs,
		Memory:      cfg.Resources.Memory,
		NetworkMode: cfg.Network.Mode,
		SSHPort:     22,
		SSHKeyPath:  sshKeyPath,
		Backend:     "native",
	}
	if err := saveState(vmDir, stateFile); err != nil {
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("saving state: %w", err)
	}

	// Connect to local stereosd and provision
	if err := n.provision(ctx, inst, cfg); err != nil {
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("provisioning: %w", err)
	}

	inst.VMState = StateRunning
	return nil
}

// Boot is the exported entry point for vmhost processes. It provisions
// the agent sandbox via stereosd. The instance directory must already
// exist (created by the daemon's prepareDisk).
func (n *NativeBackend) Boot(ctx context.Context, inst *Instance) error {
	if inst.Dir == "" {
		inst.Dir = filepath.Join(VMsDir(n.baseDir), inst.Name)
	}

	cfg := inst.Config
	if cfg == nil {
		var err error
		cfg, err = config.Load(inst.JcardPath())
		if err != nil {
			return fmt.Errorf("loading config for %q: %w", inst.Name, err)
		}
		inst.Config = cfg
	}

	// Generate ephemeral SSH keypair
	sshKeyPath, sshPubKey, err := ssh.GenerateKeyPair(inst.Dir, fmt.Sprintf("mb-%s", inst.Name))
	if err != nil {
		return fmt.Errorf("generating SSH keypair: %w", err)
	}
	inst.SSHKeyPath = sshKeyPath
	inst.sshPublicKey = sshPubKey
	inst.SSHPort = 22

	// Update state
	stateFile := &StateFile{
		Name:       inst.Name,
		CreatedAt:  time.Now().UTC(),
		Mixtape:    cfg.Mixtape,
		SSHPort:    22,
		SSHKeyPath: sshKeyPath,
		Backend:    "native",
	}
	if err := saveState(inst.Dir, stateFile); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}

	return n.provision(ctx, inst, cfg)
}

// Start reconnects to stereosd and re-provisions an existing sandbox.
func (n *NativeBackend) Start(ctx context.Context, inst *Instance) error {
	if inst.Dir == "" {
		inst.Dir = filepath.Join(VMsDir(n.baseDir), inst.Name)
	}

	cfg := inst.Config
	if cfg == nil {
		var err error
		cfg, err = config.Load(inst.JcardPath())
		if err != nil {
			return fmt.Errorf("loading config for %q: %w", inst.Name, err)
		}
		inst.Config = cfg
	}

	if err := n.provision(ctx, inst, cfg); err != nil {
		return fmt.Errorf("re-provisioning: %w", err)
	}

	inst.VMState = StateRunning
	return nil
}

// Down stops the agent process without shutting down the host.
func (n *NativeBackend) Down(ctx context.Context, inst *Instance, timeout time.Duration) error {
	transport := &vsock.UnixTransport{Path: n.socketPath}
	client, err := vsock.Connect(transport, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connecting to stereosd: %w", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.StopAgent(ctx, "mb down"); err != nil {
		return fmt.Errorf("stopping agent: %w", err)
	}

	inst.VMState = StateStopped
	return nil
}

// ForceDown immediately kills the agent process.
func (n *NativeBackend) ForceDown(ctx context.Context, inst *Instance) error {
	transport := &vsock.UnixTransport{Path: n.socketPath}
	client, err := vsock.Connect(transport, 5*time.Second)
	if err != nil {
		inst.VMState = StateStopped
		return nil
	}
	defer func() { _ = client.Close() }()

	_ = client.ForceStopAgent(ctx, "mb force-down")
	inst.VMState = StateStopped
	return nil
}

// Destroy stops the agent and removes the instance directory.
func (n *NativeBackend) Destroy(ctx context.Context, inst *Instance) error {
	// Try to stop first
	_ = n.Down(ctx, inst, 10*time.Second)

	if err := os.RemoveAll(inst.Dir); err != nil {
		return fmt.Errorf("removing instance directory: %w", err)
	}
	return nil
}

// Status queries stereosd for the current agent health.
func (n *NativeBackend) Status(_ context.Context, inst *Instance) (State, error) {
	transport := &vsock.UnixTransport{Path: n.socketPath}
	client, err := vsock.Connect(transport, 2*time.Second)
	if err != nil {
		return StateStopped, nil
	}
	defer func() { _ = client.Close() }()

	health, err := client.GetHealth(context.Background())
	if err != nil {
		return StateError, nil
	}

	switch health.State {
	case vsock.StateReady, vsock.StateHealthy:
		return StateRunning, nil
	case vsock.StateShutdown:
		return StateStopped, nil
	default:
		return StateRunning, nil
	}
}

// List returns all native instances by scanning the vms directory.
func (n *NativeBackend) List(ctx context.Context) ([]*Instance, error) {
	vmsDir := VMsDir(n.baseDir)
	entries, err := os.ReadDir(vmsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading instances directory: %w", err)
	}

	var instances []*Instance
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		state, err := loadState(filepath.Join(vmsDir, entry.Name()))
		if err != nil {
			continue
		}
		if state.Backend != "native" {
			continue
		}

		inst, err := n.LoadInstance(entry.Name())
		if err != nil {
			continue
		}
		status, _ := n.Status(ctx, inst)
		inst.VMState = status
		instances = append(instances, inst)
	}

	return instances, nil
}

// LoadInstance reads a persisted native instance by name.
func (n *NativeBackend) LoadInstance(name string) (*Instance, error) {
	vmDir := filepath.Join(VMsDir(n.baseDir), name)
	state, err := loadState(vmDir)
	if err != nil {
		return nil, fmt.Errorf("loading instance %q: %w", name, err)
	}

	return &Instance{
		Name:       state.Name,
		Dir:        vmDir,
		SSHPort:    state.SSHPort,
		SSHKeyPath: state.SSHKeyPath,
	}, nil
}

// provision connects to local stereosd and sends config, secrets, SSH keys,
// and shared directory mounts.
func (n *NativeBackend) provision(ctx context.Context, inst *Instance, cfg *config.JcardConfig) error {
	transport := &vsock.UnixTransport{Path: n.socketPath}

	// Connect with retry
	var client *vsock.Client
	var err error
	deadline := time.Now().Add(30 * time.Second)
	backoff := 100 * time.Millisecond
	for time.Now().Before(deadline) {
		client, err = vsock.Connect(transport, 2*time.Second)
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > time.Second {
			backoff = time.Second
		}
	}
	if client == nil {
		return fmt.Errorf("could not connect to stereosd at %s after 30s: %w", n.socketPath, err)
	}
	defer func() { _ = client.Close() }()

	// Wait for ready
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.WaitForReady(readyCtx, 200*time.Millisecond); err != nil {
		return fmt.Errorf("waiting for stereosd ready: %w", err)
	}

	// Send config
	cfgBytes, err := config.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := client.SetConfig(ctx, string(cfgBytes)); err != nil {
		return fmt.Errorf("setting config: %w", err)
	}

	// Inject SSH key
	if inst.sshPublicKey != "" {
		if err := client.InjectSSHKey(ctx, "admin", inst.sshPublicKey); err != nil {
			return fmt.Errorf("injecting SSH key: %w", err)
		}
	}

	// Inject secrets
	for name, value := range cfg.Secrets {
		if err := client.InjectSecret(ctx, name, value); err != nil {
			return fmt.Errorf("injecting secret %q: %w", name, err)
		}
	}

	// Shared directories — on native, stereosd handles bind mounts directly
	for i, shared := range cfg.Shared {
		tag := fmt.Sprintf("share%d", i)
		if err := client.Mount(ctx, tag, shared.Guest, "bind", shared.ReadOnly); err != nil {
			return fmt.Errorf("mounting %q at %q: %w", shared.Host, shared.Guest, err)
		}
	}

	return nil
}

// PrepareNativeDir creates the instance directory for a native backend
// sandbox. No disk image is needed — the agent runs in a gVisor sandbox
// on the local system.
func PrepareNativeDir(baseDir string, inst *Instance) error {
	if inst.Config == nil {
		return fmt.Errorf("instance %q has no configuration", inst.Name)
	}

	vmDir := filepath.Join(VMsDir(baseDir), inst.Name)
	if _, err := os.Stat(vmDir); err == nil {
		return fmt.Errorf("sandbox %q already exists at %s", inst.Name, vmDir)
	}
	if err := os.MkdirAll(vmDir, 0755); err != nil {
		return fmt.Errorf("creating instance directory: %w", err)
	}
	inst.Dir = vmDir

	// Save jcard.toml
	cfg := inst.Config
	if err := saveJcard(vmDir, cfg); err != nil {
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("saving jcard config: %w", err)
	}

	// Write initial state
	stateFile := &StateFile{
		Name:        inst.Name,
		CreatedAt:   time.Now().UTC(),
		Mixtape:     cfg.Mixtape,
		CPUs:        cfg.Resources.CPUs,
		Memory:      cfg.Resources.Memory,
		NetworkMode: cfg.Network.Mode,
		SSHPort:     22,
		Backend:     "native",
	}
	if err := saveState(vmDir, stateFile); err != nil {
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("saving state: %w", err)
	}

	return nil
}
