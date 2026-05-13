// Reflink-staged shared mounts.
//
// For [[shared]] entries with `reflink = true`, we materialize a CoW
// snapshot of Host under <vmDir>/staging/share<i> at prepare time and
// rewrite Host to point at the snapshot. The bind mount inside the
// guest then sees the snapshot, so writes never reach the user's
// original directory.

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

// StageReflinks rewrites cfg.Shared in-place: every entry with
// Reflink = true gets its Host replaced by a CoW snapshot under
// <vmDir>/staging/share<i>. Idempotent — re-runs (e.g. on
// down → up of an existing sandbox) skip mounts that already point
// inside vmDir, preserving the snapshot's contents across restarts.
func StageReflinks(vmDir string, cfg *config.JcardConfig) error {
	for i := range cfg.Shared {
		if !cfg.Shared[i].Reflink {
			continue
		}

		// Idempotency. If Host already lives under <vmDir>/staging/, this
		// is a restart of a previously-prepared VM — keep the existing
		// snapshot so the agent's prior writes survive a down → up.
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
		// Wipe any partial stage from a previously failed prepare so
		// `cp` sees an empty destination.
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

// cloneTree recursively copies src to dst using reflink/clonefile where
// the filesystem supports it. Shelled out to coreutils `cp` rather than
// reimplemented in Go: handling directories, symlinks, sparse files and
// permissions correctly is exactly what cp already does, and a pure-Go
// rewrite would be a less-tested approximation.
//
// On Linux, `--reflink=auto` uses the FICLONE ioctl on btrfs / xfs /
// bcachefs / overlayfs and falls back to a byte copy elsewhere.
// On macOS, `-c` uses the clonefile(2) syscall (CoW on APFS).
func cloneTree(src, dst string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("cp", "-c", "-R", src, dst)
	default: // linux + other unixes
		cmd = exec.Command("cp", "-a", "--reflink=auto", src, dst)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w (%s)", strings.Join(cmd.Args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
