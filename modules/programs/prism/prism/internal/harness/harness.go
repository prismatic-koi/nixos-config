// Package harness defines the Harness interface and its supporting types.
//
// A Harness is a pluggable adapter that teaches the prism sidecar how to
// communicate with a specific agent runtime (e.g. opencode, PI, Claude Code).
// Each harness implementation encapsulates the container command, health-check
// logic, prompt-delivery mechanism, and event-stream mapping for one runtime.
//
// This package contains only the interface definition and its supporting types.
// Concrete implementations live in sub-packages (e.g. internal/harness/opencode/).
// The sidecar receives a Harness at construction time (via sidecar.Config.Harness)
// and delegates all runtime-specific behaviour through it (Phase 0a, RFC #691).
package harness

import (
	"context"

	"github.com/prismatic-koi/prism/internal/agent"
)

// Harness is the interface that each agent runtime adapter must implement.
// The sidecar receives a Harness at construction time and delegates all
// runtime-specific behaviour through it.
type Harness interface {
	// ContainerCommand returns the command string used to launch the agent
	// runtime as the main process inside its container.
	// Example: "opencode --port 4096 --hostname 0.0.0.0"
	ContainerCommand() string

	// HealthCheck verifies that the agent runtime is ready to accept requests
	// on the given port. It should block until the runtime is healthy or ctx
	// is cancelled.
	HealthCheck(ctx context.Context, port int) error

	// ConfigMountPath returns the XDG config path inside the container where
	// the runtime expects its configuration to be mounted.
	// Example: "/home/user/.config/opencode"
	ConfigMountPath() string

	// DeliverInitialPrompt delivers the initial prompt and role system prompt
	// to the agent runtime after the container is healthy. The role parameter
	// identifies the agent role (e.g. "worker" or "coordinator").
	DeliverInitialPrompt(ctx context.Context, prompt, role string) error

	// DeliverPrompt delivers a follow-up prompt to a running agent session.
	DeliverPrompt(ctx context.Context, prompt string) error

	// Subscribe returns a channel of HarnessEvents from the agent runtime's
	// event stream. The channel is closed when the stream ends or ctx is
	// cancelled.
	Subscribe(ctx context.Context) (<-chan HarnessEvent, error)

	// MapEvent maps a HarnessEvent to a StateTransition. The second return
	// value is false if the event does not represent a state transition.
	MapEvent(evt HarnessEvent) (StateTransition, bool)

	// ExtractMessage extracts a Message from a HarnessEvent. The second
	// return value is false if the event does not carry a message.
	ExtractMessage(evt HarnessEvent) (Message, bool)

	// CreateSession creates a new session on the agent runtime and returns its
	// ID. The session ID is also stored internally so that DeliverInitialPrompt
	// can use it without the caller having to pass it back. This ordering
	// matters for the opencode attach TUI flow: the sidecar writes the .sid
	// file between CreateSession and DeliverInitialPrompt so that the TUI can
	// attach to the correct session before the prompt is delivered.
	CreateSession(ctx context.Context) (string, error)

	// SessionID returns the most recently created session ID (set by
	// CreateSession). Returns an empty string if CreateSession has not been
	// called or failed.
	SessionID() string

	// ExtractEventType returns the canonical event type string for the given
	// HarnessEvent. Harnesses that embed the event type inside the payload
	// (e.g. opencode, which uses the JSON "type" field rather than the SSE
	// "event:" header) must implement this to decode it. Harnesses that use the
	// SSE "event:" field directly may return evt.Type unchanged.
	ExtractEventType(evt HarnessEvent) string
}

// HarnessEvent is a raw event received from the agent runtime's event stream.
// It carries the event type and the raw payload bytes so that each harness
// implementation can decode them in its own way.
type HarnessEvent struct {
	// Type is the event type identifier as emitted by the runtime.
	Type string

	// Data is the raw event payload (e.g. JSON bytes from an SSE stream or
	// a JSON Lines record).
	Data []byte
}

// StateTransition represents a change in agent lifecycle state as reported by
// a HarnessEvent. The harness's MapEvent method produces one of these when an
// event carries state information.
type StateTransition struct {
	// State is the new agent state indicated by the event.
	State agent.AgentState
}

// Message represents an assistant or user message extracted from a HarnessEvent.
// The harness's ExtractMessage method produces one of these when an event
// carries message content.
type Message struct {
	// ID is the runtime-specific unique identifier for this message.
	ID string

	// Role is the message role: "user" or "assistant".
	Role string

	// Text is the plain-text content of the message.
	Text string
}
