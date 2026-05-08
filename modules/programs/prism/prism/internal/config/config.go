// Package config loads the prism runtime configuration from a JSON file.
//
// The config file is read from $PRISM_CONFIG_FILE if set, otherwise from
// $XDG_CONFIG_HOME/prism/config.json, falling back to
// $HOME/.config/prism/config.json. If the file is absent or unreadable the
// package returns compiled-in defaults (gruvbox-dark colours, ~/code paths)
// so that `go build` and `go test` work correctly without a Nix build.
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// IsolationMode represents the isolation mechanism for agent sessions.
// Valid values are "bwrap", "sandbox-exec", and "host".
type IsolationMode string

const (
	// IsolationBwrap runs opencode inside a bubblewrap sandbox, launched and
	// owned by the tmux pane via "prism agent-run". The sidecar does not
	// manage the process lifecycle. Linux only.
	IsolationBwrap IsolationMode = "bwrap"

	// IsolationSandboxExec runs opencode inside an Apple sandbox-exec profile
	// sandbox, launched and owned by the tmux pane via "prism agent-run". The
	// sidecar does not manage the process lifecycle. macOS (Darwin) only.
	IsolationSandboxExec IsolationMode = "sandbox-exec"

	// IsolationHost runs opencode directly in the tmux pane with no isolation.
	// Equivalent to the legacy --host-mode flag.
	IsolationHost IsolationMode = "host"
)

// ValidIsolationModes lists all valid isolation mode strings.
var ValidIsolationModes = []IsolationMode{IsolationBwrap, IsolationSandboxExec, IsolationHost}

// IsValidIsolationMode reports whether s is a valid isolation mode.
// Valid values are "bwrap", "sandbox-exec", and "host".
func IsValidIsolationMode(s string) bool {
	for _, m := range ValidIsolationModes {
		if string(m) == s {
			return true
		}
	}
	return false
}

