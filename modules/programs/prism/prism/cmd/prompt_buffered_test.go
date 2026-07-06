package cmd

// prompt_buffered_test.go — regression tests for the buffered-outcome
// surfacing added by issue #2359 Gap B.
//
// Before this change, the `prism prompt` CLI silently reported success
// even when the sidecar's host-API /prompt handler responded
// {"buffered": true}. The coordinator then had no way to distinguish
// "delivered on the wire" from "parked awaiting reconnect", which was
// half the reason the incident in #2359 went undetected.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/promptdelivery"
)

// TestEmitPromptOutcome_HumanBufferedNamesTheOutcome verifies the AC:
// "prism prompt's human-readable output names the buffered outcome".
func TestEmitPromptOutcome_HumanBufferedNamesTheOutcome(t *testing.T) {
	var out bytes.Buffer
	err := emitPromptOutcome(&out, false, "myrepo@worker", "socket-pipe", promptdelivery.DeliveryOutcome{
		Buffered:   true,
		DeliveryID: "abcd-1234",
	})
	if err != nil {
		t.Fatalf("emitPromptOutcome: %v", err)
	}
	got := out.String()
	// The message must call out the buffered state so a bash-tool or
	// human reader cannot mistake it for a synchronous delivery.
	if !strings.Contains(got, "buffered") {
		t.Errorf("human output missing 'buffered' word: %q", got)
	}
	if !strings.Contains(got, "myrepo@worker") {
		t.Errorf("human output missing target session: %q", got)
	}
	if !strings.Contains(got, "abcd-1234") {
		t.Errorf("human output missing delivery_id: %q", got)
	}
	if !strings.Contains(strings.ToLower(got), "disconnected") &&
		!strings.Contains(strings.ToLower(got), "next handshake") &&
		!strings.Contains(strings.ToLower(got), "replay") {
		t.Errorf("human output should hint at the reason (disconnected / replay): %q", got)
	}
}

// TestEmitPromptOutcome_JSONCarriesBufferedField verifies the AC:
// "--json output carries a buffered: true field".
func TestEmitPromptOutcome_JSONCarriesBufferedField(t *testing.T) {
	var out bytes.Buffer
	err := emitPromptOutcome(&out, true, "myrepo@worker", "socket-pipe", promptdelivery.DeliveryOutcome{
		Buffered:   true,
		DeliveryID: "abcd-1234",
	})
	if err != nil {
		t.Fatalf("emitPromptOutcome: %v", err)
	}
	// The JSON envelope must be a single object on stdout.
	body := strings.TrimSpace(out.String())
	if strings.Count(body, "\n") != 0 {
		t.Errorf("--json output should be a single line, got: %q", body)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("json.Unmarshal: %v (body=%q)", err, body)
	}
	if buffered, _ := parsed["buffered"].(bool); !buffered {
		t.Errorf("--json output missing buffered=true: %v", parsed)
	}
	if got, _ := parsed["delivered_to"].(string); got != "myrepo@worker" {
		t.Errorf("--json delivered_to = %q, want %q", got, "myrepo@worker")
	}
	if got, _ := parsed["delivery_id"].(string); got != "abcd-1234" {
		t.Errorf("--json delivery_id = %q, want %q", got, "abcd-1234")
	}
	if got, _ := parsed["transport"].(string); got != "socket-pipe" {
		t.Errorf("--json transport = %q, want %q", got, "socket-pipe")
	}
	if replayed, _ := parsed["replayed"].(bool); replayed {
		t.Errorf("--json replayed must be false when only buffered is set: %v", parsed)
	}
}

// TestEmitPromptOutcome_HumanSynchronousUnchanged verifies the pre-#2359
// behaviour is preserved: a synchronous delivery still prints the classic
// "prompt delivered to <session> via <transport>" line so existing callers
// (grep / eyeball) keep working.
func TestEmitPromptOutcome_HumanSynchronousUnchanged(t *testing.T) {
	var out bytes.Buffer
	err := emitPromptOutcome(&out, false, "myrepo@worker", "socket-pipe", promptdelivery.DeliveryOutcome{
		DeliveryID: "sync-id",
	})
	if err != nil {
		t.Fatalf("emitPromptOutcome: %v", err)
	}
	got := strings.TrimSpace(out.String())
	if !strings.Contains(got, "prompt delivered to myrepo@worker via socket-pipe") {
		t.Errorf("synchronous human output changed: %q", got)
	}
	if strings.Contains(strings.ToLower(got), "buffered") {
		t.Errorf("synchronous human output must NOT mention buffered: %q", got)
	}
}

// TestEmitPromptOutcome_JSONSynchronous verifies the JSON envelope shape
// when the delivery is synchronous.
func TestEmitPromptOutcome_JSONSynchronous(t *testing.T) {
	var out bytes.Buffer
	err := emitPromptOutcome(&out, true, "myrepo@worker", "http", promptdelivery.DeliveryOutcome{
		DeliveryID: "sync-id",
	})
	if err != nil {
		t.Fatalf("emitPromptOutcome: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.String())), &parsed); err != nil {
		t.Fatalf("json.Unmarshal: %v (body=%q)", err, out.String())
	}
	if buffered, _ := parsed["buffered"].(bool); buffered {
		t.Errorf("synchronous JSON must have buffered=false: %v", parsed)
	}
	if replayed, _ := parsed["replayed"].(bool); replayed {
		t.Errorf("synchronous JSON must have replayed=false: %v", parsed)
	}
}

// TestEmitPromptOutcome_HumanReplayed verifies that a dedup-replay is
// surfaced distinctly from buffered — the two states have different
// operational meanings.
func TestEmitPromptOutcome_HumanReplayed(t *testing.T) {
	var out bytes.Buffer
	err := emitPromptOutcome(&out, false, "myrepo@worker", "socket-pipe", promptdelivery.DeliveryOutcome{
		Replayed:   true,
		DeliveryID: "dedup-hit",
	})
	if err != nil {
		t.Fatalf("emitPromptOutcome: %v", err)
	}
	got := out.String()
	if !strings.Contains(strings.ToLower(got), "replay") && !strings.Contains(strings.ToLower(got), "dedup") {
		t.Errorf("replayed human output should say so: %q", got)
	}
}
