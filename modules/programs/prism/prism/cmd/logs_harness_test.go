package cmd

// Tests for `prism logs --harness-events` (P5.LOGS / #1218).

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/prismatic-koi/prism/internal/db"
)

func setUpHarnessFramesDB(t *testing.T) *db.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prism.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	SetTestDBPath(path)
	t.Cleanup(func() { SetTestDBPath("") })
	return d
}

func writeFrameForCLI(t *testing.T, d *db.DB, sessionName, direction, typ, payload string, ts time.Time) {
	t.Helper()
	if err := d.WriteHarnessFrame(db.HarnessFrame{
		ID:          uuid.New().String(),
		SessionName: sessionName,
		Direction:   direction,
		Type:        typ,
		Payload:     payload,
		CreatedAt:   ts,
	}); err != nil {
		t.Fatalf("WriteHarnessFrame: %v", err)
	}
}

// TestRunHarnessEvents_NonPISession exercises the "no harness frames recorded"
// edge case (AC: non-PI session prints a clear hint and exits non-zero).
func TestRunHarnessEvents_NonPISession(t *testing.T) {
	setUpHarnessFramesDB(t)

	var buf bytes.Buffer
	err := runHarnessEvents("opencode-session@main", "", "", false, deliverSink{kind:"stdout"}, &buf)
	if err == nil {
		t.Fatal("expected error for non-PI session, got nil")
	}
	if !errors.Is(err, errNoHarnessFrames) {
		t.Errorf("err = %v; want errNoHarnessFrames sentinel", err)
	}
	if buf.Len() != 0 {
		t.Errorf("stdout should be empty on no-frames path, got: %q", buf.String())
	}
}

// TestRunHarnessEvents_PrintsAllFramesChronologically covers the basic
// "print all frames in chronological order, one JSON object per line" AC.
func TestRunHarnessEvents_PrintsAllFramesChronologically(t *testing.T) {
	d := setUpHarnessFramesDB(t)

	t0 := time.UnixMilli(1_700_000_000_000)
	writeFrameForCLI(t, d, "s@main", "in", "hello", `{"type":"hello","i":1}`, t0)
	writeFrameForCLI(t, d, "s@main", "out", "hello_ack", `{"type":"hello_ack","i":2}`, t0.Add(1*time.Millisecond))
	writeFrameForCLI(t, d, "s@main", "in", "tool_call", `{"type":"tool_call","i":3}`, t0.Add(2*time.Millisecond))

	var buf bytes.Buffer
	if err := runHarnessEvents("s@main", "", "", false, deliverSink{kind:"stdout"}, &buf); err != nil {
		t.Fatalf("runHarnessEvents: %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3:\n%s", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], `"i":1`) ||
		!strings.Contains(lines[1], `"i":2`) ||
		!strings.Contains(lines[2], `"i":3`) {
		t.Errorf("lines not in chronological order:\n%s", buf.String())
	}
}

// TestRunHarnessEvents_DirectionFilter covers the --direction in/out AC.
func TestRunHarnessEvents_DirectionFilter(t *testing.T) {
	d := setUpHarnessFramesDB(t)

	t0 := time.UnixMilli(1_700_000_000_000)
	writeFrameForCLI(t, d, "s@main", "in", "hello", `{"d":"in1"}`, t0)
	writeFrameForCLI(t, d, "s@main", "out", "hello_ack", `{"d":"out1"}`, t0.Add(1*time.Millisecond))
	writeFrameForCLI(t, d, "s@main", "in", "tool_call", `{"d":"in2"}`, t0.Add(2*time.Millisecond))

	var inBuf bytes.Buffer
	if err := runHarnessEvents("s@main", "in", "", false, deliverSink{kind:"stdout"}, &inBuf); err != nil {
		t.Fatalf("runHarnessEvents in: %v", err)
	}
	if !strings.Contains(inBuf.String(), "in1") || !strings.Contains(inBuf.String(), "in2") {
		t.Errorf("inbound output missing in1/in2:\n%s", inBuf.String())
	}
	if strings.Contains(inBuf.String(), "out1") {
		t.Errorf("inbound output unexpectedly contains out1:\n%s", inBuf.String())
	}

	var outBuf bytes.Buffer
	if err := runHarnessEvents("s@main", "out", "", false, deliverSink{kind:"stdout"}, &outBuf); err != nil {
		t.Fatalf("runHarnessEvents out: %v", err)
	}
	if !strings.Contains(outBuf.String(), "out1") {
		t.Errorf("outbound output missing out1:\n%s", outBuf.String())
	}
	if strings.Contains(outBuf.String(), "in1") || strings.Contains(outBuf.String(), "in2") {
		t.Errorf("outbound output should not contain inbound frames:\n%s", outBuf.String())
	}
}