// Config holds all host-specific runtime configuration for prism.
type Config struct {
	// Theme colours (hex strings, e.g. "#d4be98").
	ColorPrimary    string `json:"color_primary"`
	ColorSecondary  string `json:"color_secondary"`
	ColorPurple     string `json:"color_purple"`
	ColorYellow     string `json:"color_yellow"`
	ColorGreen      string `json:"color_green"`
	ColorBlue       string `json:"color_blue"`
	ColorRed        string `json:"color_red"`
	ColorForeground string `json:"color_foreground"`
	ColorBg0        string `json:"color_bg0"`

	// Binary paths.
	KittyBin string `json:"kitty_bin"`

	// Sidecar container settings.
	// DefaultIsolationMode is the machine-level default isolation mode for new
	// agent sessions. Valid values: "bwrap", "sandbox-exec", "host".
	DefaultIsolationMode IsolationMode `json:"default_isolation_mode,omitempty"`
	// SidecarPluginPath is the host-side path to the opencode plugin file that
	// is bind-mounted into the container. Empty string = no plugin.
	SidecarPluginPath string `json:"sidecar_plugin_path"`
	// GitUserName is the git user.name to write into the container's .gitconfig.
	// Sourced from the host's NixOS/home-manager git configuration.
	GitUserName string `json:"git_user_name"`
	// GitUserEmail is the git user.email to write into the container's .gitconfig.
	// Sourced from the host's NixOS/home-manager git configuration.
	GitUserEmail string `json:"git_user_email"`
	// SshAccessKeyName is the filename (not full path) of the SSH access key in ~/.ssh/.
	// Defaults to "prismatic-koi-ed25519" if empty.
	SshAccessKeyName string `json:"ssh_access_key_name"`
	// SshSigningKeyName is the base filename (not full path) of the SSH signing key in ~/.ssh/.
	// The public key is derived by appending ".pub". Defaults to "prismatic-koi-ed25519-signingkey" if empty.
	SshSigningKeyName string `json:"ssh_signing_key_name"`
	// SshBin is the absolute path to the ssh binary to use for GIT_SSH_COMMAND
	// in sandbox-exec sessions. When set (typically to a Nix-store openssh path
	// baked in by prism-tui.nix), this Nix-built ssh is used instead of
	// /usr/bin/ssh. The Nix openssh links against its own libresolv/libldns
	// rather than Apple's libnetwork.dylib, so it resolves hostnames without
	// needing the system-network sandbox rules (dafsaData.bin etc.).
	// When empty, GIT_SSH_COMMAND falls back to bare "ssh" (PATH lookup).
	SshBin string `json:"ssh_bin"`

	// Restore behaviour.
	// RestoreStaggerDelayMs is the delay in milliseconds inserted between
	// successive session creates in `prism restore` to flatten the startup
	// curve. Zero means use the compiled-in default (500ms). Set to a negative
	// value to disable the stagger entirely.
	RestoreStaggerDelayMs int `json:"restore_stagger_delay_ms"`
	// SidecarCircuitBreakerThreshold is the number of consecutive non-zero
	// sidecar exits that causes `prism restore` to skip re-spawning that
	// session. Zero means use the compiled-in default (3).
	SidecarCircuitBreakerThreshold int `json:"sidecar_circuit_breaker_threshold"`

	// BwrapConcurrencyCap is the maximum number of active bwrap sessions
	// (agent_status rows with ended_at IS NULL AND isolation_mode = 'bwrap')
	// before new bwrap spawns are refused. Zero means uncapped (not "cap of
	// zero"). The default of 20 is conservative enough for any machine without
	// an explicit per-machine override.
	BwrapConcurrencyCap int `json:"bwrap_concurrency_cap"`

	// SandboxExecConcurrencyCap is the maximum number of active sandbox-exec
	// sessions (agent_status rows with ended_at IS NULL AND
	// isolation_mode = 'sandbox-exec') before new sandbox-exec spawns are
	// refused. Zero means uncapped (not "cap of zero"). The default of 20
	// mirrors BwrapConcurrencyCap. Can be overridden per-machine via the Nix
	// sandboxExecConcurrencyCap option (written to config.json). Darwin only.
	SandboxExecConcurrencyCap int `json:"sandbox_exec_concurrency_cap"`

	// Project layout (JSON arrays).
	WorktreeExclude  []string `json:"worktree_exclude"`
	ProjectLocations []string `json:"project_locations"`
	ProjectSpecific  []string `json:"project_specific"`

	// PIExtensionDir is the absolute host path to the directory containing
	// the prism PI extension file (prism.ts). Written by Nix to config.json
	// so that agent-run knows where to find the extension at runtime.
	// When empty, agent-run falls back to a relative-to-executable heuristic.
	PIExtensionDir string `json:"pi_extension_dir"`

	// FeedbackEndpoint is the upstream URL that `prism feedback` POSTs each
	// new entry to (in addition to writing it locally). Empty means upstream
	// reporting is disabled — the local JSONL store remains the source of
	// truth. The PRISM_FEEDBACK_ENDPOINT environment variable takes
	// precedence over this config key, so a one-off `PRISM_FEEDBACK_ENDPOINT=...
	// prism feedback ...` invocation works without editing config.json.
	FeedbackEndpoint string `json:"feedback_endpoint,omitempty"`

	// ProjectIsolationOverrides maps path strings (with optional "~/" prefix)
	// to isolation mode strings. When a session path matches a key (after "~/"
	// expansion), the associated mode is used instead of DefaultIsolationMode.
	// Invalid mode values are silently ignored (same treatment as an invalid
	// DefaultIsolationMode). Keys must exactly match the path after "~/"
	// expansion; no glob or prefix matching.
	ProjectIsolationOverrides map[string]string `json:"project_isolation_overrides,omitempty"`
}

