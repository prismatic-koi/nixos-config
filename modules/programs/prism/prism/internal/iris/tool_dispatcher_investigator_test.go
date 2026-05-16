package iris

// tool_dispatcher_investigator_test.go — unit tests for the read-only
// gates on `write` and `edit` when role == "investigate".
//
// The investigator must NEVER reach the sandbox subprocess for these
// tools: the gate runs before path validation so it produces a clear
// "investigator is read-only" output regardless of args.

import (
	"context"
	"strings"
	"testing"
)

func TestRunWrite_InvestigatorRoleIsReadOnly(t *testing.T) {
	d := &toolDispatcher{role: "investigate"}
	got := d.runWrite(context.Background(), ToolExecFrame{
		Name: "write",
		Args: map[string]any{
			"file_path": "/some/path",
			"content":   "anything",
		},
	})
	if got.Success {
		t.Fatalf("runWrite(investigate): success = true, want false")
	}
	if !got.IsError {
		t.Errorf("runWrite(investigate): is_error = false, want true")
	}
	if !strings.Contains(got.Output, "investigator is read-only") {
		t.Errorf("output = %q, want 'investigator is read-only'", got.Output)
	}
	if !strings.Contains(got.Output, "write") {
		t.Errorf("output = %q, want it to mention the blocked tool 'write'", got.Output)
	}
}

func TestRunEdit_InvestigatorRoleIsReadOnly(t *testing.T) {
	d := &toolDispatcher{role: "investigate"}
	got := d.runEdit(context.Background(), ToolExecFrame{
		Name: "edit",
		Args: map[string]any{
			"file_path":  "/some/path",
			"old_string": "foo",
			"new_string": "bar",
		},
	})
	if got.Success {
		t.Fatalf("runEdit(investigate): success = true, want false")
	}
	if !got.IsError {
		t.Errorf("runEdit(investigate): is_error = false, want true")
	}
	if !strings.Contains(got.Output, "investigator is read-only") {
		t.Errorf("output = %q, want 'investigator is read-only'", got.Output)
	}
	if !strings.Contains(got.Output, "edit") {
		t.Errorf("output = %q, want it to mention the blocked tool 'edit'", got.Output)
	}
}

// TestRunWrite_NonInvestigatorRolePassesGate asserts that the read-only
// gate is strictly role-keyed. A worker session that sends a write with
// a missing file_path arg trips the next layer's "missing 'file_path'"
// error — which is the contract we want: the gate is invisible for
// non-investigator roles.
func TestRunWrite_NonInvestigatorRolePassesGate(t *testing.T) {
	d := &toolDispatcher{role: "worker"}
	got := d.runWrite(context.Background(), ToolExecFrame{
		Name: "write",
		Args: map[string]any{
			// Intentionally no file_path: tests that we hit the next
			// validation layer, not the read-only gate.
			"content": "anything",
		},
	})
	if got.Success {
		t.Fatalf("runWrite(worker, no file_path): success = true, want false")
	}
	if strings.Contains(got.Output, "investigator is read-only") {
		t.Errorf("output = %q; non-investigator role must NOT trip the read-only gate", got.Output)
	}
	if !strings.Contains(got.Output, "file_path") {
		t.Errorf("output = %q, want next-layer 'missing file_path' error", got.Output)
	}
}

func TestRunEdit_NonInvestigatorRolePassesGate(t *testing.T) {
	d := &toolDispatcher{role: "worker"}
	got := d.runEdit(context.Background(), ToolExecFrame{
		Name: "edit",
		Args: map[string]any{
			"old_string": "foo",
			"new_string": "bar",
		},
	})
	if got.Success {
		t.Fatalf("runEdit(worker, no file_path): success = true, want false")
	}
	if strings.Contains(got.Output, "investigator is read-only") {
		t.Errorf("output = %q; non-investigator role must NOT trip the read-only gate", got.Output)
	}
	if !strings.Contains(got.Output, "file_path") {
		t.Errorf("output = %q, want next-layer 'missing file_path' error", got.Output)
	}
}
