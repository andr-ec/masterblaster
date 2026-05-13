// Package ssh provides SSH connectivity to StereOS sandbox VMs.
package ssh

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// ExecSSH replaces the current process with the ssh binary, providing a
// clean interactive experience. Signal handling, terminal resizing, SSH
// agent forwarding, and ~. escape sequences all work correctly because
// the user talks directly to OpenSSH.
//
// If identityFile is non-empty, it is passed as -i to ssh along with
// -o IdentitiesOnly=yes to prevent the SSH agent or default keys from
// being tried (which could exhaust MaxAuthTries before the correct
// ephemeral key is attempted).
//
// If workdir is non-empty, the remote shell cd's there before becoming
// interactive. Mirrors [agent].workdir from jcard.toml so the operator
// lands in the same dir the harness runs in.
func ExecSSH(user, host string, port int, identityFile, workdir string) error {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh binary not found: %w", err)
	}

	args := []string{
		"ssh",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-t", // Force PTY allocation
		"-p", fmt.Sprintf("%d", port),
	}

	if identityFile != "" {
		args = append(args, "-i", identityFile)
		args = append(args, "-o", "IdentitiesOnly=yes")
	}

	args = append(args, fmt.Sprintf("%s@%s", user, host))

	// Land in the configured workdir, then exec a login shell. `cd`
	// is silent-failing so a missing workdir doesn't leave the user at
	// a broken shell. Prefer zsh because the stereOS home-manager
	// generates a .zshrc with direnv / zoxide / completion hooks, but
	// the agent user's /etc/passwd shell is bash-based (agent-ns-shell)
	// and `.bashrc` doesn't exist — so plain `$SHELL` skips all the
	// nice ergonomics. Fall back to bash if zsh isn't on PATH.
	if workdir != "" {
		args = append(args, fmt.Sprintf(
			"cd %s 2>/dev/null; "+
				"if command -v zsh >/dev/null 2>&1; then exec zsh -l; "+
				"else exec ${SHELL:-/bin/bash} -l; fi",
			shellQuote(workdir),
		))
	}

	// Replace process -- never returns on success
	return syscall.Exec(sshBin, args, os.Environ())
}

// shellQuote single-quotes a path for safe inclusion in a remote
// command. Wrap in single quotes; embedded single quotes are escaped
// by closing-and-reopening: `'\''`.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
