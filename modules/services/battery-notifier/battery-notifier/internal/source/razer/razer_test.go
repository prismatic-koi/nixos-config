package razer

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prismatic-koi/battery-notifier/internal/state"
)

func TestScaleLevel(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, 0},
		{255, 100},
		{128, 50},
		{127, 50}, // 127*100+127 = 12827; /255 = 50
		{1, 0},    // 1*100+127 = 227; /255 = 0
		{-1, 0},   // clamped
		{500, 100},
	}
	for _, c := range cases {
		if got := ScaleLevel(c.in); got != c.want {
			t.Errorf("ScaleLevel(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// fixtureRoot builds a sysfs-shaped fake under t.TempDir() and
// returns its path. With level=255 and status=1 the resulting
// Sample should be Level=100, Status=Charging.
func fixtureRoot(t *testing.T, level, status int) string {
	t.Helper()
	root := t.TempDir()
	devDir := filepath.Join(root, "0003:1532:0084.0001")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "charge_level"),
		[]byte("128\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "charge_status"),
		[]byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Apply requested values.
	if err := os.WriteFile(filepath.Join(devDir, "charge_level"),
		[]byte(itoa(level)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "charge_status"),
		[]byte(itoa(status)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// itoa avoids strconv import here.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func TestReadOnce_PresentCharging(t *testing.T) {
	root := fixtureRoot(t, 255, 1)
	r := New("mouse", slog.New(slog.NewTextHandler(io.Discard, nil))).WithRoot(root)
	s, err := r.readOnce()
	if err != nil {
		t.Fatalf("readOnce: %v", err)
	}
	if !s.Present {
		t.Errorf("Present=false, want true")
	}
	if s.Level != 100 {
		t.Errorf("Level=%d, want 100", s.Level)
	}
	if s.Status != state.StatusCharging {
		t.Errorf("Status=%v, want Charging", s.Status)
	}
}

func TestReadOnce_PresentDischarging(t *testing.T) {
	root := fixtureRoot(t, 128, 0)
	r := New("mouse", slog.New(slog.NewTextHandler(io.Discard, nil))).WithRoot(root)
	s, err := r.readOnce()
	if err != nil {
		t.Fatalf("readOnce: %v", err)
	}
	if s.Level != 50 {
		t.Errorf("Level=%d, want 50", s.Level)
	}
	if s.Status != state.StatusDischarging {
		t.Errorf("Status=%v, want Discharging", s.Status)
	}
}

func TestReadOnce_AbsentWhenNoMatchingDir(t *testing.T) {
	root := t.TempDir()
	// No 1532 dir at all.
	r := New("mouse", slog.New(slog.NewTextHandler(io.Discard, nil))).WithRoot(root)
	_, err := r.readOnce()
	if err == nil {
		t.Fatalf("readOnce: want fs.ErrNotExist, got nil")
	}
	if !os.IsNotExist(err) {
		t.Errorf("expected ErrNotExist-style error, got %v", err)
	}
}

func TestReadOnce_AbsentWhenRootMissing(t *testing.T) {
	r := New("mouse", slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithRoot(filepath.Join(t.TempDir(), "does-not-exist"))
	_, err := r.readOnce()
	if !os.IsNotExist(err) {
		t.Errorf("expected ErrNotExist when root missing, got %v", err)
	}
}

func TestRun_EmitsInitialSampleAndTicks(t *testing.T) {
	root := fixtureRoot(t, 255, 1)
	r := New("mouse", slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithRoot(root).
		WithInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan state.Sample, 4)

	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, out) }()

	// First sample (initial).
	select {
	case s := <-out:
		if s.Level != 100 || s.Status != state.StatusCharging {
			t.Errorf("initial sample %+v, want Level=100 Charging", s)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for initial sample")
	}

	// At least one ticked sample.
	select {
	case <-out:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ticked sample")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRun_EmitsAbsentWhenDeviceMissing(t *testing.T) {
	root := t.TempDir() // no 1532 dir
	r := New("mouse", slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithRoot(root).
		WithInterval(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan state.Sample, 4)
	go r.Run(ctx, out)

	select {
	case s := <-out:
		if s.Present {
			t.Errorf("expected Present=false when device missing, got %+v", s)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for absent sample")
	}
}
