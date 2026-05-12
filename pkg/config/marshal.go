package config

import (
	"bytes"

	"github.com/BurntSushi/toml"
)

// Marshal serializes a JcardConfig to TOML in the SAME schema Load
// accepts (singular `[agent]` table). Round-trip safe: Load(Marshal(c))
// returns an equivalent c. Used for on-disk persistence under the mb
// state directory (`~/.config/mb/vms/<name>/jcard.toml`), where vmhost
// reads it back via config.Load.
//
// For the wire form sent to stereosd/agentd (which expects `[[agents]]`),
// use MarshalForAgentd. The two schemas exist because mb keeps a
// one-agent-per-sandbox user surface while agentd parses the multi-agent
// array introduced in papercomputeco/agentd PR #9.
func Marshal(cfg *JcardConfig) ([]byte, error) {
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(cfg); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// wireAgentConfig is one entry in the `[[agents]]` array agentd parses.
// On top of mb's user-facing AgentConfig fields it carries `name` and
// `type` (always "native" for the in-host backend) which agentd needs.
type wireAgentConfig struct {
	Name string `toml:"name"`
	Type string `toml:"type"`
	AgentConfig
}

// wireJcard mirrors JcardConfig but with the singular Agent rewritten
// as a singleton `[[agents]]` array.
type wireJcard struct {
	Backend       string            `toml:"backend,omitempty"`
	Mixtape       string            `toml:"mixtape,omitempty"`
	MixtapeDigest string            `toml:"mixtape_digest,omitempty"`
	Name          string            `toml:"name,omitempty"`
	Resources     ResourcesConfig   `toml:"resources"`
	Network       NetworkConfig     `toml:"network"`
	Shared        []SharedMount     `toml:"shared,omitempty"`
	Secrets       map[string]string `toml:"secrets"`
	Agents        []wireAgentConfig `toml:"agents"`
}

// MarshalForAgentd serializes a JcardConfig in the form agentd expects:
// the agent block is emitted as a single-element `[[agents]]` array
// with `name` (taken from cfg.Name) and `type = "native"` added. The
// user-facing input format and the on-disk-saved form still use the
// singular `[agent]` table — this is purely a wire-format adapter so
// the multi-agent schema doesn't bleed into user-edited jcards or the
// mb state directory.
func MarshalForAgentd(cfg *JcardConfig) ([]byte, error) {
	out := wireJcard{
		Backend:       cfg.Backend,
		Mixtape:       cfg.Mixtape,
		MixtapeDigest: cfg.MixtapeDigest,
		Name:          cfg.Name,
		Resources:     cfg.Resources,
		Network:       cfg.Network,
		Shared:        cfg.Shared,
		Secrets:       cfg.Secrets,
		Agents: []wireAgentConfig{{
			Name:        cfg.Name,
			Type:        "native",
			AgentConfig: cfg.Agent,
		}},
	}

	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(&out); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DefaultJcardTOML returns a well-commented default jcard.toml suitable for
// scaffolding with `mb init`.
func DefaultJcardTOML() string {
	return `# jcard.toml - Masterblaster sandbox configuration

# Which mixtape to boot, in "name:tag" format.
# The tag defaults to "latest" when omitted.
# Pull mixtapes with: mb pull <name:tag>
#
# Examples:
#   "base:latest"              -> StereOS base image (latest tag)
#   "opencode-mixtape:0.1.0"   -> pinned to a specific version
#   "base"                     -> shorthand for "base:latest"
mixtape = "base:latest"

# Pin to an exact digest for reproducible builds (optional).
# When set, this takes precedence over the tag in mixtape.
# mixtape_digest = "sha256:abc123..."

# Human-readable name for this sandbox.
# Defaults to the parent directory name.
# Must be unique across running sandboxes managed by the daemon.
# name = "my-agent-sandbox"

# Resources for the sandbox
[resources]
cpus   = 2          # Virtual CPUs allocated to the sandbox
memory = "4GiB"     # RAM - supports KiB, MiB, GiB suffixes
disk   = "20GiB"    # Root disk size (grows the image on first boot)

# Sandbox network
[network]
# Network mode:
#   "nat"      -> sandbox can reach the internet via host NAT (default)
#   "bridged"  -> sandbox gets an IP on the host's network
#   "none"     -> no network access (fully air-gapped sandbox)
mode = "nat"

# Port forwards from host -> sandbox (nat mode only).
# Each entry is { host = <port>, guest = <port>, proto = "tcp"|"udp" }
# forwards = [
#     { host = 8080, guest = 8080, proto = "tcp" },
# ]

# Egress allowlist.
# When set, only these domains/CIDRs are reachable from
# the sandbox. Useful for locking an agent down to specific LLM API
# endpoints. An empty list means no restrictions.
# egress_allow = ["api.anthropic.com", "api.openai.com"]

# Shared directories
# Mount host directories into the sandbox. Each entry maps a host path
# to a guest mount point. Paths are relative to the jcard.toml location.
# readonly prevents the agent from modifying host files (default: false).
# [[shared]]
# host = "./"
# guest = "/workspace"
# readonly = false

# Secrets injected into the sandbox at runtime.
# These are written to tmpfs inside the guest (never persisted to disk).
# Use ${ENV_VAR} to reference host environment variables.
[secrets]
# ANTHROPIC_API_KEY = "${ANTHROPIC_API_KEY}"

# Agent runtime configuration.
# Defines what agent harness to run and how agentd manages it.
[agent]
# The agent harness to use.
# Built-in harnesses: "claude-code", "opencode", "gemini-cli", "custom"
harness = "claude-code"

# The prompt or command to give the agent on boot.
# If empty, the harness starts in interactive mode.
# prompt = "review the code in /workspace and fix any failing tests"

# Alternatively, a prompt file (relative to jcard.toml).
# Useful for long, version-controlled prompts. Takes precedence.
# prompt_file = "./prompts/review.md"

# Working directory inside the sandbox where the agent starts.
# Defaults to the first [[shared]] guest mount, or /workspace.
# workdir = "/workspace"

# Whether to restart the agent if it exits.
#   "no"         -> agent exits, sandbox stays up (default)
#   "on-failure" -> restart only on non-zero exit
#   "always"     -> restart unconditionally
restart = "no"

# Maximum number of restart attempts before giving up (only applies
# when restart != "no"). 0 = unlimited.
# max_restarts = 5

# Timeout for the agent to complete. After this duration, agentd
# sends SIGTERM -> waits grace_period -> SIGKILL.
# Unset means no timeout (agent runs until it exits or sandbox goes down).
# timeout = "2h"

# Grace period for SIGTERM before SIGKILL on shutdown or timeout.
# grace_period = "30s"

# Environment variables set *only* for the agent process.
# [agent.env]
# MY_VAR = "my_value"
`
}