func TestRunHarnessEvents_DirectionInvalid(t *testing.T) {
	setUpHarnessFramesDB(t)
	var buf bytes.Buffer
	err := runHarnessEvents("s@main", "sideways", "", false, deliverSink{kind:"stdout"}, &buf)
	if err == nil {
		t.Fatal("expected error for invalid direction")
	}
	if !strings.Contains(err.Error(), "--direction") {
		t.Errorf("err = %v; want a --direction message", err)
	}
}

// TestRunHarnessEvents_TypesFilter covers the --types AC.
func TestRunHarnessEvents_TypesFilter(t *testing.T) {
	d := setUpHarnessFramesDB(t)

	t0 := time.UnixMilli(1_700_000_000_000)
	writeFrameForCLI(t, d, "s@main", "in", "hello", `{"t":"hello"}`, t0)
	writeFrameForCLI(t, d, "s@main", "in", "tool_call", `{"t":"tool_call"}`, t0.Add(1*time.Millisecond))
	writeFrameForCLI(t, d, "s@main", "in", "tool_result", `{"t":"tool_result"}`, t0.Add(2*time.Millisecond))
	writeFrameForCLI(t, d, "s@main", "in", "msg_assistant", `{"t":"msg_assistant"}`, t0.Add(3*time.Millisecond))

	var buf bytes.Buffer
	if err := runHarnessEvents("s@main", "", "tool_call,tool_result", false, deliverSink{kind:"stdout"}, &buf); err != nil {
		t.Fatalf("runHarnessEvents: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"tool_call"`) || !strings.Contains(out, `"tool_result"`) {
		t.Errorf("output missing tool_call/tool_result:\n%s", out)
	}
	if strings.Contains(out, `"hello"`) || strings.Contains(out, `"msg_assistant"`) {
		t.Errorf("output unexpectedly contains filtered-out types:\n%s", out)
	}
}

// syncBuf is a goroutine-safe bytes.Buffer used by the --follow test.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *syncBuf) waitForContains(needle string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if strings.Contains(s.String(), needle) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %q; got:\n%s", needle, s.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRunHarnessEvents_FollowStreamsNewFrames covers the --follow AC: frames
// added to the DB after the call begins are streamed live.
func TestRunHarnessEvents_FollowStreamsNewFrames(t *testing.T) {
	d := setUpHarnessFramesDB(t)

	t0 := time.UnixMilli(1_700_000_000_000)
	writeFrameForCLI(t, d, "s@main", "in", "hello", `{"f":1}`, t0)

	sb := &syncBuf{}

	done := make(chan error, 1)
	go func() {
		done <- runHarnessEvents("s@main", "", "", true, deliverSink{kind:"stdout"}, sb)
	}()

	// Wait for the initial frame to appear.
	if err := sb.waitForContains(`"f":1`, 2*time.Second); err != nil {
		t.Fatalf("initial frame did not appear: %v", err)
	}

	// Inject a new frame; --follow polls every 500ms so allow up to ~1.5s
	// plus generous slack.
	writeFrameForCLI(t, d, "s@main", "in", "tool_call", `{"f":2}`, time.Now())
	if err := sb.waitForContains(`"f":2`, 3*time.Second); err != nil {
		t.Fatalf("follow did not pick up new frame: %v", err)
	}

	// Cancel the follow loop by sending SIGINT to ourselves.
	if p, perr := os.FindProcess(os.Getpid()); perr == nil {
		_ = p.Signal(syscall.SIGINT)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runHarnessEvents (follow) returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runHarnessEvents (follow) did not exit after SIGINT")
	}
}

func TestParseTypesCSV(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{",", nil},
		{"  ", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{" tool_call , tool_result ", []string{"tool_call", "tool_result"}},
		{"a,,b", []string{"a", "b"}},
	}
	for _, c := range cases {
		got := parseTypesCSV(c.in)
		if len(got) != len(c.want) {
			t.Errorf("parseTypesCSV(%q) = %v; want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseTypesCSV(%q)[%d] = %q; want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}
