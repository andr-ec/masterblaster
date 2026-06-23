package vm

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/papercomputeco/masterblaster/pkg/config"
	"github.com/papercomputeco/masterblaster/pkg/ssh"
)

// NspawnBackend implements the Backend interface using systemd-nspawn
// containers. Unlike QEMU there is no guest kernel: the container shares the
// host kernel and the live host /nix store is bind-mounted in, so devbox/nix
// resolve the package closure with zero copy. Each sandbox gets its own
// network namespace via a veth pair on a host bridge, so concurrent sandboxes
// can each bind the same ports (e.g. :5432) without collision.
//
// The container runs as a transient systemd unit (mb-<name>) created with
// systemd-run, so it survives the vmhost process and is controllable via
// machinectl/systemctl. Privileged operations go through sudo (the operator
// is expected to have passwordless sudo, matching the proven manual recipe).
type NspawnBackend struct {
	baseDir string
	bridge  string
}

// Networking constants. The bridge + NAT for this subnet are provisioned
// out-of-band on the host (a NixOS module mirroring stereos.nix's
// agent-netns). Override the bridge with MB_NSPAWN_BRIDGE.
const (
	nspawnDefaultBridge = "mbbr0"
	nspawnSubnet        = "10.251.0"
	nspawnGateway       = "10.251.0.1"
)

// NewNspawnBackend creates a new systemd-nspawn backend.
func NewNspawnBackend(baseDir string) *NspawnBackend {
	bridge := os.Getenv("MB_NSPAWN_BRIDGE")
	if bridge == "" {
		bridge = nspawnDefaultBridge
	}
	return &NspawnBackend{baseDir: baseDir, bridge: bridge}
}

// nspawnMachine returns the machinectl/systemd unit name for a sandbox.
func nspawnMachine(name string) string { return "mb-" + name }

// nspawnIP derives a stable per-sandbox IPv4 in 10.251.0.20-10.251.0.219
// from the sandbox name. Collisions are possible across many names but
// acceptable for the trusted, handful-of-sandboxes use case; a future
// version can track allocations in the daemon.
func nspawnIP(name string) string {
	sum := sha256.Sum256([]byte(name))
	host := 20 + int(sum[0])%200
	return fmt.Sprintf("%s.%d", nspawnSubnet, host)
}

// Up creates and launches a new nspawn sandbox.
func (n *NspawnBackend) Up(ctx context.Context, inst *Instance) error {
	if inst.Config == nil {
		return fmt.Errorf("instance %q has no configuration", inst.Name)
	}
	vmDir := filepath.Join(VMsDir(n.baseDir), inst.Name)
	if _, err := os.Stat(vmDir); err == nil {
		return fmt.Errorf("sandbox %q already exists at %s", inst.Name, vmDir)
	}
	if err := os.MkdirAll(vmDir, 0755); err != nil {
		return fmt.Errorf("creating instance directory: %w", err)
	}
	inst.Dir = vmDir
	if err := saveJcard(vmDir, inst.Config); err != nil {
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("saving jcard config: %w", err)
	}
	if err := n.bootInternal(ctx, inst, inst.Config); err != nil {
		_ = os.RemoveAll(vmDir)
		return err
	}
	return nil
}

// Boot is the exported entry point for vmhost processes. The instance
// directory already exists (created by the daemon's prepareDisk).
func (n *NspawnBackend) Boot(ctx context.Context, inst *Instance) error {
	if inst.Dir == "" {
		inst.Dir = filepath.Join(VMsDir(n.baseDir), inst.Name)
	}
	cfg, err := n.loadConfig(inst)
	if err != nil {
		return err
	}
	return n.bootInternal(ctx, inst, cfg)
}

// Start re-launches an existing stopped sandbox.
func (n *NspawnBackend) Start(ctx context.Context, inst *Instance) error {
	if inst.Dir == "" {
		inst.Dir = filepath.Join(VMsDir(n.baseDir), inst.Name)
	}
	cfg, err := n.loadConfig(inst)
	if err != nil {
		return err
	}
	return n.bootInternal(ctx, inst, cfg)
}

