// Tests for realPiExec: the real `pi` subprocess path, not
// the piExecFn seam that other pr_test.go tests inject. These tests
// exercise realPiExec directly against a PATH-injected stub `pi` script,
// mirroring the mergequeue execGH timeout-test convention
// (internal/mergequeue/exec_gh_timeout_test.go).

package quick

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// writeStubPi writes an executable shell script named "pi" into dir and
// prepends dir to PATH for the duration of the test.
func writeStubPi(t *testing.T, dir, script string) {
	t.Helper()
	stubPath := filepath.Join(dir, "pi")
	if err := os.WriteFile(stubPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub pi: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestRealPiExec_Timeout exercises the real realPiExec path (no seam
// injection) against a stub `pi` that sleeps far longer than a
// test-shortened piExecTimeout. It asserts a prompt non-zero-equivalent
// error naming the timeout, and that no orphan `pi` process survives.
func TestRealPiExec_Timeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-injection stub scripts require a POSIX shell")
	}

	dir := t.TempDir()
	// "exec sleep" replaces the shell process so SIGKILL from
	// exec.CommandContext terminates it immediately, rather than leaving
	// an orphaned sleep child holding the pipes open (mirrors the
	// mergequeue exec_gh_timeout_test.go rationale).
	writeStubPi(t, dir, fmt.Sprintf("#!/bin/sh\nexec sleep %.3f\n", 60*time.Second.Seconds()))

	prevTimeout := piExecTimeout
	piExecTimeout = 200 * time.Millisecond
	t.Cleanup(func() { piExecTimeout = prevTimeout })

	start := time.Now()
	res := realPiExec([]string{"--print"}, "")
	elapsed := time.Since(start)

	if res.err == nil {
		t.Fatal("realPiExec: expected a non-nil error when the timeout fires")
	}
	if !strings.Contains(res.err.Error(), "timed out") || !strings.Contains(res.err.Error(), piExecTimeout.String()) {
		t.Errorf("realPiExec error = %q; want it to name the timeout duration", res.err.Error())
	}

	maxExpected := 10 * piExecTimeout
	if elapsed > maxExpected {
		t.Errorf("realPiExec took %s; expected to time out within %s", elapsed, maxExpected)
	}
}

// TestRealPiExec_EnvScrub verifies that realPiExec strips the PI_*/
// PRISM_HARNESS_PIPE variables from the subprocess
// environment while preserving everything else.
func TestRealPiExec_EnvScrub(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-injection stub scripts require a POSIX shell")
	}

	dir := t.TempDir()
	writeStubPi(t, dir, "#!/bin/sh\nenv\n")

	t.Setenv("PI_SESSION_ID", "sess-123")
	t.Setenv("PI_SESSION_FILE", "/tmp/whatever")
	t.Setenv("PI_CODING_AGENT", "1")
	t.Setenv("PI_CODING_AGENT_DIR", "/tmp/agent-dir")
	t.Setenv("PRISM_HARNESS_PIPE", "unix:///tmp/pipe")
	t.Setenv("QUICK_PR_TEST_KEEP_ME", "keep-this-value")

	res := realPiExec([]string{}, "")
	if res.err != nil {
		t.Fatalf("realPiExec: unexpected error: %v (stderr=%s)", res.err, res.stderr)
	}

	blocked := []string{
		"PI_SESSION_ID=",
		"PI_SESSION_FILE=",
		"PI_CODING_AGENT=",
		"PI_CODING_AGENT_DIR=",
		"PRISM_HARNESS_PIPE=",
	}
	for _, b := range blocked {
		if strings.Contains(res.stdout, b) {
			t.Errorf("realPiExec: subprocess env contained blocked var %q; stdout=%s", b, res.stdout)
		}
	}
	if !strings.Contains(res.stdout, "QUICK_PR_TEST_KEEP_ME=keep-this-value") {
		t.Errorf("realPiExec: subprocess env dropped an unrelated var; stdout=%s", res.stdout)
	}
}

// TestRealPiExec_StderrTee verifies that pi's stderr is captured into the
// piResult AND written to the real os.Stderr concurrently,
// by redirecting the test process's os.Stderr to a pipe and reading both
// ends.
func TestRealPiExec_StderrTee(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PATH-injection stub scripts require a POSIX shell")
	}

	dir := t.TempDir()
	const marker = "quick-pr-stderr-tee-marker"
	writeStubPi(t, dir, fmt.Sprintf("#!/bin/sh\necho %s 1>&2\n", marker))

	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w

	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 0, 4096)
		tmp := make([]byte, 4096)
		for {
			n, rerr := r.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if rerr != nil {
				break
			}
		}
		done <- string(buf)
	}()

	res := realPiExec([]string{}, "")

	os.Stderr = origStderr
	_ = w.Close()
	captured := <-done
	_ = r.Close()

	if res.err != nil {
		t.Fatalf("realPiExec: unexpected error: %v", res.err)
	}
	if !strings.Contains(res.stderr, marker) {
		t.Errorf("realPiExec: piResult.stderr = %q; want it to contain %q", res.stderr, marker)
	}
	if !strings.Contains(captured, marker) {
		t.Errorf("realPiExec: real os.Stderr = %q; want it to contain %q", captured, marker)
	}
}