// parsedConfig mirrors Config but uses pointer slices so that a JSON null or
// absent key is distinguishable from an explicit empty array [].
type parsedConfig struct {
	ColorPrimary                   string    `json:"color_primary"`
	ColorSecondary                 string    `json:"color_secondary"`
	ColorPurple                    string    `json:"color_purple"`
	ColorYellow                    string    `json:"color_yellow"`
	ColorGreen                     string    `json:"color_green"`
	ColorBlue                      string    `json:"color_blue"`
	ColorRed                       string    `json:"color_red"`
	ColorForeground                string    `json:"color_foreground"`
	ColorBg0                       string    `json:"color_bg0"`
	KittyBin                       string    `json:"kitty_bin"`
	DefaultIsolationMode           string    `json:"default_isolation_mode"`
	SidecarPluginPath              string    `json:"sidecar_plugin_path"`
	GitUserName                    string    `json:"git_user_name"`
	GitUserEmail                   string    `json:"git_user_email"`
	SshAccessKeyName               string    `json:"ssh_access_key_name"`
	SshSigningKeyName              string    `json:"ssh_signing_key_name"`
	SshBin                         string    `json:"ssh_bin"`
	PIExtensionDir                 string    `json:"pi_extension_dir"`
	RestoreStaggerDelayMs          *int      `json:"restore_stagger_delay_ms"`
	SidecarCircuitBreakerThreshold *int      `json:"sidecar_circuit_breaker_threshold"`
	BwrapConcurrencyCap            *int      `json:"bwrap_concurrency_cap"`
	SandboxExecConcurrencyCap      *int      `json:"sandbox_exec_concurrency_cap"`
	WorktreeExclude                *[]string          `json:"worktree_exclude"`
	ProjectLocations               *[]string          `json:"project_locations"`
	ProjectSpecific                *[]string          `json:"project_specific"`
	ProjectIsolationOverrides      *map[string]string `json:"project_isolation_overrides"`
	FeedbackEndpoint               string             `json:"feedback_endpoint"`
}

// DefaultBwrapConcurrencyCap is the compiled-in default maximum number of
// concurrent bwrap sessions. A value of 20 is conservative enough for any
// machine without an explicit override. The cap can be raised per-machine
// via the Nix bwrapConcurrencyCap option (written to config.json).
// Zero means uncapped.
const DefaultBwrapConcurrencyCap = 20

// DefaultSandboxExecConcurrencyCap is the compiled-in default maximum number
// of concurrent sandbox-exec sessions. Mirrors DefaultBwrapConcurrencyCap —
// 20 is conservative enough for any Darwin machine without an explicit
// override. The cap can be raised per-machine via the Nix
// sandboxExecConcurrencyCap option (written to config.json). Zero means uncapped.
const DefaultSandboxExecConcurrencyCap = 20

// defaults returns the compiled-in fallback Config (gruvbox-dark palette,
// standard paths). These values are used whenever no config file is found.
func defaults() Config {
	return Config{
		ColorPrimary:         "#d4be98",
		ColorSecondary:       "#a89984",
		ColorPurple:          "#d3869b",
		ColorYellow:          "#d8a657",
		ColorGreen:           "#a9b665",
		ColorBlue:            "#7daea3",
		ColorRed:             "#ea6962",
		ColorForeground:      "#d3c6aa",
		ColorBg0:             "#2d353b",
		KittyBin:                  "kitty",
		DefaultIsolationMode:      IsolationHost,
		SshAccessKeyName:          "prismatic-koi-ed25519",
		SshSigningKeyName:         "prismatic-koi-ed25519-signingkey",
		BwrapConcurrencyCap:       DefaultBwrapConcurrencyCap,
		SandboxExecConcurrencyCap: DefaultSandboxExecConcurrencyCap,
		WorktreeExclude:  []string{"obsidian"},
		ProjectLocations: []string{"~/code"},
		ProjectSpecific:  []string{"~/documents/obsidian"},
		ProjectIsolationOverrides: map[string]string{
			"~/documents/obsidian": "host",
		},
	}
}

var (
	once   sync.Once
	loaded Config
)