// loadConfig returns inst.Config, loading it from the saved jcard if unset.
func (n *NspawnBackend) loadConfig(inst *Instance) (*config.JcardConfig, error) {
	if inst.Config != nil {
		return inst.Config, nil
	}
	cfg, err := config.Load(inst.JcardPath())
	if err != nil {
		return nil, fmt.Errorf("loading config for %q: %w", inst.Name, err)
	}
	inst.Config = cfg
	return cfg, nil
}

// bootInternal generates the SSH key, persists state, builds the container
// root, and launches the systemd-nspawn unit.
func (n *NspawnBackend) bootInternal(ctx context.Context, inst *Instance, cfg *config.JcardConfig) error {
	sshKeyPath, sshPubKey, err := ssh.GenerateKeyPair(inst.Dir, fmt.Sprintf("mb-%s", inst.Name))
	if err != nil {
		return fmt.Errorf("generating SSH keypair: %w", err)
	}
	inst.SSHKeyPath = sshKeyPath
	inst.sshPublicKey = sshPubKey
	inst.SSHPort = 22
	inst.IPAddr = nspawnIP(inst.Name)

	stateFile := &StateFile{
		Name:        inst.Name,
		CreatedAt:   time.Now().UTC(),
		Mixtape:     cfg.Mixtape,
		CPUs:        cfg.Resources.CPUs,
		Memory:      cfg.Resources.Memory,
		NetworkMode: cfg.Network.Mode,
		SSHPort:     22,
		SSHKeyPath:  sshKeyPath,
		IPAddr:      inst.IPAddr,
		Backend:     "nspawn",
	}
	if err := saveState(inst.Dir, stateFile); err != nil {
		return fmt.Errorf("saving state: %w", err)
	}

	if err := n.launch(ctx, inst, cfg); err != nil {
		return err
	}
	inst.VMState = StateRunning
	return nil
}

// launch builds the container root and starts the transient nspawn unit.
// On any failure it sudo-removes the (possibly root-owned) rootfs so a
// subsequent retry isn't blocked by leftovers the daemon's user-level
// cleanup can't delete.
func (n *NspawnBackend) launch(ctx context.Context, inst *Instance, cfg *config.JcardConfig) (retErr error) {
	machine := nspawnMachine(inst.Name)

	// If a unit with this name is lingering from a prior run, clear it so
	// systemd-run doesn't fail with "unit already exists".
	_ = exec.Command("sudo", "systemctl", "stop", machine).Run()
	_ = exec.Command("sudo", "systemctl", "reset-failed", machine).Run()

	root := filepath.Join(inst.Dir, "root")
	defer func() {
		if retErr != nil {
			// Stop any partially-started container before removing its
			// (possibly root-owned) rootfs.
			_ = exec.Command("sudo", "machinectl", "terminate", machine).Run()
			_ = exec.Command("sudo", "systemctl", "stop", machine).Run()
			sudoRemoveAll(root)
		}
	}()

	if _, err := n.buildRoot(inst, cfg); err != nil {
		return fmt.Errorf("building container root: %w", err)
	}

	// Chdir into the agent's working dir (defaults to the first shared mount's
	// guest path) rather than a hardcoded /workspace, so the shell lands in the
	// workspace wherever it's mounted — e.g. when the project is mounted at its
	// host-identical path so host-absolute artifacts (.venv, devbox profiles)
	// resolve.
	workdir := cfg.Agent.Workdir
	if workdir == "" {
		workdir = "/workspace"
	}

	// Assemble the systemd-nspawn invocation. The leading args are for
	// systemd-run (transient unit), then the nspawn command itself.
	args := []string{
		"systemd-run",
		"--collect",
		"--unit=" + machine,
		"--description=mb sandbox " + inst.Name,
		"--",
		"systemd-nspawn", "-q",
		"--machine=" + machine,
		"-D", root,
		"--bind=/nix",           // LIVE host store + DB + daemon socket
		"--bind-ro=/etc/static", // NixOS /etc tree (certs, profiles)
		"--bind-ro=/etc/ssl",
		"--bind-ro=/etc/nix",
		"--network-bridge=" + n.bridge,
		"--capability=CAP_NET_ADMIN",
		"--link-journal=no", // don't create a root-owned journal tree in the rootfs
		"--chdir=" + workdir,
		"--setenv=NIX_REMOTE=daemon",
	}

	// Bind operator-home dirs the mirrored .zshrc references literally (e.g.
	// ~/.zsh/plugins) read-only at the same path so those sources resolve.
	for _, b := range resolveOperatorShell().Binds {
		args = append(args, "--bind-ro="+b+":"+b)
	}

	// Shared mounts. clone-mode shares are reflinked into per-sandbox
	// staging first (reusing the same helper the native backend uses).
	for _, sh := range cfg.Shared {
		src := sh.Host
		if sh.Mode == "clone" {
			cloned, err := ensureCloneStaging(inst.Dir, sh)
			if err != nil {
				return fmt.Errorf("clone-staging %q: %w", sh.Host, err)
			}
			src = cloned
		}
		flag := "--bind="
		if sh.ReadOnly {
			flag = "--bind-ro="
		}
		args = append(args, fmt.Sprintf("%s%s:%s", flag, src, sh.Guest))
	}

	// Payload: configure networking, start sshd. Pass the IP as $1.
	args = append(args, "/bin/sh", "/init.sh", inst.IPAddr)

	cmd := exec.CommandContext(ctx, "sudo", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("launching nspawn unit %s: %w: %s", machine, err, strings.TrimSpace(string(out)))
	}

	// Wait until sshd is accepting connections — but fail fast (with the
	// container's journal) if the transient unit dies first.
	if err := n.waitReady(ctx, machine, inst.IPAddr+":22", 60*time.Second); err != nil {
		return fmt.Errorf("sandbox %q: %w", inst.Name, err)
	}
	return nil
}

