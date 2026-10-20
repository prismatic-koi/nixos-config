package db_test

// Tests for #3192: `prism reset` passes the sessions whose sidecar did not
// stop to MarkAllEnded and ClearAllResumePointers, and those rows stay as
// they are.

import "testing"

func TestMarkAllEnded_KeepsNamedRows_3192(t *testing.T) {
	d := openTestDB(t)
	keptIID := insertActiveSession(t, d, "repo@kept", "repo", "/wt/kept")
	endedIID := insertActiveSession(t, d, "repo@ended", "repo", "/wt/ended")

	n, err := d.MarkAllEnded("repo@kept")
	if err != nil {
		t.Fatalf("MarkAllEnded: %v", err)
	}
	if n != 1 {
		t.Errorf("MarkAllEnded = %d, want 1", n)
	}
	if st, _ := d.CurrentStatus("repo@kept"); st == nil || st.EndedAt != nil {
		t.Errorf("kept row = %+v, want open", st)
	}
	if v := sessionsEndedAt(t, d, keptIID); v != nil {
		t.Errorf("sessions row of the kept session ended at %v, want open", *v)
	}
	if st, _ := d.CurrentStatus("repo@ended"); st == nil || st.EndedAt == nil {
		t.Errorf("other row = %+v, want ended", st)
	}
	if v := sessionsEndedAt(t, d, endedIID); v == nil {
		t.Error("sessions row of the other session open, want ended")
	}
}

func TestClearAllResumePointers_KeepsNamedRows_3192(t *testing.T) {
	d := openTestDB(t)
	for _, name := range []string{"repo@kept", "repo@cleared"} {
		insertActiveSession(t, d, name, "repo", "/wt/"+name)
		if err := d.UpdateHarnessSessionID(name, "sid-"+name); err != nil {
			t.Fatalf("UpdateHarnessSessionID(%q): %v", name, err)
		}
	}

	n, err := d.ClearAllResumePointers("repo@kept")
	if err != nil {
		t.Fatalf("ClearAllResumePointers: %v", err)
	}
	if n != 1 {
		t.Errorf("ClearAllResumePointers = %d, want 1", n)
	}
	if st, _ := d.CurrentStatus("repo@kept"); st == nil || st.HarnessSessionID == nil {
		t.Errorf("kept row = %+v, want its resume pointer", st)
	}
	if st, _ := d.CurrentStatus("repo@cleared"); st == nil || st.HarnessSessionID != nil {
		t.Errorf("other row = %+v, want no resume pointer", st)
	}
}
