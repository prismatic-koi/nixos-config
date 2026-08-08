package metrics

import (
	"fmt"
	"math"
	"sync"
)

// CounterVec is a labelled Prometheus counter.
//
// It is deliberately write-restricted: Add refuses a negative delta and
// there is no Set. A counter that can move backwards is read by Prometheus
// as a process restart, and rate() / increase() then return wrong numbers
// across that boundary with no error anywhere (#2699 section 3). Restore is
// the single exception — it seeds the values from a persisted snapshot
// before the counter is first exposed, which is what makes a restart
// invisible to the scraper.
//
// A CounterVec is safe for concurrent use.
type CounterVec struct {
	name       string
	help       string
	labelNames []string
	normalise  func([]string) []string

	mu     sync.RWMutex
	values map[string]*counterEntry
}

type counterEntry struct {
	labelValues []string
	value       float64
}

// CounterOption customises a CounterVec.
type CounterOption func(*counterOptions)

type counterOptions struct {
	normalise func([]string) []string
}

// WithLabelValueNormaliser folds every label-value tuple through f before it
// becomes a series.
//
// This is the enforcement point for a bounded label set. A label name can be
// a closed set on paper and still be unbounded in practice, because the
// VALUE comes from data. agent_events.type is exactly that case: a sandboxed
// agent can write an arbitrary frame type through the harness pipe socket,
// and each distinct value would otherwise become a permanent series in a
// fleet-wide Prometheus.
//
// f is applied on Add, on Value, and on Restore, so no path can create a
// series that f would not produce — including a state file written by an
// older binary, or one an attacker reached. Restore sums the values of two
// source keys that fold to the same target, so the total is preserved and
// the counter still never decreases.
//
// f must be pure and must return a slice of the same length it is given.
func WithLabelValueNormaliser(f func([]string) []string) CounterOption {
	return func(o *counterOptions) { o.normalise = f }
}

// NewCounterVec returns a counter family. labelNames must be a closed,
// bounded set — see #2699 section 6. session_name, instance_id, and
// issue_ref are unbounded and must never appear here.
//
// A label NAME drawn from a closed set is necessary but not sufficient: when
// the label VALUE comes from data, pass WithLabelValueNormaliser to bound it.
func NewCounterVec(name, help string, labelNames []string, opts ...CounterOption) *CounterVec {
	names := make([]string, len(labelNames))
	copy(names, labelNames)
	var o counterOptions
	for _, opt := range opts {
		opt(&o)
	}
	return &CounterVec{
		name:       name,
		help:       help,
		labelNames: names,
		normalise:  o.normalise,
		values:     make(map[string]*counterEntry),
	}
}

// normaliseValues applies the configured normaliser, and verifies it kept the
// arity. A normaliser that changes the length would silently produce a sample
// whose label names and values do not line up.
func (c *CounterVec) normaliseValues(labelValues []string) ([]string, error) {
	if c.normalise == nil {
		return labelValues, nil
	}
	out := c.normalise(labelValues)
	if len(out) != len(labelValues) {
		return nil, fmt.Errorf("metrics: %s: label-value normaliser returned %d values for %d",
			c.name, len(out), len(labelValues))
	}
	return out, nil
}

func (c *CounterVec) Name() string { return c.name }
func (c *CounterVec) Help() string { return c.help }
func (c *CounterVec) Kind() Kind   { return KindCounter }

// LabelNames returns a copy of the label names of this family.
func (c *CounterVec) LabelNames() []string {
	out := make([]string, len(c.labelNames))
	copy(out, c.labelNames)
	return out
}

// Inc adds 1 to the series identified by labelValues.
func (c *CounterVec) Inc(labelValues ...string) error {
	return c.Add(1, labelValues...)
}

