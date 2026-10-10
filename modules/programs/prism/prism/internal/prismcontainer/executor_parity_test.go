package prismcontainer_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
)

// The agent must see the same `prism container build` on Linux and on
// macOS. The tests below run one request through both executors, the
// Linux ScopeExecutor (with a fake systemd-run and systemctl) and the macOS
// PlainExecutor (with the fake podman), and compare what the agent sees.

// scopeFake is a fake systemd-run that acts as podman build after the
// scope starts, and a fake systemctl that logs its calls.
type scopeFake struct {
	dir string
	exe prismcontainer.ScopeExecutor
}

func newScopeFake(t *testing.T, output string, code string, hang bool) scopeFake {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("FAKE_SCOPE_DIR", dir)
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("output", output, 0o600)
	write("code", code, 0o600)
	if hang {
		write("hang", "", 0o600)
	}
	run := write("systemd-run", `#!/bin/sh
d=$FAKE_SCOPE_DIR
while [ "$#" -gt 0 ] && [ "$1" != prism-container-build ]; do shift; done
shift
printf '%s\n' "$@" > "$d/podman.args"
cat "$d/output"
[ -f "$d/hang" ] && exec sleep 30
exit $(cat "$d/code")
`, 0o755)
	ctl := write("systemctl", `#!/bin/sh
echo "$*" >> "$FAKE_SCOPE_DIR/systemctl.log"
exit 0
`, 0o755)
	return scopeFake{dir: dir, exe: prismcontainer.ScopeExecutor{
		SystemdRun: run, Systemctl: ctl,
		Podman: "/fake/podman", Bwrap: "/fake/bwrap", Runner: &prismcontainertest.Fake{},
	}}
}

// podmanArgs returns the podman build argument vector inside the command
// that the scope runs: podman unshare bwrap <options> -- podman
// --cgroup-manager=cgroupfs build --cgroup-parent <token> <args>.
func (s scopeFake) podmanArgs(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.dir, "podman.args"))
	if err != nil {
		t.Fatalf("the fake systemd-run did not run: %v", err)
	}
	cmd := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	sep := slices.Index(cmd, "--")
	if len(cmd) < 3 || cmd[1] != "unshare" || sep < 0 || len(cmd) < sep+6 || cmd[sep+3] != "build" || cmd[sep+4] != "--cgroup-parent" {
		t.Fatalf("scope command = %q, want podman unshare bwrap ... -- podman --cgroup-manager=cgroupfs build --cgroup-parent ...", cmd)
	}
	return append([]string{"build"}, cmd[sep+6:]...)
}

func (s scopeFake) systemctlLog(t *testing.T) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(s.dir, "systemctl.log"))
	return string(data)
}

// withoutOption removes one flag and its value.
func withoutOption(args []string, flag string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == flag && i+1 < len(args) {
			i++
			continue
		}
		out = append(out, args[i])
	}
	return out
}

