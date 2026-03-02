package vm

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/papercomputeco/masterblaster/pkg/config"
	"github.com/papercomputeco/masterblaster/pkg/ssh"
	"github.com/papercomputeco/masterblaster/pkg/vsock"
)

// ProxmoxBackend implements the Backend interface by managing sibling VMs
// on a Proxmox VE cluster via its REST API. Instead of running QEMU locally,
// the backend creates/clones VMs through the Proxmox API and communicates
// with stereosd inside the guest via TCP.
type ProxmoxBackend struct {
	baseDir string
	api     *proxmoxAPI
	cfg     *config.ProxmoxConfig
}

// proxmoxPlatformData is stored in StateFile.PlatformData for Proxmox VMs.
type proxmoxPlatformData struct {
	VMID int    `json:"vmid"`
	IP   string `json:"ip,omitempty"`
}

// NewProxmoxBackend creates a new Proxmox backend.
func NewProxmoxBackend(baseDir string, pmoxCfg *config.ProxmoxConfig) *ProxmoxBackend {
	return &ProxmoxBackend{
		baseDir: baseDir,
		api:     newProxmoxAPI(pmoxCfg),
		cfg:     pmoxCfg,
	}
}

// Up creates a new Proxmox VM (by cloning a template), starts it, and
// provisions it via stereosd over TCP.
func (p *ProxmoxBackend) Up(ctx context.Context, inst *Instance) error {
	if inst.Config == nil {
		return fmt.Errorf("instance %q has no configuration", inst.Name)
	}
	cfg := inst.Config

	// Create local instance directory
	vmDir := filepath.Join(VMsDir(p.baseDir), inst.Name)
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

	// Get next available VMID
	vmid, err := p.api.nextID(ctx)
	if err != nil {
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("getting next VMID: %w", err)
	}

	// Clone template or create VM
	if p.cfg.TemplateVMID > 0 {
		if err := p.api.cloneVM(ctx, p.cfg.TemplateVMID, vmid, inst.Name); err != nil {
			_ = os.RemoveAll(vmDir)
			return fmt.Errorf("cloning template: %w", err)
		}
	} else {
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("proxmox backend requires template_vmid (direct creation not yet supported)")
	}

	// Configure VM resources
	vmCfg := map[string]string{
		"cores":  strconv.Itoa(cfg.Resources.CPUs),
		"memory": strconv.Itoa(parseMiBFromSize(cfg.Resources.Memory)),
		"name":   inst.Name,
	}
	if p.cfg.Bridge != "" {
		vmCfg["net0"] = fmt.Sprintf("virtio,bridge=%s", p.cfg.Bridge)
	}
	if err := p.api.configureVM(ctx, vmid, vmCfg); err != nil {
		_ = p.api.deleteVM(ctx, vmid)
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("configuring VM: %w", err)
	}

	// Start VM
	if err := p.api.startVM(ctx, vmid); err != nil {
		_ = p.api.deleteVM(ctx, vmid)
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("starting VM: %w", err)
	}

	// Generate ephemeral SSH keypair
	sshKeyPath, sshPubKey, err := ssh.GenerateKeyPair(inst.Dir, fmt.Sprintf("mb-%s", inst.Name))
	if err != nil {
		_ = p.api.stopVM(ctx, vmid)
		_ = p.api.deleteVM(ctx, vmid)
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("generating SSH keypair: %w", err)
	}
	inst.SSHKeyPath = sshKeyPath
	inst.sshPublicKey = sshPubKey
	inst.SSHPort = 22

	// Save platform data and state
	platData := proxmoxPlatformData{VMID: vmid}
	platBytes, _ := json.Marshal(platData)

	stateFile := &StateFile{
		Name:         inst.Name,
		CreatedAt:    time.Now().UTC(),
		Mixtape:      cfg.Mixtape,
		CPUs:         cfg.Resources.CPUs,
		Memory:       cfg.Resources.Memory,
		Disk:         cfg.Resources.Disk,
		NetworkMode:  cfg.Network.Mode,
		SSHPort:      22,
		SSHKeyPath:   sshKeyPath,
		Backend:      "proxmox",
		PlatformData: platBytes,
	}
	if err := saveState(vmDir, stateFile); err != nil {
		_ = p.api.stopVM(ctx, vmid)
		_ = p.api.deleteVM(ctx, vmid)
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("saving state: %w", err)
	}

	// Wait for VM IP and provision via stereosd
	if err := p.waitAndProvision(ctx, inst, cfg, vmid); err != nil {
		_ = p.api.stopVM(ctx, vmid)
		_ = p.api.deleteVM(ctx, vmid)
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("provisioning: %w", err)
	}

	inst.VMState = StateRunning
	return nil
}

