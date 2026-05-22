// Package state implements the per-device notification state machine.
//
// The state machine is intentionally pure: it owns no D-Bus client, no
// sysfs handle, no timer. Inputs are battery samples (Sample), outputs
// are notification actions (Action). The daemon wires actions to a
// Notifier and samples from a Source. Tests can drive Apply directly.
//
// Semantics summary:
//   - Discharging + level <= lowThreshold → emit Low (or re-emit on
//     status change, to update the body text when plugging in).
//   - Low notification is active and level >= dismissThreshold → close
//     the low notification.
//   - Charging + level >= fullThreshold and we have not already
//     notified this charge session → emit Full.
//   - Discharging→Charging transition resets the per-charge-session
//     "full already notified" flag. This replaces the old
//     reNotifyThreshold semantics (see DESIGN.md).
//   - Full notification is active and status flips to Discharging →
//     close the full notification.
package state

import "fmt"

// Status is the charging status reported by the source.
type Status int

const (
	// StatusUnknown is the zero value used before the first sample is
	// applied. It does not represent a real device state.
	StatusUnknown Status = iota
	StatusCharging
	StatusDischarging
	// StatusFull is reported by UPower when the battery is at 100% and
	// the cable is plugged in. We treat it as a charging variant for
	// dismissal purposes (a Full→Discharging transition still closes
	// the full notification), but it does NOT trigger the
	// "Discharging→Charging resets full flag" rule on its own — only a
	// genuine Charging status does.
	StatusFull
)

func (s Status) String() string {
	switch s {
	case StatusUnknown:
		return "Unknown"
	case StatusCharging:
		return "Charging"
	case StatusDischarging:
		return "Discharging"
	case StatusFull:
		return "Full"
	default:
		return fmt.Sprintf("Status(%d)", int(s))
	}
}

// isChargingLike returns true for Charging or Full — the two states in
// which a low-battery body text says "Plugged in."
func (s Status) isChargingLike() bool {
	return s == StatusCharging || s == StatusFull
}

// Sample is a single battery reading.
type Sample struct {
	// Level is 0..100. The mouse source is responsible for scaling its
	// 0..255 sysfs value before constructing a Sample.
	Level int
	// Status is the charging state at the moment of sampling.
	Status Status
	// Present indicates whether the device reading is valid. A false
	// Present (e.g. mouse asleep, sysfs path missing) suppresses any
	// state machine action — the previous state is retained. The
	// caller (Source) decides when to log the "absent" condition.
	Present bool
}

// ActionKind enumerates the side effects the state machine asks for.
type ActionKind int

const (
	// ActionNone means no notification activity is required.
	ActionNone ActionKind = iota
	// ActionNotifyLow asks the notifier to send (or update via
	// replaces_id) a low-battery notification.
	ActionNotifyLow
	// ActionNotifyFull asks the notifier to send a fully-charged
	// notification. We always pass replaces_id=0 here because the
	// full notification is a one-shot per charge session.
	ActionNotifyFull
	// ActionCloseLow asks the notifier to close the active low
	// notification via CloseNotification.
	ActionCloseLow
	// ActionCloseFull asks the notifier to close the active full
	// notification via CloseNotification.
	ActionCloseFull
)

func (a ActionKind) String() string {
	switch a {
	case ActionNone:
		return "none"
	case ActionNotifyLow:
		return "notify_low"
	case ActionNotifyFull:
		return "notify_full"
	case ActionCloseLow:
		return "close_low"
	case ActionCloseFull:
		return "close_full"
	default:
		return fmt.Sprintf("ActionKind(%d)", int(a))
	}
}

// Action is a state machine output. Multiple actions per Apply are
// possible (e.g. close-full + notify-low if the level dropped through
// the full→low band in a single update).
type Action struct {
	Kind   ActionKind
	Level  int
	Status Status
	// Critical is true for low notifications (mapped to
	// NotificationUrgencyCritical in the notifier). Full notifications
	// use a normal urgency. Used by the notifier layer; populated for
	// completeness on every action.
	Critical bool
}

// Machine is the per-device state machine. The zero value is ready to
// receive its first Apply.
type Machine struct {
	low              int
	full             int
	dismiss          int
	ignoreZero       bool
	lastStatus       Status
	lastLevel        int
	lowActive        bool
	fullActive       bool
	fullDoneThisSess bool
	// Initialised becomes true after the first Apply with Present=true.
	initialised bool
}

