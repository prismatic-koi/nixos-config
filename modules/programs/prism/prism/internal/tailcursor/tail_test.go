package tailcursor_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/tailcursor"
)

// fakeSource is an in-memory Source with the same id semantics as a SQLite
// rowid: ids ascend, and deleting rows does not renumber the survivors.
type fakeSource struct {
	records  []tailcursor.Record[string]
	maxErr   error
	readErr  error
	reads    int
	descend  bool // return records in the wrong order, to test the guard
	repeatID bool // return an id at or behind afterID, to test the guard
}

func (s *fakeSource) MaxID(context.Context) (int64, error) {
	if s.maxErr != nil {
		return 0, s.maxErr
	}
	var max int64
	for _, r := range s.records {
		if r.ID > max {
			max = r.ID
		}
	}
	return max, nil
}

func (s *fakeSource) Records(_ context.Context, afterID int64, limit int) ([]tailcursor.Record[string], error) {
	s.reads++
	if s.readErr != nil {
		return nil, s.readErr
	}
	var out []tailcursor.Record[string]
	for _, r := range s.records {
		if r.ID > afterID {
			out = append(out, r)
		}
		if len(out) == limit {
			break
		}
	}
	if s.repeatID && len(out) > 0 {
		out[0].ID = afterID
	}
	if s.descend && len(out) > 1 {
		out[0], out[len(out)-1] = out[len(out)-1], out[0]
	}
	return out, nil
}

// deleteUpTo removes every record with id <= id, exactly as Prune removes
// the oldest rows and leaves the survivors' rowids alone.
func (s *fakeSource) deleteUpTo(id int64) {
	var kept []tailcursor.Record[string]
	for _, r := range s.records {
		if r.ID > id {
			kept = append(kept, r)
		}
	}
	s.records = kept
}

func recordsFrom(values ...string) []tailcursor.Record[string] {
	out := make([]tailcursor.Record[string], 0, len(values))
	for i, v := range values {
		out = append(out, tailcursor.Record[string]{ID: int64(i + 1), Value: v})
	}
	return out
}