// Boot is the entry point for vmhost processes. It reads the saved state,
// starts the Proxmox VM if needed, and provisions via stereosd.
func (p *ProxmoxBackend) Boot(ctx context.Context, inst *Instance) error {
	if inst.Dir == "" {
		inst.Dir = filepath.Join(VMsDir(p.baseDir), inst.Name)
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

	// Load platform data to get VMID
	platData, err := p.loadPlatformData(inst)
	if err != nil {
		return fmt.Errorf("loading platform data: %w", err)
	}

	// Start VM if not already running
	status, err := p.api.vmStatus(ctx, platData.VMID)
	if err != nil {
		return fmt.Errorf("checking VM status: %w", err)
	}
	if status != "running" {
		if err := p.api.startVM(ctx, platData.VMID); err != nil {
			return fmt.Errorf("starting VM: %w", err)
		}
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
		Backend:    "proxmox",
	}
	platBytes, _ := json.Marshal(platData)
	stateFile.PlatformData = platBytes
	if err := saveState(inst.Dir, stateFile); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}

	return p.waitAndProvision(ctx, inst, cfg, platData.VMID)
}

// Start re-starts an existing stopped Proxmox VM.
func (p *ProxmoxBackend) Start(ctx context.Context, inst *Instance) error {
	if inst.Dir == "" {
		inst.Dir = filepath.Join(VMsDir(p.baseDir), inst.Name)
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

	platData, err := p.loadPlatformData(inst)
	if err != nil {
		return fmt.Errorf("loading platform data: %w", err)
	}

	if err := p.api.startVM(ctx, platData.VMID); err != nil {
		return fmt.Errorf("starting VM: %w", err)
	}

	if err := p.waitAndProvision(ctx, inst, cfg, platData.VMID); err != nil {
		return fmt.Errorf("provisioning: %w", err)
	}

	inst.VMState = StateRunning
	return nil
}

// Down gracefully shuts down the Proxmox VM via stereosd, falling back
// to the Proxmox API.
func (p *ProxmoxBackend) Down(ctx context.Context, inst *Instance, timeout time.Duration) error {
	platData, err := p.loadPlatformData(inst)
	if err != nil {
		return fmt.Errorf("loading platform data: %w", err)
	}

	// Try stereosd shutdown first (clean guest shutdown)
	if platData.IP != "" {
		transport := &vsock.TCPTransport{Host: platData.IP, Port: vsock.VsockPort}
		client, err := vsock.Connect(transport, 5*time.Second)
		if err == nil {
			_ = client.Shutdown(ctx, "mb down")
			_ = client.Close()

			// Wait for Proxmox to report stopped
			deadline := time.Now().Add(timeout)
			for time.Now().Before(deadline) {
				status, err := p.api.vmStatus(ctx, platData.VMID)
				if err == nil && status == "stopped" {
					inst.VMState = StateStopped
					return nil
				}
				time.Sleep(1 * time.Second)
			}
		}
	}

	// Fallback: Proxmox API shutdown (sends ACPI shutdown)
	if err := p.api.shutdownVM(ctx, platData.VMID); err != nil {
		// Last resort: force stop
		return p.ForceDown(ctx, inst)
	}

	// Wait for stop
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := p.api.vmStatus(ctx, platData.VMID)
		if err == nil && status == "stopped" {
			inst.VMState = StateStopped
			return nil
		}
		time.Sleep(1 * time.Second)
	}

	return p.ForceDown(ctx, inst)
}

// ForceDown immediately stops the Proxmox VM.
func (p *ProxmoxBackend) ForceDown(ctx context.Context, inst *Instance) error {
	platData, err := p.loadPlatformData(inst)
	if err != nil {
		inst.VMState = StateStopped
		return nil
	}

	_ = p.api.stopVM(ctx, platData.VMID)
	inst.VMState = StateStopped
	return nil
}

// Destroy stops and deletes the Proxmox VM and removes the local instance directory.
func (p *ProxmoxBackend) Destroy(ctx context.Context, inst *Instance) error {
	platData, err := p.loadPlatformData(inst)
	if err == nil {
		// Stop if running
		status, _ := p.api.vmStatus(ctx, platData.VMID)
		if status == "running" {
			_ = p.api.stopVM(ctx, platData.VMID)
			// Wait briefly for stop
			for i := 0; i < 10; i++ {
				time.Sleep(1 * time.Second)
				s, _ := p.api.vmStatus(ctx, platData.VMID)
				if s == "stopped" {
					break
				}
			}
		}
		_ = p.api.deleteVM(ctx, platData.VMID)
	}

	if err := os.RemoveAll(inst.Dir); err != nil {
		return fmt.Errorf("removing instance directory: %w", err)
	}
	return nil
}

