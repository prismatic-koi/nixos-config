package proglog

import (
	"bytes"
	"sync"
	"testing"
)

// resetForTest restores proglog to a pristine state so each test can pin a
// fresh effective level and capture writes into a bytes.Buffer. It exists
// only in test builds.
//
// The sync.Once on the production path means the env-var read happens at
// most once per process; without this reset, the first test would fix the
// cached level for every subsequent test in the run. Tests that need the
// real env-var read path use t.Setenv before calling this helper.
func resetForTest(t *testing.T, w *bytes.Buffer) {
	t.Helper()
	writerMu.Lock()
	defer writerMu.Unlock()
	once = sync.Once{}
	cached = LevelError
	writer = w
}

func TestParseLevel(t *testing.T) {
	cases := []struct {
		in      string
		want    Level
		wantOK  bool
	}{
		{"error", LevelError, true},
		{"ERROR", LevelError, true},
		{"Error", LevelError, true},
		{"warn", LevelWarn, true},
		{"WARN", LevelWarn, true},
		{"warning", LevelWarn, true},
		{"info", LevelInfo, true},
		{"INFO", LevelInfo, true},
		{"debug", LevelDebug, true},
		{"DEBUG", LevelDebug, true},
		{"  debug  ", LevelDebug, true},
		{"", LevelError, false},
		{"trace", LevelError, false},
		{"verbose", LevelError, false},
		{"nonsense", LevelError, false},
	}
	for _, tc := range cases {
		got, ok := ParseLevel(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("ParseLevel(%q) = (%d, %v), want (%d, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

// TestEffectiveDefaults verifies that an unset PRISM_LOG_LEVEL caches LevelError.
func TestEffectiveDefaults(t *testing.T) {
	t.Setenv("PRISM_LOG_LEVEL", "")
	var buf bytes.Buffer
	resetForTest(t, &buf)
	if got := effective(); got != LevelError {
		t.Errorf("effective() with unset env = %d, want %d", got, LevelError)
	}
}

// TestEffectiveCached verifies the env var is read exactly once.
func TestEffectiveCached(t *testing.T) {
	t.Setenv("PRISM_LOG_LEVEL", "debug")
	var buf bytes.Buffer
	resetForTest(t, &buf)
	if got := effective(); got != LevelDebug {
		t.Fatalf("first call effective() = %d, want %d", got, LevelDebug)
	}
	// Mutating the env var after the first call must NOT change the cached value.
	t.Setenv("PRISM_LOG_LEVEL", "error")
	if got := effective(); got != LevelDebug {
		t.Errorf("after env change effective() = %d, want %d (cached)", got, LevelDebug)
	}
}

// TestEmitFilter verifies that each level value gates the four functions
// correctly. For each scenario we run all four emit functions and check
// which of them actually wrote to the captured buffer.
func TestEmitFilter(t *testing.T) {
	type emitFlags struct {
		err, warn, info, debug bool
	}
	cases := []struct {
		envVal string
		want   emitFlags
	}{
		{"", emitFlags{err: true}},                                                       // unset → error
		{"trace", emitFlags{err: true}},                                                  // unrecognised → error
		{"error", emitFlags{err: true}},                                                  // explicit error
		{"warn", emitFlags{err: true, warn: true}},                                       // warn
		{"WARN", emitFlags{err: true, warn: true}},                                       // case-insensitive
		{"info", emitFlags{err: true, warn: true, info: true}},                           // info
		{"debug", emitFlags{err: true, warn: true, info: true, debug: true}},             // debug
	}
	for _, tc := range cases {
		t.Run(tc.envVal, func(t *testing.T) {
			t.Setenv("PRISM_LOG_LEVEL", tc.envVal)
			var buf bytes.Buffer
			resetForTest(t, &buf)

			Errorf("E:%s\n", "x")
			Warnf("W:%s\n", "x")
			Infof("I:%s\n", "x")
			Debugf("D:%s\n", "x")

			out := buf.String()
			got := emitFlags{
				err:   bytes.Contains([]byte(out), []byte("E:x\n")),
				warn:  bytes.Contains([]byte(out), []byte("W:x\n")),
				info:  bytes.Contains([]byte(out), []byte("I:x\n")),
				debug: bytes.Contains([]byte(out), []byte("D:x\n")),
			}
			if got != tc.want {
				t.Errorf("env=%q: got %+v, want %+v (buffer: %q)", tc.envVal, got, tc.want, out)
			}
		})
	}
}

// TestFormatStringPreserved confirms the helper passes format+args through
// to fmt.Fprintf verbatim (no prefix, no timestamp, no trailing additions).
func TestFormatStringPreserved(t *testing.T) {
	t.Setenv("PRISM_LOG_LEVEL", "debug")
	var buf bytes.Buffer
	resetForTest(t, &buf)

	Debugf("[timing] %s: %dms\n", "pre-exec", 5)
	got := buf.String()
	want := "[timing] pre-exec: 5ms\n"
	if got != want {
		t.Errorf("Debugf output = %q, want %q", got, want)
	}
}