// Load returns the configuration, loading it on the first call and caching
// the result for subsequent calls. Use LoadFresh for tests.
func Load() Config {
	once.Do(func() {
		loaded = load()
	})
	return loaded
}

// LoadFresh loads the configuration without the singleton cache. Intended for
// tests that need to exercise different config file paths.
func LoadFresh() Config {
	return load()
}

// load resolves the config file path, reads and parses it, filling any missing
// fields with defaults.
func load() Config {
	cfg := defaults()

	path := configFilePath()
	if path == "" {
		return cfg
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}

	var parsed parsedConfig
	if err := json.Unmarshal(data, &parsed); err != nil {
		return cfg
	}

	// Merge: use parsed value when non-empty string, otherwise keep default.
	if parsed.ColorPrimary != "" {
		cfg.ColorPrimary = parsed.ColorPrimary
	}
	if parsed.ColorSecondary != "" {
		cfg.ColorSecondary = parsed.ColorSecondary
	}
	if parsed.ColorPurple != "" {
		cfg.ColorPurple = parsed.ColorPurple
	}
	if parsed.ColorYellow != "" {
		cfg.ColorYellow = parsed.ColorYellow
	}
	if parsed.ColorGreen != "" {
		cfg.ColorGreen = parsed.ColorGreen
	}
	if parsed.ColorBlue != "" {
		cfg.ColorBlue = parsed.ColorBlue
	}
	if parsed.ColorRed != "" {
		cfg.ColorRed = parsed.ColorRed
	}
	if parsed.ColorForeground != "" {
		cfg.ColorForeground = parsed.ColorForeground
	}
	if parsed.ColorBg0 != "" {
		cfg.ColorBg0 = parsed.ColorBg0
	}
	if parsed.KittyBin != "" {
		cfg.KittyBin = parsed.KittyBin
	}
	// DefaultIsolationMode: use the parsed value when present and valid;
	// otherwise keep the compiled-in default. "podman" is treated as absent
	// (falls back to bwrap) since podman isolation has been removed.
	// Unknown values are silently ignored.
	if parsed.DefaultIsolationMode == "podman" {
		cfg.DefaultIsolationMode = IsolationBwrap
	} else if parsed.DefaultIsolationMode != "" && IsValidIsolationMode(parsed.DefaultIsolationMode) {
		cfg.DefaultIsolationMode = IsolationMode(parsed.DefaultIsolationMode)
	}
	if parsed.SidecarPluginPath != "" {
		cfg.SidecarPluginPath = parsed.SidecarPluginPath
	}
	if parsed.GitUserName != "" {
		cfg.GitUserName = parsed.GitUserName
	}
	if parsed.GitUserEmail != "" {
		cfg.GitUserEmail = parsed.GitUserEmail
	}
	if parsed.SshAccessKeyName != "" {
		cfg.SshAccessKeyName = parsed.SshAccessKeyName
	}
	if parsed.SshSigningKeyName != "" {
		cfg.SshSigningKeyName = parsed.SshSigningKeyName
	}
	if parsed.SshBin != "" {
		cfg.SshBin = parsed.SshBin
	}
	if parsed.PIExtensionDir != "" {
		cfg.PIExtensionDir = parsed.PIExtensionDir
	}
	if parsed.FeedbackEndpoint != "" {
		cfg.FeedbackEndpoint = parsed.FeedbackEndpoint
	}

	// For integer pointer fields: nil means absent (keep default); non-nil
	// means use the parsed value (including 0 and negative values).
	if parsed.RestoreStaggerDelayMs != nil {
		cfg.RestoreStaggerDelayMs = *parsed.RestoreStaggerDelayMs
	}
	if parsed.SidecarCircuitBreakerThreshold != nil {
		cfg.SidecarCircuitBreakerThreshold = *parsed.SidecarCircuitBreakerThreshold
	}
	if parsed.BwrapConcurrencyCap != nil {
		cfg.BwrapConcurrencyCap = *parsed.BwrapConcurrencyCap
	}
	if parsed.SandboxExecConcurrencyCap != nil {
		cfg.SandboxExecConcurrencyCap = *parsed.SandboxExecConcurrencyCap
	}

	// For slice fields: nil pointer means absent (keep default); non-nil
	// pointer (including pointer to empty slice) means use the parsed value.
	if parsed.WorktreeExclude != nil {
		cfg.WorktreeExclude = *parsed.WorktreeExclude
	}
	if parsed.ProjectLocations != nil {
		cfg.ProjectLocations = *parsed.ProjectLocations
	}
	if parsed.ProjectSpecific != nil {
		cfg.ProjectSpecific = *parsed.ProjectSpecific
	}

	// For map fields: nil pointer means absent (keep default); non-nil
	// pointer (including pointer to empty map) means replace entirely.
	if parsed.ProjectIsolationOverrides != nil {
		cfg.ProjectIsolationOverrides = *parsed.ProjectIsolationOverrides
	}

	return cfg
}

