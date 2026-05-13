// Reflink-staged shared mounts and dotfile bundles.
//
// `[[shared]] reflink = true` materializes a CoW snapshot of Host under
// <vmDir>/staging/share<i> at prepare time and rewrites Host to point
// at the snapshot. `[dotfiles]` bundles a list of host paths into one
// CoW tree at <vmDir>/staging/dotfiles and appends a synthetic
// [[shared]] surfacing it at GuestHome.
//
// Writes inside the guest land on the snapshot, never on the user's
// originals — necessary when the share points at a live workspace or
// real credentials.

package vm

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/papercomputeco/masterblaster/pkg/config"
)

// StageReflinks rewrites cfg.Shared in place: every entry with
// Reflink = true gets its Host replaced by a CoW snapshot under
// <vmDir>/staging/share<i>. Idempotent — entries already pointing
// inside vmDir are left alone, so existing snapshots survive a
// down → up cycle.
func StageReflinks(vmDir string, cfg *config.JcardConfig) error {
	for i := range cfg.Shared {
		if !cfg.Shared[i].Reflink {
			continue
		}

		// Idempotency: a Host already under <vmDir>/staging/ means
		// this is a restart of a previously-prepared sandbox — keep
		// the existing snapshot.
		if strings.HasPrefix(cfg.Shared[i].Host, vmDir+string(filepath.Separator)) {
			continue
		}

		src := cfg.Shared[i].Host
		if _, err := os.Stat(src); err != nil {
			return fmt.Errorf("share%d host %q: %w", i, src, err)
		}

		stagingRoot := filepath.Join(vmDir, "staging")
		if err := os.MkdirAll(stagingRoot, 0o755); err != nil {
			return fmt.Errorf("share%d mkdir staging: %w", i, err)
		}

		target := filepath.Join(stagingRoot, fmt.Sprintf("share%d", i))
		if err := os.RemoveAll(target); err != nil {
			return fmt.Errorf("share%d cleanup: %w", i, err)
		}

		if err := cloneTree(src, target); err != nil {
			return fmt.Errorf("share%d clone %s -> %s: %w", i, src, target, err)
		}
		cfg.Shared[i].Host = target
	}
	return nil
}

// StageDotfiles, when cfg.Dotfiles is set, assembles a CoW bundle
// under <vmDir>/staging/dotfiles/ and appends a synthetic SharedMount
// that surfaces the bundle at GuestHome.
//
// Two layers:
//
//  1. If IncludeHomeManagerFor is set, the named user's home-manager
//     generation (home-files/) is reflinked into the bundle as a
//     base layer. Surfaces .zshrc / .zshenv / .zsh/ / etc. that the
//     bundle mount would otherwise shadow.
//  2. Each Paths entry is reflinked on top. Paths under $HOME keep
//     their relative position (~/.claude -> <bundle>/.claude); paths
//     outside $HOME use their basename. User paths overwrite any
//     same-named entry from the home-manager base.
//
// Missing source paths in Paths are warned-and-skipped so an absent
// dotfile doesn't block boot.
//
// Idempotent: re-runs detect the existing synthetic mount and no-op.
// Schema-list edits require `mb destroy` to re-stage.
func StageDotfiles(vmDir string, cfg *config.JcardConfig) error {
	if cfg.Dotfiles == nil ||
		(len(cfg.Dotfiles.Paths) == 0 && cfg.Dotfiles.IncludeHomeManagerFor == "") {
		return nil
	}
	if cfg.Dotfiles.GuestHome == "" {
		return fmt.Errorf("dotfiles.guest_home must be set when dotfiles.paths or include_home_manager_for is non-empty")
	}

	bundleDir := filepath.Join(vmDir, "staging", "dotfiles")

	// Idempotency.
	for _, m := range cfg.Shared {
		if m.Host == bundleDir {
			return nil
		}
	}

	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		return fmt.Errorf("dotfiles: mkdir staging: %w", err)
	}

	// Base layer: home-manager-generated files for the named user. Each
	// top-level entry of <generation>/home-files/ becomes a bundle
	// entry. Empty mode-555 nix-store paths give the agent a working
	// shell init even though our bind mount at GuestHome shadows
	// /home/<user>'s real symlinks.
	if cfg.Dotfiles.IncludeHomeManagerFor != "" {
		hmRoot, err := resolveHomeManagerHomeFiles(cfg.Dotfiles.IncludeHomeManagerFor)
		if err != nil {
			return fmt.Errorf("dotfiles: include_home_manager_for=%q: %w",
				cfg.Dotfiles.IncludeHomeManagerFor, err)
		}
		entries, err := os.ReadDir(hmRoot)
		if err != nil {
			return fmt.Errorf("dotfiles: reading %s: %w", hmRoot, err)
		}
		for _, e := range entries {
			src := filepath.Join(hmRoot, e.Name())
			dst := filepath.Join(bundleDir, e.Name())
			if err := cloneTree(src, dst); err != nil {
				return fmt.Errorf("dotfiles: clone home-manager %q: %w", e.Name(), err)
			}
		}
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("dotfiles: resolving host $HOME: %w", err)
	}

	for _, p := range cfg.Dotfiles.Paths {
		if _, err := os.Lstat(p); err != nil {
			if os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "dotfiles: skipping %q (not found)\n", p)
				continue
			}
			return fmt.Errorf("dotfiles: lstat %q: %w", p, err)
		}

		var rel string
		if strings.HasPrefix(p, home+string(filepath.Separator)) {
			rel = strings.TrimPrefix(p, home+string(filepath.Separator))
		} else {
			rel = filepath.Base(p)
		}
		dst := filepath.Join(bundleDir, rel)

		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("dotfiles: mkdir parent for %q: %w", rel, err)
		}
		// Remove any home-manager file at this path so the user's
		// version wins (cp would otherwise nest into the existing dir).
		if err := os.RemoveAll(dst); err != nil {
			return fmt.Errorf("dotfiles: remove existing %q: %w", rel, err)
		}
		if err := cloneTree(p, dst); err != nil {
			return fmt.Errorf("dotfiles: clone %q -> %q: %w", p, dst, err)
		}
	}

	// Create empty mount-point directories inside the bundle for any
	// other [[shared]] mount whose Guest is *inside* GuestHome. Without
	// this, the dotfile bundle would shadow those guest paths because
	// the bundle has no matching subdir. (Example: [[shared]] guest =
	// "/home/agent/workspace" + [dotfiles] guest_home = "/home/agent".)
	prefix := strings.TrimRight(cfg.Dotfiles.GuestHome, "/") + "/"
	for _, m := range cfg.Shared {
		if !strings.HasPrefix(m.Guest, prefix) {
			continue
		}
		rel := strings.TrimPrefix(m.Guest, prefix)
		if rel == "" {
			continue
		}
		if err := os.MkdirAll(filepath.Join(bundleDir, rel), 0o755); err != nil {
			return fmt.Errorf("dotfiles: mkdir bundle subdir for %q: %w", m.Guest, err)
		}
	}

	// Prepend (not append) so mounts that overlay subdirs inside
	// GuestHome (e.g. workspace at /home/agent/workspace) get applied
	// AFTER the bundle is mounted and land on the pre-created subdirs.
	cfg.Shared = append(
		[]config.SharedMount{{Host: bundleDir, Guest: cfg.Dotfiles.GuestHome}},
		cfg.Shared...,
	)
	return nil
}