func TestBuildExecutors_SameRequestSameResult(t *testing.T) {
	const output = "STEP 1/2: FROM alpine\nSTEP 2/2: RUN make\nmake: done\n"
	cases := []struct {
		name string
		code int
	}{
		{"success", 0},
		{"failure", 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCaller(t)
			writeFile(t, c.Worktree, "app/Containerfile", "FROM alpine\nRUN make\n")
			writeFile(t, c.Worktree, "app/Makefile", "all:\n")
			req := prismcontainer.BuildRequest{
				Context:   "app",
				BuildArgs: []string{"VERSION=1", "OPT=--network=host"},
				Tag:       "app:v1",
			}

			f := &prismcontainertest.Fake{}
			f.OnBuild = func(_ context.Context, out io.Writer, _ []string) (int, error) {
				_, _ = io.WriteString(out, output)
				return tc.code, nil
			}
			plain := prismcontainer.Build(context.Background(), buildDeps(f), c, req)

			sf := newScopeFake(t, output, string(rune('0'+tc.code)), false)
			d := deps(&prismcontainertest.Fake{})
			d.BuildExecutor = sf.exe
			scope := prismcontainer.Build(context.Background(), d, c, req)

			if plain.ExitCode != tc.code || string(plain.Output) != output {
				t.Fatalf("plain result = %+v, want exit %d and the output", plain, tc.code)
			}
			if scope.ExitCode != plain.ExitCode || scope.Image != plain.Image ||
				string(scope.Output) != string(plain.Output) || scope.Message != plain.Message {
				t.Errorf("results differ:\nscope %+v\nplain %+v", scope, plain)
			}

			// Each executor adds only its own options: the process limit, and on
			// Linux the signature policy.
			plainArgs := prismcontainertest.MaskBuildArgs(withoutOption(f.BuildCalls()[0], "--ulimit"))
			scopeArgs := prismcontainertest.MaskBuildArgs(withoutOption(sf.podmanArgs(t), "--signature-policy"))
			if !slices.Equal(plainArgs, scopeArgs) {
				t.Errorf("podman argv differs:\nscope %q\nplain %q", scopeArgs, plainArgs)
			}
		})
	}
}

func TestBuildExecutors_SameTimeoutResult(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM alpine\nRUN sleep infinity\n")
	req := prismcontainer.BuildRequest{TimeoutSeconds: 1}

	f := &prismcontainertest.Fake{}
	f.OnBuild = func(ctx context.Context, out io.Writer, _ []string) (int, error) {
		_, _ = io.WriteString(out, "STEP 2/2: RUN sleep infinity\n")
		<-ctx.Done()
		return -1, ctx.Err()
	}
	plain := prismcontainer.Build(context.Background(), buildDeps(f), c, req)

	sf := newScopeFake(t, "STEP 2/2: RUN sleep infinity\n", "0", true)
	d := deps(&prismcontainertest.Fake{})
	d.BuildExecutor = sf.exe
	scope := prismcontainer.Build(context.Background(), d, c, req)

	if plain.ExitCode != prismcontainer.ExitTimeout || plain.Message != "the --timeout of 1s expired, so prism stopped the build" {
		t.Fatalf("plain result = %+v, want the timeout result", plain)
	}
	if scope.ExitCode != plain.ExitCode || scope.Message != plain.Message || string(scope.Output) != string(plain.Output) || scope.Image != "" {
		t.Errorf("timeout results differ:\nscope %+v\nplain %+v", scope, plain)
	}
	unit := "prism-build-" + container.InstanceTokenForID(testInstanceID) + "-"
	if log := sf.systemctlLog(t); !strings.Contains(log, "--user kill --signal=SIGKILL "+unit) {
		t.Errorf("systemctl log %q holds no SIGKILL of a scope with prefix %s", log, unit)
	}
}

func TestBuildExecutors_SameRefusal(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM alpine\nCOPY --from=tarball:/home/u/x.tar / /x\n")
	req := prismcontainer.BuildRequest{}

	f := &prismcontainertest.Fake{}
	plain := prismcontainer.Build(context.Background(), buildDeps(f), c, req)

	sf := newScopeFake(t, "", "0", false)
	d := deps(&prismcontainertest.Fake{})
	d.BuildExecutor = sf.exe
	scope := prismcontainer.Build(context.Background(), d, c, req)

	if plain.ExitCode != prismcontainer.ExitRefused || !strings.Contains(plain.Message, `the "tarball" transport`) {
		t.Fatalf("plain result = %+v, want the transport refusal", plain)
	}
	if scope.ExitCode != plain.ExitCode || scope.Message != plain.Message {
		t.Errorf("refusals differ:\nscope %+v\nplain %+v", scope, plain)
	}
	if _, err := os.Stat(filepath.Join(sf.dir, "podman.args")); !os.IsNotExist(err) {
		t.Errorf("the scope executor ran podman: %v", err)
	}
}
