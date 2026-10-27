package sidecar

// Integration tests for runStartupStdio coalescing.
//
// These tests exercise the msg_assistant accumulator added to the
// runStartupStdio scanner loop. The harness binary is
// sidecartest.FakeStdioHarness. PRISM_FAKE_STDIO_HARNESS selects the JSONL
// frames that it writes to stdout.

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/harness"
	pih "github.com/prismatic-koi/prism/internal/harness/pi"
	"github.com/prismatic-koi/prism/internal/sidecar/sidecartest"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// legacyFakeHarness implements harness.Harness but NOT harness.FrameNormaliser,
// so runStartupStdio falls through to the legacy fallback path.
type legacyFakeHarness struct {
	harness.FakeHarness
}

// newStdioSidecar creates a Sidecar for stdio testing. mode is set via
// t.Setenv so the fake harness writes the right frames.
// h is the Harness to use; pass nil to use the PI adapter (has FrameNormaliser).
func newStdioSidecar(t *testing.T, mode string, h harness.Harness) *Sidecar {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("PRISM_TEST_MODE_RESTRICT_HOSTAPI", "1")
	t.Setenv("PRISM_FAKE_STDIO_HARNESS", mode)

	harnessBin := sidecartest.FakeStdioHarness(t)

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
		HarnessBinaryPath: harnessBin,
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

// requireUsableBwrap skips the test when bwrap is on PATH but cannot run.
// buildStdioHarnessCmd falls back to direct exec only when bwrap is absent
// from PATH. Where a kernel or AppArmor policy blocks the uid-map setup, or
// in a prism bwrap sandbox (--disable-userns), the wrapped exec fails and
// the harness exits before it writes a frame.
func requireUsableBwrap(t *testing.T) {
	t.Helper()
	bin, err := exec.LookPath("bwrap")
	if err != nil {
		return
	}
	probe := exec.Command(bin, "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
		"--unshare-pid", "/bin/sh", "-c", "true")
	if out, err := probe.CombinedOutput(); err != nil {
		t.Skipf("bwrap is on PATH but cannot run here: %v — %s", err, out)
	}
}

// ── tests ─────────────────────────────────────────────────────────────────────

// TestStdio_MsgAssistantCoalesced verifies that multiple msg_assistant
// fragments between turn_start and turn_end produce exactly one msg_assistant
// row per turn with concatenated text and token/cost fields.
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
// without causing StateError.
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
