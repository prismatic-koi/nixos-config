package integration_test

// TestMain runs the fake stdio harness when PRISM_FAKE_STDIO_HARNESS is set,
// allowing the test binary to double as the harness process in integration
// tests. This keeps the fake harness code inside the test binary and out of
// the production binary.
//
// Modes (value of PRISM_FAKE_STDIO_HARNESS):
//
//   - "normal": writes 3 JSONL frames (state_change started, msg_assistant, state_change
//     finished) then exits 0.
//   - "silent": exits 0 immediately without writing any frames. Exercises the
//     "pipe closed before first frame" error path in runStartupStdio.
//
// When PRISM_FAKE_STDIO_HARNESS is unset, TestMain runs the normal test suite.

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(m *testing.M) {
	switch os.Getenv("PRISM_FAKE_STDIO_HARNESS") {
	case "normal":
		runFakeHarnessNormal()
		os.Exit(0)
	case "silent":
		// Exit immediately without writing any frames.
		os.Exit(0)
	}
	// Default: run the test suite.
	os.Exit(m.Run())
}

// runFakeHarnessNormal writes 3 valid JSONL event frames to stdout and exits.
// States use agent.AgentState values (active, finished) so the DB state
// machine does not reject the transitions.
func runFakeHarnessNormal() {
	frames := []string{
		`{"type":"state_change","state":"active"}`,
		`{"type":"msg_assistant","text":"hello from fake harness"}`,
		`{"type":"state_change","state":"finished"}`,
	}
	for _, f := range frames {
		fmt.Println(f)
	}
}
