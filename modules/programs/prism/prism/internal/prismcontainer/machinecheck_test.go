package prismcontainer

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// machineStub answers `podman machine list` and `podman machine inspect`.
type machineStub struct {
	list, inspect string
	listRC        int
	err           error
}

func (m machineStub) Run(_ context.Context, stdout, stderr io.Writer, args ...string) (int, error) {
	if m.err != nil {
		return -1, m.err
	}
	if len(args) < 2 || args[0] != "machine" {
		return 125, nil
	}
	switch args[1] {
	case "list":
		if m.listRC != 0 {
			_, _ = io.WriteString(stderr, "Cannot connect")
			return m.listRC, nil
		}
		_, _ = io.WriteString(stdout, m.list)
	case "inspect":
		_, _ = io.WriteString(stdout, m.inspect)
	}
	return 0, nil
}

const (
	stubList    = `[{"Name":"other","Default":false},{"Name":"podman-machine-default","Default":true,"Running":true}]`
	stubInspect = `[{"Name":"podman-machine-default","ConfigDir":{"Path":"/Users/u/.config/containers/podman/machine/applehv"}}]`
	stubConfig  = "/Users/u/.config/containers/podman/machine/applehv/podman-machine-default.json"
)

// fixtureReader returns the testdata machine config at stubConfig. The
// fixture follows the MachineConfig layout of podman 5.8.7. It is not
// copied from a real machine.
func fixtureReader(t *testing.T) func(string) ([]byte, error) {
	t.Helper()
	data, err := os.ReadFile("testdata/podman-machine-config.json")
	if err != nil {
		t.Fatal(err)
	}
	return func(path string) ([]byte, error) {
		if path != stubConfig {
			return nil, os.ErrNotExist
		}
		return data, nil
	}
}

func mountsReader(json string) func(string) ([]byte, error) {
	return func(string) ([]byte, error) { return []byte(json), nil }
}

func TestMachineCheck_DefaultMountsRefused(t *testing.T) {
	r := machineStub{list: stubList, inspect: stubInspect}
	err := checkMachineMounts(context.Background(), r, fixtureReader(t), []string{"/Users/u/code"})
	if err == nil {
		t.Fatal("the default machine mounts passed the check")
	}
	msg := err.Error()
	for _, want := range []string{
		`podman machine "podman-machine-default" mounts Mac paths that are not in the allowlist`,
		"\n  /Users (mounted at /Users in the machine)",
		"\n  /private (mounted at /private in the machine)",
		"\n  /var/folders (mounted at /var/folders in the machine)",
		"The allowlist is: /Users/u/code.",
		MachineAllowlistNixOption,
		"[machine]",
		`volumes = ["/Users/u/code:/Users/u/code"]`,
		"podman machine stop podman-machine-default && podman machine rm podman-machine-default && podman machine init podman-machine-default && podman machine start podman-machine-default",
		"CAUTION: podman machine rm deletes the images",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal lacks %q:\n%s", want, msg)
		}
	}
}