// Status queries the Proxmox API for VM state.
func (p *ProxmoxBackend) Status(ctx context.Context, inst *Instance) (State, error) {
	platData, err := p.loadPlatformData(inst)
	if err != nil {
		return StateStopped, nil
	}

	status, err := p.api.vmStatus(ctx, platData.VMID)
	if err != nil {
		return StateError, nil
	}

	switch status {
	case "running":
		return StateRunning, nil
	case "stopped":
		return StateStopped, nil
	default:
		return StateRunning, nil
	}
}

// List returns all Proxmox instances by scanning the vms directory.
func (p *ProxmoxBackend) List(ctx context.Context) ([]*Instance, error) {
	vmsDir := VMsDir(p.baseDir)
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
		if state.Backend != "proxmox" {
			continue
		}

		inst, err := p.LoadInstance(entry.Name())
		if err != nil {
			continue
		}
		status, _ := p.Status(ctx, inst)
		inst.VMState = status
		instances = append(instances, inst)
	}

	return instances, nil
}

// LoadInstance reads a persisted Proxmox instance by name.
func (p *ProxmoxBackend) LoadInstance(name string) (*Instance, error) {
	vmDir := filepath.Join(VMsDir(p.baseDir), name)
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

// loadPlatformData reads the Proxmox-specific state from the instance.
func (p *ProxmoxBackend) loadPlatformData(inst *Instance) (*proxmoxPlatformData, error) {
	state, err := loadState(inst.Dir)
	if err != nil {
		return nil, err
	}
	if len(state.PlatformData) == 0 {
		return nil, fmt.Errorf("no platform data for instance %q", inst.Name)
	}

	var platData proxmoxPlatformData
	if err := json.Unmarshal(state.PlatformData, &platData); err != nil {
		return nil, fmt.Errorf("parsing platform data: %w", err)
	}
	return &platData, nil
}

// waitAndProvision discovers the VM's IP address and provisions it via stereosd.
func (p *ProxmoxBackend) waitAndProvision(ctx context.Context, inst *Instance, cfg *config.JcardConfig, vmid int) error {
	// Wait for the VM to get an IP address
	ip, err := p.waitForIP(ctx, vmid, 120*time.Second)
	if err != nil {
		return fmt.Errorf("waiting for VM IP: %w", err)
	}

	// Update platform data with discovered IP
	platData := proxmoxPlatformData{VMID: vmid, IP: ip}
	platBytes, _ := json.Marshal(platData)

	state, err := loadState(inst.Dir)
	if err != nil {
		return fmt.Errorf("loading state for IP update: %w", err)
	}
	state.PlatformData = platBytes
	if err := saveState(inst.Dir, state); err != nil {
		return fmt.Errorf("saving state with IP: %w", err)
	}

	// Connect to stereosd via TCP
	transport := &vsock.TCPTransport{Host: ip, Port: vsock.VsockPort}

	var client *vsock.Client
	var connectErr error
	deadline := time.Now().Add(120 * time.Second)
	backoff := 100 * time.Millisecond
	for time.Now().Before(deadline) {
		client, connectErr = vsock.Connect(transport, 2*time.Second)
		if connectErr == nil {
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
		return fmt.Errorf("could not connect to stereosd at %s:%d after 120s: %w", ip, vsock.VsockPort, connectErr)
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

	// Mount shared directories (virtiofs for Proxmox VMs)
	for i, shared := range cfg.Shared {
		tag := fmt.Sprintf("share%d", i)
		if err := client.Mount(ctx, tag, shared.Guest, "9p", shared.ReadOnly); err != nil {
			return fmt.Errorf("mounting %q at %q: %w", shared.Host, shared.Guest, err)
		}
	}

	return nil
}

// waitForIP polls the Proxmox API until the VM has an IP address.
func (p *ProxmoxBackend) waitForIP(ctx context.Context, vmid int, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	backoff := 1 * time.Second

	for time.Now().Before(deadline) {
		ip, err := p.api.vmIPAddress(ctx, vmid)
		if err == nil && ip != "" {
			return ip, nil
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff += time.Second
		}
	}
	return "", fmt.Errorf("VM %d did not get an IP address within %s", vmid, timeout)
}

// PrepareProxmoxDir creates the local instance directory for a Proxmox
// backend VM. No disk image is needed locally — Proxmox manages storage.
func PrepareProxmoxDir(baseDir string, inst *Instance) error {
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

	cfg := inst.Config
	if err := saveJcard(vmDir, cfg); err != nil {
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("saving jcard config: %w", err)
	}

	stateFile := &StateFile{
		Name:        inst.Name,
		CreatedAt:   time.Now().UTC(),
		Mixtape:     cfg.Mixtape,
		CPUs:        cfg.Resources.CPUs,
		Memory:      cfg.Resources.Memory,
		Disk:        cfg.Resources.Disk,
		NetworkMode: cfg.Network.Mode,
		SSHPort:     22,
		Backend:     "proxmox",
	}
	if err := saveState(vmDir, stateFile); err != nil {
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("saving state: %w", err)
	}

	return nil
}

// parseMiBFromSize converts a human-readable size like "4GiB" or "512MiB"
// to megabytes (as an int) for the Proxmox API.
func parseMiBFromSize(s string) int {
	s = strings.TrimSpace(s)
	multipliers := map[string]int{
		"GiB": 1024,
		"gib": 1024,
		"G":   1024,
		"g":   1024,
		"MiB": 1,
		"mib": 1,
		"M":   1,
		"m":   1,
	}
	for suffix, mult := range multipliers {
		if strings.HasSuffix(s, suffix) {
			numStr := strings.TrimSuffix(s, suffix)
			if n, err := strconv.Atoi(strings.TrimSpace(numStr)); err == nil {
				return n * mult
			}
		}
	}
	// Default: try parsing as raw number (assume MiB)
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return 4096 // fallback 4GiB
}

// --------------------------------------------------------------------------
// Proxmox REST API client
// --------------------------------------------------------------------------

type proxmoxAPI struct {
	baseURL    string
	node       string
	tokenID    string
	tokenValue string
	httpClient *http.Client
}

func newProxmoxAPI(cfg *config.ProxmoxConfig) *proxmoxAPI {
	return &proxmoxAPI{
		baseURL:    strings.TrimRight(cfg.Host, "/"),
		node:       cfg.Node,
		tokenID:    cfg.TokenID,
		tokenValue: cfg.TokenSecret,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true, // Proxmox self-signed certs
				},
			},
		},
	}
}

