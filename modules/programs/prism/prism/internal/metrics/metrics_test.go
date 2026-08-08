package metrics_test

import (
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/prismatic-koi/prism/internal/metrics"
	"github.com/prismatic-koi/prism/internal/metrics/metricstest"
)

func TestRegistry_GatherProducesParseablePrometheusText(t *testing.T) {
	r := metrics.NewRegistry()
	r.MustRegister(metrics.NewGaugeFunc(
		"prism_test_build_info", "Build info.",
		[]string{"version"}, []string{"1.2.3"},
		func() float64 { return 1 },
	))
	c := metrics.NewCounterVec("prism_test_total", "A counter.", []string{"type"})
	r.MustRegister(c)
	if err := c.Add(3, "tool_call"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := c.Inc("turn_start"); err != nil {
		t.Fatalf("Inc: %v", err)
	}

	var b strings.Builder
	if err := r.Gather(&b); err != nil {
		t.Fatalf("Gather: %v", err)
	}
	exp := metricstest.MustParse(t, b.String())

	if got := exp.Family(t, "prism_test_total").Type; got != "counter" {
		t.Errorf("counter family declared as %q, want counter", got)
	}
	if got := exp.Family(t, "prism_test_build_info").Type; got != "gauge" {
		t.Errorf("gauge family declared as %q, want gauge", got)
	}
	if v, ok := exp.Value("prism_test_total", map[string]string{"type": "tool_call"}); !ok || v != 3 {
		t.Errorf("prism_test_total{type=tool_call} = %v (found=%v), want 3", v, ok)
	}
	if v, ok := exp.Value("prism_test_total", map[string]string{"type": "turn_start"}); !ok || v != 1 {
		t.Errorf("prism_test_total{type=turn_start} = %v (found=%v), want 1", v, ok)
	}
	if v, ok := exp.Value("prism_test_build_info", map[string]string{"version": "1.2.3"}); !ok || v != 1 {
		t.Errorf("prism_test_build_info{version=1.2.3} = %v (found=%v), want 1", v, ok)
	}
}

func TestRegistry_GatherIsDeterministic(t *testing.T) {
	r := metrics.NewRegistry()
	c := metrics.NewCounterVec("z_total", "z", []string{"type"})
	g := metrics.NewGaugeFunc("a_gauge", "a", nil, nil, func() float64 { return 7 })
	r.MustRegister(c)
	r.MustRegister(g)
	for _, v := range []string{"delta", "alpha", "charlie", "bravo"} {
		if err := c.Inc(v); err != nil {
			t.Fatalf("Inc(%q): %v", v, err)
		}
	}

	var first strings.Builder
	if err := r.Gather(&first); err != nil {
		t.Fatalf("Gather: %v", err)
	}
	for i := 0; i < 5; i++ {
		var again strings.Builder
		if err := r.Gather(&again); err != nil {
			t.Fatalf("Gather: %v", err)
		}
		if again.String() != first.String() {
			t.Fatalf("Gather output is not stable across calls:\n--- first ---\n%s\n--- again ---\n%s",
				first.String(), again.String())
		}
	}
	// Families sorted by name: a_gauge before z_total.
	if idxA, idxZ := strings.Index(first.String(), "a_gauge"), strings.Index(first.String(), "z_total"); idxA > idxZ {
		t.Errorf("families are not sorted by name:\n%s", first.String())
	}
}

func TestCounterVec_RejectsEveryDecreasingWrite(t *testing.T) {
	c := metrics.NewCounterVec("prism_test_total", "help", []string{"type"})
	if err := c.Add(5, "x"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	for _, tc := range []struct {
		name  string
		delta float64
	}{
		{"negative", -1},
		{"negative fraction", -0.0001},
		{"negative infinity", math.Inf(-1)},
		{"positive infinity", math.Inf(1)},
		{"NaN", math.NaN()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := c.Add(tc.delta, "x"); err == nil {
				t.Fatalf("Add(%v) succeeded; a counter must not accept it", tc.delta)
			}
			if v, _ := c.Value("x"); v != 5 {
				t.Fatalf("rejected Add still changed the value: got %v, want 5", v)
			}
		})
	}
}

func TestCounterVec_RejectsWrongLabelCount(t *testing.T) {
	c := metrics.NewCounterVec("prism_test_total", "help", []string{"type"})
	if err := c.Inc(); err == nil {
		t.Error("Inc with no label values succeeded, want an error")
	}
	if err := c.Inc("a", "b"); err == nil {
		t.Error("Inc with two label values succeeded, want an error")
	}
	if got := c.Cardinality(); got != 0 {
		t.Errorf("Cardinality = %d after only invalid writes, want 0", got)
	}
}

func TestCounterVec_SnapshotRestoreRoundTrip(t *testing.T) {
	src := metrics.NewCounterVec("prism_test_total", "help", []string{"type"})
	// Label values chosen to break a naive delimiter-joined key.
	for _, v := range []string{`plain`, `has,comma`, `has"quote`, `has\backslash`, ``} {
		if err := src.Add(2, v); err != nil {
			t.Fatalf("Add(%q): %v", v, err)
		}
	}
	snap := src.Snapshot()

	dst := metrics.NewCounterVec("prism_test_total", "help", []string{"type"})
	if err := dst.Restore(snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got, want := dst.Cardinality(), src.Cardinality(); got != want {
		t.Fatalf("restored cardinality %d, want %d", got, want)
	}
	for _, v := range []string{`plain`, `has,comma`, `has"quote`, `has\backslash`, ``} {
		got, ok := dst.Value(v)
		if !ok || got != 2 {
			t.Errorf("restored value for %q = %v (found=%v), want 2", v, got, ok)
		}
	}
}

func TestCounterVec_RestoreRejectsUnusableSnapshots(t *testing.T) {
	for _, tc := range []struct {
		name string
		snap map[string]float64
	}{
		{"not JSON", map[string]float64{"not-json": 1}},
		{"wrong label count", map[string]float64{`["a","b"]`: 1}},
		{"negative value", map[string]float64{`["a"]`: -1}},
		{"NaN value", map[string]float64{`["a"]`: math.NaN()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := metrics.NewCounterVec("prism_test_total", "help", []string{"type"})
			if err := c.Restore(tc.snap); err == nil {
				t.Fatal("Restore succeeded on an unusable snapshot, want an error")
			}
			if got := c.Cardinality(); got != 0 {
				t.Errorf("failed Restore left %d series behind, want 0", got)
			}
		})
	}
}

// A label name drawn from a closed set is not enough when the VALUE comes
// from data. The normaliser is the enforcement point, and it has to cover
// every write path — Add, Value, and Restore — or a state file becomes a way
// to reintroduce a series the normaliser would never produce.
func TestCounterVec_LabelValueNormaliserBoundsEveryWritePath(t *testing.T) {
	allowed := map[string]bool{"alpha": true, "beta": true}
	fold := func(vals []string) []string {
		if len(vals) != 1 || !allowed[vals[0]] {
			return []string{"other"}
		}
		return vals
	}

	t.Run("Add folds", func(t *testing.T) {
		c := metrics.NewCounterVec("t_total", "h", []string{"k"}, metrics.WithLabelValueNormaliser(fold))
		for i := 0; i < 1000; i++ {
			if err := c.Inc("junk" + strconv.Itoa(i)); err != nil {
				t.Fatalf("Inc: %v", err)
			}
		}
		if err := c.Inc("alpha"); err != nil {
			t.Fatalf("Inc: %v", err)
		}
		if got := c.Cardinality(); got != 2 {
			t.Fatalf("cardinality = %d after 1000 distinct junk values, want 2", got)
		}
		if v, ok := c.Value("other"); !ok || v != 1000 {
			t.Errorf("other = %v (found=%v), want 1000", v, ok)
		}
		if v, ok := c.Value("alpha"); !ok || v != 1 {
			t.Errorf("alpha = %v (found=%v), want 1", v, ok)
		}
	})

	t.Run("Value folds", func(t *testing.T) {
		c := metrics.NewCounterVec("t_total", "h", []string{"k"}, metrics.WithLabelValueNormaliser(fold))
		if err := c.Inc("junk"); err != nil {
			t.Fatalf("Inc: %v", err)
		}
		// Reading back with the raw value must find the folded series, or
		// Add and Value disagree about which series they name.
		if v, ok := c.Value("junk"); !ok || v != 1 {
			t.Errorf("Value(junk) = %v (found=%v), want the folded series with 1", v, ok)
		}
	})

	t.Run("Restore folds and sums", func(t *testing.T) {
		c := metrics.NewCounterVec("t_total", "h", []string{"k"}, metrics.WithLabelValueNormaliser(fold))
		snap := map[string]float64{
			`["alpha"]`:    5,
			`["poison_a"]`: 3,
			`["poison_b"]`: 4,
			`["other"]`:    1,
		}
		if err := c.Restore(snap); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if got := c.Cardinality(); got != 2 {
			t.Fatalf("cardinality = %d after restoring 2 hostile keys, want 2", got)
		}
		if v, ok := c.Value("alpha"); !ok || v != 5 {
			t.Errorf("alpha = %v (found=%v), want 5", v, ok)
		}
		// 3 + 4 + 1 — folding must sum, never drop, or the counter would
		// decrease across the restart.
		if v, ok := c.Value("other"); !ok || v != 8 {
			t.Errorf("other = %v (found=%v), want 8 (3+4+1)", v, ok)
		}
	})

	t.Run("Snapshot round-trips the folded keys", func(t *testing.T) {
		c := metrics.NewCounterVec("t_total", "h", []string{"k"}, metrics.WithLabelValueNormaliser(fold))
		if err := c.Inc("junk"); err != nil {
			t.Fatalf("Inc: %v", err)
		}
		for key := range c.Snapshot() {
			if strings.Contains(key, "junk") {
				t.Errorf("snapshot key %q carries the unfolded value; the state file would leak it", key)
			}
		}
	})
}

func TestCounterVec_NormaliserThatChangesArityIsRejected(t *testing.T) {
	c := metrics.NewCounterVec("t_total", "h", []string{"k"},
		metrics.WithLabelValueNormaliser(func([]string) []string { return []string{"a", "b"} }))
	if err := c.Inc("x"); err == nil {
		t.Fatal("Inc succeeded with a normaliser that changed the label count")
	}
	if got := c.Cardinality(); got != 0 {
		t.Errorf("cardinality = %d after a rejected write, want 0", got)
	}
}

func TestGaugeFunc_IsEvaluatedOnEveryGather(t *testing.T) {
	calls := 0
	r := metrics.NewRegistry()
	r.MustRegister(metrics.NewGaugeFunc("prism_test_gauge", "help", nil, nil, func() float64 {
		calls++
		return float64(calls)
	}))

	for want := 1; want <= 3; want++ {
		var b strings.Builder
		if err := r.Gather(&b); err != nil {
			t.Fatalf("Gather: %v", err)
		}
		exp := metricstest.MustParse(t, b.String())
		v, ok := exp.Value("prism_test_gauge", nil)
		if !ok {
			t.Fatalf("gauge missing from exposition:\n%s", b.String())
		}
		if v != float64(want) {
			t.Fatalf("gauge value %v on gather %d, want %d — the function is not being re-evaluated", v, want, want)
		}
	}
}

func TestRegistry_RejectsDuplicateAndInvalidNames(t *testing.T) {
	r := metrics.NewRegistry()
	r.MustRegister(metrics.NewCounterVec("prism_test_total", "help", nil))

	if err := r.Register(metrics.NewCounterVec("prism_test_total", "help", nil)); err == nil {
		t.Error("registering a duplicate name succeeded, want an error")
	}
	if err := r.Register(metrics.NewCounterVec("not a valid name", "help", nil)); err == nil {
		t.Error("registering an invalid metric name succeeded, want an error")
	}
	if err := r.Register(nil); err == nil {
		t.Error("registering nil succeeded, want an error")
	}
}

func TestRegistry_EscapesHelpAndLabelValues(t *testing.T) {
	r := metrics.NewRegistry()
	c := metrics.NewCounterVec("prism_test_total", "help with \\ and \n newline", []string{"type"})
	r.MustRegister(c)
	if err := c.Inc("value with \" quote, \\ backslash and \n newline"); err != nil {
		t.Fatalf("Inc: %v", err)
	}

	var b strings.Builder
	if err := r.Gather(&b); err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if strings.Count(b.String(), "\n") != 3 {
		t.Fatalf("escaped output should be exactly 3 lines (HELP, TYPE, sample), got:\n%q", b.String())
	}
	exp := metricstest.MustParse(t, b.String())
	f := exp.Family(t, "prism_test_total")
	if len(f.Samples) != 1 {
		t.Fatalf("got %d samples, want 1", len(f.Samples))
	}
	if got := f.Samples[0].Labels["type"]; got != "value with \" quote, \\ backslash and \n newline" {
		t.Errorf("label value did not round-trip through escaping: %q", got)
	}
}

func TestRegistry_SnapshotAndRestoreCoverPersistentCollectorsOnly(t *testing.T) {
	r := metrics.NewRegistry()
	c := metrics.NewCounterVec("prism_test_total", "help", []string{"type"})
	r.MustRegister(c)
	r.MustRegister(metrics.NewGaugeFunc("prism_test_gauge", "help", nil, nil, func() float64 { return 42 }))
	if err := c.Add(9, "x"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	snap := r.Snapshot()
	if _, ok := snap["prism_test_gauge"]; ok {
		t.Error("a gauge appeared in the persistable snapshot; gauges are recomputed, not persisted")
	}
	if len(snap) != 1 {
		t.Fatalf("snapshot has %d entries, want 1: %v", len(snap), snap)
	}

	r2 := metrics.NewRegistry()
	c2 := metrics.NewCounterVec("prism_test_total", "help", []string{"type"})
	r2.MustRegister(c2)
	// An entry for a metric the new registry does not have must be ignored,
	// not rejected — that is the shape of an old state file read by a newer
	// binary.
	snap["prism_dropped_total"] = map[string]float64{`["y"]`: 1}
	if err := r2.Restore(snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if v, ok := c2.Value("x"); !ok || v != 9 {
		t.Errorf("restored value = %v (found=%v), want 9", v, ok)
	}
}

func TestCounterVec_ConcurrentAddIsRaceFree(t *testing.T) {
	c := metrics.NewCounterVec("prism_test_total", "help", []string{"type"})
	const goroutines, perGoroutine = 8, 200

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				if err := c.Inc("x"); err != nil {
					t.Errorf("Inc: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if v, _ := c.Value("x"); v != goroutines*perGoroutine {
		t.Errorf("value = %v, want %d", v, goroutines*perGoroutine)
	}
}
