// Package harness defines the Harness interface and its supporting types.
//
// A Harness is a pluggable adapter that teaches the prism sidecar how to
// communicate with a specific agent runtime (e.g. pi / Claude Code).
// Each harness implementation encapsulates the container command, health-check
// logic, prompt-delivery mechanism, and event-stream mapping for one runtime.
//
// This package contains only the interface definition and its supporting types.
// Concrete implementations live in sub-packages (e.g. internal/harness/pi/).
// The sidecar receives a Harness at construction time (via sidecar.Config.Harness)
// and delegates all runtime-specific behaviour through it (Phase 0a, RFC #691).
package harness

import (
	"context"
	"io"

	"github.com/prismatic-koi/prism/internal/agent"
)

// Harness is the interface that each agent runtime adapter must implement.
// The sidecar receives a Harness at construction time and delegates all
// runtime-specific behaviour through it.
type Harness interface {
	// ContainerCommand returns the command string used to launch the agent
	// runtime as the main process inside its container.
	// Example: "pi --port 4096 --hostname 0.0.0.0"
	ContainerCommand() string

	// HealthCheck verifies that the agent runtime is ready to accept requests
	// on the given port. It should block until the runtime is healthy or ctx
	// is cancelled.
	HealthCheck(ctx context.Context, port int) error

	// ConfigMountPath returns the XDG config path inside the container where
	// the runtime expects its configuration to be mounted.
	// Example: "/home/user/.config/pi"
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

	// CreateSession retrieves (or creates) a session on the agent runtime and
	// returns its ID. The session ID is also stored internally so that
	// DeliverInitialPrompt can use it without the caller having to pass it back.
	// For the pi harness in combined TUI + HTTP mode, this retrieves the
	// existing session the agent auto-created at startup (via GET /session) so
	// the prompt is delivered to the session already visible in the TUI.
	CreateSession(ctx context.Context) (string, error)

	// SessionID returns the most recently created session ID (set by
	// CreateSession). Returns an empty string if CreateSession has not been
	// called or failed.
	SessionID() string

	// ExtractEventType returns the canonical event type string for the given
	// HarnessEvent. Harnesses that embed the event type inside the payload
	// (e.g. pi, which uses the JSON "type" field rather than the SSE
	// "event:" header) must implement this to decode it. Harnesses that use the
	// SSE "event:" field directly may return evt.Type unchanged.
	ExtractEventType(evt HarnessEvent) string

	// ConfigEnvVar returns the environment variable name this harness uses
	// to receive its serialised config content (e.g. "OPENCODE_CONFIG_CONTENT"
	// for opencode-compat harnesses — empty for pi). The returned name is used in both host-mode (inline
	// shell env-var prefix) and container-mode (podman --env) session
	// creation to inject config overrides into the agent runtime.
	ConfigEnvVar() string

	// RuntimeEnv returns additional environment variables this harness needs
	// set in the container / process it runs in. For pi, this includes
	// experimental flags like the bash-tool timeout. The returned map is
	// merged into the session environment alongside other env vars —
	// existing vars with the same name are NOT overwritten.
	RuntimeEnv() map[string]string

	// ValidateAgentRole reports whether a given agent role is supported by
	// this harness. For pi, this means the agent definition file
	// exists in the agents directory. Returns nil when the role is valid.
	// The error message should mention the harness name and the role.
	ValidateAgentRole(role string) error

	// EffectiveModel returns the model identifier this harness uses for a
	// given agent role, applying any harness-specific defaults or
	// role-specific overrides. Returns an empty string if the model cannot
	// be determined (e.g. config file missing or the role has no explicit
	// model).
	EffectiveModel(role string) string
}

// ModelOverridesSetter is an optional interface implemented by harness adapters
// that support per-role model overrides (C.2). Callers that need to apply a
// full role→model map after construction use harness.NewWithModelOverrides or
// harness.NewContainerWithModelOverrides, which call SetModelOverrides via this
// interface.
type ModelOverridesSetter interface {
	// SetModelOverrides replaces the adapter's per-role model map with m.
	// A nil map clears all overrides. The call is not thread-safe; callers
	// must invoke it before the adapter is shared across goroutines.
	SetModelOverrides(m map[string]string)
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

// FrameNormaliser is an optional interface that a harness.Harness implementation
// may satisfy to hook into the sidecar's stdio JSONL frame processing loop.
//
// When the harness passed to a sidecar's Config implements FrameNormaliser, the
// sidecar calls NormaliseFrame for each raw JSONL line instead of writing the raw
// bytes as the event payload. This is the mechanism used by the PI harness adapter
// to implement the B5.TR Translate payload strategy: PI's native JSONL frames are
// normalised to pi-shaped payload.* structs at write time, so downstream
// consumers (cmd/checkin, cmd/stats, cmd/audit) require no harness-specific branches.
//
// Returning (_, _, false) from NormaliseFrame means the frame should be skipped
// (the adapter is responsible for logging at info level — not silently dropping).
// Returning (_, _, true) with a non-empty eventType means the frame is written
// to agent_events normally.
//
// The interface is defined here (in the harness package) so that the sidecar can
// do a type-assertion against it without importing any concrete harness package.
type FrameNormaliser interface {
	// NormaliseFrame maps a raw JSONL line to a canonical (eventType, payload,
	// shouldWrite) tuple.
	//
	//   - rawLine is a single JSONL record (no trailing newline).
	//   - eventType is the canonical agent_events type string (e.g. "msg_assistant").
	//   - normPayload is a value suitable for json.Marshal (typically a payload.* struct).
	//   - shouldWrite is false when the frame is intentionally skipped.
	NormaliseFrame(rawLine []byte) (eventType string, normPayload any, shouldWrite bool)
}

// StdinReceiver is an optional interface that a stdio-pipe Harness may implement
// to receive the process's stdin pipe after the harness process has been started.
//
// When the harness passed to a sidecar implements StdinReceiver, the sidecar calls
// SetStdinPipe immediately after cmd.StdinPipe() succeeds and before cmd.Start(),
// so that DeliverInitialPrompt / DeliverPrompt can write to the running process.
// Harnesses that do not use stdin delivery (e.g. those that receive prompts on the
// command line) do not need to implement this interface.
type StdinReceiver interface {
	// SetStdinPipe hands the harness the open stdin pipe for the running process.
	// The harness must close the pipe when the session ends.
	SetStdinPipe(pipe io.WriteCloser)
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