// New builds a Machine from the per-device config knobs.
func New(low, full, dismiss int, ignoreZero bool) *Machine {
	return &Machine{
		low:        low,
		full:       full,
		dismiss:    dismiss,
		ignoreZero: ignoreZero,
		lastStatus: StatusUnknown,
	}
}

// LastStatus returns the most recently observed Status. Useful for
// logging "no transition" cases.
func (m *Machine) LastStatus() Status { return m.lastStatus }

// LastLevel returns the most recently accepted level (post-ignoreZero
// filter). Zero before the first Present sample.
func (m *Machine) LastLevel() int { return m.lastLevel }

// Transition describes a status change observed during Apply. Both
// From and To are populated on every Apply (From == StatusUnknown on
// the very first sample). Use Changed() to detect a real transition.
type Transition struct {
	From Status
	To   Status
}

// Changed returns true when From and To differ and From was a known
// status. The first sample (From == StatusUnknown) is intentionally
// not considered a transition so that startup does not generate a
// spurious "status_transition" log line.
func (t Transition) Changed() bool {
	return t.From != StatusUnknown && t.From != t.To
}

// Apply consumes one Sample and returns the resulting actions plus the
// observed transition. Actions are returned in the order they should
// be executed: dismissals first, then new notifications. The caller
// should treat each action as idempotent to apply.
func (m *Machine) Apply(s Sample) ([]Action, Transition) {
	if !s.Present {
		// Absent reading: do nothing. The Source layer is
		// responsible for logging the absent condition once per
		// transition; the state machine intentionally preserves
		// its previous state so a brief absence does not corrupt
		// the lowActive/fullActive flags.
		return nil, Transition{From: m.lastStatus, To: m.lastStatus}
	}

	if m.ignoreZero && s.Level == 0 {
		// Drop the sample entirely — do not update lastLevel or
		// lastStatus. This protects against bogus 0% readings on
		// the Razer mouse (the documented motivation for
		// ignoreZero in the original Python module).
		return nil, Transition{From: m.lastStatus, To: m.lastStatus}
	}

	prevStatus := m.lastStatus
	trans := Transition{From: prevStatus, To: s.Status}

	var actions []Action

	// 1. Reset the "full done" latch on Discharging → Charging.
	// This is the new semantics that replaces reNotifyThreshold:
	// every charge session that subsequently reaches FullThreshold
	// gets one notification. StatusFull does NOT reset the latch
	// (a Full→Charging->Full cycle from the same plug-in event
	// should not double-notify).
	if prevStatus == StatusDischarging && s.Status == StatusCharging {
		m.fullDoneThisSess = false
	}

	// 2. Dismissal: full → discharging closes any active full
	// notification. We do this before evaluating new notifications
	// so a single Apply can close-full and immediately notify-low
	// if the level is already in the low band.
	if m.fullActive && s.Status == StatusDischarging {
		actions = append(actions, Action{
			Kind:   ActionCloseFull,
			Level:  s.Level,
			Status: s.Status,
		})
		m.fullActive = false
	}

	// 3. Dismissal: low notification when level rises to dismiss.
	if m.lowActive && s.Level >= m.dismiss {
		actions = append(actions, Action{
			Kind:   ActionCloseLow,
			Level:  s.Level,
			Status: s.Status,
		})
		m.lowActive = false
	}

	// 4. Notification creation.
	switch {
	case s.Level <= m.low:
		// Emit low if:
		//   - first time entering low (lowActive flipping from false to true), OR
		//   - still low and discharging (persistent reminders — re-emit so the
		//     notification stays at the top of the stack and the body text refreshes), OR
		//   - status changed (e.g. just unplugged while already low; the body
		//     should update from "Plugged in." to "Please plug in.").
		statusChanged := trans.Changed()
		shouldEmit := !m.lowActive || s.Status == StatusDischarging || statusChanged
		if shouldEmit {
			actions = append(actions, Action{
				Kind:     ActionNotifyLow,
				Level:    s.Level,
				Status:   s.Status,
				Critical: true,
			})
			m.lowActive = true
		}
	case s.Level >= m.full && s.Status.isChargingLike():
		// Emit full only once per charge session.
		if !m.fullActive && !m.fullDoneThisSess {
			actions = append(actions, Action{
				Kind:   ActionNotifyFull,
				Level:  s.Level,
				Status: s.Status,
			})
			m.fullActive = true
			m.fullDoneThisSess = true
		}
	}

	m.lastStatus = s.Status
	m.lastLevel = s.Level
	m.initialised = true
	return actions, trans
}
