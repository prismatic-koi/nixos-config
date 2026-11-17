package cmd

// Tests for #3496: the --isolation flag of prism switch and prism launch.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/prismatic-koi/prism/internal/config"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
	"github.com/prismatic-koi/prism/internal/tmux"
)

// sandboxModeForPlatform is the sandboxed isolation mode that this platform
// supports.
func sandboxModeForPlatform() config.IsolationMode {
	if runtime.GOOS == "darwin" {
		return config.IsolationSandboxExec
	}
	return config.IsolationBwrap
}

// unsupportedModeForPlatform is an isolation mode that this platform does
// not support.
func unsupportedModeForPlatform() config.IsolationMode {
	if runtime.GOOS == "darwin" {
		return config.IsolationBwrap
	}
	return config.IsolationSandboxExec
}

// switchIsolationFixture is the config of prism switch for these tests: the
// machine default is the sandboxed mode of the platform, and
// project_isolation_overrides gives overrideDir the host mode.
type switchIsolationFixture struct {
	plainDir    string
	overrideDir string
	envVars     map[string]string
}

func newSwitchIsolationFixture(t *testing.T) switchIsolationFixture {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	f := switchIsolationFixture{
		plainDir:    filepath.Join(t.TempDir(), "plain-3496"),
		overrideDir: filepath.Join(t.TempDir(), "override-3496"),
		envVars:     map[string]string{"PRISM_TEST_ENV_3496": "from-profiles"},
	}
	for _, dir := range []string{f.plainDir, f.overrideDir} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	profiles, err := json.Marshal(map[string]any{
		"default":        "p3496",
		"profiles":       map[string]any{"p3496": map[string]any{"worker": map[string]any{"model": "test/model"}}},
		"agent_env_vars": f.envVars,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(configHome, "prism"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configHome, "prism", "profiles.json"), profiles, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.LoadFresh()
	cfg.DefaultIsolationMode = sandboxModeForPlatform()
	cfg.ProjectIsolationOverrides = map[string]string{f.overrideDir: string(config.IsolationHost)}
	cfg.PIExtensionDir = "/test/prism-pi-extension"
	prev := loadSwitchConfig
	loadSwitchConfig = func() config.Config { return cfg }
	t.Cleanup(func() { loadSwitchConfig = prev })
	return f
}

// optsForPath returns the session options that prism switch --path gives
// to ensureAndSwitch for the plain dir path.
func optsForPath(t *testing.T, isolation, path string) session.Opts {
	t.Helper()
	s, err := loadSwitchSetup(false, isolation)
	if err != nil {
		t.Fatalf("loadSwitchSetup(--isolation %q): %v", isolation, err)
	}
	o := s.opts
	applyPathIsolationOverride(path, s.cfg, &o, s.isoCaps, s.pf)
	return o
}

// AC (#3496): --isolation selects the mode of the session. It takes
// precedence over project_isolation_overrides. A host session that the flag
// selects from a sandboxed machine default gets the agent_env_vars of the
// profiles file. Without the flag, the mode is the override for the path,
// then the machine default, as before.
func TestSwitchIsolationFlag_ModeSelection_3496(t *testing.T) {
	f := newSwitchIsolationFixture(t)
	sandboxed := string(sandboxModeForPlatform())
	for _, tc := range []struct {
		desc      string
		isolation string
		path      string
		wantMode  string
		wantEnv   bool
		explicit  bool
	}{
		{"flag host over a sandboxed default", "host", f.plainDir, "host", true, true},
		{"flag over a matching override", sandboxed, f.overrideDir, sandboxed, false, true},
		{"no flag, override for the path", "", f.overrideDir, "host", true, false},
		{"no flag, machine default", "", f.plainDir, sandboxed, false, false},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			o := optsForPath(t, tc.isolation, tc.path)
			if o.IsolationMode != tc.wantMode {
				t.Errorf("IsolationMode = %q, want %q", o.IsolationMode, tc.wantMode)
			}
			if o.ExplicitIsolation != tc.explicit {
				t.Errorf("ExplicitIsolation = %v, want %v", o.ExplicitIsolation, tc.explicit)
			}
			if gotEnv := o.AgentEnvVars["PRISM_TEST_ENV_3496"] == "from-profiles"; gotEnv != tc.wantEnv {
				t.Errorf("AgentEnvVars = %v, want the agent_env_vars of the profiles file: %v", o.AgentEnvVars, tc.wantEnv)
			}
		})
	}
}

