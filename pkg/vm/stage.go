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

// StageDotfiles, when cfg.Dotfiles is set, gathers every listed host
// path into a CoW bundle under <vmDir>/staging/dotfiles/ and appends a
// synthetic SharedMount that surfaces the bundle at GuestHome.
//
// Layout: paths under $HOME keep their relative position
// (~/.claude -> <bundle>/.claude); paths outside $HOME use their
// basename. Missing source paths are warned-and-skipped so an absent
// dotfile doesn't block boot.
//
// Idempotent: re-runs detect the existing synthetic mount and no-op.
// Schema-list edits require `mb destroy` to re-stage.
func StageDotfiles(vmDir string, cfg *config.JcardConfig) error {
	if cfg.Dotfiles == nil || len(cfg.Dotfiles.Paths) == 0 {
		return nil
	}
	if cfg.Dotfiles.GuestHome == "" {
		return fmt.Errorf("dotfiles.guest_home must be set when dotfiles.paths is non-empty")
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
