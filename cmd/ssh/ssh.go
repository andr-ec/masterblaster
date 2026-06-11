// Package sshcmder provides the ssh command for connecting to a running
// sandbox via SSH.
package sshcmder

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/papercomputeco/masterblaster/pkg/daemon/client"
	"github.com/papercomputeco/masterblaster/pkg/ssh"
	"github.com/papercomputeco/masterblaster/pkg/ui"
)

const sshLongDesc string = `Connect to a running sandbox via SSH. Replaces the current process
with the ssh binary for a clean interactive experience.

If no name is given and only one sandbox is running, connects to that one.
The default user is "agent" (where the harness runs). Use --user admin
for the operator account in StereOS.

Examples:
  mb ssh
  mb ssh my-sandbox
  mb ssh --user admin my-sandbox`

const sshShortDesc string = "SSH into a running sandbox"

// NewSSHCmd creates the ssh command. verboseFn is called at runtime to check
// whether verbose output is enabled (resolved via viper).
func NewSSHCmd(configDirFn func() string, verboseFn func() bool) *cobra.Command {
	var user string

	cmd := &cobra.Command{
		Use:   "ssh [name]",
		Short: sshShortDesc,
		Long:  sshLongDesc,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			name := ""
			if len(args) > 0 {
				name = args[0]
			}
			userExplicit := c.Flags().Changed("user")
			return runSSH(configDirFn(), name, user, userExplicit, verboseFn())
		},
	}

	// Sentinel default. If the user doesn't pass --user, runSSH picks
	// the sandbox's own User field (sb-<name>) at runtime.
	cmd.Flags().StringVarP(&user, "user", "u", "", "SSH user (default: sandbox's sb-<name>)")

	return cmd
}

func runSSH(baseDir, name, user string, userExplicit, verbose bool) error {
	if err := client.EnsureDaemon(baseDir); err != nil {
		return err
	}

	c := client.New(baseDir)
	resp, err := c.Status(name, false)
	if err != nil {
		return err
	}

	if len(resp.Sandboxes) == 0 {
		return fmt.Errorf("no sandbox found")
	}

	sb := resp.Sandboxes[0]
	if sb.State != "running" {
		return fmt.Errorf("sandbox %q is not running (state: %s)", sb.Name, sb.State)
	}

	// If the caller didn't pass --user, default to the sandbox's own
	// User (sb-<name>). Fall back to "agent" for old sandboxes that
	// haven't been re-provisioned since the sb-<name> rollout.
	if !userExplicit {
		switch {
		case sb.User != "":
			user = sb.User
		default:
			user = "agent"
		}
	}

	// Container backends report their bridge IP in SSHHost; qemu/native
	// report 127.0.0.1 with a forwarded SSHPort. Fall back to loopback
	// for older daemons that don't send SSHHost.
	host := sb.SSHHost
	if host == "" {
		host = "127.0.0.1"
	}

	if verbose {
		ui.Info("Connecting to %s@%s:%d", user, host, sb.SSHPort)
	}

	return ssh.ExecSSH(user, host, sb.SSHPort, sb.SSHKeyPath, sb.Workdir)
}
