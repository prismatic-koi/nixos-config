// Package daemon wires Sources → state.Machine → Notifier for every
// configured device and runs them until signalled to stop.
//
// One goroutine per device drains its Source channel. The state
// machine for that device is owned by the goroutine so no mutex is
// needed. The Notifier is shared across goroutines and is itself
// thread-safe via godbus.
//
// Debounce strategy. The state machine treats *every* sample as
// authoritative; rapid Charging↔Discharging flips would, without
// debouncing, produce an Action sequence per flip. The daemon
// debounces *only* the flicker signature — a status change that
// would revert to the previously-applied status within the debounce
// window (e.g. Charging→Discharging→Charging within 3s on a flaky
// AC cable). Monotonic status progressions (Discharging→Charging→
// Full) are applied immediately so legitimate transitions are not
// artificially delayed. Level-only changes (no status change) are
// also applied immediately.
//
// Concretely: when an incoming sample has the same status as
// `lastApplied` the pending sample is dropped (it would be a no-op
// transition anyway, since the state machine sees no change). When
// an incoming sample's status differs from `lastApplied`, it is
// applied immediately *and* `lastApplied` is updated — but a copy
// is stashed as `flickerCandidate` for the duration of one debounce
// window. If, during that window, another sample arrives that would
// revert the status back to `flickerCandidate`'s prior value, the
// daemon drops it. After the window elapses the candidate is
// cleared. This gives at most one Apply per status change even on
// a 4-fold flicker burst, while never delaying a real transition.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prismatic-koi/battery-notifier/internal/config"
	"github.com/prismatic-koi/battery-notifier/internal/notify"
	"github.com/prismatic-koi/battery-notifier/internal/source"
	"github.com/prismatic-koi/battery-notifier/internal/state"
)

// DefaultDebounce is the coalesce window for status flips. 3 seconds
// is long enough to absorb a flaky AC cable bounce but short enough
// to feel responsive when the user genuinely plugs in. Overridable
// via Options for tests.
const DefaultDebounce = 3 * time.Second

// Options groups the runtime knobs. Zero values pick reasonable
// defaults via withDefaults.
type Options struct {
	Logger   *slog.Logger
	Debounce time.Duration
	// AppName is sent in the "app_name" field of every Notify call.
	AppName string
}

func (o Options) withDefaults() Options {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Debounce <= 0 {
		o.Debounce = DefaultDebounce
	}
	if o.AppName == "" {
		o.AppName = "battery-notifier"
	}
	return o
}

// Daemon owns the runtime wiring.
type Daemon struct {
	opts     Options
	notifier notify.Notifier
}

// New constructs a Daemon. The notifier is injected so tests can
// supply a fake.
func New(notifier notify.Notifier, opts Options) *Daemon {
	return &Daemon{opts: opts.withDefaults(), notifier: notifier}
}

// Run starts one goroutine per device and blocks until ctx is
// cancelled.
func (d *Daemon) Run(ctx context.Context, devices []config.Device, sources []source.Source) error {
	if len(devices) != len(sources) {
		return fmt.Errorf("daemon: %d devices but %d sources", len(devices), len(sources))
	}

	var wg sync.WaitGroup
	for i := range devices {
		wg.Add(1)
		go func(dev config.Device, src source.Source) {
			defer wg.Done()
			d.runDevice(ctx, dev, src)
		}(devices[i], sources[i])
	}
	wg.Wait()
	return nil
}

// runDevice owns the state machine and notification cookies for one
// device. It exits when its source's channel closes.
func (d *Daemon) runDevice(ctx context.Context, dev config.Device, src source.Source) {
	log := d.opts.Logger.With("device", dev.Name)
	m := state.New(dev.LowThreshold, dev.FullThreshold, dev.DismissThreshold, dev.IgnoreZero)

	samples := make(chan state.Sample, 8)
	sourceDone := make(chan error, 1)
	go func() { sourceDone <- src.Run(ctx, samples) }()

	// Notification cookies, owned by this device goroutine.
	var lowID uint32
	var fullID uint32

	// Flicker suppression. When the daemon applies a status change,
	// it stashes the *previous* status as `revertedFromStatus` and
	// arms a timer for the debounce window. If, during the window,
	// a sample arrives that would change status back to
	// `revertedFromStatus`, the daemon drops it (a flaky-cable bounce).
	// When the timer fires, the candidate is cleared and any further
	// status change applies normally.
	var revertedFromStatus state.Status = state.StatusUnknown
	var revertedFromValid bool
	var lastApplied state.Sample
	var lastAppliedInit bool

	var debounceTimer *time.Timer
	stopTimer := func() {
		if debounceTimer != nil {
			debounceTimer.Stop()
			debounceTimer = nil
		}
		revertedFromValid = false
	}

	applySample := func(s state.Sample) {
		actions, trans := m.Apply(s)
		if trans.Changed() {
			log.Info("status transition",
				"event", "status_transition",
				"from", trans.From.String(),
				"to", trans.To.String(),
				"level", s.Level)
		}
		for _, a := range actions {
			d.handleAction(log, dev, a, &lowID, &fullID)
		}
		if s.Present {
			lastApplied = s
			lastAppliedInit = true
		}
	}

	for {
		var debounceC <-chan time.Time
		if debounceTimer != nil {
			debounceC = debounceTimer.C
		}
		select {
		case <-ctx.Done():
			stopTimer()
			<-sourceDone
			return
		case err := <-sourceDone:
			stopTimer()
			if err != nil && ctx.Err() == nil {
				log.Error("source exited",
					"event", "source_exit", "err", err)
			}
			return
		case <-debounceC:
			debounceTimer = nil
			revertedFromValid = false
		case s, ok := <-samples:
			if !ok {
				stopTimer()
				return
			}
			// Flicker suppression: if we are inside a debounce
			// window and this sample's status equals the status
			// we just left, treat it as a bounce and drop it.
			if revertedFromValid && s.Present && s.Status == revertedFromStatus {
				log.Debug("flicker suppressed",
					"event", "flicker_suppressed",
					"status", s.Status.String(),
					"level", s.Level)
				continue
			}
			// Status change — arm a new flicker window using the
			// status we are about to leave.
			if lastAppliedInit && s.Present && s.Status != state.StatusUnknown &&
				lastApplied.Status != s.Status {
				revertedFromStatus = lastApplied.Status
				revertedFromValid = true
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				debounceTimer = time.NewTimer(d.opts.Debounce)
			}
			applySample(s)
		}
	}
}

