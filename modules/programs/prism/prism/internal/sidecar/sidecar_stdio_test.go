package sidecar

// Integration tests for runStartupStdio coalescing (issue #1316).
//
// These tests exercise the msg_assistant accumulator added to the
// runStartupStdio scanner loop. The "harness binary" is the test binary
// itself, re-invoked via SIDECAR_TEST_STDIO_MODE which controls which JSONL
// frames it writes to stdout.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
)

// ── subprocess harness ────────────────────────────────────────────────────────

// init runs as the fake stdio harness when the test binary is re-invoked with
// PRISM_FAKE_STDIO_HARNESS set. Each mode writes a specific sequence of JSONL
// frames to stdout then exits.
//
// PRISM_FAKE_STDIO_HARNESS is already forwarded through bwrap by
// buildStdioHarnessCmd, so this works both with and without sandboxing.
func init() {
	mode := os.Getenv("PRISM_FAKE_STDIO_HARNESS")
	if mode == "" {
		return // normal test execution
	}

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
		// No msg_assistant frames.
		writeFrame(map[string]any{"type": "turn_end"})
		writeFrame(sc("finished"))

	case "partial_exit":
		writeFrame(sc("active"))
		writeFrame(map[string]any{"type": "turn_start"})
		writeFrame(map[string]any{"type": "msg_assistant", "text": "partial"})
		// Abrupt exit without turn_end or state=finished.
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
		// Emit a single msg_assistant frame whose text is 128 KiB — well above
		// the default 64 KiB bufio.Scanner limit — to exercise the enlarged
		// scanner buffer added in issue #1852.
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
		fmt.Fprintf(os.Stderr, "unknown SIDECAR_TEST_STDIO_MODE: %q\n", mode)
		os.Exit(2)
	}

	os.Exit(0)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// legacyFakeHarness implements harness.Harness but NOT harness.FrameNormaliser,
// so runStartupStdio falls through to the legacy fallback path.
type legacyFakeHarness struct {
	harness.FakeHarness
}

// newStdioSidecar creates a Sidecar for stdio testing. mode is set via
// t.Setenv so the re-invoked test binary writes the right frames.
// h is the Harness to use; pass nil to use the PI adapter (has FrameNormaliser).
func newStdioSidecar(t *testing.T, mode string, h harness.Harness) *Sidecar {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
	t.Setenv("PRISM_FAKE_STDIO_HARNESS", mode)

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	d := openTestDB(t)
	clk := newTestClock()

	if h == nil {
		h = pih.New("", "", "")
	}

	cfg := Config{
		SessionName:       "testrepo@stdio",
		Repo:              "testrepo",
		Worktree:          t.TempDir(),
		DB:                d,
		Clock:             clk,
		AgentRole:         "worker",
		HarnessName:       "pi",
		HarnessBinaryPath: exe,
		Harness:           h,
	}
	return New(cfg)
}

// runStdioSidecarAsync starts runStartupStdio in a goroutine and returns a
// wait function. The wait function blocks until the loop exits and returns
// the error.
func runStdioSidecarAsync(sc *Sidecar) func() error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- sc.runStartupStdio(context.Background())
	}()
	return func() error {
		select {
		case err := <-errCh:
			return err
		case <-time.After(10 * time.Second):
			return fmt.Errorf("runStartupStdio timed out")
		}
	}
}

