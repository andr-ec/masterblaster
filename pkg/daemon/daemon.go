package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/papercomputeco/masterblaster/pkg/config"
	"github.com/papercomputeco/masterblaster/pkg/vm"
	"github.com/papercomputeco/masterblaster/pkg/vmhost"
	"github.com/papercomputeco/masterblaster/pkg/vsock"
)

// managedVM holds the daemon's per-VM state including the vmhost client
// connection. Each running VM has a dedicated vmhost child process; the
// daemon communicates with it over vmhost.sock.
type managedVM struct {
	inst    *vm.Instance
	backend string         // "qemu" or "applevirt" (from state.json)
	client  *vmhost.Client // connection to vmhost.sock (nil if stopped)
	pid     int            // vmhost process PID (for liveness checks)
}

// Daemon is the long-lived Masterblaster service that manages sandbox VMs.
// It acts as a multiplexer: each VM gets its own vmhost child process that
// holds the hypervisor handle and control socket. The daemon spawns vmhost
// processes, monitors their health, and routes CLI requests to them.
type Daemon struct {
	mu sync.RWMutex

	// In-memory mapping of known VM instances and their vmhost connections.
	vms map[string]*managedVM

	// Base directory for all mb state (~/.mb/).
	baseDir string

	// Unix socket listener for CLI communication.
	listener net.Listener

	// Logger for daemon events.
	logger *log.Logger
}

// New creates a new Daemon with the given base directory. The daemon no
// longer takes a Backend parameter; instead, it spawns vmhost child
// processes that use the appropriate backend internally.
func New(baseDir string) *Daemon {
	return &Daemon{
		vms:     make(map[string]*managedVM),
		baseDir: baseDir,
		logger:  log.New(os.Stderr, "[mb-daemon] ", log.LstdFlags),
	}
}

// SocketPath returns the path to the daemon's unix socket.
func SocketPath(baseDir string) string {
	return filepath.Join(baseDir, "mb.sock")
}

// PIDFilePath returns the path to the daemon's PID file.
func PIDFilePath(baseDir string) string {
	return filepath.Join(baseDir, "daemon.pid")
}

// Run starts the daemon, listens for CLI requests, and blocks until
// interrupted or the context is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	// Ensure base directories exist
	for _, dir := range []string{
		d.baseDir,
		filepath.Join(d.baseDir, "vms"),
		filepath.Join(d.baseDir, "mixtapes"),
	} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("creating directory %s: %w", dir, err)
		}
	}

	// Clean up stale socket
	sockPath := SocketPath(d.baseDir)
	_ = os.Remove(sockPath)

	// Write PID file
	pidPath := PIDFilePath(d.baseDir)
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		return fmt.Errorf("writing PID file: %w", err)
	}
	defer func() { _ = os.Remove(pidPath) }()

	// Open unix socket
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", sockPath, err)
	}
	d.listener = listener
	defer func() {
		_ = listener.Close()
		_ = os.Remove(sockPath)
	}()

	// Load existing VMs into memory
	d.loadExistingVMs()

	d.logger.Printf("daemon started, listening on %s (PID %d)", sockPath, os.Getpid())

	// Handle signals for clean shutdown
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Accept connections
	errCh := make(chan error, 1)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
					errCh <- err
					return
				}
			}
			go d.handleConnection(ctx, conn)
		}
	}()

	select {
	case <-ctx.Done():
		d.logger.Println("shutting down daemon (vmhost processes will continue running)...")
		return nil
	case err := <-errCh:
		return fmt.Errorf("accept error: %w", err)
	}
}

