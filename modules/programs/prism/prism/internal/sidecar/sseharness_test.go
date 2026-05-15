package sidecar

import (
	"encoding/json"

	"github.com/prismatic-koi/prism/internal/agent"
	"github.com/prismatic-koi/prism/internal/harness"
)

// newSSEHarness returns a *harness.FakeHarness that parses SSE events in the
// opencode wire format (JSON envelope with a "type" field). This allows
// sidecar tests to call HandleEvent() with makeSSE() events and have the
// harness correctly extract event types, map state transitions, and extract
// messages — without depending on the removed opencode harness package.
//
// Event format produced by makeSSE():
//
//	data: {"type":"session.status","properties":{...}}
//
// The SSE client sets HarnessEvent.Type to "message" (SSE default when no
// `event:` field is present); the real type is in the JSON payload.
func newSSEHarness() *harness.FakeHarness {
	h := &harness.FakeHarness{}

	// ExtractEventType: read the "type" field from the JSON envelope when
	// the outer SSE type is "" or "message" (opencode wire format).
	h.ExtractEventTypeFn = func(evt harness.HarnessEvent) string {
		t := evt.Type
		if t == "" || t == "message" {
			var env struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(evt.Data, &env); err == nil && env.Type != "" {
				t = env.Type
			}
		}
		return t
	}

	// MapEvent: map the opencode-style event types to StateTransitions.
	h.MapEventFn = func(evt harness.HarnessEvent) (harness.StateTransition, bool) {
		eventType := h.ExtractEventTypeFn(evt)
		switch eventType {
		case "session.created", "session.updated":
			return harness.StateTransition{State: agent.StateActive}, true
		case "session.deleted":
			return harness.StateTransition{State: agent.StateDeleted}, true
		case "session.error":
			return harness.StateTransition{State: agent.StateError}, true
		case "permission.asked", "question.asked":
			return harness.StateTransition{State: agent.StateWaiting}, true
		case "permission.replied", "question.replied", "question.rejected":
			return harness.StateTransition{State: agent.StateActive}, true
		}
		return harness.StateTransition{}, false
	}

	// ExtractMessage: extract a harness.Message from message.updated events.
	h.ExtractMessageFn = func(evt harness.HarnessEvent) (harness.Message, bool) {
		eventType := h.ExtractEventTypeFn(evt)
		if eventType != "message.updated" {
			return harness.Message{}, false
		}
		var payload struct {
			Properties struct {
				Info struct {
					ID   string `json:"id"`
					Role string `json:"role"`
					Time *struct {
						Completed *float64 `json:"completed"`
					} `json:"time"`
				} `json:"info"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(evt.Data, &payload); err != nil {
			return harness.Message{}, false
		}
		info := payload.Properties.Info
		if info.ID == "" || info.Role == "" {
			return harness.Message{}, false
		}
		if info.Role == "assistant" && (info.Time == nil || info.Time.Completed == nil) {
			return harness.Message{}, false
		}
		return harness.Message{ID: info.ID, Role: info.Role}, true
	}

	return h
}