// resolveHomeManagerHomeFiles looks up the host user's home-manager
// generation by inspecting its systemd unit (`home-manager-<user>.service`)
// and returns the absolute path to the generation's `home-files/`
// directory — the tree home-manager would symlink into /home/<user>.
//
// We can't just read /home/<user> because by the time mb runs it, the
// bundle bind mount has typically shadowed those symlinks. The systemd
// unit's ExecStart names the generation path verbatim, so we parse it
// out and let the user opt in via [dotfiles].include_home_manager_for.
func resolveHomeManagerHomeFiles(user string) (string, error) {
	unit := fmt.Sprintf("home-manager-%s.service", user)
	out, err := exec.Command("systemctl", "show", unit,
		"--property=ExecStart", "--no-pager").Output()
	if err != nil {
		return "", fmt.Errorf("systemctl show %s: %w", unit, err)
	}
	// Example output:
	//   ExecStart={ path=/nix/store/.../hm-setup-env ; argv[]=/nix/store/.../hm-setup-env /nix/store/.../home-manager-generation ; ignore_errors=no ; ... }
	const marker = "argv[]="
	idx := strings.Index(string(out), marker)
	if idx < 0 {
		return "", fmt.Errorf("no argv[] in: %s", strings.TrimSpace(string(out)))
	}
	// Tokenize the argv list: split on whitespace, take the second
	// element (the generation path; first is the setup-env binary).
	fields := strings.Fields(string(out[idx+len(marker):]))
	if len(fields) < 2 {
		return "", fmt.Errorf("argv[] has fewer than 2 fields: %s", strings.TrimSpace(string(out)))
	}
	generation := fields[1]
	homeFiles := filepath.Join(generation, "home-files")
	if _, err := os.Stat(homeFiles); err != nil {
		return "", fmt.Errorf("home-files not under %q: %w", generation, err)
	}
	return homeFiles, nil
}

// cloneTree recursively copies src to dst using reflink/clonefile where
// the filesystem supports it. Shelled out to coreutils `cp` rather than
// reimplemented in Go: handling directories, symlinks, sparse files and
// permissions correctly is what cp already does.
//
// Linux: `cp -a --reflink=auto` (FICLONE on btrfs/xfs/bcachefs).
// macOS: `cp -c -R` (clonefile syscall on APFS).
func cloneTree(src, dst string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("cp", "-c", "-R", src, dst)
	default:
		cmd = exec.Command("cp", "-a", "--reflink=auto", src, dst)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w (%s)", strings.Join(cmd.Args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