// loadExistingVMs scans the vms directory for surviving vmhost processes
// and reconnects to them. This is called on daemon startup to recover
// VMs that outlived the previous daemon instance.
func (d *Daemon) loadExistingVMs() {
	vmsDir := vm.VMsDir(d.baseDir)
	entries, err := os.ReadDir(vmsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			d.logger.Printf("warning: scanning VMs directory: %v", err)
		}
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		vmDir := filepath.Join(vmsDir, name)

		// Read state.json to get backend type
		state, err := vm.LoadStateFromDisk(d.baseDir, name)
		if err != nil {
			continue
		}

		inst := &vm.Instance{
			Name:      state.Name,
			Dir:       vmDir,
			QMPSocket: filepath.Join(vmDir, "qmp.sock"),
			SSHPort:   state.SSHPort,
			VsockPort: state.VsockPort,
		}

		mvm := &managedVM{
			inst:    inst,
			backend: state.Backend,
		}

		// Check if a vmhost process is still alive
		pidPath := inst.VMHostPIDPath()
		pidData, err := os.ReadFile(pidPath)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(pidData))); err == nil && pid > 0 {
				if processAlive(pid) {
					// Try to connect to the vmhost control socket
					client := vmhost.NewClient(inst.VMHostSocketPath())
					if client.IsAlive() {
						mvm.client = client
						mvm.pid = pid
						inst.VMState = vm.StateRunning
						d.logger.Printf("reconnected to running VM %q (vmhost PID %d)", name, pid)
					} else {
						// PID alive but socket not responding — stale
						inst.VMState = vm.StateStopped
						d.logger.Printf("found VM %q with stale vmhost (PID %d alive but socket unresponsive)", name, pid)
					}
				} else {
					inst.VMState = vm.StateStopped
					// Clean up stale PID/socket files
					_ = os.Remove(pidPath)
					_ = os.Remove(inst.VMHostSocketPath())
				}
			}
		} else {
			inst.VMState = vm.StateStopped
		}

		d.vms[name] = mvm
		d.logger.Printf("loaded VM: %s (state: %s, backend: %s)", name, inst.VMState, state.Backend)
	}
}

// handleConnection processes a single CLI RPC connection.
func (d *Daemon) handleConnection(ctx context.Context, conn net.Conn) {
	defer func() { _ = conn.Close() }()

	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	var req Request
	if err := dec.Decode(&req); err != nil {
		_ = enc.Encode(Response{Error: fmt.Sprintf("invalid request: %v", err)})
		return
	}

	resp := d.handleRequest(ctx, &req)
	_ = enc.Encode(resp)
}

// handleRequest dispatches an RPC request to the appropriate handler.
func (d *Daemon) handleRequest(ctx context.Context, req *Request) Response {
	switch req.Method {
	case MethodPing:
		return Response{OK: true}

	case MethodUp:
		return d.handleUp(ctx, req)

	case MethodDown:
		return d.handleDown(ctx, req)

	case MethodStatus:
		return d.handleStatus(ctx, req)

	case MethodDestroy:
		return d.handleDestroy(ctx, req)

	case MethodList:
		return d.handleList(ctx)

	default:
		return Response{Error: fmt.Sprintf("unknown method: %s", req.Method)}
	}
}

func (d *Daemon) handleUp(ctx context.Context, req *Request) Response {
	if req.ConfigPath == "" {
		return Response{Error: "config_path is required"}
	}

	cfg, err := config.Load(req.ConfigPath)
	if err != nil {
		return Response{Error: fmt.Sprintf("loading config: %v", err)}
	}

	// Use name from request or config
	name := req.Name
	if name == "" {
		name = cfg.Name
	}

	// Determine backend
	backend := resolveBackend(cfg)

	// Check if a sandbox with this name already exists
	d.mu.RLock()
	existing, exists := d.vms[name]
	d.mu.RUnlock()

	if exists {
		// Check if the vmhost is still alive
		state := d.queryVMState(existing)
		existing.inst.VMState = state

		switch state {
		case vm.StateRunning:
			// Already running — idempotent no-op
			d.logger.Printf("sandbox %q is already running", name)
			return Response{
				OK:        true,
				Sandboxes: []SandboxInfo{d.instanceToInfo(existing)},
			}

		default:
			// Stopped — re-boot by spawning a new vmhost
			existing.inst.Config = cfg
			if err := d.spawnVMHost(ctx, existing, backend); err != nil {
				return Response{Error: fmt.Sprintf("re-starting sandbox: %v", err)}
			}

			existing.inst.VMState = vm.StateRunning
			d.logger.Printf("sandbox %q re-started", name)
			return Response{
				OK:        true,
				Sandboxes: []SandboxInfo{d.instanceToInfo(existing)},
			}
		}
	}

	// New sandbox — prepare disk and spawn vmhost
	inst := &vm.Instance{
		Name:   name,
		Config: cfg,
	}

	// Prepare the VM directory and disk (daemon-side, before vmhost spawn)
	if err := d.prepareDisk(inst, cfg, backend); err != nil {
		return Response{Error: fmt.Sprintf("preparing sandbox: %v", err)}
	}

	mvm := &managedVM{
		inst:    inst,
		backend: backend,
	}

	// Spawn the vmhost process
	if err := d.spawnVMHost(ctx, mvm, backend); err != nil {
		// Clean up the VM directory on spawn failure
		vm.CleanupVMDir(inst)
		return Response{Error: fmt.Sprintf("starting sandbox: %v", err)}
	}

	d.mu.Lock()
	d.vms[name] = mvm
	d.mu.Unlock()

	inst.VMState = vm.StateRunning
	d.logger.Printf("sandbox %q started (backend=%s)", name, backend)

	return Response{
		OK: true,
		Sandboxes: []SandboxInfo{
			d.instanceToInfo(mvm),
		},
	}
}

