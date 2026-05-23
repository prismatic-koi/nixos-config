package state

import (
	"reflect"
	"testing"
)

// kinds extracts the ActionKind sequence from a slice of Actions for
// concise comparison in tests.
func kinds(actions []Action) []ActionKind {
	out := make([]ActionKind, len(actions))
	for i, a := range actions {
		out[i] = a.Kind
	}
	return out
}

func TestApply_LowOnDischarge(t *testing.T) {
	m := New(20, 100, 50, false)
	// First sample arrives discharging at 15% — straight into low.
	actions, _ := m.Apply(Sample{Level: 15, Status: StatusDischarging, Present: true})
	if !reflect.DeepEqual(kinds(actions), []ActionKind{ActionNotifyLow}) {
		t.Fatalf("expected one ActionNotifyLow, got %v", kinds(actions))
	}
	if !actions[0].Critical {
		t.Errorf("low action should be Critical=true")
	}
}

func TestApply_LowDismissOnRiseToDismissThreshold(t *testing.T) {
	m := New(20, 100, 50, false)
	m.Apply(Sample{Level: 15, Status: StatusDischarging, Present: true}) // arms lowActive
	// Plug in, climb past dismiss.
	actions, _ := m.Apply(Sample{Level: 50, Status: StatusCharging, Present: true})
	if !reflect.DeepEqual(kinds(actions), []ActionKind{ActionCloseLow}) {
		t.Fatalf("expected ActionCloseLow once level >= dismiss, got %v", kinds(actions))
	}
}

func TestApply_LowChargingTransitionUpdatesNotification(t *testing.T) {
	// While low, plugging in should re-emit the low notification so
	// the body text can switch to "Plugged in.". This mirrors the
	// existing Python behaviour: status_has_changed forces a re-send.
	m := New(20, 100, 50, false)
	m.Apply(Sample{Level: 15, Status: StatusDischarging, Present: true})
	actions, trans := m.Apply(Sample{Level: 15, Status: StatusCharging, Present: true})
	if !trans.Changed() {
		t.Fatalf("expected transition from Discharging to Charging")
	}
	// Discharging→Charging dismisses nothing (low is still in range)
	// but should re-emit the low notification with replaces_id so it
	// updates in place.
	if !reflect.DeepEqual(kinds(actions), []ActionKind{ActionNotifyLow}) {
		t.Fatalf("expected ActionNotifyLow on status flip while low, got %v", kinds(actions))
	}
}

func TestApply_DischargingToChargingResetsFullFlag(t *testing.T) {
	// The new semantics that replaces reNotifyThreshold: every fresh
	// Discharging→Charging transition arms a new full-notification
	// allowance for the next time level hits FullThreshold.
	m := New(20, 100, 50, false)
	// Initial charge → 100% → notify_full → unplug → discharge a bit.
	m.Apply(Sample{Level: 80, Status: StatusCharging, Present: true})
	if actions, _ := m.Apply(Sample{Level: 100, Status: StatusFull, Present: true}); !reflect.DeepEqual(kinds(actions), []ActionKind{ActionNotifyFull}) {
		t.Fatalf("first full notification missed, got %v", kinds(actions))
	}
	// Unplug — full notif dismissed.
	if actions, _ := m.Apply(Sample{Level: 99, Status: StatusDischarging, Present: true}); !reflect.DeepEqual(kinds(actions), []ActionKind{ActionCloseFull}) {
		t.Fatalf("expected ActionCloseFull on Discharging, got %v", kinds(actions))
	}
	// Discharge to 70%.
	m.Apply(Sample{Level: 70, Status: StatusDischarging, Present: true})
	// Replug — does NOT notify yet (not at full).
	if actions, _ := m.Apply(Sample{Level: 70, Status: StatusCharging, Present: true}); len(actions) != 0 {
		t.Fatalf("expected no actions on plug-in at 70%%, got %v", kinds(actions))
	}
	// Reach 100% — must notify again under the new semantics, even
	// though we did not first dip below the old reNotifyThreshold.
	if actions, _ := m.Apply(Sample{Level: 100, Status: StatusFull, Present: true}); !reflect.DeepEqual(kinds(actions), []ActionKind{ActionNotifyFull}) {
		t.Fatalf("expected second ActionNotifyFull after Discharging→Charging→Full, got %v", kinds(actions))
	}
}

func TestApply_FullOnlyOncePerChargeSession(t *testing.T) {
	m := New(20, 100, 50, false)
	m.Apply(Sample{Level: 90, Status: StatusCharging, Present: true})
	if actions, _ := m.Apply(Sample{Level: 100, Status: StatusFull, Present: true}); !reflect.DeepEqual(kinds(actions), []ActionKind{ActionNotifyFull}) {
		t.Fatalf("first full notify missing, got %v", kinds(actions))
	}
	// Further samples at full status must not re-notify.
	for i := 0; i < 5; i++ {
		if actions, _ := m.Apply(Sample{Level: 100, Status: StatusFull, Present: true}); len(actions) != 0 {
			t.Fatalf("unexpected action on subsequent Full sample %d: %v", i, kinds(actions))
		}
	}
}

func TestApply_FullDismissOnDischarge(t *testing.T) {
	m := New(20, 100, 50, false)
	m.Apply(Sample{Level: 90, Status: StatusCharging, Present: true})
	m.Apply(Sample{Level: 100, Status: StatusFull, Present: true})
	actions, _ := m.Apply(Sample{Level: 99, Status: StatusDischarging, Present: true})
	if !reflect.DeepEqual(kinds(actions), []ActionKind{ActionCloseFull}) {
		t.Fatalf("expected ActionCloseFull on unplug, got %v", kinds(actions))
	}
}