// spawnIsolationError returns the error of prism spawn for --isolation value.
func spawnIsolationError(t *testing.T, value string) error {
	t.Helper()
	c := &cobra.Command{Use: "spawn"}
	c.Flags().String("isolation", "", "")
	if err := c.Flags().Set("isolation", value); err != nil {
		t.Fatal(err)
	}
	_, err := resolveIsolationMode(c, loadSwitchConfig())
	return err
}

// newSwitchTestCmd returns a prism switch command with fresh flags, so a
// test does not change the flag state of switchCmd.
func newSwitchTestCmd(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	c := &cobra.Command{Use: "switch", RunE: switchCmd.RunE}
	for _, name := range []string{"path", "isolation"} {
		if switchCmd.Flags().Lookup(name) == nil {
			t.Fatalf("prism switch has no --%s flag", name)
		}
		c.Flags().String(name, "", "")
	}
	c.Flags().Bool("fresh", false, "")
	if err := c.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	return c
}

// AC (#3496): an unknown --isolation value, and a mode that the platform
// does not support, give the error of prism spawn for that value. The
// unknown-value error lists the valid values. prism switch exits non-zero
// and runs no tmux command, so it starts no session.
func TestSwitchIsolationFlag_Refused_3496(t *testing.T) {
	f := newSwitchIsolationFixture(t)
	t.Setenv("PRISM_HOST_API", "")
	for _, value := range []string{"podman", string(unsupportedModeForPlatform())} {
		t.Run(value, func(t *testing.T) {
			tmuxLog := withConvertCheckTmux(t, noTmuxServerListing)
			want := spawnIsolationError(t, value)
			if want == nil {
				t.Fatalf("control: prism spawn accepts --isolation %s", value)
			}
			c := newSwitchTestCmd(t, "--path", f.plainDir, "--isolation", value)
			err := c.RunE(c, nil)
			if err == nil || err.Error() != want.Error() {
				t.Errorf("prism switch --isolation %s = %v, want the error of prism spawn: %v", value, err, want)
			}
			if value == "podman" {
				for _, m := range config.ValidIsolationModes {
					if !strings.Contains(want.Error(), string(m)) {
						t.Errorf("error %q does not list the valid value %q", want, m)
					}
				}
			}
			if calls := tmuxCalls(t, tmuxLog); len(calls) != 0 {
				t.Errorf("tmux calls %v, want none", calls)
			}
		})
	}
}

// recordingHostAPI is a host API that records the path and body of each
// request.
type recordingHostAPI struct {
	mu       sync.Mutex
	requests []string
}

func startRecordingHostAPI(t *testing.T) (*recordingHostAPI, string) {
	t.Helper()
	sockPath := filepath.Join(stateguardtest.ShortTempDir(t, "p"), "host.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	r := &recordingHostAPI{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.requests = append(r.requests, req.URL.Path+" "+string(body))
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return r, "unix://" + sockPath
}

func (r *recordingHostAPI) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.requests)
}

// AC (#3496): inside a sandbox, prism switch --isolation exits non-zero,
// says that --isolation is not available there, and sends no request to the
// host API. Without --isolation, the proxy sends the path only.
func TestSwitchIsolationFlag_InsideSandbox_3496(t *testing.T) {
	api, apiURL := startRecordingHostAPI(t)
	t.Setenv("PRISM_HOST_API", apiURL)

	c := newSwitchTestCmd(t, "--path", "/some/dir", "--isolation", "host")
	err := c.RunE(c, nil)
	if err == nil || !strings.Contains(err.Error(), "--isolation is not available inside a sandbox") {
		t.Errorf("prism switch --isolation in a sandbox = %v, want a refusal", err)
	}
	if reqs := api.got(); len(reqs) != 0 {
		t.Fatalf("host API requests %q, want none", reqs)
	}

	c = newSwitchTestCmd(t, "--path", "/some/dir")
	if err := c.RunE(c, nil); err != nil {
		t.Fatalf("control: prism switch without --isolation in a sandbox: %v", err)
	}
	reqs := api.got()
	if len(reqs) != 1 || !strings.HasPrefix(reqs[0], "/switch ") {
		t.Fatalf("control: host API requests %q, want one /switch request", reqs)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(reqs[0], "/switch ")), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["isolation"]; ok || body["session"] != "/some/dir" {
		t.Errorf("control: /switch body = %v, want the path only", body)
	}
}

// runningSessionFixture is a live session of a plain dir in a test tmux
// server. Its agent_status row has a fresh last_seen.
type runningSessionFixture struct {
	srv         *cmdTestServer
	dir         string
	sessionName string
	tmuxID      string
}