// DefaultRestoreStaggerDelay is the stagger between session restores when not
// overridden in config.json. 500ms is enough to flatten the podman burst.
const DefaultRestoreStaggerDelay = 500 // milliseconds

// DefaultSidecarCircuitBreakerThreshold is the default consecutive-failure
// count at which prism restore stops re-spawning a broken sidecar.
const DefaultSidecarCircuitBreakerThreshold = 3

// RestoreStaggerDelay returns the configured stagger delay as a time.Duration,
// applying the compiled-in default (500ms) when RestoreStaggerDelayMs == 0.
// A negative RestoreStaggerDelayMs disables the stagger (returns 0).
func (c Config) RestoreStaggerDelay() time.Duration {
	ms := c.RestoreStaggerDelayMs
	if ms == 0 {
		ms = DefaultRestoreStaggerDelay
	}
	if ms < 0 {
		return 0
	}
	return time.Duration(ms) * time.Millisecond
}

// CircuitBreakerThreshold returns the configured circuit-breaker threshold,
// applying the compiled-in default (3) when SidecarCircuitBreakerThreshold == 0.
// Returns 0 if SidecarCircuitBreakerThreshold is negative (effectively disables
// the circuit breaker — all sessions are restored regardless of failure history).
func (c Config) CircuitBreakerThreshold() int {
	n := c.SidecarCircuitBreakerThreshold
	if n == 0 {
		return DefaultSidecarCircuitBreakerThreshold
	}
	if n < 0 {
		return 0
	}
	return n
}

// IsolationOverrideForPath looks up path in ProjectIsolationOverrides and
// returns the configured IsolationMode if the path matches and the value is
// valid. Returns "" if the path is not present in the map or the mapped value
// is not a valid isolation mode (silently ignored).
//
// path is expanded: a leading "~/" is replaced with the user home directory
// before lookup. The keys in ProjectIsolationOverrides are also expanded
// before comparison, so both "~/documents/obsidian" and an already-expanded
// absolute path work as keys.
func (c Config) IsolationOverrideForPath(path string) IsolationMode {
	if len(c.ProjectIsolationOverrides) == 0 {
		return ""
	}
	expanded := expandHomePath(path)
	for k, v := range c.ProjectIsolationOverrides {
		if expandHomePath(k) == expanded {
			if IsValidIsolationMode(v) {
				return IsolationMode(v)
			}
			return ""
		}
	}
	return ""
}

// expandHomePath expands a leading "~/" in p to the user's home directory.
// If os.UserHomeDir() fails or p does not start with "~/", p is returned
// unchanged.
func expandHomePath(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, p[2:])
}

// configFilePath returns the path to look for the config file.
// Returns "" if no home directory is discoverable.
func configFilePath() string {
	if p := os.Getenv("PRISM_CONFIG_FILE"); p != "" {
		return p
	}
	configHome := os.Getenv("XDG_CONFIG_HOME")
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "prism", "config.json")
}