// waitReady blocks until addr accepts a TCP connection. If the transient
// unit exits (e.g. the init script failed) it returns immediately with the
// unit's journal tail, instead of waiting out the full timeout.
func (n *NspawnBackend) waitReady(ctx context.Context, machine, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
			_ = conn.Close()
			return nil
		}
		out, _ := exec.Command("sudo", "systemctl", "is-active", machine).Output()
		switch strings.TrimSpace(string(out)) {
		case "active", "activating":
			// still coming up
		default:
			j, _ := exec.Command("sudo", "journalctl", "-u", machine, "--no-pager", "-n", "15").CombinedOutput()
			return fmt.Errorf("container unit %s exited before %s became reachable:\n%s", machine, addr, strings.TrimSpace(string(j)))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s not reachable within %s", addr, timeout)
}

// buildRoot creates the minimal container root under inst.Dir/root: the
// handful of files systemd-nspawn and a shell need (the rest of userspace
// comes from the bind-mounted /nix), plus the init payload and the injected
// SSH authorized_keys.
func (n *NspawnBackend) buildRoot(inst *Instance, cfg *config.JcardConfig) (string, error) {
	root := filepath.Join(inst.Dir, "root")
	for _, d := range []string{"etc", "bin", "usr/lib", "workspace", "proc", "sys", "dev", "run", "tmp", "root/.ssh", "etc/ssh", "var/empty"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0755); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	// /bin/sh -> the live store bash (valid inside because /nix is bound).
	bash, err := resolveHostBin("bash")
	if err != nil {
		return "", err
	}
	shPath := filepath.Join(root, "bin/sh")
	_ = os.Remove(shPath)
	if err := os.Symlink(bash, shPath); err != nil {
		return "", fmt.Errorf("symlink /bin/sh: %w", err)
	}

	// Shared PATH + cert env so SSH sessions (interactive and
	// `ssh host cmd`) have coreutils/nix on PATH and trust the host CA.
	pathDirs, err := n.containerPathDirs()
	if err != nil {
		return "", err
	}
	pathStr := strings.Join(pathDirs, ":")
	caBundle := resolveCABundle()
	sessionEnv := fmt.Sprintf("PATH=%s NIX_REMOTE=daemon", pathStr)
	if caBundle != "" {
		sessionEnv += fmt.Sprintf(" SSL_CERT_FILE=%s NIX_SSL_CERT_FILE=%s", caBundle, caBundle)
	}
	profile := fmt.Sprintf("export PATH=%s\nexport NIX_REMOTE=daemon\n", pathStr)
	if caBundle != "" {
		profile += fmt.Sprintf("export SSL_CERT_FILE=%s\nexport NIX_SSL_CERT_FILE=%s\n", caBundle, caBundle)
	}

	// UTF-8 locale, mirroring the host. Without this glibc defaults to C/POSIX
	// (latin1), which e.g. makes the Erlang VM warn and mis-encode. LANG alone
	// is enough (LC_* inherit it); LOCALE_ARCHIVE points glibc at the host's
	// locale data (a bound /nix path) since /run/current-system isn't mounted.
	sessionEnv += " LANG=en_US.UTF-8"
	profile += "export LANG=en_US.UTF-8\n"
	if la := resolveLocaleArchive(); la != "" {
		sessionEnv += " LOCALE_ARCHIVE=" + la
		profile += "export LOCALE_ARCHIVE=" + la + "\n"
	}

	machine := nspawnMachine(inst.Name)

	// Mirror the operator's home-manager login shell (zsh + dotfiles) into the
	// sandbox; falls back to /bin/sh when not on NixOS.
	op := resolveOperatorShell()
	rootShell := "/bin/sh"
	if op.ShellBin != "" {
		rootShell = op.ShellBin
	}

	files := map[string]string{
		// root plus the privilege-separation user modern OpenSSH requires
		// (its home /var/empty must exist and be root-owned).
		"etc/passwd":        fmt.Sprintf("root:x:0:0:root:/root:%s\nsshd:x:498:65534:sshd:/var/empty:/bin/sh\n", rootShell),
		"etc/group":         "root:x:0:\nusers:x:100:\nnogroup:x:65534:\n",
		"etc/hosts":         fmt.Sprintf("127.0.0.1 localhost\n%s %s\n", inst.IPAddr, machine),
		"etc/nsswitch.conf": "hosts: files dns\n",
		"etc/resolv.conf":   "nameserver 1.1.1.1\n",
		// The store-level zsh global rc sources /etc/zshenv only when /etc/NIXOS
		// exists. We use that hook to defang compinit: the operator's zsh plugin
		// dirs are bind-mounted from their home (uid != root), so as container-root
		// compinit's compaudit flags them "insecure" and prompts. The alias makes
		// the .zshrc's bare `compinit` run as `compinit -i` (load them, no prompt).
		"etc/NIXOS":          "",
		"etc/zshenv":         "alias compinit='compinit -i'\n",
		"usr/lib/os-release": "PRETTY_NAME=\"mb nspawn sandbox\"\nID=mb-nspawn\nNAME=mb-nspawn\nVERSION_ID=1\n",
		// Minimal sshd config: key-only root login. Host keys are generated
		// by `ssh-keygen -A` in init.sh; AuthorizedKeysFile defaults to
		// /root/.ssh/authorized_keys (where buildRoot injects the pubkey).
		"etc/ssh/sshd_config": "Port 22\nPermitRootLogin prohibit-password\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nUsePAM no\nStrictModes no\nSetEnv " + sessionEnv + "\n",
		// Sourced by interactive login shells (`mb ssh`). SetEnv above
		// covers non-interactive `ssh host cmd`; this is the belt-and-
		// suspenders for an interactive shell that resets PATH.
		"etc/profile": profile,
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0644); err != nil {
			return "", fmt.Errorf("write %s: %w", rel, err)
		}
	}

	// Inject the ephemeral SSH public key for root login.
	if inst.sshPublicKey != "" {
		if err := os.WriteFile(filepath.Join(root, "root/.ssh/authorized_keys"), []byte(inst.sshPublicKey+"\n"), 0600); err != nil {
			return "", fmt.Errorf("write authorized_keys: %w", err)
		}
	}

	// Symlink the operator's resolved zsh dotfiles into /root so the login shell
	// loads the same config as the host dev VM (targets are bound /nix paths).
	for dest, src := range op.Dotfiles {
		dst := filepath.Join(root, dest)
		_ = os.Remove(dst)
		if err := os.Symlink(src, dst); err != nil {
			return "", fmt.Errorf("symlink %s: %w", dest, err)
		}
	}

	init, err := n.buildInit()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(root, "init.sh"), []byte(init), 0755); err != nil {
		return "", fmt.Errorf("write init.sh: %w", err)
	}
	return root, nil
}

