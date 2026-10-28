package cmd

// switch_two_step_3330_test.go — the two-step path of issue #3330, from end
// to end.
//
// Step 1: a sandboxed session writes an agent_status row through the /event
// proxy that names a dir outside every prism project, for example under
// ~/.npm. Step 2: it calls /switch, and the host runs `prism switch --path`
// with that dir, so host git runs in it.
//
// The sandboxed half is the real CLI client (`prism event`, `prism switch`)
// with PRISM_HOST_API set. The host half is the real host-API handler of a
// coordinator sidecar, which runs a real prism binary for /event. The host
// `prism switch` is a stub that records its argv, so no tmux session starts.

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/git"
	"github.com/prismatic-koi/prism/internal/harness"
	"github.com/prismatic-koi/prism/internal/sidecar"
	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

const (
	tsRepo  = "prism-test-twostep"
	tsCoord = tsRepo + "@main"
	// tsPlanted is a bare name: `prism switch --path` on the host names the
	// session for a plain dir after its basename.
	tsPlanted = "prism-test-twostep-npm"
)

type twoStepFixture struct {
	d        *db.DB
	coordWT  string
	writable string // a dir in no prism project, with a planted .bare
	switchLg string // argv of each host `prism switch`
}

// newTwoStepFixture builds the prism binary, a prism project under a project
// location in config.json, a writable dir outside it, and the sidecar of a
// coordinator that runs in a sandbox.
func newTwoStepFixture(t *testing.T) *twoStepFixture {
	t.Helper()
	bin, root := isolatedPrismBinary(t)

	f := &twoStepFixture{switchLg: filepath.Join(root, "switch-argv.log")}
	location := filepath.Join(root, "work")
	f.writable = filepath.Join(root, "npm", tsPlanted)
	for _, dir := range []string{location, f.writable} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	addProjectLocation(t, root, location)

	// A real prism project with a main worktree, and a planted .bare in the
	// writable dir.
	src := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", src},
		{"-C", src, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"init", "-q", "--bare", filepath.Join(f.writable, ".bare")},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	projectRoot := filepath.Join(location, tsRepo)
	if err := git.CloneWorktree(src, projectRoot, func(string) {}); err != nil {
		t.Fatalf("CloneWorktree: %v", err)
	}
	f.coordWT = filepath.Join(projectRoot, "main")

	// The host prism: `switch` records its argv, every other verb is the
	// real binary. The test process runs the sandboxed client, so the host
	// command must not inherit PRISM_HOST_API.
	hostPrism := filepath.Join(root, "bin", "host-prism")
	wrapper := "#!/bin/sh\nunset PRISM_HOST_API\n" +
		"if [ \"$1\" = switch ]; then echo \"$*\" >> '" + f.switchLg + "'; exit 0; fi\n" +
		"exec '" + bin + "' \"$@\"\n"
	if err := os.WriteFile(hostPrism, []byte(wrapper), 0o755); err != nil {
		t.Fatalf("write host prism wrapper: %v", err)
	}

	dbPath := rootDBPath(root)
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(dbPath)
	if err != nil {
		t.Fatalf("open the host DB: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	f.d = d
	if err := d.UpsertStatusSeedRootAgentName(tsCoord, tsRepo, f.coordWT, "active", nil, nil, "coordinator", "", ""); err != nil {
		t.Fatalf("seed %q: %v", tsCoord, err)
	}

	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
	sc := sidecar.New(sidecar.Config{
		SessionName:     tsCoord,
		Repo:            tsRepo,
		Worktree:        f.coordWT,
		DB:              d,
		Clock:           sidecar.RealClock(),
		AgentRole:       "coordinator",
		PrismBinaryPath: hostPrism,
		Harness:         &harness.FakeHarness{},
		Logger:          log.New(io.Discard, "", 0),
	})
	sock := filepath.Join(stateguardtest.ShortTempDir(t, "tsp"), "host.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen %s: %v", sock, err)
	}
	srv := &http.Server{Handler: sc.HostAPIHandlerForTest()}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	t.Setenv("PRISM_HOST_API", "unix://"+sock)

	SetTestDBPath(filepath.Join(t.TempDir(), "shadow.db"))
	t.Cleanup(func() { SetTestDBPath("") })
	return f
}

// addProjectLocation adds location to project_locations in the config.json
// of the isolated prism binary. The binary refuses a PRISM_CONFIG_FILE
// outside its root, so the test does not use configtest.
func addProjectLocation(t *testing.T, root, location string) {
	t.Helper()
	path := filepath.Join(root, "config", "prism", "config.json")
	cfg := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	cfg["project_locations"] = []string{location}
	if data, err = json.Marshal(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func (f *twoStepFixture) switchArgv(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(f.switchLg)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	_ = os.Remove(f.switchLg)
	return strings.TrimSpace(string(data))
}

// TestSwitch_TwoStepPathToAWritableDir_3330 tries each form of step 1 for a
// coordinator in a sandbox: its own row, a new bare name, and a name in its
// repo. Then it runs step 2 for each name. No host `prism switch` may get the
// writable dir. /event refuses each form of step 1 (#3207), so no row names
// the dir. /switch takes the dir from the project roots (#3330), so it does
// not read a row.
func TestSwitch_TwoStepPathToAWritableDir_3330(t *testing.T) {
	f := newTwoStepFixture(t)

	step1 := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"move its own row by state", []string{"event", "state-change", "--session", tsCoord, "--state", "active", "--worktree", f.writable}, "only its own worktree"},
		{"move its own row by start", []string{"event", "tmux-session-start", "--session", tsCoord, "--worktree", f.writable}, "only its own worktree"},
		{"new bare name", []string{"event", "tmux-session-start", "--session", tsPlanted, "--worktree", f.writable}, "only for itself"},
		{"new name in its repo", []string{"event", "tmux-session-start", "--session", tsRepo + "@npm", "--worktree", f.writable}, "only for itself"},
		{"state of a new name", []string{"event", "state-change", "--session", tsRepo + "@npm", "--state", "active", "--worktree", f.writable}, "only for itself"},
	}
	for _, tc := range step1 {
		err := runSandboxed(t, tc.args...)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("step 1, %s: err = %v, want an error with %q", tc.name, err, tc.wantErr)
		}
	}
	rows, err := f.d.AllStatusesWithPrefix(tsRepo)
	if err != nil {
		t.Fatalf("AllStatusesWithPrefix: %v", err)
	}
	for _, r := range rows {
		if strings.HasPrefix(r.Worktree, filepath.Dir(f.writable)) {
			t.Errorf("step 1 wrote a row that names the writable dir: %s worktree=%s", r.SessionName, r.Worktree)
		}
	}

	for _, name := range []string{tsCoord, tsPlanted, tsRepo + "@npm"} {
		_ = runSandboxed(t, "switch", "--path", name)
		argv := f.switchArgv(t)
		if strings.Contains(argv, filepath.Dir(f.writable)) {
			t.Errorf("step 2, /switch %s: host ran `prism %s`, which names the writable dir", name, argv)
		}
		if name == tsCoord && argv != "switch --path "+f.coordWT {
			t.Errorf("step 2, /switch %s: host argv = %q, want %q", name, argv, "switch --path "+f.coordWT)
		}
	}
}
