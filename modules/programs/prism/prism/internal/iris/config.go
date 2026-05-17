package iris

import (
	"encoding/json"
	"os"
)

// Config holds the iris runtime configuration loaded from
// ~/.config/iris/config.json (§10.1 path table). All fields are optional;
// absent fields fall back to compiled-in defaults.
type Config struct {
	// LogLevel controls the verbosity of iris daemon log output.
	// Valid values: "debug", "info", "warn", "error". Default: "info".
	LogLevel string `json:"log_level"`

	// AllowedExtensions is the extension allowlist (§11.9 of the
	// daemon-mode design doc). The default list is
	// ["prism", "atlassian", "anthropic-oauth"] per §3.5 of the design doc.
	// An empty slice in the config file means use the compiled-in defaults.
	AllowedExtensions []string `json:"allowed_extensions"`

	// RestartThreshold is the maximum number of consecutive non-zero exits
	// before the supervisor's circuit breaker opens (§11.2, §3.6.1). Default: 3.
	RestartThreshold int `json:"restart_threshold"`

	// PIBinaryPath is the absolute path to the pi binary. When empty, iris
	// resolves pi via $PATH.
	PIBinaryPath string `json:"pi_binary_path"`

	// PIExtensionPath is the absolute path to the prism.ts extension file
	// that iris loads into each pi child via --extension.
	PIExtensionPath string `json:"pi_extension_path"`

	// PIProvider is the LLM provider passed to pi via `--provider`. When
	// empty, the flag is omitted and pi falls back to its own defaults
	// (which currently picks github-copilot/gpt-5.4 — see issue #1777).
	PIProvider string `json:"pi_provider"`

	// PIModel is the LLM model passed to pi via `--model`. When empty, the
	// flag is omitted.
	PIModel string `json:"pi_model"`

	// PIThinking is the thinking level passed to pi via `--thinking`
	// (e.g. "off", "low", "medium", "high"). When empty, the flag is
	// omitted.
	PIThinking string `json:"pi_thinking"`
}

// DefaultAllowedExtensions is the initial extension allowlist per §3.5 of the
// daemon-mode design doc. Add an extension to this list only after it has been
// reviewed for iris compatibility.
var DefaultAllowedExtensions = []string{"prism", "atlassian", "anthropic-oauth"}

// defaults returns a Config with compiled-in defaults. These are used when
// the config file is absent or when a field is missing from the JSON.
func defaults() Config {
	return Config{
		LogLevel:          "info",
		AllowedExtensions: DefaultAllowedExtensions,
		RestartThreshold:  DefaultRestartThreshold,
		// Sensible defaults that match what the now-deprecated
		// writePerSessionPIConfig used to write into per-session
		// settings.json (issue #1777). The user can override these via
		// ~/.config/iris/config.json or the iris nix module options
		// (programs.iris.pi.{provider,model,thinking}).
		PIProvider: "anthropic",
		PIModel:    "claude-sonnet-4-20250514",
		PIThinking: "medium",
	}
}

// LoadConfig reads the iris config file at the given path. If the file does
// not exist, defaults are returned without error. Malformed JSON returns an
// error; partial JSON is merged with defaults (explicit fields override).
//
// The path is expected to be p.ConfigFile from ResolvePaths(). It is passed
// explicitly (rather than derived inside the function) so callers in tests can
// redirect to an arbitrary temp-dir path.
func LoadConfig(path string) (Config, error) {
	cfg := defaults()

	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// Absent config file is not an error — use defaults.
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}

	var parsed Config
	if err := json.Unmarshal(data, &parsed); err != nil {
		return cfg, err
	}

	// Merge: explicit non-zero values in the parsed config override defaults.
	if parsed.LogLevel != "" {
		cfg.LogLevel = parsed.LogLevel
	}
	if len(parsed.AllowedExtensions) > 0 {
		cfg.AllowedExtensions = parsed.AllowedExtensions
	}
	if parsed.RestartThreshold > 0 {
		cfg.RestartThreshold = parsed.RestartThreshold
	}
	if parsed.PIBinaryPath != "" {
		cfg.PIBinaryPath = parsed.PIBinaryPath
	}
	if parsed.PIExtensionPath != "" {
		cfg.PIExtensionPath = parsed.PIExtensionPath
	}
	if parsed.PIProvider != "" {
		cfg.PIProvider = parsed.PIProvider
	}
	if parsed.PIModel != "" {
		cfg.PIModel = parsed.PIModel
	}
	if parsed.PIThinking != "" {
		cfg.PIThinking = parsed.PIThinking
	}

	return cfg, nil
}
