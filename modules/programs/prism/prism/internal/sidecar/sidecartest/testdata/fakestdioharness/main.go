// Command fakestdioharness is the fake stdio harness for the runStartupStdio
// tests. It writes the JSONL frames of one mode to stdout, then exits. The
// value of PRISM_FAKE_STDIO_HARNESS selects the mode. buildStdioHarnessCmd
// forwards that variable into the bwrap sandbox.
//
// Import only the standard library. sidecartest.FakeStdioHarness builds this
// package with CGO_ENABLED=0, so the binary is static and runs in the
// harness sandbox, which binds no host loader (/lib64).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// SilentStartedMarker is the one non-frame line that the silent mode writes.
// The tests match the same text.
const SilentStartedMarker = "fakestdioharness-started"

func main() {
	mode := os.Getenv("PRISM_FAKE_STDIO_HARNESS")

	enc := json.NewEncoder(os.Stdout)
	writeFrame := func(v any) {
		if err := enc.Encode(v); err != nil {
			fmt.Fprintf(os.Stderr, "fake harness encode: %v\n", err)
			os.Exit(1)
		}
	}
	sc := func(state string) map[string]any {
		return map[string]any{"type": "state_change", "state": state}
	}

	switch mode {
	case "normal":
		writeFrame(sc("active"))
		writeFrame(map[string]any{"type": "msg_assistant", "text": "hello from fake harness"})
		writeFrame(sc("finished"))

	case "silent":
		// No frames: the "pipe closed before first frame" error path.
		// One line that is not a JSON frame proves that the harness ran.
		// The sidecar logs it as an unparseable line and does not count it
		// as a frame.
		fmt.Println(SilentStartedMarker)

	case "coalesced_2turn":
		writeFrame(sc("active"))
		writeFrame(map[string]any{"type": "turn_start"})
		writeFrame(map[string]any{"type": "msg_assistant", "text": "Hello"})
		writeFrame(map[string]any{"type": "msg_assistant", "text": " world"})
		writeFrame(map[string]any{"type": "msg_assistant", "text": "!"})
		writeFrame(map[string]any{
			"type": "turn_end",
			"usage": map[string]any{
				"input": 10, "output": 5, "cache_read": 2, "cache_write": 1, "cost": 0.001,
			},
		})
		writeFrame(map[string]any{"type": "turn_start"})
		writeFrame(map[string]any{"type": "msg_assistant", "text": "Second"})
		writeFrame(map[string]any{"type": "msg_assistant", "text": " turn"})
		writeFrame(map[string]any{
			"type": "turn_end",
			"usage": map[string]any{
				"input": 20, "output": 8, "cache_read": 0, "cache_write": 0, "cost": 0.002,
			},
		})
		writeFrame(sc("finished"))

	case "tool_only_turn":
		writeFrame(sc("active"))
		writeFrame(map[string]any{"type": "turn_start"})
		writeFrame(map[string]any{"type": "turn_end"})
		writeFrame(sc("finished"))

	case "partial_exit":
		writeFrame(sc("active"))
		writeFrame(map[string]any{"type": "turn_start"})
		writeFrame(map[string]any{"type": "msg_assistant", "text": "partial"})
		// Exit mid-turn: no turn_end and no state=finished.
		os.Exit(1)

	case "legacy_coalesced":
		writeFrame(sc("active"))
		writeFrame(map[string]any{"type": "turn_start"})
		writeFrame(map[string]any{"type": "msg_assistant", "text": "A"})
		writeFrame(map[string]any{"type": "msg_assistant", "text": "B"})
		writeFrame(map[string]any{
			"type":  "turn_end",
			"usage": map[string]any{"input": 5, "output": 3},
		})
		writeFrame(sc("finished"))

	case "large_frame":
		// 128 KiB of text is more than the 64 KiB default bufio.Scanner
		// limit, so this mode needs the enlarged scanner buffer.
		writeFrame(sc("active"))
		writeFrame(map[string]any{"type": "turn_start"})
		writeFrame(map[string]any{
			"type": "msg_assistant",
			"text": strings.Repeat("A", 128*1024),
		})
		writeFrame(map[string]any{
			"type":  "turn_end",
			"usage": map[string]any{"input": 1, "output": 1},
		})
		writeFrame(sc("finished"))

	default:
		fmt.Fprintf(os.Stderr, "fake harness: unknown PRISM_FAKE_STDIO_HARNESS mode %q\n", mode)
		os.Exit(2)
	}
}