// buildInit renders the container init script. It configures the veth IP and
// default route, then execs sshd. All binaries are referenced by their
// absolute /nix store paths (resolved on the host, valid inside because the
// same store is bound) so the script works before any PATH is set up.
func (n *NspawnBackend) buildInit() (string, error) {
	ipBin, err := resolveHostBin("ip")
	if err != nil {
		return "", err
	}
	sshd, err := resolveHostBin("sshd")
	if err != nil {
		return "", err
	}
	sshKeygen, err := resolveHostBin("ssh-keygen")
	if err != nil {
		return "", err
	}
	// coreutils (mkdir et al.) — referenced absolutely and added to PATH,
	// since /run/current-system and the default profile aren't bound in.
	mkdir, err := resolveHostBin("mkdir")
	if err != nil {
		return "", err
	}
	caBundle := resolveCABundle()

	// PATH built from resolved store bin dirs so a shell inside the sandbox
	// has coreutils + bash + the nix client without relying on bound /run.
	pathDirs, err := n.containerPathDirs()
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(`#!/bin/sh
set -e
ip="$1"
%[1]s addr add "${ip}/24" dev host0 2>/dev/null || true
%[1]s link set host0 up 2>/dev/null || true
%[1]s link set lo up 2>/dev/null || true
%[1]s route add default via %[2]s 2>/dev/null || true

export PATH=%[6]s
export NIX_REMOTE=daemon
export SSL_CERT_FILE=%[5]s
export NIX_SSL_CERT_FILE=%[5]s

%[7]s -p /run/sshd /etc/ssh
# The rootfs was created by the (unprivileged) host user; sshd's privilege
# separation dir must be root-owned. We are container-root here, so fix it.
chown 0:0 /var/empty 2>/dev/null || true
chmod 0755 /var/empty 2>/dev/null || true
%[4]s -A >/dev/null 2>&1 || true
exec %[3]s -D -e -f /etc/ssh/sshd_config
`, ipBin, nspawnGateway, sshd, sshKeygen, caBundle, strings.Join(pathDirs, ":"), mkdir), nil
}

