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
// interface is implementable by non-opencode harnesses.
//
// This satisfies the acceptance criterion: "A stub harness (for tests, or even
// just a fakeHarness in a test file) compiles against the updated interface."
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
}

// Compile-time assertion that *FakeHarness implements Harness.
var _ Harness = (*FakeHarness)(nil)

func (f *FakeHarness) ContainerCommand() string { return f.ContainerCommandValue }

func (f *FakeHarness) HealthCheck(_ context.Context, _ int) error { return nil }

func (f *FakeHarness) ConfigMountPath() string { return f.ConfigMountPathValue }

func (f *FakeHarness) DeliverInitialPrompt(_ context.Context, _, _ string) error { return nil }

func (f *FakeHarness) DeliverPrompt(_ context.Context, _ string) error { return nil }

func (f *FakeHarness) Subscribe(_ context.Context) (<-chan HarnessEvent, error) {
	ch := make(chan HarnessEvent)
	close(ch)
	return ch, nil
}

func (f *FakeHarness) MapEvent(_ HarnessEvent) (StateTransition, bool) {
	return StateTransition{}, false
}

func (f *FakeHarness) ExtractMessage(_ HarnessEvent) (Message, bool) {
	return Message{}, false
}

func (f *FakeHarness) CreateSession(_ context.Context) (string, error) {
	return "fake-session-id", nil
}

func (f *FakeHarness) SessionID() string { return "fake-session-id" }

func (f *FakeHarness) ExtractEventType(evt HarnessEvent) string { return evt.Type }

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
