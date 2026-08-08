// Package metrics is a small, dependency-free Prometheus metric registry and
// text-exposition writer (issue #2700, parent #2699).
//
// # Why not github.com/prometheus/client_golang
//
// prism vendors its Go dependencies through nix (`vendorHash` in
// pkgs/prism.nix). The exposition format this exporter needs is a few dozen
// lines of text, and the registry below is under 400 lines. Pulling the
// upstream client library in for that would grow the vendor tree and the
// build surface for no gain. The types here mirror the upstream names
// (Registry, CounterVec, GaugeFunc) so the shape is familiar.
//
// # The counter contract
//
// A Prometheus counter must never decrease while the process lives.
// CounterVec enforces that mechanically: Add rejects a negative delta, and
// there is no Set. Producers of counter values must therefore accumulate
// forward — see internal/tailcursor for the mechanism prism uses, and
// #2699 section 3 for why a full-table SQL aggregate is not allowed to
// produce one.
//
// # The histogram seam (#2706)
//
// Gather does not switch on concrete collector types. It asks each
// Collector for its Kind and its Samples, and a Sample carries a name
// suffix. A future HistogramVec therefore needs no change here: it reports
// KindHistogram and emits "_bucket", "_sum", and "_count" samples. Keep it
// that way — histogram buckets cannot be changed after the fact without
// discarding history, so the registry must not be the thing that blocks
// adding them.
package metrics

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Kind is the Prometheus metric type of a collector, written verbatim into
// the "# TYPE" line.
type Kind string

const (
	KindGauge   Kind = "gauge"
	KindCounter Kind = "counter"
	// KindHistogram is reserved for #2706. Nothing implements it yet; it
	// exists so the exposition writer already has a name for it.
	KindHistogram Kind = "histogram"
)

// ContentType is the value to send in the Content-Type response header of a
// /metrics endpoint serving this exposition format.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

var (
	metricNameRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRe  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
)

// Sample is one exposed time series of a collector.
//
// Suffix is appended to the collector name for this sample. It is empty for
// a plain gauge or counter. A histogram uses "_bucket", "_sum", and
// "_count".
type Sample struct {
	Suffix      string
	LabelNames  []string
	LabelValues []string
	Value       float64
}

// Collector is one metric family in a Registry.
type Collector interface {
	Name() string
	Help() string
	Kind() Kind
	// Collect returns the current samples. It is called on every scrape,
	// so a gauge collector is free to recompute here — gauges carry no
	// monotonicity contract (see #2699 section 4).
	Collect() []Sample
}

// Persistent is implemented by a collector whose values must survive a
// process restart — counters, and later histograms.
//
// The registry does not persist anything itself. It only exposes the
// snapshot/restore pair so a caller (internal/exporter) can write the values
// into the same state file as its tail cursors, in one atomic write. That
// coupling is what makes a crash recoverable: the cursor and the counter
// values it produced are always saved together, so a resume from the saved
// cursor rebuilds exactly the saved values.
//
// The map key is an opaque, stable encoding of the label values. Callers
// must treat it as opaque and round-trip it unchanged.
type Persistent interface {
	Collector
	Snapshot() map[string]float64
	Restore(map[string]float64) error
}

// Registry holds a set of collectors and writes them out in the Prometheus
// text exposition format. A Registry is safe for concurrent use.
type Registry struct {
	mu     sync.RWMutex
	order  []Collector
	byName map[string]Collector
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byName: make(map[string]Collector)}
}