// containerPathDirs returns the PATH directories a shell inside the sandbox
// needs (coreutils + bash + the ssh/nix client), resolved to live /nix store
// dirs. Shared by init.sh (the sshd PATH) and the sshd_config SetEnv / profile
// so an interactive `mb ssh` and a non-interactive `ssh host cmd` see the same
// PATH. The default nix profile bin is included so devbox-installed tools land
// on PATH once realized.
func (n *NspawnBackend) containerPathDirs() ([]string, error) {
	mkdir, err := resolveHostBin("mkdir")
	if err != nil {
		return nil, err
	}
	bash, err := resolveHostBin("bash")
	if err != nil {
		return nil, err
	}
	sshd, err := resolveHostBin("sshd")
	if err != nil {
		return nil, err
	}
	// Prepend the operator's home-manager profile bin so the sandbox shell sees
	// the same dev toolchain (and zsh) as the host dev VM. Empty if not on NixOS.
	dirs := []string{}
	if op := resolveOperatorShell(); op.ProfileBin != "" {
		dirs = append(dirs, op.ProfileBin)
	}
	dirs = append(dirs,
		filepath.Dir(mkdir),
		filepath.Dir(bash),
		filepath.Dir(sshd),
		"/nix/var/nix/profiles/default/bin",
		"/usr/bin", "/bin",
	)
	return dedupeDirs(dirs...), nil
}