// requireUsableBwrap skips the test when bwrap is not available in a usable
// form. buildStdioHarnessCmd resolves bwrap via exec.LookPath and only falls
// back to direct exec when bwrap is absent from PATH — so on environments
// where bwrap is in PATH but cannot actually run (e.g. GitHub Actions
// ubuntu-latest, where unprivileged userns uid-map setup is blocked by the
// runner's apparmor profile), the wrapped exec fails with "setting up uid
// map: Permission denied" and the harness exits before writing any frames.
//
// The skip message is loud and named per issue #1510 — see the follow-up for
// alternative-runner / privileged-container investigations.
func requireUsableBwrap(t *testing.T) {
	t.Helper()
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Skipf("skipping on GitHub Actions ubuntu-latest: %s — see #1510",
			"unprivileged userns uid-map setup is disallowed (kernel.apparmor_restrict_unprivileged_userns=1)")
	}
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestStdio_MsgAssistantCoalesced verifies that multiple msg_assistant
// fragments between turn_start and turn_end produce exactly one msg_assistant
// row per turn with concatenated text and token/cost fields (issue #1316 AC).
func TestStdio_MsgAssistantCoalesced(t *testing.T) {
	requireUsableBwrap(t)
	sc := newStdioSidecar(t, "coalesced_2turn", nil)
	wait := runStdioSidecarAsync(sc)
	_ = wait() // state=finished is written, so this returns nil; ignore either way

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	var msgEvents []db.Event
	for _, ev := range events {
		if ev.Type == "msg_assistant" {
			msgEvents = append(msgEvents, ev)
		}
	}

	if got := len(msgEvents); got != 2 {
		t.Fatalf("expected 2 msg_assistant events (one per turn), got %d", got)
	}

	// Build a map of text → payload for order-independent assertions.
	type msgPayload struct {
		Text             string  `json:"text"`
		InputTokens      int     `json:"inputTokens"`
		OutputTokens     int     `json:"outputTokens"`
		CacheReadTokens  int     `json:"cacheReadTokens"`
		CacheWriteTokens int     `json:"cacheWriteTokens"`
		Cost             float64 `json:"cost"`
	}
	byText := map[string]msgPayload{}
	for _, ev := range msgEvents {
		var p msgPayload
		if err := json.Unmarshal([]byte(ev.Payload), &p); err != nil {
			t.Fatalf("unmarshal msg_assistant payload: %v", err)
		}
		byText[p.Text] = p
	}

	// Turn 1 assertions.
	p0, ok := byText["Hello world!"]
	if !ok {
		t.Fatalf("no msg_assistant event with text=%q; got keys: %v", "Hello world!", mapKeys(byText))
	}
	if p0.InputTokens != 10 {
		t.Errorf("turn1 InputTokens = %d, want 10", p0.InputTokens)
	}
	if p0.OutputTokens != 5 {
		t.Errorf("turn1 OutputTokens = %d, want 5", p0.OutputTokens)
	}
	if p0.CacheReadTokens != 2 {
		t.Errorf("turn1 CacheReadTokens = %d, want 2", p0.CacheReadTokens)
	}
	if p0.CacheWriteTokens != 1 {
		t.Errorf("turn1 CacheWriteTokens = %d, want 1", p0.CacheWriteTokens)
	}
	if p0.Cost != 0.001 {
		t.Errorf("turn1 Cost = %v, want 0.001", p0.Cost)
	}

	// Turn 2 assertions.
	p1, ok := byText["Second turn"]
	if !ok {
		t.Fatalf("no msg_assistant event with text=%q; got keys: %v", "Second turn", mapKeys(byText))
	}
	if p1.InputTokens != 20 {
		t.Errorf("turn2 InputTokens = %d, want 20", p1.InputTokens)
	}
	if p1.Cost != 0.002 {
		t.Errorf("turn2 Cost = %v, want 0.002", p1.Cost)
	}
}