func TestMachineCheck_Allowlist(t *testing.T) {
	cases := []struct {
		name      string
		mounts    string
		allowlist []string
		ok        bool
	}{
		{"no mounts, empty allowlist", `{"Mounts":[]}`, nil, true},
		{"no Mounts field", `{"Name":"x"}`, nil, true},
		{"root itself", `{"Mounts":[{"Source":"/Users/u/code","Target":"/Users/u/code"}]}`, []string{"/Users/u/code"}, true},
		{"inside a root", `{"Mounts":[{"Source":"/Users/u/code/proj/","Target":"/w"}]}`, []string{"/Users/u/code"}, true},
		{"second root", `{"Mounts":[{"Source":"/Users/u/work","Target":"/w"}]}`, []string{"/Users/u/code", "/Users/u/work"}, true},
		{"prefix is not inside", `{"Mounts":[{"Source":"/Users/u/code2","Target":"/c"}]}`, []string{"/Users/u/code"}, false},
		{"parent of a root", `{"Mounts":[{"Source":"/Users/u","Target":"/u"}]}`, []string{"/Users/u/code"}, false},
		{"dot-dot out of a root", `{"Mounts":[{"Source":"/Users/u/code/../.ssh","Target":"/s"}]}`, []string{"/Users/u/code"}, false},
		{"relative source", `{"Mounts":[{"Source":"code","Target":"/c"}]}`, []string{"/Users/u/code"}, false},
		{"empty allowlist", `{"Mounts":[{"Source":"/Users/u/code","Target":"/c"}]}`, nil, false},
		{"relative allowlist entry", `{"Mounts":[{"Source":"/Users/u/code","Target":"/c"}]}`, []string{"code"}, false},
		{"read-only mount outside", `{"Mounts":[{"Source":"/private","Target":"/private","ReadOnly":true}]}`, []string{"/Users/u/code"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := machineStub{list: stubList, inspect: stubInspect}
			err := checkMachineMounts(context.Background(), r, mountsReader(tc.mounts), tc.allowlist)
			if (err == nil) != tc.ok {
				t.Errorf("err = %v, want ok = %v", err, tc.ok)
			}
		})
	}
}

// TestMachineCheck_FailsClosed: when prism cannot read the mounts, it
// refuses the build.
func TestMachineCheck_FailsClosed(t *testing.T) {
	read := fixtureReader(t)
	cases := map[string]struct {
		r    Runner
		read func(string) ([]byte, error)
		want string
	}{
		"podman fails":     {machineStub{err: errors.New("exec: podman not found")}, read, "cannot list the podman machines"},
		"list exits 125":   {machineStub{listRC: 125}, read, "cannot list the podman machines"},
		"list not JSON":    {machineStub{list: "x", inspect: stubInspect}, read, "cannot parse the podman machine list"},
		"no default":       {machineStub{list: `[{"Name":"a","Default":false}]`, inspect: stubInspect}, read, "no default machine"},
		"no machines":      {machineStub{list: `[]`, inspect: stubInspect}, read, "no default machine"},
		"inspect empty":    {machineStub{list: stubList, inspect: `[]`}, read, "cannot read the config dir"},
		"inspect no dir":   {machineStub{list: stubList, inspect: `[{"Name":"podman-machine-default"}]`}, read, "cannot read the config dir"},
		"relative dir":     {machineStub{list: stubList, inspect: `[{"Name":"podman-machine-default","ConfigDir":{"Path":"x"}}]`}, read, "not an absolute path"},
		"config missing":   {machineStub{list: stubList, inspect: stubInspect}, func(string) ([]byte, error) { return nil, os.ErrNotExist }, "cannot read the mounts"},
		"config not JSON":  {machineStub{list: stubList, inspect: stubInspect}, mountsReader("{"), "cannot parse the mounts"},
		"mounts not array": {machineStub{list: stubList, inspect: stubInspect}, mountsReader(`{"Mounts":"x"}`), "cannot parse the mounts"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := checkMachineMounts(context.Background(), tc.r, tc.read, []string{"/Users/u/code"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want a refusal that contains %q", err, tc.want)
			}
		})
	}
}

func TestPlainExecutor_Preflight(t *testing.T) {
	e := PlainExecutor{Runner: machineStub{list: stubList, inspect: stubInspect}, ReadFile: fixtureReader(t), MountAllowlist: []string{"/Users/u/code"}}
	if err := e.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "/private") {
		t.Errorf("Preflight = %v, want a refusal that names /private", err)
	}
	e.ReadFile = mountsReader(`{"Mounts":[{"Source":"/Users/u/code","Target":"/Users/u/code"}]}`)
	if err := e.Preflight(context.Background()); err != nil {
		t.Errorf("Preflight with allowed mounts = %v, want nil", err)
	}
}