func newRunningSessionFixture(t *testing.T, mode string) runningSessionFixture {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	srv := newCmdTestServer(t)
	withCmdServer(t, srv)
	d := openRestoreTestDB(t)
	SetTestDBPath(d.Path())
	t.Cleanup(func() { SetTestDBPath("") })

	dir := filepath.Join(t.TempDir(), "running-3496")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := runningSessionFixture{srv: srv, dir: dir, sessionName: mustNameFor(t, dir, "")}
	if err := tmux.NewSessionDetached(f.sessionName, dir); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	seedStatus(t, d, f.sessionName, dir, nil)
	if mode != "" {
		if err := d.SetIsolationMode(f.sessionName, mode); err != nil {
			t.Fatal(err)
		}
	}
	f.tmuxID = staleSwitchFixture{srv: srv, sessionName: f.sessionName}.sessionID(t)
	return f
}

// AC (#3496): when the session already runs, an explicit --isolation of
// another mode, or of a mode that prism cannot read, exits non-zero. The
// error names the current mode and tells the user to close the session
// first. An explicit mode that matches, and no flag, attach as before.
func TestEnsureAndSwitch_RunningSessionIsolation_3496(t *testing.T) {
	for _, tc := range []struct {
		desc      string
		running   string
		requested string
		explicit  bool
		wantErr   []string
	}{
		{"explicit mode differs", "bwrap", "host", true, []string{`isolation mode "bwrap", not "host"`, "Close the session first"}},
		{"explicit mode, mode not recorded", "", "host", true, []string{"cannot read its isolation mode", "Close the session first"}},
		{"explicit mode matches", "bwrap", "bwrap", true, nil},
		{"no flag, mode differs", "bwrap", "host", false, nil},
	} {
		t.Run(tc.desc, func(t *testing.T) {
			f := newRunningSessionFixture(t, tc.running)
			opts := switchTestOpts()
			opts.IsolationMode = tc.requested
			opts.ExplicitIsolation = tc.explicit

			var err error
			_, stderr := captureStdoutStderr(t, func() { err = ensureAndSwitch(f.dir, "", opts) })
			if tc.wantErr == nil && err != nil {
				t.Fatalf("ensureAndSwitch: %v (stderr %q), want the attach", err, stderr)
			}
			for _, want := range tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("ensureAndSwitch = %v, want an error with %q", err, want)
				}
			}
			if got := (staleSwitchFixture{srv: f.srv, sessionName: f.sessionName}).sessionID(t); got != f.tmuxID {
				t.Errorf("tmux session_id = %s, want %s: the running session must stay", got, f.tmuxID)
			}
		})
	}
}

// AC (#3496): prism launch --isolation without --path exits non-zero, says
// that --isolation needs --path, and starts no session.
func TestRunLaunch_IsolationNeedsPath_3496(t *testing.T) {
	tmuxLog := withConvertCheckTmux(t, noTmuxServerListing)
	resetLaunchFlags(t)
	launchIsolation = "host"
	for _, inTerminal := range []bool{false, true} {
		launchInTerminal = inTerminal
		err := runLaunch(nil, nil)
		if err == nil || !strings.Contains(err.Error(), "--isolation needs --path") {
			t.Errorf("prism launch --isolation (in-terminal %v) = %v, want a refusal", inTerminal, err)
		}
	}
	if calls := tmuxCalls(t, tmuxLog); len(calls) != 0 {
		t.Errorf("tmux calls %v, want none", calls)
	}
}

// AC (#3496): prism launch --path refuses an unknown --isolation value, and
// a mode that the platform does not support, with the error of prism
// spawn. It does so before it starts a session or a popup.
func TestRunLaunchWithPath_IsolationRefused_3496(t *testing.T) {
	newSwitchIsolationFixture(t)
	tmuxLog := withConvertCheckTmux(t, noTmuxServerListing)
	resetLaunchFlags(t)
	launchPath = t.TempDir()
	for _, value := range []string{"podman", string(unsupportedModeForPlatform())} {
		launchIsolation = value
		want := spawnIsolationError(t, value)
		if err := runLaunch(nil, nil); err == nil || want == nil || err.Error() != want.Error() {
			t.Errorf("prism launch --isolation %s = %v, want the error of prism spawn: %v", value, err, want)
		}
	}
	if calls := tmuxCalls(t, tmuxLog); len(calls) != 0 {
		t.Errorf("tmux calls %v, want none", calls)
	}
}

