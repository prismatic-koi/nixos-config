package prismcontainer

// The podman machine mount check (macOS).
//
// On macOS the build runs in the podman machine VM, and the VM reads every
// image source. By default the VM mounts /Users, /private, and
// /var/folders of the Mac, so a tarball: or oci: source can read nearly any
// Mac file. Before each build, prism reads the mounts of the default
// machine and refuses the build when one of them is outside the allowlist.
// The check fails closed: when prism cannot read the mounts, it refuses.
//
// podman machine inspect does not show the mounts (podman 5.x InspectInfo
// has no Mounts field). The machine config file <ConfigDir>/<name>.json
// has them, so prism reads that file.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// machineListEntry is the part of `podman machine list --format json` that
// prism reads.
type machineListEntry struct {
	Name    string `json:"Name"`
	Default bool   `json:"Default"`
}

// machineInspectEntry is the part of `podman machine inspect` that prism
// reads.
type machineInspectEntry struct {
	Name      string `json:"Name"`
	ConfigDir struct {
		Path string `json:"Path"`
	} `json:"ConfigDir"`
}

// MachineMount is one mount of the podman machine, from its config file.
type MachineMount struct {
	Source   string `json:"Source"`
	Target   string `json:"Target"`
	ReadOnly bool   `json:"ReadOnly"`
	Type     string `json:"Type"`
}

// machineConfig is the part of the machine config file that prism reads.
type machineConfig struct {
	Mounts []MachineMount `json:"Mounts"`
}

// checkMachineMounts refuses a build when the default podman machine
// mounts a host path outside allowlist. readFile reads the machine config
// file. Every error is a refusal.
func checkMachineMounts(ctx context.Context, r Runner, readFile func(string) ([]byte, error), allowlist []string) error {
	name, err := defaultMachine(ctx, r)
	if err != nil {
		return err
	}
	configDir, err := machineConfigDir(ctx, r, name)
	if err != nil {
		return err
	}
	path := filepath.Join(configDir, name+".json")
	data, err := readFile(path)
	if err != nil {
		return fmt.Errorf("prism cannot read the mounts of podman machine %q from %s (%v), so it cannot make sure that the build cannot read Mac files", name, path, err)
	}
	var cfg machineConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return fmt.Errorf("prism cannot parse the mounts of podman machine %q in %s (%v), so it cannot make sure that the build cannot read Mac files", name, path, err)
	}
	var bad []MachineMount
	for _, m := range cfg.Mounts {
		if !insideAllowlist(m.Source, allowlist) {
			bad = append(bad, m)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return machineMountRefusal(name, bad, allowlist)
}

// defaultMachine returns the name of the default podman machine.
func defaultMachine(ctx context.Context, r Runner) (string, error) {
	out, err := runPodmanJSON(ctx, r, "machine", "list", "--format", "json")
	if err != nil {
		return "", fmt.Errorf("prism cannot list the podman machines (%v), so it cannot check their mounts", err)
	}
	var list []machineListEntry
	if err := json.Unmarshal(out, &list); err != nil {
		return "", fmt.Errorf("prism cannot parse the podman machine list (%v), so it cannot check the machine mounts", err)
	}
	for _, m := range list {
		if m.Default && m.Name != "" {
			return m.Name, nil
		}
	}
	return "", fmt.Errorf("podman has no default machine, so prism cannot check which Mac paths the build can read. Run `podman machine init`, then try again")
}

// machineConfigDir returns the config dir of machine name.
func machineConfigDir(ctx context.Context, r Runner, name string) (string, error) {
	out, err := runPodmanJSON(ctx, r, "machine", "inspect", name)
	if err != nil {
		return "", fmt.Errorf("prism cannot inspect podman machine %q (%v), so it cannot check its mounts", name, err)
	}
	var list []machineInspectEntry
	if err := json.Unmarshal(out, &list); err != nil || len(list) != 1 || list[0].ConfigDir.Path == "" {
		return "", fmt.Errorf("prism cannot read the config dir of podman machine %q from podman machine inspect, so it cannot check its mounts", name)
	}
	if !filepath.IsAbs(list[0].ConfigDir.Path) {
		return "", fmt.Errorf("podman machine %q has a config dir that is not an absolute path (%q)", name, list[0].ConfigDir.Path)
	}
	return list[0].ConfigDir.Path, nil
}

func runPodmanJSON(ctx context.Context, r Runner, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	code, err := r.Run(ctx, &stdout, &stderr, args...)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("podman %s exited %d: %s", strings.Join(args[:2], " "), code, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// insideAllowlist reports whether source is an allowlisted root or a path
// inside one. A relative path is never inside.
func insideAllowlist(source string, allowlist []string) bool {
	if !filepath.IsAbs(source) {
		return false
	}
	source = filepath.Clean(source)
	for _, root := range allowlist {
		if !filepath.IsAbs(root) {
			continue
		}
		rel, err := filepath.Rel(filepath.Clean(root), source)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// machineMountRefusal tells the user which mounts are not allowed and how
// to recreate the machine with the allowed mounts only.
func machineMountRefusal(name string, bad []MachineMount, allowlist []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "podman machine %q mounts Mac paths that are not in the allowlist, so a build in the machine can read Mac files:", name)
	for _, m := range bad {
		fmt.Fprintf(&b, "\n  %s (mounted at %s in the machine)", m.Source, m.Target)
	}
	var volumes []string
	for _, root := range allowlist {
		volumes = append(volumes, fmt.Sprintf("%q", root+":"+root))
	}
	allowed := "(empty)"
	if len(allowlist) > 0 {
		allowed = strings.Join(allowlist, ", ")
	}
	fmt.Fprintf(&b, "\nThe allowlist is: %s. The Nix option %s sets it (default: the prism project locations).", allowed, MachineAllowlistNixOption)
	b.WriteString("\nTell the user. To correct this, the user does these steps on the Mac:")
	fmt.Fprintf(&b, "\n  1. In ~/.config/containers/containers.conf, set:\n       [machine]\n       volumes = [%s]", strings.Join(volumes, ", "))
	fmt.Fprintf(&b, "\n  2. Run: podman machine stop %s && podman machine rm %s && podman machine init %s && podman machine start %s", name, name, name, name)
	b.WriteString("\n     CAUTION: podman machine rm deletes the images and containers in the machine.")
	return fmt.Errorf("%s", b.String())
}

// MachineAllowlistNixOption is the Nix option that sets the Mac paths that
// the podman machine can mount. The refusal message names it.
const MachineAllowlistNixOption = "nx.programs.prism.containerMachineMountAllowlist"

// Preflight checks the mounts of the podman machine before a build.
func (e PlainExecutor) Preflight(ctx context.Context) error {
	readFile := e.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	return checkMachineMounts(ctx, e.runner(), readFile, e.MountAllowlist)
}

func (e PlainExecutor) runner() Runner {
	if e.Runner == nil {
		return ExecRunner{}
	}
	return e.Runner
}
