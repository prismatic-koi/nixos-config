// Package harness defines the Harness interface and its supporting types.
// This file provides a FakeHarness for use in tests.

package harness

import (
	"context"
	"fmt"
)

// FakeHarness is a minimal stub implementation of the Harness interface for
// use in tests. It compiles against the full interface — any new method added
// to Harness must be added here too, providing a compile-time check that the
// interface is implementable by non-pi harnesses.
//
// Function fields (MapEventFn, ExtractMessageFn, ExtractEventTypeFn) allow
// individual tests to inject custom event-handling logic without subclassing.
type FakeHarness struct {
	// ConfigEnvVarValue is the value returned by ConfigEnvVar().
	ConfigEnvVarValue string

	// RuntimeEnvValue is the map returned by RuntimeEnv().
	RuntimeEnvValue map[string]string

	// ValidRoles is the set of roles accepted by ValidateAgentRole().
	// If nil, all roles are accepted.
	ValidRoles map[string]bool

	// ModelsByRole maps role names to model identifiers for EffectiveModel().
	ModelsByRole map[string]string

	// ContainerCommandValue is returned by ContainerCommand().
	ContainerCommandValue string

	// ConfigMountPathValue is returned by ConfigMountPath().
	ConfigMountPathValue string

	// MapEventFn, when non-nil, is called by MapEvent instead of the default
	// stub (which always returns (StateTransition{}, false)).
	MapEventFn func(HarnessEvent) (StateTransition, bool)

	// ExtractMessageFn, when non-nil, is called by ExtractMessage instead of
	// the default stub (which always returns (Message{}, false)).
	ExtractMessageFn func(HarnessEvent) (Message, bool)

	// ExtractEventTypeFn, when non-nil, is called by ExtractEventType instead
	// of the default stub (which returns evt.Type).
	ExtractEventTypeFn func(HarnessEvent) string

	// SubscribeFn, when non-nil, is called by Subscribe instead of the
	// default stub (which returns a closed channel).
	SubscribeFn func(context.Context) (<-chan HarnessEvent, error)
}

// Compile-time assertion that *FakeHarness implements Harness.
var _ Harness = (*FakeHarness)(nil)

func (f *FakeHarness) ContainerCommand() string { return f.ContainerCommandValue }

func (f *FakeHarness) HealthCheck(_ context.Context, _ int) error { return nil }

func (f *FakeHarness) ConfigMountPath() string { return f.ConfigMountPathValue }

func (f *FakeHarness) DeliverInitialPrompt(_ context.Context, _, _ string) error { return nil }

func (f *FakeHarness) DeliverPrompt(_ context.Context, _ string) error { return nil }

func (f *FakeHarness) Subscribe(ctx context.Context) (<-chan HarnessEvent, error) {
	if f.SubscribeFn != nil {
		return f.SubscribeFn(ctx)
	}
	ch := make(chan HarnessEvent)
	close(ch)
	return ch, nil
}

func (f *FakeHarness) MapEvent(evt HarnessEvent) (StateTransition, bool) {
	if f.MapEventFn != nil {
		return f.MapEventFn(evt)
	}
	return StateTransition{}, false
}

func (f *FakeHarness) ExtractMessage(evt HarnessEvent) (Message, bool) {
	if f.ExtractMessageFn != nil {
		return f.ExtractMessageFn(evt)
	}
	return Message{}, false
}

func (f *FakeHarness) CreateSession(_ context.Context) (string, error) {
	return "fake-session-id", nil
}

func (f *FakeHarness) SessionID() string { return "fake-session-id" }

func (f *FakeHarness) ExtractEventType(evt HarnessEvent) string {
	if f.ExtractEventTypeFn != nil {
		return f.ExtractEventTypeFn(evt)
	}
	return evt.Type
}

func (f *FakeHarness) ConfigEnvVar() string {
	if f.ConfigEnvVarValue != "" {
		return f.ConfigEnvVarValue
	}
	return "FAKE_CONFIG_CONTENT"
}

func (f *FakeHarness) RuntimeEnv() map[string]string {
	if f.RuntimeEnvValue != nil {
		return f.RuntimeEnvValue
	}
	return map[string]string{}
}

func (f *FakeHarness) ValidateAgentRole(role string) error {
	if f.ValidRoles == nil {
		return nil // accept all
	}
	if f.ValidRoles[role] {
		return nil
	}
	return fmt.Errorf("fake: agent role %q is not supported", role)
}

func (f *FakeHarness) EffectiveModel(role string) string {
	if f.ModelsByRole != nil {
		return f.ModelsByRole[role]
	}
	return ""
}