func TestApply_IgnoreZeroDoesNotCorruptState(t *testing.T) {
	// AC edge case: when ignoreZero is true and a 0 reading arrives,
	// the daemon must not notify AND must not update lastLevel. We
	// verify the latter by ensuring a subsequent legitimate low
	// reading still fires (i.e. the 0 did not flip lowActive).
	m := New(20, 100, 50, true)
	m.Apply(Sample{Level: 50, Status: StatusDischarging, Present: true})
	if actions, _ := m.Apply(Sample{Level: 0, Status: StatusDischarging, Present: true}); len(actions) != 0 {
		t.Fatalf("ignoreZero should suppress all actions on level==0, got %v", kinds(actions))
	}
	if m.LastLevel() != 50 {
		t.Errorf("ignoreZero should not update LastLevel, want 50 got %d", m.LastLevel())
	}
	// A real low reading still fires.
	if actions, _ := m.Apply(Sample{Level: 15, Status: StatusDischarging, Present: true}); !reflect.DeepEqual(kinds(actions), []ActionKind{ActionNotifyLow}) {
		t.Fatalf("low should fire after ignored zero, got %v", kinds(actions))
	}
}

func TestApply_IgnoreZeroFalseStillNotifies(t *testing.T) {
	// When ignoreZero is false, a 0% discharging reading is a real
	// emergency: notify.
	m := New(20, 100, 50, false)
	actions, _ := m.Apply(Sample{Level: 0, Status: StatusDischarging, Present: true})
	if !reflect.DeepEqual(kinds(actions), []ActionKind{ActionNotifyLow}) {
		t.Fatalf("expected ActionNotifyLow on real 0%%, got %v", kinds(actions))
	}
}

func TestApply_AbsentSamplePreservesState(t *testing.T) {
	// AC edge case: missing sysfs path / mouse asleep → Present=false
	// → no actions, no state mutation. A subsequent real reading must
	// still see the prior lastStatus so transitions evaluate
	// correctly.
	m := New(20, 100, 50, false)
	m.Apply(Sample{Level: 80, Status: StatusCharging, Present: true})
	if actions, _ := m.Apply(Sample{Present: false}); len(actions) != 0 {
		t.Fatalf("absent sample should produce no actions, got %v", kinds(actions))
	}
	if m.LastStatus() != StatusCharging {
		t.Errorf("absent sample must preserve LastStatus, got %v", m.LastStatus())
	}
	if m.LastLevel() != 80 {
		t.Errorf("absent sample must preserve LastLevel, got %d", m.LastLevel())
	}
}

func TestApply_RestartReNotifiesLow(t *testing.T) {
	// AC edge case: after a daemon restart, if the device is in the
	// low state, exactly one low notification fires. We model this
	// by constructing a fresh Machine (zero state) and applying a
	// low sample — the first Apply must emit notify_low.
	m := New(20, 100, 50, false)
	actions, _ := m.Apply(Sample{Level: 10, Status: StatusDischarging, Present: true})
	if !reflect.DeepEqual(kinds(actions), []ActionKind{ActionNotifyLow}) {
		t.Fatalf("restart-with-low should fire one notify_low, got %v", kinds(actions))
	}
	// And subsequent persistent-discharge samples re-emit (replaces_id
	// makes that visible to the user as a single updating bubble).
	actions, _ = m.Apply(Sample{Level: 9, Status: StatusDischarging, Present: true})
	if !reflect.DeepEqual(kinds(actions), []ActionKind{ActionNotifyLow}) {
		t.Fatalf("persistent discharge should re-emit notify_low, got %v", kinds(actions))
	}
}

func TestApply_FullToCloseThenLowInOneSample(t *testing.T) {
	// Sanity: if a sample arrives with status=Discharging and
	// level<=low while the full notification is active (pathological
	// but possible across a long sysfs gap), we should both close
	// full AND emit low in order.
	m := New(20, 100, 50, false)
	m.Apply(Sample{Level: 90, Status: StatusCharging, Present: true})
	m.Apply(Sample{Level: 100, Status: StatusFull, Present: true})
	actions, _ := m.Apply(Sample{Level: 15, Status: StatusDischarging, Present: true})
	if !reflect.DeepEqual(kinds(actions), []ActionKind{ActionCloseFull, ActionNotifyLow}) {
		t.Fatalf("expected ActionCloseFull then ActionNotifyLow, got %v", kinds(actions))
	}
}

func TestApply_RapidFlipDebouncedByStatusEquality(t *testing.T) {
	// AC edge case: rapid Charging↔Discharging flips. The state
	// machine itself only acts on "confirmed" transitions in the
	// sense that the *same* status repeated produces no action.
	// The daemon adds a timer-based debounce on top of this for the
	// laptop source (see daemon.go); the state machine test here
	// verifies that repeated identical samples do not stack
	// notifications.
	m := New(20, 100, 50, false)
	m.Apply(Sample{Level: 90, Status: StatusCharging, Present: true})
	// Flip Charging→Discharging→Charging at the same level. The
	// Charging→Discharging step does nothing (no active full, no
	// low). The Discharging→Charging step resets fullDoneThisSess —
	// but no full action because level<100.
	if actions, _ := m.Apply(Sample{Level: 90, Status: StatusDischarging, Present: true}); len(actions) != 0 {
		t.Fatalf("90%% Discharging should not notify, got %v", kinds(actions))
	}
	if actions, _ := m.Apply(Sample{Level: 90, Status: StatusCharging, Present: true}); len(actions) != 0 {
		t.Fatalf("90%% Charging should not notify, got %v", kinds(actions))
	}
}