// mapKeys returns the keys of a map[string]T for use in error messages.
func mapKeys[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// TestStdio_ToolOnlyTurnNoSpuriousRow verifies that a turn with zero
// msg_assistant fragments does not produce a spurious empty msg_assistant row.
func TestStdio_ToolOnlyTurnNoSpuriousRow(t *testing.T) {
	sc := newStdioSidecar(t, "tool_only_turn", nil)
	wait := runStdioSidecarAsync(sc)
	_ = wait()

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	for _, ev := range events {
		if ev.Type == "msg_assistant" {
			t.Errorf("spurious msg_assistant row written for tool-only turn: %s", ev.Payload)
		}
	}
}

// TestStdio_PartialAccumulatorFlushedOnExit verifies that if the harness exits
// mid-turn without turn_end, any accumulated text is flushed as a partial
// msg_assistant row.
func TestStdio_PartialAccumulatorFlushedOnExit(t *testing.T) {
	requireUsableBwrap(t)
	sc := newStdioSidecar(t, "partial_exit", nil)
	wait := runStdioSidecarAsync(sc)
	_ = wait() // expected to return an error; ignore

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	found := false
	for _, ev := range events {
		if ev.Type == "msg_assistant" {
			found = true
			var p struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal([]byte(ev.Payload), &p)
			if p.Text != "partial" {
				t.Errorf("partial flush text = %q, want %q", p.Text, "partial")
			}
		}
	}
	if !found {
		t.Error("partial accumulator not flushed on harness exit mid-turn")
	}
}

// TestStdio_LegacyCoalesced verifies that the legacy fallback path (no
// FrameNormaliser) also coalesces msg_assistant fragments per turn.
func TestStdio_LegacyCoalesced(t *testing.T) {
	requireUsableBwrap(t)
	sc := newStdioSidecar(t, "legacy_coalesced", &legacyFakeHarness{})
	wait := runStdioSidecarAsync(sc)
	_ = wait()

	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	var msgEvents []db.Event
	for _, ev := range events {
		if ev.Type == "msg_assistant" {
			msgEvents = append(msgEvents, ev)
		}
	}
	if got := len(msgEvents); got != 1 {
		t.Fatalf("legacy path: expected 1 msg_assistant event, got %d", got)
	}
	var p struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(msgEvents[0].Payload), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Text != "AB" {
		t.Errorf("legacy coalesced text = %q, want %q", p.Text, "AB")
	}
}

// TestStdio_LargeFrameNoErrTooLong verifies that a 128 KiB single-line frame
// emitted by the stdio harness is processed without bufio.ErrTooLong and
// without causing StateError (issue #1852).
//
// Prior to the fix the scanner used the default 64 KiB max token size; a
// large msg_assistant frame would cause scanner.Scan() to return false with
// scanner.Err() == bufio.ErrTooLong, the loop would exit with framesRead > 0
// but lastState != "finished", and writeStartupError would be called.
func TestStdio_LargeFrameNoErrTooLong(t *testing.T) {
	requireUsableBwrap(t)
	sc := newStdioSidecar(t, "large_frame", nil)
	wait := runStdioSidecarAsync(sc)

	err := wait()
	if err != nil {
		t.Fatalf("runStartupStdio returned error: %v", err)
	}

	// The harness finished cleanly — confirm no StateError was written.
	status, dbErr := sc.cfg.DB.CurrentStatus(sc.cfg.SessionName)
	if dbErr != nil {
		t.Fatalf("CurrentStatus: %v", dbErr)
	}
	if status != nil && status.State == string(agent.StateError) {
		t.Errorf("session entered StateError; expected clean finish after 128 KiB frame")
	}

	// The 128 KiB msg_assistant frame must appear in agent_events.
	events := getEvents(t, sc.cfg.DB, sc.cfg.SessionName)
	var msgEvents []db.Event
	for _, ev := range events {
		if ev.Type == "msg_assistant" {
			msgEvents = append(msgEvents, ev)
		}
	}
	if len(msgEvents) == 0 {
		t.Fatal("no msg_assistant event recorded; large frame was dropped")
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(msgEvents[0].Payload), &payload); err != nil {
		t.Fatalf("unmarshal msg_assistant payload: %v", err)
	}
	const wantLen = 128 * 1024
	if got := len(payload.Text); got != wantLen {
		t.Errorf("msg_assistant text length = %d, want %d", got, wantLen)
	}
}