func newTailer(t *testing.T, src tailcursor.Source[string], sink *[]string, opts ...tailcursor.Option) *tailcursor.Tailer[string] {
	t.Helper()
	tl, err := tailcursor.New[string]("agent_events", src, func(v string) error {
		*sink = append(*sink, v)
		return nil
	}, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tl
}

func TestTailer_InitAtHeadDoesNotBackfillHistory(t *testing.T) {
	src := &fakeSource{records: recordsFrom("a", "b", "c")}
	var applied []string
	tl := newTailer(t, src, &applied)

	if err := tl.InitAtHead(context.Background()); err != nil {
		t.Fatalf("InitAtHead: %v", err)
	}
	if got := tl.Cursor(); got != 3 {
		t.Fatalf("cursor = %d after InitAtHead, want 3 (the current head)", got)
	}
	n, err := tl.Advance(context.Background())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if n != 0 || len(applied) != 0 {
		t.Fatalf("Advance applied %d records (%v) right after InitAtHead; history must not be backfilled", n, applied)
	}
}

func TestTailer_AdvanceAppliesNewRecordsInOrder(t *testing.T) {
	src := &fakeSource{}
	var applied []string
	tl := newTailer(t, src, &applied)
	if err := tl.InitAtHead(context.Background()); err != nil {
		t.Fatalf("InitAtHead: %v", err)
	}

	src.records = recordsFrom("a", "b", "c")
	n, err := tl.Advance(context.Background())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if n != 3 {
		t.Fatalf("Advance applied %d, want 3", n)
	}
	if strings.Join(applied, ",") != "a,b,c" {
		t.Fatalf("applied %v, want [a b c] in order", applied)
	}
	if tl.Cursor() != 3 {
		t.Fatalf("cursor = %d, want 3", tl.Cursor())
	}

	// A second advance with nothing new must be a no-op.
	n, err = tl.Advance(context.Background())
	if err != nil || n != 0 {
		t.Fatalf("second Advance = (%d, %v), want (0, nil)", n, err)
	}
}

func TestTailer_AdvanceDrainsAcrossBatches(t *testing.T) {
	src := &fakeSource{}
	var applied []string
	tl := newTailer(t, src, &applied, tailcursor.WithBatchSize(2))
	if err := tl.InitAtHead(context.Background()); err != nil {
		t.Fatalf("InitAtHead: %v", err)
	}

	src.records = recordsFrom("a", "b", "c", "d", "e")
	n, err := tl.Advance(context.Background())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if n != 5 {
		t.Fatalf("Advance applied %d, want 5 — it must loop until the source is drained", n)
	}
	if src.reads < 3 {
		t.Errorf("source was read %d times for 5 records at batch size 2, want at least 3", src.reads)
	}
}

// Deleting rows BEHIND the cursor is what Prune does. It must be invisible
// to the tailer: no re-read, no cursor move, no re-application.
func TestTailer_DeletingRecordsBehindTheCursorChangesNothing(t *testing.T) {
	src := &fakeSource{}
	var applied []string
	tl := newTailer(t, src, &applied)
	if err := tl.InitAtHead(context.Background()); err != nil {
		t.Fatalf("InitAtHead: %v", err)
	}

	src.records = recordsFrom("a", "b", "c", "d")
	if _, err := tl.Advance(context.Background()); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	cursorBefore, appliedBefore := tl.Cursor(), len(applied)

	src.deleteUpTo(3) // prune everything but the last record

	n, err := tl.Advance(context.Background())
	if err != nil {
		t.Fatalf("Advance after prune: %v", err)
	}
	if n != 0 {
		t.Errorf("Advance applied %d records after a prune behind the cursor, want 0", n)
	}
	if tl.Cursor() != cursorBefore {
		t.Errorf("cursor moved from %d to %d after a prune behind it", cursorBefore, tl.Cursor())
	}
	if len(applied) != appliedBefore {
		t.Errorf("applied set grew from %d to %d after a prune behind the cursor", appliedBefore, len(applied))
	}
}

// SQLite reuses a rowid once the highest-rowid row is deleted, because
// agent_events has no AUTOINCREMENT. A 90-day prune that empties the table
// therefore leaves the cursor stranded above every future insert. The tailer
// must notice and clamp down, without re-applying anything.
func TestTailer_ClampsWhenTheSourceHeadFallsBehindTheCursor(t *testing.T) {
	var logBuf bytes.Buffer
	src := &fakeSource{}
	var applied []string
	tl := newTailer(t, src, &applied, tailcursor.WithLogger(log.New(&logBuf, "", 0)))
	if err := tl.InitAtHead(context.Background()); err != nil {
		t.Fatalf("InitAtHead: %v", err)
	}

	src.records = recordsFrom("a", "b", "c")
	if _, err := tl.Advance(context.Background()); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if tl.Cursor() != 3 {
		t.Fatalf("cursor = %d, want 3", tl.Cursor())
	}

	// Prune empties the table, then SQLite hands rowid 1 to the next
	// insert.
	src.deleteUpTo(3)
	n, err := tl.Advance(context.Background())
	if err != nil {
		t.Fatalf("Advance on an emptied source: %v", err)
	}
	if n != 0 {
		t.Errorf("Advance applied %d records on an emptied source, want 0", n)
	}
	if tl.Cursor() != 0 {
		t.Fatalf("cursor = %d after the source emptied, want 0 — a stranded cursor never advances again", tl.Cursor())
	}
	if !strings.Contains(logBuf.String(), "behind cursor") {
		t.Errorf("the clamp was not logged; operators need to see it. Log was: %q", logBuf.String())
	}

	src.records = []tailcursor.Record[string]{{ID: 1, Value: "reused"}}
	n, err = tl.Advance(context.Background())
	if err != nil {
		t.Fatalf("Advance after rowid reuse: %v", err)
	}
	if n != 1 || len(applied) != 4 || applied[3] != "reused" {
		t.Fatalf("after rowid reuse Advance applied %d (%v), want the reused row to be counted", n, applied)
	}
}

// The clamp must not re-apply the rows that are still present, or the
// counter would jump by the size of the surviving table.
func TestTailer_ClampDoesNotReapplySurvivingRecords(t *testing.T) {
	src := &fakeSource{}
	var applied []string
	tl := newTailer(t, src, &applied)
	if err := tl.InitAtHead(context.Background()); err != nil {
		t.Fatalf("InitAtHead: %v", err)
	}
	src.records = recordsFrom("a", "b", "c", "d", "e")
	if _, err := tl.Advance(context.Background()); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	// Something removes the top of the table but leaves the bottom.
	src.records = src.records[:2] // ids 1 and 2 survive, head is now 2
	n, err := tl.Advance(context.Background())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if n != 0 {
		t.Fatalf("clamp re-applied %d surviving records, want 0", n)
	}
	if len(applied) != 5 {
		t.Fatalf("applied set is %d, want 5 — the clamp must not re-count", len(applied))
	}
	if tl.Cursor() != 2 {
		t.Fatalf("cursor = %d, want 2 (the new head)", tl.Cursor())
	}
}

func TestTailer_ResumeRestoresTheCursor(t *testing.T) {
	src := &fakeSource{records: recordsFrom("a", "b", "c", "d")}
	var applied []string
	tl := newTailer(t, src, &applied)

	if err := tl.Resume(context.Background(), 2); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	n, err := tl.Advance(context.Background())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if n != 2 || strings.Join(applied, ",") != "c,d" {
		t.Fatalf("Advance after Resume(2) applied %d (%v), want [c d]", n, applied)
	}
}

func TestTailer_ResumeRejectsNegativeCursor(t *testing.T) {
	src := &fakeSource{}
	var applied []string
	tl := newTailer(t, src, &applied)
	if err := tl.Resume(context.Background(), -1); err == nil {
		t.Fatal("Resume(-1) succeeded, want an error so the caller treats the state file as corrupt")
	}
}

func TestTailer_RejectsSourceThatReturnsIDsAtOrBehindTheCursor(t *testing.T) {
	src := &fakeSource{records: recordsFrom("a", "b"), repeatID: true}
	var applied []string
	tl := newTailer(t, src, &applied)
	if _, err := tl.Advance(context.Background()); err == nil {
		t.Fatal("Advance accepted a record at or behind the cursor; that would loop forever or double-count")
	}
}

func TestTailer_RejectsSourceThatReturnsDescendingIDs(t *testing.T) {
	src := &fakeSource{records: recordsFrom("a", "b", "c"), descend: true}
	var applied []string
	tl := newTailer(t, src, &applied)
	if _, err := tl.Advance(context.Background()); err == nil {
		t.Fatal("Advance accepted descending ids; the cursor would skip records")
	}
}

func TestTailer_PropagatesSourceErrors(t *testing.T) {
	sentinel := errors.New("boom")

	t.Run("MaxID", func(t *testing.T) {
		src := &fakeSource{maxErr: sentinel}
		var applied []string
		tl := newTailer(t, src, &applied)
		if _, err := tl.Advance(context.Background()); !errors.Is(err, sentinel) {
			t.Fatalf("Advance = %v, want the source error", err)
		}
		if err := tl.InitAtHead(context.Background()); !errors.Is(err, sentinel) {
			t.Fatalf("InitAtHead = %v, want the source error", err)
		}
	})

	t.Run("Records", func(t *testing.T) {
		src := &fakeSource{records: recordsFrom("a"), readErr: sentinel}
		var applied []string
		tl := newTailer(t, src, &applied)
		if _, err := tl.Advance(context.Background()); !errors.Is(err, sentinel) {
			t.Fatalf("Advance = %v, want the source error", err)
		}
	})
}

func TestTailer_ApplyErrorStopsAtTheFailedRecord(t *testing.T) {
	src := &fakeSource{records: recordsFrom("a", "b", "c")}
	var applied []string
	tl, err := tailcursor.New[string]("agent_events", src, func(v string) error {
		if v == "b" {
			return errors.New("apply failed")
		}
		applied = append(applied, v)
		return nil
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	n, err := tl.Advance(context.Background())
	if err == nil {
		t.Fatal("Advance succeeded despite an apply error")
	}
	if n != 1 || len(applied) != 1 {
		t.Fatalf("Advance applied %d (%v), want exactly the record before the failure", n, applied)
	}
	if tl.Cursor() != 1 {
		t.Fatalf("cursor = %d, want 1 — it must stop at the last successfully applied record", tl.Cursor())
	}
}

func TestTailer_AdvanceStopsOnContextCancellation(t *testing.T) {
	src := &fakeSource{records: recordsFrom("a", "b", "c")}
	var applied []string
	tl := newTailer(t, src, &applied)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tl.Advance(ctx); err == nil {
		t.Fatal("Advance on a cancelled context succeeded, want an error")
	}
}

func TestNew_ValidatesArguments(t *testing.T) {
	src := &fakeSource{}
	apply := func(string) error { return nil }

	if _, err := tailcursor.New[string]("", src, apply); err == nil {
		t.Error("New with an empty name succeeded")
	}
	if _, err := tailcursor.New[string]("n", nil, apply); err == nil {
		t.Error("New with a nil source succeeded")
	}
	if _, err := tailcursor.New[string]("n", src, nil); err == nil {
		t.Error("New with a nil apply succeeded")
	}
}

// A daemon that runs several tailers over different row types holds them in
// one slice.
func TestTailer_SatisfiesTheNonGenericAdvancerInterface(t *testing.T) {
	strSrc := &fakeSource{records: recordsFrom("a")}
	intTailer, err := tailcursor.New[int]("ints", intSource{}, func(int) error { return nil })
	if err != nil {
		t.Fatalf("New[int]: %v", err)
	}
	var applied []string
	strTailer := newTailer(t, strSrc, &applied)

	all := []tailcursor.Advancer{strTailer, intTailer}
	for _, a := range all {
		if err := a.InitAtHead(context.Background()); err != nil {
			t.Fatalf("%s: InitAtHead: %v", a.Name(), err)
		}
		if _, err := a.Advance(context.Background()); err != nil {
			t.Fatalf("%s: Advance: %v", a.Name(), err)
		}
	}
	if len(all) != 2 {
		t.Fatal("unreachable")
	}
}

type intSource struct{}

func (intSource) MaxID(context.Context) (int64, error) { return 0, nil }
func (intSource) Records(context.Context, int64, int) ([]tailcursor.Record[int], error) {
	return nil, nil
}

func TestCorruptError_Unwraps(t *testing.T) {
	inner := errors.New("inner")
	err := error(&tailcursor.CorruptError{Path: "/tmp/x", Err: inner})
	if !errors.Is(err, inner) {
		t.Error("CorruptError does not unwrap to its cause")
	}
	if !strings.Contains(err.Error(), "/tmp/x") {
		t.Errorf("CorruptError message %q does not name the path", err.Error())
	}
	_ = fmt.Sprint(err)
}