// Register adds c to the registry. It returns an error when the name is
// invalid or already registered.
func (r *Registry) Register(c Collector) error {
	if c == nil {
		return errors.New("metrics: Register(nil)")
	}
	name := c.Name()
	if !metricNameRe.MatchString(name) {
		return fmt.Errorf("metrics: invalid metric name %q", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.byName[name]; dup {
		return fmt.Errorf("metrics: metric %q is already registered", name)
	}
	r.byName[name] = c
	r.order = append(r.order, c)
	return nil
}

// MustRegister is Register, and panics on error. Use it for the fixed set of
// metrics a binary declares at start-up, where a duplicate name is a
// programming error rather than a runtime condition.
func (r *Registry) MustRegister(c Collector) {
	if err := r.Register(c); err != nil {
		panic(err)
	}
}

// Collectors returns the registered collectors, sorted by name.
func (r *Registry) Collectors() []Collector {
	r.mu.RLock()
	out := make([]Collector, len(r.order))
	copy(out, r.order)
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// PersistentCollectors returns the registered collectors that implement
// Persistent, sorted by name.
func (r *Registry) PersistentCollectors() []Persistent {
	var out []Persistent
	for _, c := range r.Collectors() {
		if p, ok := c.(Persistent); ok {
			out = append(out, p)
		}
	}
	return out
}

// Snapshot returns the persistable values of every Persistent collector,
// keyed by metric name. The result is safe to serialise directly.
func (r *Registry) Snapshot() map[string]map[string]float64 {
	out := make(map[string]map[string]float64)
	for _, p := range r.PersistentCollectors() {
		out[p.Name()] = p.Snapshot()
	}
	return out
}

// Restore loads previously snapshotted values back into the matching
// Persistent collectors. Entries for metrics that are not registered are
// ignored — that is the expected shape when an older state file is read by a
// newer binary that has renamed or dropped a metric.
func (r *Registry) Restore(snap map[string]map[string]float64) error {
	for _, p := range r.PersistentCollectors() {
		values, ok := snap[p.Name()]
		if !ok {
			continue
		}
		if err := p.Restore(values); err != nil {
			return fmt.Errorf("metrics: restore %q: %w", p.Name(), err)
		}
	}
	return nil
}

// Gather writes the whole registry to w in the Prometheus text exposition
// format. Output is deterministic: families are ordered by name, and samples
// within a family are ordered by their label values.
func (r *Registry) Gather(w io.Writer) error {
	var b strings.Builder
	for _, c := range r.Collectors() {
		samples := c.Collect()
		writeHeader(&b, c)
		if len(samples) == 0 {
			continue
		}
		sorted := make([]Sample, len(samples))
		copy(sorted, samples)
		sort.SliceStable(sorted, func(i, j int) bool {
			if sorted[i].Suffix != sorted[j].Suffix {
				return sorted[i].Suffix < sorted[j].Suffix
			}
			return lessLabelValues(sorted[i].LabelValues, sorted[j].LabelValues)
		})
		for _, s := range sorted {
			if err := writeSample(&b, c.Name(), s); err != nil {
				return err
			}
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

func writeHeader(b *strings.Builder, c Collector) {
	name := c.Name()
	if help := c.Help(); help != "" {
		b.WriteString("# HELP ")
		b.WriteString(name)
		b.WriteByte(' ')
		b.WriteString(escapeHelp(help))
		b.WriteByte('\n')
	}
	b.WriteString("# TYPE ")
	b.WriteString(name)
	b.WriteByte(' ')
	b.WriteString(string(c.Kind()))
	b.WriteByte('\n')
}

func writeSample(b *strings.Builder, family string, s Sample) error {
	if len(s.LabelNames) != len(s.LabelValues) {
		return fmt.Errorf("metrics: %s: %d label names but %d label values",
			family, len(s.LabelNames), len(s.LabelValues))
	}
	b.WriteString(family)
	b.WriteString(s.Suffix)
	if len(s.LabelNames) > 0 {
		b.WriteByte('{')
		for i, n := range s.LabelNames {
			if !labelNameRe.MatchString(n) {
				return fmt.Errorf("metrics: %s: invalid label name %q", family, n)
			}
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(n)
			b.WriteString(`="`)
			b.WriteString(escapeLabelValue(s.LabelValues[i]))
			b.WriteByte('"')
		}
		b.WriteByte('}')
	}
	b.WriteByte(' ')
	b.WriteString(formatValue(s.Value))
	b.WriteByte('\n')
	return nil
}

// formatValue renders a float in the form the text exposition format
// expects.
//
// The three non-finite cases are explicit for readability, not for
// correctness: strconv.FormatFloat already renders "+Inf", "-Inf", and
// "NaN", which are the spellings Prometheus wants. Naming them here means a
// reader does not have to know that.
func formatValue(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

func escapeLabelValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

func lessLabelValues(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// encodeLabelValues produces the opaque, stable map key used by
// CounterVec.Snapshot. JSON of the value slice is unambiguous for any label
// value (quotes, commas, and backslashes included) and round-trips exactly.
func encodeLabelValues(values []string) string {
	b, err := json.Marshal(values)
	if err != nil {
		// json.Marshal of a []string cannot fail.
		panic(fmt.Sprintf("metrics: encode label values: %v", err))
	}
	return string(b)
}

func decodeLabelValues(key string) ([]string, error) {
	var out []string
	if err := json.Unmarshal([]byte(key), &out); err != nil {
		return nil, fmt.Errorf("metrics: decode label-value key %q: %w", key, err)
	}
	return out, nil
}