// prepareDisk creates the VM directory and sets up the disk image based
// on the backend type. This runs in the daemon before spawning vmhost.
func (d *Daemon) prepareDisk(inst *vm.Instance, cfg *config.JcardConfig, backend string) error {
	switch backend {
	case "qemu":
		// Use the default QEMU platform config for disk preparation
		platform := defaultQEMUPlatform()
		return vm.PrepareQEMUDisk(d.baseDir, inst, platform)
	case "applevirt":
		return vm.PrepareAppleVirtDisk(d.baseDir, inst)
	case "native":
		return vm.PrepareNativeDir(d.baseDir, inst)
	case "nspawn":
		return vm.PrepareNspawnDir(d.baseDir, inst)
	default:
		return fmt.Errorf("unknown backend: %s", backend)
	}
}

// spawnVMHost launches a vmhost child process for the given VM. The vmhost
// process boots the VM using the specified backend and exposes a control
// socket. The daemon polls the socket until the vmhost signals readiness.
func (d *Daemon) spawnVMHost(_ context.Context, mvm *managedVM, backend string) error {
	inst := mvm.inst

	// Clean up stale runtime files from previous runs
	_ = os.Remove(inst.VMHostSocketPath())
	_ = os.Remove(inst.VMHostPIDPath())

	mbBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("finding mb binary: %w", err)
	}

	cmd := exec.Command(mbBin, "vmhost",
		"--name", inst.Name,
		"--backend", backend,
		"--config-dir", d.baseDir,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true, // new session — survives daemon exit
	}
	// Don't capture stdout/stderr; vmhost logs to vmhost.log
	cmd.Stdout = nil
	cmd.Stderr = nil

	d.logger.Printf("spawning vmhost: %s", strings.Join(cmd.Args, " "))

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawning vmhost: %w", err)
	}

	// Release the process so the daemon doesn't wait on it
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("releasing vmhost process: %w", err)
	}

	// Poll vmhost.sock until it responds (same pattern as ensureDaemon)
	client := vmhost.NewClient(inst.VMHostSocketPath())
	deadline := time.Now().Add(120 * time.Second)
	backoff := 200 * time.Millisecond
	const maxBackoff = 2 * time.Second

	for time.Now().Before(deadline) {
		time.Sleep(backoff)

		if client.IsAlive() {
			// Get info to populate instance fields
			resp, err := client.Info()
			if err == nil {
				inst.SSHPort = resp.SSHPort
			}

			mvm.client = client
			// Read PID from vmhost.pid
			if pidData, err := os.ReadFile(inst.VMHostPIDPath()); err == nil {
				if pid, err := strconv.Atoi(strings.TrimSpace(string(pidData))); err == nil {
					mvm.pid = pid
				}
			}

			d.logger.Printf("vmhost for %q is ready (PID %d)", inst.Name, mvm.pid)
			return nil
		}

		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}

	return fmt.Errorf("vmhost for %q did not become ready within 120s (check %s)", inst.Name, inst.VMHostLogPath())
}

