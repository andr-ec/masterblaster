// Package upcmder provides the up command for creating and starting
// a StereOS sandbox VM.
package upcmder

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/papercomputeco/masterblaster/pkg/daemon"
	"github.com/papercomputeco/masterblaster/pkg/daemon/client"
	"github.com/papercomputeco/masterblaster/pkg/ui"
)

const upLongDesc string = `Boot a new StereOS sandbox VM using the jcard.toml in the current
directory (or the path given with --config). Communicates with the
Masterblaster daemon to create, configure, and start the VM.

If the daemon is not running, it will be automatically started in the
background.

Use --name to override the jcard's name, which lets you run several
sandboxes from the SAME jcard/workspace at once: each name gets its own
instance dir, reflink clone, and bridge IP, so they don't collide.

Examples:
  mb up
  mb up --config /path/to/jcard.toml
  mb up --name myproj-2          # a second, independent sandbox of this project`

const upShortDesc string = "Create and start a sandbox"

// NewUpCmd creates the up command.
func NewUpCmd(configDirFn func() string) *cobra.Command {
	var cfgPath string
	var name string

	cmd := &cobra.Command{
		Use:   "up",
		Short: upShortDesc,
		Long:  upLongDesc,
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runUp(configDirFn(), cfgPath, name)
		},
	}

	cmd.Flags().StringVar(&cfgPath, "config", "", "Path to jcard.toml (default: ./jcard.toml)")
	cmd.Flags().StringVar(&name, "name", "", "Sandbox name, overriding the jcard's (run multiple from one workspace)")

	return cmd
}

func runUp(baseDir, cfgPath, name string) error {
	// Resolve config path
	if cfgPath == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		cfgPath = filepath.Join(cwd, "jcard.toml")
	}

	var err error
	cfgPath, err = filepath.Abs(cfgPath)
	if err != nil {
		return fmt.Errorf("resolving config path: %w", err)
	}

	if _, err := os.Stat(cfgPath); err != nil {
		return fmt.Errorf("config not found at %s\n\nCreate one with: mb init", cfgPath)
	}

	if err := client.EnsureDaemon(baseDir); err != nil {
		return err
	}

	c := client.New(baseDir)
	var resp *daemon.Response
	if err := ui.Step(os.Stderr, "Starting sandbox...", func() error {
		var stepErr error
		resp, stepErr = c.Up(name, cfgPath)
		return stepErr
	}); err != nil {
		return err
	}

	if len(resp.Sandboxes) > 0 {
		sb := resp.Sandboxes[0]
		fmt.Fprintln(os.Stderr)
		ui.Success("Sandbox %q launched", sb.Name)
		fmt.Fprintln(os.Stderr)
		// Container backends (nspawn/incus/podman) report a bridge IP +
		// login user via SSHHost/User; qemu/native report the loopback
		// forward. Build the example line from whatever the daemon sent.
		host := sb.SSHHost
		if host == "" {
			host = "127.0.0.1"
		}
		user := sb.User
		if user == "" {
			user = "admin"
		}
		// Container backends get a routable bridge IP. Surface it up front so
		// a dev can reach a service inside the sandbox (e.g. a dev server at
		// http://<ip>:<port>) from the tailnet, where the host advertises the
		// sandbox subnet as a route. qemu/native stay on 127.0.0.1 and skip this.
		if host != "127.0.0.1" {
			ui.Info("IP: %s  (reachable on the tailnet)", host)
		}
		if sb.SSHKeyPath != "" {
			short := shortenHome(sb.SSHKeyPath)
			ui.Info("ssh -p %d -i %s %s@%s", sb.SSHPort, short, user, host)
		} else {
			ui.Info("ssh -p %d %s@%s", sb.SSHPort, user, host)
		}
		ui.Info("mb ssh %s", sb.Name)
	}

	return nil
}

// shortenHome replaces the user's home directory prefix with ~.
func shortenHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if strings.HasPrefix(path, home) {
		return "~" + strings.TrimPrefix(path, home)
	}
	return path
}