// launchSwitchArgv runs the popup command cmd in sh, with prism as a shell
// function that prints its arguments, and returns those arguments.
func launchSwitchArgv(t *testing.T, cmd string) []string {
	t.Helper()
	out, err := exec.Command("sh", "-c", `prism() { printf '%s\n' "$@"; }; `+cmd).Output()
	if err != nil {
		t.Fatalf("sh -c %q: %v", cmd, err)
	}
	return strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
}

// launchTestDir is a dir whose name has a space, a quote, and a $.
func launchTestDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "it's my $HOME dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// AC (#3496): each route of prism launch --path gives the popup a prism
// switch command with the same --path and --isolation values, also for a
// dir with a space in its name. The inside-tmux route and the kitty route
// give the command to tmux as one argument. The --in-terminal route puts it
// in a tmux hook command, which tmux parses first.
func TestRunLaunchWithPath_ForwardsIsolation_3496(t *testing.T) {
	newSwitchIsolationFixture(t)
	dir := launchTestDir(t)
	wantArgv := []string{"switch", "--path", dir, "--fresh", "--isolation", "host"}
	popupPrefix := "display-popup -w 80% -h 80% -E "

	setFlags := func(t *testing.T) {
		resetLaunchFlags(t)
		launchPath = dir
		launchFresh = true
		launchIsolation = "host"
	}

	t.Run("inside tmux", func(t *testing.T) {
		setFlags(t)
		t.Setenv("TMUX", "fake/0,0")
		logFile := withNavSpy(t, "caller", "client-1")
		if err := runLaunch(nil, nil); err != nil {
			t.Fatalf("runLaunch: %v", err)
		}
		var popup []string
		for _, c := range tmuxCalls(t, logFile) {
			if c[0] == "display-popup" {
				popup = c
			}
		}
		if len(popup) == 0 {
			t.Fatal("no display-popup call")
		}
		if got := launchSwitchArgv(t, popup[len(popup)-1]); !slices.Equal(got, wantArgv) {
			t.Errorf("popup prism argv = %q, want %q", got, wantArgv)
		}
	})

	t.Run("in terminal", func(t *testing.T) {
		setFlags(t)
		t.Setenv("TMUX", "")
		launchInTerminal = true
		logFile := withNavSpy(t, "caller", "client-1")
		origExec := syscallExec
		syscallExec = func(string, []string, []string) error { return nil }
		t.Cleanup(func() { syscallExec = origExec })
		if err := runLaunch(nil, nil); err != nil {
			t.Fatalf("runLaunch: %v", err)
		}
		var hook string
		for _, c := range tmuxCalls(t, logFile) {
			if c[0] == "set-hook" {
				hook = c[len(c)-1]
			}
		}
		if !strings.Contains(hook, popupPrefix) {
			t.Fatalf("set-hook command %q has no %q", hook, popupPrefix)
		}

		// Let a real tmux parse the hook command. set-option stores the
		// popup command as tmux parsed it.
		srv := newCmdTestServer(t)
		parsed := strings.Replace(hook, popupPrefix, "set-option -g @prism-test-3496 ", 1)
		conf := filepath.Join(t.TempDir(), "hook.conf")
		if err := os.WriteFile(conf, []byte(parsed+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = srv.run("source-file", conf)
		popupCmd, err := srv.output("show-options", "-gv", "@prism-test-3496")
		if err != nil {
			t.Fatalf("tmux did not parse the hook command %q: %v", hook, err)
		}
		if got := launchSwitchArgv(t, strings.TrimSuffix(popupCmd, "\n")); !slices.Equal(got, wantArgv) {
			t.Errorf("popup prism argv = %q, want %q", got, wantArgv)
		}
	})

	t.Run("new kitty window", func(t *testing.T) {
		setFlags(t)
		t.Setenv("TMUX", "")
		var argv []string
		orig := execStart
		execStart = func(c *exec.Cmd) error { argv = c.Args; return nil }
		t.Cleanup(func() { execStart = orig })
		if err := runLaunch(nil, nil); err != nil {
			t.Fatalf("runLaunch: %v", err)
		}
		if len(argv) < 2 || argv[len(argv)-2] != "-E" {
			t.Fatalf("kitty argv %q does not end with display-popup -E <command>", argv)
		}
		if got := launchSwitchArgv(t, argv[len(argv)-1]); !slices.Equal(got, wantArgv) {
			t.Errorf("popup prism argv = %q, want %q", got, wantArgv)
		}
	})
}
