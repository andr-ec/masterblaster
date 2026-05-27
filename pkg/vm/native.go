package vm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/papercomputeco/masterblaster/pkg/config"
	"github.com/papercomputeco/masterblaster/pkg/ssh"
	"github.com/papercomputeco/masterblaster/pkg/vsock"
)

// NativeBackend implements the Backend interface for running on a host that
// already has stereosd and agentd running (e.g., an existing NixOS system
// with stereOS modules imported). No VM is created — the agent runs in a
// gVisor sandbox managed by the local agentd.
//
// Communicates with stereosd via TCP on localhost:1024 (ndjson protocol),
// NOT the unix socket (which speaks HTTP).
type NativeBackend struct {
	baseDir string
	host    string
	port    int
}

// NewNativeBackend creates a new native backend.
func NewNativeBackend(baseDir string) *NativeBackend {
	host := os.Getenv("MB_STEREOSD_HOST")
	if host == "" {
		host = "127.0.0.1"
	}
	return &NativeBackend{
		baseDir: baseDir,
		host:    host,
		port:    vsock.VsockPort,
	}
}

// transport returns a TCPTransport for connecting to local stereosd.
func (n *NativeBackend) transport() vsock.Transport {
	return &vsock.TCPTransport{Host: n.host, Port: n.port}
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
	transport := n.transport()
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
	transport := n.transport()
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

// Destroy stops the agent, unmounts shared directories, and removes the
// instance directory. Unmount must happen BEFORE removing inst.Dir,
// otherwise the bindfs mounts in stereosd would survive with dangling
// source paths — accumulating zombie mounts across mb up/destroy cycles
// (one per shared mount per cycle).
func (n *NativeBackend) Destroy(ctx context.Context, inst *Instance) error {
	// Best-effort agent stop. Don't fail destroy if the daemon is gone.
	_ = n.Down(ctx, inst, 10*time.Second)

	// Best-effort unmount of every share. Read mounts from the on-disk
	// jcard if inst.Config wasn't loaded by the caller, so destroy works
	// for instances loaded by name only.
	cfg := inst.Config
	if cfg == nil {
		if loaded, err := config.Load(inst.JcardPath()); err == nil {
			cfg = loaded
		}
	}
	transport := n.transport()
	if client, err := vsock.Connect(transport, 5*time.Second); err == nil {
		defer func() { _ = client.Close() }()

		// Unmount every share, rewriting guest paths to the per-sandbox
		// home in case the on-disk jcard still has the historical
		// /home/agent path. Best-effort: a leftover mount is recoverable.
		if cfg != nil {
			for _, shared := range cfg.Shared {
				guest := shared.Guest
				const oldHome = "/home/agent"
				if guest == oldHome || strings.HasPrefix(guest, oldHome+"/") {
					guest = "/home/sb-" + inst.Name + guest[len(oldHome):]
				}
				if err := client.Unmount(ctx, guest); err != nil {
					fmt.Fprintf(os.Stderr, "destroy: unmount %s: %v\n", guest, err)
				}
			}
		}

		// Destroy the per-sandbox user (sb-<name>): kills procs,
		// lazy-umounts under /home/sb-<name>, userdel -r, removes the
		// login-shell wrapper. Idempotent on stereosd's side.
		if err := client.DestroySandboxUser(ctx, inst.Name); err != nil {
			fmt.Fprintf(os.Stderr, "destroy: drop sandbox user %s: %v\n", inst.Name, err)
		}
	} else {
		fmt.Fprintf(os.Stderr, "destroy: cannot connect to stereosd: %v\n", err)
	}

	if err := os.RemoveAll(inst.Dir); err != nil {
		return fmt.Errorf("removing instance directory: %w", err)
	}
	return nil
}

// Status queries stereosd for the current agent health.
func (n *NativeBackend) Status(_ context.Context, inst *Instance) (State, error) {
	transport := n.transport()
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
	transport := n.transport()

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
		return fmt.Errorf("could not connect to stereosd at %s:%d after 30s: %w", n.host, n.port, err)
	}
	defer func() { _ = client.Close() }()

	// Wait for ready
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := client.WaitForReady(readyCtx, 200*time.Millisecond); err != nil {
		return fmt.Errorf("waiting for stereosd ready: %w", err)
	}

	// Provision the per-sandbox user (sb-<name>) before anything that
	// references the user. Idempotent on stereosd's side, so re-running
	// provision (e.g. mb down → mb up) doesn't churn the user. After
	// this returns, /home/sb-<inst.Name> and the login-shell wrapper
	// exist; rewrite jcard paths to land in that home instead of the
	// historical /home/agent shared dir.
	if err := client.CreateSandboxUser(ctx, inst.Name); err != nil {
		return fmt.Errorf("creating sandbox user: %w", err)
	}
	rewriteAgentHomeToSandbox(cfg, inst.Name)

	// Send config to stereosd. agentd reads the result via its own config
	// package (papercomputeco/agentd) which expects the `[[agents]]`
	// schema — use MarshalForAgentd, NOT the round-trippable Marshal we
	// use for on-disk state.
	cfgBytes, err := config.MarshalForAgentd(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := client.SetConfig(ctx, string(cfgBytes)); err != nil {
		return fmt.Errorf("setting config: %w", err)
	}

	// Shared directories — mount first so that SSH key injection lands
	// inside the mounted tree (e.g. when guest = "/home/admin").
	//
	// For `mode = "clone"` shares, reflink the host source into a per-sandbox
	// staging dir under inst.Dir/shared/ and bind THAT instead. Writes from
	// inside the sandbox land in the clone; the original host tree is not
	// touched. Idempotent: if the staging dir already exists (e.g. mb down →
	// mb up flow re-enters provision), reuse it so the agent's prior work
	// survives.
	for _, shared := range cfg.Shared {
		source := shared.Host
		if shared.Mode == "clone" {
			cloned, err := ensureCloneStaging(inst.Dir, shared)
			if err != nil {
				return fmt.Errorf("clone-staging %q: %w", shared.Host, err)
			}
			source = cloned
		}
		if err := client.Mount(ctx, source, shared.Guest, "bind", shared.ReadOnly); err != nil {
			return fmt.Errorf("mounting %q at %q: %w", source, shared.Guest, err)
		}
	}

	// Inject SSH key for admin (operator account) and the per-sandbox
	// sb-<name> user (where the harness runs). Best-effort for the
	// legacy agent user to keep `mb ssh -u agent` working for sandboxes
	// that haven't migrated yet.
	if inst.sshPublicKey != "" {
		if err := client.InjectSSHKey(ctx, "admin", inst.sshPublicKey); err != nil {
			return fmt.Errorf("injecting SSH key for admin: %w", err)
		}
		sbUser := "sb-" + inst.Name
		if err := client.InjectSSHKey(ctx, sbUser, inst.sshPublicKey); err != nil {
			return fmt.Errorf("injecting SSH key for %s: %w", sbUser, err)
		}
		if err := client.InjectSSHKey(ctx, "agent", inst.sshPublicKey); err != nil {
			fmt.Fprintf(os.Stderr, "provision: inject SSH key for agent: %v (use `mb ssh` and hop)\n", err)
		}
	}

	// Inject secrets
	for name, value := range cfg.Secrets {
		if err := client.InjectSecret(ctx, name, value); err != nil {
			return fmt.Errorf("injecting secret %q: %w", name, err)
		}
	}

	return nil
}

// ensureCloneStaging reflink-clones a shared.Host directory into a
// per-sandbox staging path and returns that path. The staging path is
// stable for the lifetime of the instance (key = sha256 prefix of the
// guest mount point), so re-entries through provision() (mb down → mb up)
// reuse the existing clone rather than refreshing from host.
//
// Reflinks require the staging filesystem to support extent sharing
// (XFS with reflink=1, btrfs). `cp -aT --reflink=always` errors out on
// unsupported filesystems instead of silently falling back to a full
// copy, which would make clone mode a foot-gun on the wrong host.
//
// stereosd's bindfs layer remaps ownership on top of this mount
// (--force-user=agent), so we deliberately do NOT chown the clone here
// — files keep the invoking user's ownership on disk, and the agent
// sees them as `agent` through the bind. New writes from inside the
// sandbox land on the host owned by the invoking user too.
func ensureCloneStaging(instDir string, shared config.SharedMount) (string, error) {
	dst := cloneStagingPath(instDir, shared.Guest)
	if _, err := os.Stat(dst); err == nil {
		// Reuse existing clone — survives mb down/up.
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return "", fmt.Errorf("mkdir staging parent: %w", err)
	}
	cmd := exec.Command("cp", "-aT", "--reflink=always", shared.Host, dst)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Best-effort cleanup of a partial clone — leftover bytes here
		// would otherwise be mistaken for a cache hit next time.
		_ = os.RemoveAll(dst)
		return "", fmt.Errorf("cp -aT --reflink=always %q -> %q (is the instance dir on XFS/btrfs?): %w: %s",
			shared.Host, dst, err, strings.TrimSpace(string(out)))
	}
	return dst, nil
}