func (d *Daemon) handleDown(_ context.Context, req *Request) Response {
	mvm, err := d.resolveVM(req.Name)
	if err != nil {
		return Response{Error: err.Error()}
	}

	if mvm.client == nil {
		mvm.inst.VMState = vm.StateStopped
		return Response{OK: true}
	}

	var resp *vmhost.Response
	var stopErr error
	if req.Force {
		resp, stopErr = mvm.client.ForceStop()
	} else {
		resp, stopErr = mvm.client.Stop(30)
	}

	if stopErr != nil {
		return Response{Error: fmt.Sprintf("stopping sandbox: %v", stopErr)}
	}

	_ = resp // response contains "stopping" state

	// Wait for the vmhost process to exit
	if mvm.pid > 0 {
		deadline := time.Now().Add(45 * time.Second)
		for time.Now().Before(deadline) {
			if !processAlive(mvm.pid) {
				break
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	mvm.inst.VMState = vm.StateStopped
	mvm.client = nil
	mvm.pid = 0

	d.logger.Printf("sandbox %q stopped", mvm.inst.Name)
	return Response{OK: true}
}

func (d *Daemon) handleStatus(_ context.Context, req *Request) Response {
	if req.All {
		return d.handleList(context.Background())
	}

	mvm, err := d.resolveVM(req.Name)
	if err != nil {
		return Response{Error: err.Error()}
	}

	state := d.queryVMState(mvm)
	mvm.inst.VMState = state

	return Response{
		OK:        true,
		Sandboxes: []SandboxInfo{d.instanceToInfo(mvm)},
	}
}

func (d *Daemon) handleDestroy(ctx context.Context, req *Request) Response {
	mvm, err := d.resolveVM(req.Name)
	if err != nil {
		return Response{Error: err.Error()}
	}

	// Stop the VM if it's running
	state := d.queryVMState(mvm)
	if state == vm.StateRunning && mvm.client != nil {
		_, _ = mvm.client.Stop(10)
		// Wait briefly for vmhost to exit
		if mvm.pid > 0 {
			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				if !processAlive(mvm.pid) {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			// Force kill if still alive
			if processAlive(mvm.pid) {
				if proc, err := os.FindProcess(mvm.pid); err == nil {
					_ = proc.Signal(syscall.SIGKILL)
				}
			}
		}
	}

	// Native backend cleanup: unmount the bindfs shares stereosd set up
	// at /home/sb-<name>/... and tear down the per-sandbox sb-<name>
	// user. Best-effort — a stereosd that's down or doesn't speak the
	// new RPCs (older deployments) shouldn't block destroy. QEMU /
	// applevirt backends shut down the whole VM, so no host-side
	// cleanup is needed for them.
	switch mvm.backend {
	case "native":
		destroyNativeRuntime(ctx, mvm.inst)
	case "nspawn":
		// The nspawn rootfs holds root-owned files (host keys, container
		// writes), so stop the unit and sudo-remove the dir; the generic
		// RemoveAll below is then a no-op.
		destroyNspawnRuntime(mvm.inst)
	}

	// Remove VM directory
	if mvm.inst.Dir != "" {
		if err := os.RemoveAll(mvm.inst.Dir); err != nil {
			return Response{Error: fmt.Sprintf("removing VM directory: %v", err)}
		}
	}

	d.mu.Lock()
	delete(d.vms, mvm.inst.Name)
	d.mu.Unlock()

	d.logger.Printf("sandbox %q destroyed", mvm.inst.Name)
	return Response{OK: true}
}

func (d *Daemon) handleList(_ context.Context) Response {
	d.mu.RLock()
	defer d.mu.RUnlock()

	infos := make([]SandboxInfo, 0, len(d.vms))
	for _, mvm := range d.vms {
		state := d.queryVMState(mvm)
		mvm.inst.VMState = state
		infos = append(infos, d.instanceToInfo(mvm))
	}

	return Response{OK: true, Sandboxes: infos}
}

// queryVMState determines the current state of a VM by checking its
// vmhost process liveness and control socket responsiveness.
func (d *Daemon) queryVMState(mvm *managedVM) vm.State {
	// If we have a client, try to query it
	if mvm.client != nil {
		resp, err := mvm.client.Status()
		if err == nil {
			switch resp.State {
			case "running":
				return vm.StateRunning
			case "error":
				return vm.StateError
			default:
				return vm.StateStopped
			}
		}
		// Socket error — vmhost may have crashed
		mvm.client = nil
	}

	// Check PID liveness as fallback
	if mvm.pid > 0 && processAlive(mvm.pid) {
		// PID alive but socket not responding — try to reconnect
		client := vmhost.NewClient(mvm.inst.VMHostSocketPath())
		if client.IsAlive() {
			mvm.client = client
			return vm.StateRunning
		}
		return vm.StateError
	}

	return vm.StateStopped
}

// resolveVM finds a managed VM by name. If no name is given, it looks
// for a single sandbox and returns it. When multiple sandboxes exist and
// no name is provided, it returns an error listing the options.
func (d *Daemon) resolveVM(name string) (*managedVM, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if name != "" {
		mvm, ok := d.vms[name]
		if ok {
			return mvm, nil
		}

		// Try loading from disk (VM may have been created by a previous daemon)
		state, err := vm.LoadStateFromDisk(d.baseDir, name)
		if err != nil {
			return nil, fmt.Errorf("sandbox %q not found", name)
		}

		inst, err := vm.LoadInstanceFromDisk(d.baseDir, name)
		if err != nil {
			return nil, fmt.Errorf("loading sandbox %q: %w", name, err)
		}

		mvm = &managedVM{
			inst:    inst,
			backend: state.Backend,
		}

		// Check if vmhost is alive
		pidPath := inst.VMHostPIDPath()
		if pidData, err := os.ReadFile(pidPath); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(pidData))); err == nil && processAlive(pid) {
				client := vmhost.NewClient(inst.VMHostSocketPath())
				if client.IsAlive() {
					mvm.client = client
					mvm.pid = pid
					inst.VMState = vm.StateRunning
				}
			}
		}

		// Register in the map (upgrade from RLock would be needed, but
		// this is a rare path — accept the small race)
		return mvm, nil
	}

	// No name: find a single sandbox
	if len(d.vms) == 0 {
		return nil, fmt.Errorf("no sandboxes found")
	}

	if len(d.vms) == 1 {
		for _, mvm := range d.vms {
			return mvm, nil
		}
	}

	names := make([]string, 0, len(d.vms))
	for n := range d.vms {
		names = append(names, n)
	}
	return nil, fmt.Errorf("multiple sandboxes exist, please specify one: %v", names)
}

func (d *Daemon) instanceToInfo(mvm *managedVM) SandboxInfo {
	inst := mvm.inst
	info := SandboxInfo{
		Name:       inst.Name,
		State:      string(inst.VMState),
		SSHPort:    inst.SSHPort,
		SSHHost:    "127.0.0.1",
		SSHAddress: fmt.Sprintf("127.0.0.1:%d", inst.SSHPort),
		SSHKeyPath: inst.SSHKeyPath,
		VsockPort:  inst.VsockPort,
	}

	// Resolve the sandbox IP. The daemon's in-memory inst may not carry
	// it (container backends assign it in the vmhost process), so fall
	// back to the persisted state below.
	ipAddr := inst.IPAddr

	// Try loading state for extra info
	if state, err := inst.LoadState(); err == nil {
		info.Mixtape = state.Mixtape
		info.CPUs = state.CPUs
		info.Memory = state.Memory
		info.NetworkMode = state.NetworkMode
		if info.SSHKeyPath == "" {
			info.SSHKeyPath = state.SSHKeyPath
		}
		if ipAddr == "" {
			ipAddr = state.IPAddr
		}
	}

	// Container backends reach SSH directly at the sandbox's bridge IP
	// on port 22 (no host port-forward). qemu/native leave IPAddr empty
	// and keep the 127.0.0.1:<forwarded-port> form set above.
	if ipAddr != "" {
		info.SSHHost = ipAddr
		info.SSHAddress = fmt.Sprintf("%s:%d", ipAddr, inst.SSHPort)
	}

	// Pull workdir from the saved jcard so `mb ssh` can land in the
	// agent's intended cwd. Falls back to inst.Config if loaded, then
	// to reading the saved jcard, then empty (= $HOME).
	//
	// Also rewrite /home/agent paths to the per-sandbox /home/sb-<name>
	// so `mb ssh` cd's into the right place. mb up does the same
	// rewrite at provision time; this mirrors it for `mb ssh` consumers.
	switch {
	case inst.Config != nil:
		info.Workdir = inst.Config.Agent.Workdir
	default:
		if cfg, err := config.Load(inst.JcardPath()); err == nil {
			info.Workdir = cfg.Agent.Workdir
		}
	}
	const oldHome = "/home/agent"
	if info.Workdir == oldHome || strings.HasPrefix(info.Workdir, oldHome+"/") {
		info.Workdir = "/home/sb-" + inst.Name + info.Workdir[len(oldHome):]
	}

	// `mb ssh` uses info.User as the default login. Container backends (nspawn)
	// provision the invoking operator as a non-root user and inject the key into
	// that user's ~/.ssh, so log in as them; native uses the per-sandbox
	// sb-<name> harness user.
	if ipAddr != "" {
		info.User = vm.OperatorUsername()
	} else {
		info.User = "sb-" + inst.Name
	}

	return info
}

// destroyNativeRuntime tears down host-side state stereosd set up for a
// native sandbox: bindfs mounts at /home/sb-<name>/... and the
// per-sandbox sb-<name> user (which in turn lazy-umounts under its home
// and SIGKILLs any procs still owned by it).
//
// Best-effort throughout — a stereosd that's down, lacks the new RPCs,
// or fails partway must not block destroy. Errors are logged to the
// daemon log; the caller continues to RemoveAll the instance dir.
func destroyNativeRuntime(ctx context.Context, inst *vm.Instance) {
	transport := &vsock.TCPTransport{Host: "127.0.0.1", Port: 1024}
	client, err := vsock.Connect(transport, 5*time.Second)
	if err != nil {
		log.Printf("destroy %s: cannot reach stereosd for runtime cleanup: %v", inst.Name, err)
		return
	}
	defer func() { _ = client.Close() }()

	// Unmount each share the user declared, rewriting /home/agent paths
	// to the per-sandbox home so we hit the bindfs mb up actually
	// created. The on-disk jcard still holds the user-facing path.
	if cfg, err := config.Load(inst.JcardPath()); err == nil {
		const oldHome = "/home/agent"
		newHome := "/home/sb-" + inst.Name
		for _, shared := range cfg.Shared {
			guest := shared.Guest
			if guest == oldHome || strings.HasPrefix(guest, oldHome+"/") {
				guest = newHome + guest[len(oldHome):]
			}
			if err := client.Unmount(ctx, guest); err != nil {
				log.Printf("destroy %s: unmount %s: %v", inst.Name, guest, err)
			}
		}
	}

	// Tear down sb-<name>: stereosd's UserManager kills user procs,
	// lazy-umounts under /home/sb-<name>, userdel -r, removes the
	// shell wrapper. Idempotent on stereosd's side.
	if err := client.DestroySandboxUser(ctx, inst.Name); err != nil {
		log.Printf("destroy %s: drop sandbox user: %v", inst.Name, err)
	}
}

// destroyNspawnRuntime stops the transient nspawn unit and removes its
// (root-owned) rootfs. Best-effort: errors are logged, not fatal.
func destroyNspawnRuntime(inst *vm.Instance) {
	machine := "mb-" + inst.Name
	_ = exec.Command("sudo", "machinectl", "terminate", machine).Run()
	_ = exec.Command("sudo", "systemctl", "stop", machine).Run()
	_ = exec.Command("sudo", "systemctl", "reset-failed", machine).Run()
	if inst.Dir != "" {
		if out, err := exec.Command("sudo", "rm", "-rf", inst.Dir).CombinedOutput(); err != nil {
			log.Printf("destroy %s: sudo rm -rf %s: %v: %s", inst.Name, inst.Dir, err, strings.TrimSpace(string(out)))
		}
	}
}

// resolveBackend determines the backend type for a VM. Precedence:
//  1. jcard.toml backend field
//  2. MB_BACKEND environment variable
//  3. Platform default: "applevirt" on darwin/arm64, "qemu" elsewhere
func resolveBackend(cfg *config.JcardConfig) string {
	if cfg.Backend != "" {
		return cfg.Backend
	}
	if env := os.Getenv("MB_BACKEND"); env != "" {
		return env
	}
	return vm.DefaultBackend()
}

// defaultQEMUPlatform returns a minimal QEMU platform config sufficient
// for disk preparation. The vmhost process uses the full platform config.
func defaultQEMUPlatform() *vm.QEMUPlatformConfig {
	return &vm.QEMUPlatformConfig{
		DirectKernelBoot: true,
	}
}

// processAlive reports whether a process with the given PID is still running.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// IsRunning checks if the daemon is running by attempting to connect
// to the socket and sending a ping.
func IsRunning(baseDir string) bool {
	sockPath := SocketPath(baseDir)
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		return false
	}
	defer func() { _ = conn.Close() }()

	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)

	if err := enc.Encode(Request{Method: MethodPing}); err != nil {
		return false
	}

	var resp Response
	if err := dec.Decode(&resp); err != nil {
		return false
	}

	return resp.OK
}
