package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedProbeEnvVar names the file that runSeedProbe writes to. TestMain
// routes an `event` re-invocation of this test binary to runSeedProbe when
// the variable is set.
const seedProbeEnvVar = "PRISM_TEST_SEED_PROBE"

// runSeedProbe is the stub for the `prism event tmux-session-start` seed that
// setupFullLayout runs. It records whether the sidecar PID file of the
// session already existed when the seed ran. StartSidecarWithOpts writes that
// file before it returns, so "present" means the sidecar started before the
// seed.
func runSeedProbe(probePath string, args []string) int {
	session := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--session" {
			session = args[i+1]
		}
	}
	result := "no-session-flag"
	if session != "" {
		pidPath, err := SidecarPIDPath(session)
		switch {
		case err != nil:
			result = "pid-path-error"
		case fileExists(pidPath):
			result = "sidecar-pid-file=present"
		default:
			result = "sidecar-pid-file=absent"
		}
	}
	f, err := os.OpenFile(probePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 1
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(args, " ") + "\n" + result + "\n"); err != nil {
		return 1
	}
	return 0
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestSetupFullLayout_SeedRunsBeforeSidecar verifies that setupFullLayout runs
// the `prism event tmux-session-start` seed before it starts the sidecar.
//
// The seed decides the instance ID of the new incarnation. A sidecar started
// with no --instance-id reads agent_status.instance_id once at startup. If
// the sidecar starts first, it reads the instance ID of the previous, ended
// incarnation and writes every agent_events row with it (#3027).
func TestSetupFullLayout_SeedRunsBeforeSidecar(t *testing.T) {
	_, _ = openSpawnTestDB(t)
	_ = spyTmuxBin(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	probePath := filepath.Join(t.TempDir(), "seed-probe")
	t.Setenv(seedProbeEnvVar, probePath)

	const sessionName = "prism-test@seed-order"
	opts := Opts{
		SessionName:    sessionName,
		Layout:         LayoutFull,
		IsolationMode:  "bwrap",
		PIExtensionDir: testPIExtensionDir,
		ForceFresh:     true,
		Port:           14999,
	}
	if err := Create(sessionName, t.TempDir(), opts); err != nil {
		t.Fatalf("Create: %v", err)
	}

	pidPath, err := SidecarPIDPath(sessionName)
	if err != nil {
		t.Fatalf("SidecarPIDPath: %v", err)
	}
	if !fileExists(pidPath) {
		t.Fatalf("sidecar PID file %s is missing: the sidecar did not start, so the test proves nothing", pidPath)
	}

	data, err := os.ReadFile(probePath)
	if err != nil {
		t.Fatalf("read seed probe: %v (the seed did not run)", err)
	}
	got := string(data)
	if !strings.Contains(got, "tmux-session-start") {
		t.Fatalf("seed probe did not record a tmux-session-start call; got:\n%s", got)
	}
	if !strings.Contains(got, "sidecar-pid-file=absent") {
		t.Errorf("the seed ran after the sidecar started; want the seed first. probe:\n%s", got)
	}
}