// dedupeDirs returns the input dirs with duplicates removed, order preserved.
func dedupeDirs(dirs ...string) []string {
	seen := make(map[string]bool, len(dirs))
	out := make([]string, 0, len(dirs))
	for _, d := range dirs {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// Down gracefully powers off the sandbox.
func (n *NspawnBackend) Down(_ context.Context, inst *Instance, _ time.Duration) error {
	machine := nspawnMachine(inst.Name)
	if out, err := exec.Command("sudo", "machinectl", "poweroff", machine).CombinedOutput(); err != nil {
		// Fall back to stopping the transient unit directly.
		if out2, err2 := exec.Command("sudo", "systemctl", "stop", machine).CombinedOutput(); err2 != nil {
			return fmt.Errorf("stopping %s: poweroff: %s; stop: %s", machine, strings.TrimSpace(string(out)), strings.TrimSpace(string(out2)))
		}
	}
	inst.VMState = StateStopped
	return nil
}

// ForceDown immediately terminates the sandbox.
func (n *NspawnBackend) ForceDown(_ context.Context, inst *Instance) error {
	machine := nspawnMachine(inst.Name)
	_ = exec.Command("sudo", "machinectl", "terminate", machine).Run()
	_ = exec.Command("sudo", "systemctl", "stop", machine).Run()
	inst.VMState = StateStopped
	return nil
}

// Destroy terminates the sandbox and removes its on-disk resources. The
// transient unit owns the veth, which systemd-nspawn tears down on exit; the
// reflinked clone staging lives under inst.Dir and goes with it.
func (n *NspawnBackend) Destroy(ctx context.Context, inst *Instance) error {
	_ = n.ForceDown(ctx, inst)
	// The rootfs holds root-owned files the container created (host keys,
	// any root-owned writes), so a user-level RemoveAll can't delete it.
	if err := sudoRemoveAll(inst.Dir); err != nil {
		return fmt.Errorf("removing instance directory: %w", err)
	}
	return nil
}

// Status reports whether the sandbox's transient unit is active.
func (n *NspawnBackend) Status(_ context.Context, inst *Instance) (State, error) {
	machine := nspawnMachine(inst.Name)
	out, _ := exec.Command("sudo", "systemctl", "is-active", machine).Output()
	switch strings.TrimSpace(string(out)) {
	case "active", "activating":
		return StateRunning, nil
	case "failed":
		return StateError, nil
	default:
		return StateStopped, nil
	}
}

// List returns all nspawn instances by scanning the vms directory.
func (n *NspawnBackend) List(ctx context.Context) ([]*Instance, error) {
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
		if err != nil || state.Backend != "nspawn" {
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

// LoadInstance reads a persisted nspawn instance by name.
func (n *NspawnBackend) LoadInstance(name string) (*Instance, error) {
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
		IPAddr:     state.IPAddr,
	}, nil
}

// PrepareNspawnDir creates the instance directory for an nspawn sandbox.
// No disk image is needed — the rootfs is built at boot under inst.Dir/root.
func PrepareNspawnDir(baseDir string, inst *Instance) error {
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

	if err := saveJcard(vmDir, inst.Config); err != nil {
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("saving jcard config: %w", err)
	}
	stateFile := &StateFile{
		Name:        inst.Name,
		CreatedAt:   time.Now().UTC(),
		Mixtape:     inst.Config.Mixtape,
		CPUs:        inst.Config.Resources.CPUs,
		Memory:      inst.Config.Resources.Memory,
		NetworkMode: inst.Config.Network.Mode,
		SSHPort:     22,
		Backend:     "nspawn",
	}
	if err := saveState(vmDir, stateFile); err != nil {
		_ = os.RemoveAll(vmDir)
		return fmt.Errorf("saving state: %w", err)
	}

	// Pre-warm clone-mode staging here, daemon-side, BEFORE spawnVMHost's
	// 120s ready poll. A large workspace (tens of GB / many files) can take
	// minutes to reflink even on XFS; doing it inside the vmhost boot path
	// would blow that deadline and the daemon would tear the sandbox down.
	// ensureCloneStaging is cache-keyed by guest path, so launch() then gets
	// an instant hit. No timeout applies to prepareDisk.
	for _, sh := range inst.Config.Shared {
		if sh.Mode != "clone" {
			continue
		}
		if _, err := ensureCloneStaging(vmDir, sh); err != nil {
			_ = sudoRemoveAll(vmDir)
			return fmt.Errorf("pre-warming clone staging for %q: %w", sh.Host, err)
		}
	}
	return nil
}

// sudoRemoveAll removes a path that may contain root-owned files created by
// the container. Best-effort: returns the command error if it fails.
func sudoRemoveAll(path string) error {
	if path == "" || path == "/" {
		return fmt.Errorf("refusing to remove %q", path)
	}
	if out, err := exec.Command("sudo", "rm", "-rf", path).CombinedOutput(); err != nil {
		return fmt.Errorf("sudo rm -rf %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// resolveHostBin returns the absolute, symlink-resolved path to a binary on
// the host PATH (typically a /nix/store path). Because the whole /nix store
// is bind-mounted into the container, this path is valid inside it too.
func resolveHostBin(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("locating %q on host PATH: %w", name, err)
	}
	// Resolve the *directory* to its /nix store path but keep the command's
	// own basename. NixOS exposes coreutils tools (mkdir, etc.) as symlinks
	// into a single multicall `coreutils` binary that dispatches on argv[0];
	// fully resolving the symlink would yield ".../coreutils", which then
	// rejects mkdir's flags. Keeping the basename preserves dispatch.
	if dir, derr := filepath.EvalSymlinks(filepath.Dir(p)); derr == nil {
		cand := filepath.Join(dir, filepath.Base(p))
		if strings.HasPrefix(cand, "/nix/") {
			if _, serr := os.Stat(cand); serr == nil {
				return cand, nil
			}
		}
	}
	// Fall back to a full resolve (fine for non-multicall binaries, and
	// still lands in /nix).
	if real, rerr := filepath.EvalSymlinks(p); rerr == nil {
		return real, nil
	}
	return p, nil
}

// operatorShell mirrors the invoking user's home-manager login shell into a
// sandbox, so a `mb ssh` session (root inside the container) gets the same zsh
// config + dev toolchain as the host dev VM. Profile bin, shell binary, and
// dotfile sources all live in /nix (bound into the container); Binds are
// operator-home paths the generated .zshrc references literally and so must be
// bind-mounted at the same path. An empty ShellBin means "fall back to bash".
type operatorShell struct {
	ProfileBin string            // <profile>/bin, prepended to the sandbox PATH
	ShellBin   string            // resolved zsh for root's login shell
	Dotfiles   map[string]string // rootfs-relative dest -> resolved /nix source
	Binds      []string          // operator-home dirs to bind read-only as-is
}

// resolveOperatorShell inspects the invoking user's home-manager profile and
// zsh dotfiles. It never errors: any piece that is absent (no per-user profile,
// no zsh, not on NixOS) is simply omitted, degrading the sandbox to the minimal
// bash shell built by buildRoot.
func resolveOperatorShell() operatorShell {
	op := operatorShell{Dotfiles: map[string]string{}}
	u, err := user.Current()
	if err != nil {
		return op
	}
	// home-manager per-user profile: dev toolchain + zsh on PATH. Resolve the
	// /etc/profiles symlink chain to its store path (valid inside the sandbox).
	if real, err := filepath.EvalSymlinks("/etc/profiles/per-user/" + u.Username); err == nil && strings.HasPrefix(real, "/nix/") {
		op.ProfileBin = filepath.Join(real, "bin")
		if zsh, zerr := filepath.EvalSymlinks(filepath.Join(op.ProfileBin, "zsh")); zerr == nil && strings.HasPrefix(zsh, "/nix/") {
			op.ShellBin = zsh
		}
	}
	// zsh dotfiles are HM-managed symlinks into the store; resolve them so the
	// rootfs symlink targets a bound /nix path, not the operator's mutable home.
	for _, f := range []string{".zshrc", ".zshenv", ".zprofile", ".zlogin"} {
		if real, err := filepath.EvalSymlinks(filepath.Join(u.HomeDir, f)); err == nil {
			op.Dotfiles["root/"+f] = real
		}
	}
	// The generated .zshrc hardcodes <home>/.zsh/plugins (syntax-highlighting,
	// autopair). Bind that dir read-only at the same path so those sources
	// resolve inside; the files under it are themselves /nix symlinks.
	if zdir := filepath.Join(u.HomeDir, ".zsh"); dirExists(zdir) {
		op.Binds = append(op.Binds, zdir)
	}
	return op
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// resolveLocaleArchive resolves the NixOS glibc locale-archive to its store
// path (valid inside the sandbox since /nix is bound, unlike /run/current-system).
// Empty if absent (non-NixOS host) — callers then just leave the locale unset.
func resolveLocaleArchive() string {
	const def = "/run/current-system/sw/lib/locale/locale-archive"
	if real, err := filepath.EvalSymlinks(def); err == nil && strings.HasPrefix(real, "/nix/") {
		return real
	}
	return ""
}

// resolveCABundle resolves the host CA bundle to its store path, falling back
// to the conventional NixOS location.
func resolveCABundle() string {
	const def = "/etc/ssl/certs/ca-bundle.crt"
	if real, err := filepath.EvalSymlinks(def); err == nil {
		return real
	}
	return def
}

// waitTCP blocks until a TCP connection to addr succeeds or the deadline.
func waitTCP(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	backoff := 200 * time.Millisecond
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < time.Second {
			backoff *= 2
		}
	}
	return fmt.Errorf("timed out: %w", lastErr)
}
