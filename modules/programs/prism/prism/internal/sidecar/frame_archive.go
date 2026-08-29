package sidecar

// frame_archive.go — persistence helpers for the raw PI JSONL frame archive.
//
// The PI extension and sidecar exchange JSONL frames over a Unix-or-TCP socket
// (see runStartupSocketPipe in sidecar.go). Structured agent_events rows are
// derived from inbound frames at handlePipeFrame time, but the raw bytes
// themselves are not persisted there. This file adds a thin write helper that
// is called from runStartupSocketPipe at every read/write, so the operator can
// later replay the stream with `prism logs --harness-events <session>`.
//
// Design intent: the helpers MUST be cheap and tolerant of malformed input.
// A failure to archive a frame is non-fatal — the sidecar logs it and
// continues. The hot
// path (the duplex frame loop) cannot afford to block on slow DB writes.

import (
	"encoding/json"

	"github.com/google/uuid"
	"github.com/prismatic-koi/prism/internal/db"
)

// archiveInboundFrame persists a frame received from the PI extension.
// The provided bytes are the raw JSONL payload WITHOUT the trailing newline.
//
// Errors are logged and swallowed: the frame archive is a debugging aid and
// must never block or fail the live wire-protocol path.
func (s *Sidecar) archiveInboundFrame(frame []byte) {
	s.archiveFrame(db.HarnessFrameDirectionIn, frame)
}

// archiveOutboundFrame persists a frame the sidecar is sending to the PI
// extension. The provided bytes are the raw JSONL payload WITH the trailing
// newline (matching what is written to the wire); the newline is stripped
// before storage so consumers can append their own.
//
// Errors are logged and swallowed (same rationale as archiveInboundFrame).
func (s *Sidecar) archiveOutboundFrame(frame []byte) {
	// Strip a single trailing '\n' if present so the archive stores the same
	// shape as inbound frames (one JSON object, no terminator). Callers
	// already terminate outbound frames before calling Write. The trailing
	// newline must not appear in the persisted payload.
	if n := len(frame); n > 0 && frame[n-1] == '\n' {
		frame = frame[:n-1]
	}
	s.archiveFrame(db.HarnessFrameDirectionOut, frame)
}

// archiveFrame is the shared core: extract the frame type, build a HarnessFrame,
// and write it. Called from archiveInboundFrame / archiveOutboundFrame above.
func (s *Sidecar) archiveFrame(direction string, payload []byte) {
	if s == nil || s.cfg.DB == nil || len(payload) == 0 {
		return
	}

	// Best-effort extraction of the frame type. A frame that fails to parse
	// here (or whose type is empty) is still persisted — the raw payload is
	// kept so the operator can see the corrupt bytes.
	var typeOnly struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(payload, &typeOnly)

	var instanceIDPtr *string
	if s.cfg.InstanceID != "" {
		// Take a copy so callers mutating cfg.InstanceID later cannot
		// retroactively change persisted frames.
		iid := s.cfg.InstanceID
		instanceIDPtr = &iid
	}

	f := db.HarnessFrame{
		ID:          uuid.New().String(),
		SessionName: s.cfg.SessionName,
		InstanceID:  instanceIDPtr,
		Direction:   direction,
		Type:        typeOnly.Type,
		Payload:     string(payload),
		CreatedAt:   s.cfg.Clock.Now(),
	}
	if err := s.cfg.DB.WriteHarnessFrame(f); err != nil {
		// Non-fatal: log once per failure. The wire-protocol path keeps
		// running.
		s.logger().Printf("sidecar: archive harness frame (%s): %v", direction, err)
	}
}
