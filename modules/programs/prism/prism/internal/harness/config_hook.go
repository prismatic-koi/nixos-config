package harness

// config_hook.go — wires the harness registry into the config package's
// HarnessValidator hook so that profiles.json's `default_harness` field
// is validated at LoadProfiles time without `internal/config`
// importing this package (which would create a cycle: harness/<name>
// already imports config).
//
// The hook is registered in init() so any binary that imports the harness
// package — i.e. every prism binary, since spawn / pr / review / sidecar
// all do — gets validation for free. Tests that do not import this package
// see HarnessValidator == nil and skip the check, which is the documented
// behaviour.

import (
	"github.com/prismatic-koi/prism/internal/config"
)

func init() {
	config.HarnessValidator = func(name string) ([]string, bool) {
		if _, ok := Lookup(name); ok {
			return nil, true
		}
		return Names(), false
	}
}