// apiResponse is the standard Proxmox API response envelope.
type apiResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors json.RawMessage `json:"errors,omitempty"`
}

func (a *proxmoxAPI) doRequest(ctx context.Context, method, path string, form url.Values) (*apiResponse, error) {
	u := fmt.Sprintf("%s/api2/json%s", a.baseURL, path)

	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}

	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", fmt.Sprintf("PVEAPIToken=%s=%s", a.tokenID, a.tokenValue))
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("proxmox API %s %s returned %d: %s", method, path, resp.StatusCode, string(respBody))
	}

	var apiResp apiResponse
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return &apiResp, nil
}

// nextID returns the next available VMID from the Proxmox cluster.
func (a *proxmoxAPI) nextID(ctx context.Context) (int, error) {
	resp, err := a.doRequest(ctx, "GET", "/cluster/nextid", nil)
	if err != nil {
		return 0, err
	}

	var id string
	if err := json.Unmarshal(resp.Data, &id); err != nil {
		// Some Proxmox versions return an int
		var idInt int
		if err2 := json.Unmarshal(resp.Data, &idInt); err2 != nil {
			return 0, fmt.Errorf("parsing next ID: %w", err)
		}
		return idInt, nil
	}
	return strconv.Atoi(id)
}

// cloneVM clones a template VM to a new VMID.
func (a *proxmoxAPI) cloneVM(ctx context.Context, templateID, newID int, name string) error {
	form := url.Values{
		"newid": {strconv.Itoa(newID)},
		"name":  {name},
		"full":  {"1"},
	}

	resp, err := a.doRequest(ctx, "POST", fmt.Sprintf("/nodes/%s/qemu/%d/clone", a.node, templateID), form)
	if err != nil {
		return err
	}

	// Clone returns a task UPID — wait for completion
	var upid string
	if err := json.Unmarshal(resp.Data, &upid); err != nil {
		return fmt.Errorf("parsing clone UPID: %w", err)
	}

	return a.waitForTask(ctx, upid, 300*time.Second)
}