// cloneStagingPath returns the per-instance staging path for a clone-mode
// shared mount. Keyed by sha256 prefix of the guest path so multiple
// shares in one jcard can coexist without name collisions and without
// leaking filesystem-unfriendly characters from arbitrary guest paths.
func cloneStagingPath(instDir, guest string) string {
	sum := sha256.Sum256([]byte(guest))
	return filepath.Join(instDir, "shared", hex.EncodeToString(sum[:8]))
}

// rewriteAgentHomeToSandbox rewrites every /home/agent reference in cfg
// (shared mount guest paths, agent workdir) to the per-sandbox home
// /home/sb-<name>. Lets users keep writing the historical /home/agent
// paths in jcard.toml while sandboxes provision under sb-<name>.
//
// Idempotent: paths already under /home/sb-<name> are left alone.
func rewriteAgentHomeToSandbox(cfg *config.JcardConfig, sandboxName string) {
	const oldHome = "/home/agent"
	newHome := "/home/sb-" + sandboxName
	rewrite := func(p string) string {
		if p == oldHome || strings.HasPrefix(p, oldHome+"/") {
			return newHome + p[len(oldHome):]
		}
		return p
	}
	for i := range cfg.Shared {
		cfg.Shared[i].Guest = rewrite(cfg.Shared[i].Guest)
	}
	cfg.Agent.Workdir = rewrite(cfg.Agent.Workdir)
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
