package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prismatic-koi/battery-notifier/internal/config"
	"github.com/prismatic-koi/battery-notifier/internal/notify"
	"github.com/prismatic-koi/battery-notifier/internal/source"
	"github.com/prismatic-koi/battery-notifier/internal/state"
)

// fakeNotifier records every call for assertion in tests.
type fakeNotifier struct {
	mu       sync.Mutex
	notify   []notify.Notification
	closed   []uint32
	nextID   uint32
	failOnce bool
}

func (f *fakeNotifier) Notify(n notify.Notification) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOnce {
		f.failOnce = false
		return 0, errors.New("fake: forced failure")
	}
	f.notify = append(f.notify, n)
	f.nextID++
	return f.nextID, nil
}

func (f *fakeNotifier) Close(id uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, id)
	return nil
}

func (f *fakeNotifier) snapshot() ([]notify.Notification, []uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]notify.Notification(nil), f.notify...),
		append([]uint32(nil), f.closed...)
}

// scriptedSource emits a scripted slice of samples then blocks until
// ctx is cancelled. Used as the synchronous test fake.
type scriptedSource struct {
	name    string
	samples []state.Sample
}

func (s *scriptedSource) Name() string { return s.name }
func (s *scriptedSource) Run(ctx context.Context, out chan<- state.Sample) error {
	defer close(out)
	for _, sm := range s.samples {
		select {
		case out <- sm:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

// channelSource lets tests inject samples one at a time, with manual
// timing control between samples.
type channelSource struct {
	name string
	ch   chan state.Sample
}

func (c *channelSource) Name() string { return c.name }
func (c *channelSource) Run(ctx context.Context, out chan<- state.Sample) error {
	defer close(out)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case s, ok := <-c.ch:
			if !ok {
				return nil
			}
			select {
			case out <- s:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

func newTestDaemon(fn *fakeNotifier, debounce time.Duration) *Daemon {
	return New(fn, Options{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Debounce: debounce,
		AppName:  "test",
	})
}

func waitUntil(t *testing.T, fn func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("waitUntil timed out: %s", msg)
}

func runDaemon(t *testing.T, d *Daemon, dev config.Device, src source.Source) (context.CancelFunc, *sync.WaitGroup) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = d.Run(ctx, []config.Device{dev}, []source.Source{src})
	}()
	return cancel, wg
}

func defaultLaptop() config.Device {
	return config.Device{
		Name: "laptop", Kind: config.KindLaptop,
		LowThreshold: 20, FullThreshold: 100, DismissThreshold: 50,
	}
}

func TestDaemon_LowNotificationUsesReplacesID(t *testing.T) {
	fn := &fakeNotifier{}
	d := newTestDaemon(fn, 1*time.Millisecond)
	src := &scriptedSource{name: "laptop", samples: []state.Sample{
		{Level: 15, Status: state.StatusDischarging, Present: true},
		{Level: 10, Status: state.StatusDischarging, Present: true},
		{Level: 5, Status: state.StatusDischarging, Present: true},
	}}

	cancel, wg := runDaemon(t, d, defaultLaptop(), src)
	waitUntil(t, func() bool {
		n, _ := fn.snapshot()
		return len(n) >= 3
	}, 2*time.Second, "expected 3 notify calls")
	cancel()
	wg.Wait()

	n, _ := fn.snapshot()
	if len(n) < 3 {
		t.Fatalf("expected ≥3 notify calls, got %d", len(n))
	}
	if n[0].ReplacesID != 0 {
		t.Errorf("first notify ReplacesID = %d, want 0", n[0].ReplacesID)
	}
	// Each successful Notify returns a fresh cookie from the fake
	// (nextID++); the daemon must thread that cookie into the next
	// call's ReplacesID so the bubble updates in place.
	if n[1].ReplacesID != 1 {
		t.Errorf("second notify ReplacesID = %d, want 1 (cookie from first call)",
			n[1].ReplacesID)
	}
	if n[2].ReplacesID != 2 {
		t.Errorf("third notify ReplacesID = %d, want 2 (cookie from second call)",
			n[2].ReplacesID)
	}
}

func TestDaemon_DischargingToChargingResetsFullFlag(t *testing.T) {
	fn := &fakeNotifier{}
	d := newTestDaemon(fn, 1*time.Millisecond)
	// Discharge → low → plug in → full → unplug → discharge → plug in → full.
	src := &scriptedSource{name: "laptop", samples: []state.Sample{
		{Level: 15, Status: state.StatusDischarging, Present: true},
		{Level: 50, Status: state.StatusCharging, Present: true},
		{Level: 100, Status: state.StatusFull, Present: true},
		{Level: 99, Status: state.StatusDischarging, Present: true},
		{Level: 70, Status: state.StatusDischarging, Present: true},
		{Level: 70, Status: state.StatusCharging, Present: true},
		{Level: 100, Status: state.StatusFull, Present: true},
	}}

	cancel, wg := runDaemon(t, d, defaultLaptop(), src)
	waitUntil(t, func() bool {
		n, c := fn.snapshot()
		return len(n) >= 3 && len(c) >= 2
	}, 3*time.Second, "expected 3 notify + 2 close calls")
	cancel()
	wg.Wait()

	n, _ := fn.snapshot()
	fullCount := 0
	for _, x := range n {
		if x.Icon == "battery-full-charged" {
			fullCount++
		}
	}
	if fullCount != 2 {
		t.Errorf("expected 2 full notifications under new semantics, got %d", fullCount)
	}
}

func TestDaemon_NotifyFailureIsNonFatal(t *testing.T) {
	fn := &fakeNotifier{failOnce: true}
	d := newTestDaemon(fn, 1*time.Millisecond)
	src := &scriptedSource{name: "laptop", samples: []state.Sample{
		{Level: 15, Status: state.StatusDischarging, Present: true},
		{Level: 10, Status: state.StatusDischarging, Present: true},
	}}

	cancel, wg := runDaemon(t, d, defaultLaptop(), src)
	waitUntil(t, func() bool {
		n, _ := fn.snapshot()
		return len(n) >= 1
	}, 2*time.Second, "expected ≥1 successful notify after retry")
	cancel()
	wg.Wait()

	n, _ := fn.snapshot()
	if len(n) < 1 {
		t.Fatalf("expected ≥1 successful notify, got %d", len(n))
	}
	// The failed first call didn't update *lowID, so the retry
	// asks for a fresh cookie (ReplacesID=0).
	if n[0].ReplacesID != 0 {
		t.Errorf("retried notify should have ReplacesID=0, got %d", n[0].ReplacesID)
	}
}

func TestDaemon_AbsentSampleDoesNotNotify(t *testing.T) {
	fn := &fakeNotifier{}
	d := newTestDaemon(fn, 1*time.Millisecond)
	src := &scriptedSource{name: "mouse", samples: []state.Sample{
		{Present: false},
		{Present: false},
	}}

	cancel, wg := runDaemon(t, d, config.Device{
		Name: "mouse", Kind: config.KindRazer,
		LowThreshold: 20, FullThreshold: 100, DismissThreshold: 50,
		IgnoreZero: true,
	}, src)
	time.Sleep(50 * time.Millisecond)
	cancel()
	wg.Wait()

	n, c := fn.snapshot()
	if len(n) != 0 || len(c) != 0 {
		t.Errorf("absent samples produced calls: notify=%d close=%d", len(n), len(c))
	}
}

func TestDaemon_IgnoreZeroDoesNotNotify(t *testing.T) {
	// AC edge case: ignoreZero=true → a 0% reading does not notify
	// and does not corrupt "last level" so subsequent real readings
	// still behave correctly.
	fn := &fakeNotifier{}
	d := newTestDaemon(fn, 1*time.Millisecond)
	src := &scriptedSource{name: "mouse", samples: []state.Sample{
		{Level: 80, Status: state.StatusDischarging, Present: true},
		{Level: 0, Status: state.StatusDischarging, Present: true},
		{Level: 80, Status: state.StatusDischarging, Present: true},
	}}

	cancel, wg := runDaemon(t, d, config.Device{
		Name: "mouse", Kind: config.KindRazer,
		LowThreshold: 20, FullThreshold: 100, DismissThreshold: 50,
		IgnoreZero: true,
	}, src)
	time.Sleep(80 * time.Millisecond)
	cancel()
	wg.Wait()

	n, _ := fn.snapshot()
	if len(n) != 0 {
		t.Errorf("ignoreZero should suppress 0%% notification, got %d notify calls", len(n))
	}
}

func TestDaemon_DebounceCoalescesFlips(t *testing.T) {
	// AC edge case: rapid Charging↔Discharging flips coalesce to a
	// single Apply, not one per flip.
	fn := &fakeNotifier{}
	d := newTestDaemon(fn, 50*time.Millisecond)

	srcCh := make(chan state.Sample, 16)
	src := &channelSource{name: "laptop", ch: srcCh}
	cancel, wg := runDaemon(t, d, defaultLaptop(), src)

	// Stable initial state at 15% Discharging — applied immediately
	// (no prior state, no flip).
	srcCh <- state.Sample{Level: 15, Status: state.StatusDischarging, Present: true}
	waitUntil(t, func() bool {
		n, _ := fn.snapshot()
		return len(n) >= 1
	}, time.Second, "initial low notify never landed")

	beforeFlips, _ := fn.snapshot()

	// Rapid 4 flips within the debounce window. None of these
	// individually should trigger Apply.
	srcCh <- state.Sample{Level: 15, Status: state.StatusCharging, Present: true}
	srcCh <- state.Sample{Level: 15, Status: state.StatusDischarging, Present: true}
	srcCh <- state.Sample{Level: 15, Status: state.StatusCharging, Present: true}
	srcCh <- state.Sample{Level: 15, Status: state.StatusDischarging, Present: true}

	// Sleep > debounce window for the timer to fire and apply
	// only the final pending sample.
	time.Sleep(150 * time.Millisecond)

	close(srcCh)
	cancel()
	wg.Wait()

	afterFlips, _ := fn.snapshot()
	addedDuringFlips := len(afterFlips) - len(beforeFlips)
	if addedDuringFlips > 1 {
		t.Errorf("debounce coalescing failed: got %d extra notify calls during 4 flips, want ≤ 1",
			addedDuringFlips)
	}
}

func TestDaemon_FullDismissOnUnplug(t *testing.T) {
	fn := &fakeNotifier{}
	d := newTestDaemon(fn, 30*time.Millisecond)

	srcCh := make(chan state.Sample, 16)
	src := &channelSource{name: "laptop", ch: srcCh}
	cancel, wg := runDaemon(t, d, defaultLaptop(), src)

	srcCh <- state.Sample{Level: 90, Status: state.StatusCharging, Present: true}
	srcCh <- state.Sample{Level: 100, Status: state.StatusFull, Present: true}
	waitUntil(t, func() bool {
		n, _ := fn.snapshot()
		for _, x := range n {
			if x.Icon == "battery-full-charged" {
				return true
			}
		}
		return false
	}, 2*time.Second, "full notify never landed")

	// Charging→Full is not a flip (Charging→Full is a status change but
	// we don't treat Full as a flip target in the same flaky-cable
	// sense; the debouncer treats any inequality as a flip). Wait
	// the debounce window before the unplug so Full→Discharging is
	// applied.
	srcCh <- state.Sample{Level: 99, Status: state.StatusDischarging, Present: true}
	time.Sleep(100 * time.Millisecond)

	close(srcCh)
	cancel()
	wg.Wait()

	_, c := fn.snapshot()
	if len(c) < 1 {
		t.Errorf("expected ≥1 Close call (full dismiss), got %d", len(c))
	}
}

func TestDaemon_LowDismissOnRise(t *testing.T) {
	// AC: low → rise to dismiss → CloseNotification.
	fn := &fakeNotifier{}
	d := newTestDaemon(fn, 1*time.Millisecond)
	src := &scriptedSource{name: "laptop", samples: []state.Sample{
		{Level: 15, Status: state.StatusDischarging, Present: true},
		{Level: 60, Status: state.StatusCharging, Present: true},
	}}

	cancel, wg := runDaemon(t, d, defaultLaptop(), src)
	waitUntil(t, func() bool {
		_, c := fn.snapshot()
		return len(c) >= 1
	}, 2*time.Second, "expected a close call after rise above dismissThreshold")
	cancel()
	wg.Wait()
}