// configureVM sets VM configuration parameters.
func (a *proxmoxAPI) configureVM(ctx context.Context, vmid int, params map[string]string) error {
	form := url.Values{}
	for k, v := range params {
		form.Set(k, v)
	}
	_, err := a.doRequest(ctx, "PUT", fmt.Sprintf("/nodes/%s/qemu/%d/config", a.node, vmid), form)
	return err
}

// startVM starts a Proxmox VM.
func (a *proxmoxAPI) startVM(ctx context.Context, vmid int) error {
	resp, err := a.doRequest(ctx, "POST", fmt.Sprintf("/nodes/%s/qemu/%d/status/start", a.node, vmid), nil)
	if err != nil {
		return err
	}

	var upid string
	if err := json.Unmarshal(resp.Data, &upid); err == nil && upid != "" {
		return a.waitForTask(ctx, upid, 60*time.Second)
	}
	return nil
}

// stopVM immediately stops a Proxmox VM.
func (a *proxmoxAPI) stopVM(ctx context.Context, vmid int) error {
	_, err := a.doRequest(ctx, "POST", fmt.Sprintf("/nodes/%s/qemu/%d/status/stop", a.node, vmid), nil)
	return err
}

// shutdownVM sends an ACPI shutdown to a Proxmox VM.
func (a *proxmoxAPI) shutdownVM(ctx context.Context, vmid int) error {
	_, err := a.doRequest(ctx, "POST", fmt.Sprintf("/nodes/%s/qemu/%d/status/shutdown", a.node, vmid), nil)
	return err
}

// deleteVM removes a Proxmox VM. The VM must be stopped.
func (a *proxmoxAPI) deleteVM(ctx context.Context, vmid int) error {
	resp, err := a.doRequest(ctx, "DELETE", fmt.Sprintf("/nodes/%s/qemu/%d", a.node, vmid), nil)
	if err != nil {
		return err
	}

	var upid string
	if err := json.Unmarshal(resp.Data, &upid); err == nil && upid != "" {
		return a.waitForTask(ctx, upid, 60*time.Second)
	}
	return nil
}

// vmStatus returns the current status of a VM ("running", "stopped", etc.).
func (a *proxmoxAPI) vmStatus(ctx context.Context, vmid int) (string, error) {
	resp, err := a.doRequest(ctx, "GET", fmt.Sprintf("/nodes/%s/qemu/%d/status/current", a.node, vmid), nil)
	if err != nil {
		return "", err
	}

	var status struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(resp.Data, &status); err != nil {
		return "", err
	}
	return status.Status, nil
}

// vmIPAddress discovers the IP address of a running VM via the QEMU guest agent.
func (a *proxmoxAPI) vmIPAddress(ctx context.Context, vmid int) (string, error) {
	resp, err := a.doRequest(ctx, "GET", fmt.Sprintf("/nodes/%s/qemu/%d/agent/network-get-interfaces", a.node, vmid), nil)
	if err != nil {
		return "", err
	}

	var result struct {
		Result []struct {
			Name        string `json:"name"`
			IPAddresses []struct {
				IPAddress string `json:"ip-address"`
				IPType    string `json:"ip-address-type"`
			} `json:"ip-addresses"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp.Data, &result); err != nil {
		return "", err
	}

	for _, iface := range result.Result {
		if iface.Name == "lo" {
			continue
		}
		for _, addr := range iface.IPAddresses {
			if addr.IPType == "ipv4" && addr.IPAddress != "127.0.0.1" {
				return addr.IPAddress, nil
			}
		}
	}

	return "", fmt.Errorf("no IPv4 address found for VM %d", vmid)
}

// waitForTask polls a Proxmox task until it completes or times out.
func (a *proxmoxAPI) waitForTask(ctx context.Context, upid string, timeout time.Duration) error {
	encodedUPID := url.PathEscape(upid)
	deadline := time.Now().Add(timeout)
	backoff := 500 * time.Millisecond

	for time.Now().Before(deadline) {
		resp, err := a.doRequest(ctx, "GET", fmt.Sprintf("/nodes/%s/tasks/%s/status", a.node, encodedUPID), nil)
		if err != nil {
			return err
		}

		var taskStatus struct {
			Status string `json:"status"`
			ExitSt string `json:"exitstatus"`
		}
		if err := json.Unmarshal(resp.Data, &taskStatus); err != nil {
			return err
		}

		if taskStatus.Status == "stopped" {
			if taskStatus.ExitSt == "OK" {
				return nil
			}
			return fmt.Errorf("task failed: %s", taskStatus.ExitSt)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
	return fmt.Errorf("task %s did not complete within %s", upid, timeout)
}
