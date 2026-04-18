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
	"sync"
	"time"
)

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
	// ContainerMode, when true, causes spawn and switch to run opencode inside
	// a podman container managed by the sidecar, using "podman attach" in the
	// agent window rather than launching opencode directly.
	ContainerMode bool `json:"container_mode"`
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

	// Project layout (JSON arrays).
	WorktreeExclude  []string `json:"worktree_exclude"`
	ProjectLocations []string `json:"project_locations"`
	ProjectSpecific  []string `json:"project_specific"`
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
	ContainerMode                  *bool     `json:"container_mode"`
	SidecarPluginPath              string    `json:"sidecar_plugin_path"`
	GitUserName                    string    `json:"git_user_name"`
	GitUserEmail                   string    `json:"git_user_email"`
	SshAccessKeyName               string    `json:"ssh_access_key_name"`
	SshSigningKeyName              string    `json:"ssh_signing_key_name"`
	RestoreStaggerDelayMs          *int      `json:"restore_stagger_delay_ms"`
	SidecarCircuitBreakerThreshold *int      `json:"sidecar_circuit_breaker_threshold"`
	WorktreeExclude                *[]string `json:"worktree_exclude"`
	ProjectLocations               *[]string `json:"project_locations"`
	ProjectSpecific                *[]string `json:"project_specific"`
}

// defaults returns the compiled-in fallback Config (gruvbox-dark palette,
// standard paths). These values are used whenever no config file is found.
func defaults() Config {
	return Config{
		ColorPrimary:      "#d4be98",
		ColorSecondary:    "#a89984",
		ColorPurple:       "#d3869b",
		ColorYellow:       "#d8a657",
		ColorGreen:        "#a9b665",
		ColorBlue:         "#7daea3",
		ColorRed:          "#ea6962",
		ColorForeground:   "#d3c6aa",
		ColorBg0:          "#2d353b",
		KittyBin:          "kitty",
		SshAccessKeyName:  "prismatic-koi-ed25519",
		SshSigningKeyName: "prismatic-koi-ed25519-signingkey",
		WorktreeExclude:   []string{"obsidian"},
		ProjectLocations:  []string{"~/code"},
		ProjectSpecific:   []string{"~/documents/obsidian"},
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
	if parsed.ContainerMode != nil {
		cfg.ContainerMode = *parsed.ContainerMode
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

	// For integer pointer fields: nil means absent (keep default); non-nil
	// means use the parsed value (including 0 and negative values).
	if parsed.RestoreStaggerDelayMs != nil {
		cfg.RestoreStaggerDelayMs = *parsed.RestoreStaggerDelayMs
	}
	if parsed.SidecarCircuitBreakerThreshold != nil {
		cfg.SidecarCircuitBreakerThreshold = *parsed.SidecarCircuitBreakerThreshold
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