// Add adds delta to the series identified by labelValues, creating the
// series at zero first if it does not exist.
//
// Add returns an error when delta is negative or not finite, and when the
// number of label values does not match the number of label names. It never
// applies a partial update.
func (c *CounterVec) Add(delta float64, labelValues ...string) error {
	if len(labelValues) != len(c.labelNames) {
		return fmt.Errorf("metrics: %s: expected %d label values, got %d",
			c.name, len(c.labelNames), len(labelValues))
	}
	if math.IsNaN(delta) || math.IsInf(delta, 0) {
		return fmt.Errorf("metrics: %s: delta must be finite, got %v", c.name, delta)
	}
	if delta < 0 {
		return fmt.Errorf("metrics: %s: counters cannot decrease (delta %v)", c.name, delta)
	}

	labelValues, err := c.normaliseValues(labelValues)
	if err != nil {
		return err
	}
	key := encodeLabelValues(labelValues)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.values[key]
	if !ok {
		vals := make([]string, len(labelValues))
		copy(vals, labelValues)
		e = &counterEntry{labelValues: vals}
		c.values[key] = e
	}
	e.value += delta
	return nil
}

// Value returns the current value of one series and whether it exists. The
// arguments are folded through the normaliser first, so Value and Add always
// agree on which series they name.
func (c *CounterVec) Value(labelValues ...string) (float64, bool) {
	normalised, err := c.normaliseValues(labelValues)
	if err != nil {
		return 0, false
	}
	key := encodeLabelValues(normalised)
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.values[key]
	if !ok {
		return 0, false
	}
	return e.value, true
}

// Cardinality returns the number of distinct label-value tuples currently
// held. Useful as a guard rail in tests against a label set that turns out
// to be unbounded in practice.
func (c *CounterVec) Cardinality() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.values)
}

// Collect implements Collector. Both label slices are copies, so a caller
// cannot corrupt the family by mutating a returned Sample. GaugeFunc.Collect
// does the same.
func (c *CounterVec) Collect() []Sample {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Sample, 0, len(c.values))
	for _, e := range c.values {
		vals := make([]string, len(e.labelValues))
		copy(vals, e.labelValues)
		names := make([]string, len(c.labelNames))
		copy(names, c.labelNames)
		out = append(out, Sample{
			LabelNames:  names,
			LabelValues: vals,
			Value:       e.value,
		})
	}
	return out
}

// Snapshot implements Persistent.
func (c *CounterVec) Snapshot() map[string]float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]float64, len(c.values))
	for k, e := range c.values {
		out[k] = e.value
	}
	return out
}

// Restore implements Persistent. It replaces the in-memory values with the
// snapshot, which is correct only at start-up, before the counter has been
// exposed or incremented.
//
// A key whose label-value count does not match the family, or a value that
// is negative or not finite, makes Restore fail without changing anything.
// The caller treats that as a corrupt state file (see internal/tailcursor).
// A snapshot key is folded through the normaliser before it is applied, so a
// state file cannot reintroduce a series the normaliser would not produce.
// Two keys that fold to the same target are summed.
func (c *CounterVec) Restore(values map[string]float64) error {
	next := make(map[string]*counterEntry, len(values))
	for key, v := range values {
		labelValues, err := decodeLabelValues(key)
		if err != nil {
			return err
		}
		if len(labelValues) != len(c.labelNames) {
			return fmt.Errorf("metrics: %s: snapshot key %q has %d label values, want %d",
				c.name, key, len(labelValues), len(c.labelNames))
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("metrics: %s: snapshot key %q has non-finite value %v", c.name, key, v)
		}
		if v < 0 {
			return fmt.Errorf("metrics: %s: snapshot key %q has negative value %v", c.name, key, v)
		}
		labelValues, err = c.normaliseValues(labelValues)
		if err != nil {
			return err
		}
		normalisedKey := encodeLabelValues(labelValues)
		if existing, ok := next[normalisedKey]; ok {
			existing.value += v
			continue
		}
		next[normalisedKey] = &counterEntry{labelValues: labelValues, value: v}
	}
	c.mu.Lock()
	c.values = next
	c.mu.Unlock()
	return nil
}
