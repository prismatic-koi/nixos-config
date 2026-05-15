package iris

import (
	"encoding/json"
	"os"
)

// Config holds the iris runtime configuration loaded from
// ~/.config/iris/config.json (§10.1 path table). All fields are optional;
// absent fields fall back to compiled-in defaults.
//
// D-2 scope: only the fields needed to satisfy the config-load AC are defined
// here. Additional fields will be added in D-3 and later issues as the daemon
// requires them.
type Config struct {
	// LogLevel controls the verbosity of iris daemon log output.
	// Valid values: "debug", "info", "warn", "error". Default: "info".
	LogLevel string `json:"log_level"`

	// AllowedExtensions is the initial extension allowlist (§11.9 of the
	// daemon-mode design doc). An empty slice means all extensions are allowed
	// (permissive default for D-2; D-3 will enforce the list).
	AllowedExtensions []string `json:"allowed_extensions"`
}

// defaults returns a Config with compiled-in defaults. These are used when
// the config file is absent or when a field is missing from the JSON.
func defaults() Config {
	return Config{
		LogLevel:          "info",
		AllowedExtensions: []string{},
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

	return cfg, nil
}