// handleAction issues the notifier calls for one Action and updates
// the device's notification cookies.
func (d *Daemon) handleAction(
	log *slog.Logger,
	dev config.Device,
	a state.Action,
	lowID, fullID *uint32,
) {
	switch a.Kind {
	case state.ActionNotifyLow:
		title := fmt.Sprintf("%s Battery Low", titleCase(dev.Name))
		body := fmt.Sprintf("Level: %d%%.", a.Level)
		icon := "battery-caution"
		if a.Level*2 <= dev.LowThreshold {
			icon = "battery-empty"
		}
		switch a.Status {
		case state.StatusCharging, state.StatusFull:
			body += " Plugged in."
			icon = "battery-low-charging"
		default:
			body += " Please plug in."
		}
		id, err := d.notifier.Notify(notify.Notification{
			AppName:       d.opts.AppName,
			ReplacesID:    *lowID,
			Icon:          icon,
			Summary:       title,
			Body:          body,
			Urgency:       notify.UrgencyCritical,
			ExpireTimeout: -1,
		})
		if err != nil {
			log.Warn("notify low failed",
				"event", "notify_failed",
				"notification", "low",
				"err", err)
			return
		}
		*lowID = id
		log.Info("notification sent",
			"event", "notification_sent",
			"notification", "low",
			"level", a.Level,
			"status", a.Status.String())

	case state.ActionNotifyFull:
		title := fmt.Sprintf("%s Fully Charged", titleCase(dev.Name))
		body := fmt.Sprintf("Level: %d%%. You can unplug.", a.Level)
		id, err := d.notifier.Notify(notify.Notification{
			AppName:       d.opts.AppName,
			ReplacesID:    0, // full is always a fresh notification
			Icon:          "battery-full-charged",
			Summary:       title,
			Body:          body,
			Urgency:       notify.UrgencyNormal,
			ExpireTimeout: -1,
		})
		if err != nil {
			log.Warn("notify full failed",
				"event", "notify_failed",
				"notification", "full",
				"err", err)
			return
		}
		*fullID = id
		log.Info("notification sent",
			"event", "notification_sent",
			"notification", "full",
			"level", a.Level,
			"status", a.Status.String())

	case state.ActionCloseLow:
		id := *lowID
		if err := d.notifier.Close(id); err != nil {
			log.Warn("close low failed",
				"event", "close_failed",
				"notification", "low",
				"err", err)
		}
		*lowID = 0
		log.Info("notification dismissed",
			"event", "notification_dismissed",
			"notification", "low",
			"level", a.Level,
			"status", a.Status.String())

	case state.ActionCloseFull:
		id := *fullID
		if err := d.notifier.Close(id); err != nil {
			log.Warn("close full failed",
				"event", "close_failed",
				"notification", "full",
				"err", err)
		}
		*fullID = 0
		log.Info("notification dismissed",
			"event", "notification_dismissed",
			"notification", "full",
			"level", a.Level,
			"status", a.Status.String())

	case state.ActionNone:
		// Nothing to do.
	}
}

// titleCase upper-cases the first rune of name (e.g. "laptop" →
// "Laptop"). ASCII-only; the device names in our config are
// ASCII identifiers, so we don't pull in golang.org/x/text/cases for
// one upper-case operation.
func titleCase(name string) string {
	if name == "" {
		return ""
	}
	b := []byte(name)
	if b[0] >= 'a' && b[0] <= 'z' {
		b[0] -= 'a' - 'A'
	}
	return string(b)
}
