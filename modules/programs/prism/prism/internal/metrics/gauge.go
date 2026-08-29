package metrics

// GaugeFunc is a gauge whose value is produced by a function on every
// scrape.
//
// Gauges are the free half of the exporter architecture: they are
// point-in-time by definition, carry no monotonicity contract, and so the
// 90-day prune cannot hurt them. Recompute them at scrape time — including
// with a plain SQL aggregate, which a counter must never do.
//
// GaugeFunc carries a fixed label set. That is enough for a build-info
// style metric and for the single-series gauges the exporter adds. A
// labelled, multi-series gauge (GaugeVec) is not built here because nothing
// needs one yet; it slots in as another Collector when it does.
type GaugeFunc struct {
	name        string
	help        string
	labelNames  []string
	labelValues []string
	fn          func() float64
}

// NewGaugeFunc returns a gauge that calls fn on every Collect. labels is
// applied in the given order; it must be a closed, bounded set.
func NewGaugeFunc(name, help string, labelNames, labelValues []string, fn func() float64) *GaugeFunc {
	names := make([]string, len(labelNames))
	copy(names, labelNames)
	values := make([]string, len(labelValues))
	copy(values, labelValues)
	return &GaugeFunc{
		name:        name,
		help:        help,
		labelNames:  names,
		labelValues: values,
		fn:          fn,
	}
}

func (g *GaugeFunc) Name() string { return g.name }
func (g *GaugeFunc) Help() string { return g.help }
func (g *GaugeFunc) Kind() Kind   { return KindGauge }

// Collect implements Collector. A nil fn collects nothing, so a
// half-constructed gauge cannot panic a scrape.
//
// Both label slices are copies, so a caller cannot corrupt the gauge by
// mutating a returned Sample. CounterVec.Collect does the same.
func (g *GaugeFunc) Collect() []Sample {
	if g.fn == nil {
		return nil
	}
	names := make([]string, len(g.labelNames))
	copy(names, g.labelNames)
	values := make([]string, len(g.labelValues))
	copy(values, g.labelValues)
	return []Sample{{
		LabelNames:  names,
		LabelValues: values,
		Value:       g.fn(),
	}}
}
