package metricstest_test

import (
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/metrics/metricstest"
)

// The parser is the assertion behind "the output parses as Prometheus text
// format". A parser that accepts anything makes that assertion vacuous, so
// prove it rejects malformed input first.
func TestParse_RejectsMalformedExposition(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"unknown metric type", "# TYPE x_total bogus\nx_total 1\n"},
		{"invalid metric name", "1_bad_name 1\n"},
		{"sample with no value", "x_total\n"},
		{"non-numeric value", "x_total abc\n"},
		{"unterminated label set", `x_total{type="a" 1` + "\n"},
		{"unquoted label value", "x_total{type=a} 1\n"},
		{"invalid label name", `x_total{1bad="a"} 1` + "\n"},
		{"duplicate label", `x_total{type="a",type="b"} 1` + "\n"},
		{"dangling escape", `x_total{type="a\` + "\"} 1\n"},
		{"invalid escape", `x_total{type="a\q"} 1` + "\n"},
		{"duplicate TYPE", "# TYPE x_total counter\n# TYPE x_total gauge\n"},
		{"duplicate HELP", "# HELP x_total a\n# HELP x_total b\n"},
		{"trailing content after value", "x_total 1 2 3\n"},
		{"non-integer timestamp", "x_total 1 1.5\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := metricstest.Parse(tc.body); err == nil {
				t.Fatalf("Parse accepted malformed input %q", tc.body)
			}
		})
	}
}

func TestParse_AcceptsWellFormedExposition(t *testing.T) {
	body := strings.Join([]string{
		"# HELP prism_agent_events_total Total events.",
		"# TYPE prism_agent_events_total counter",
		`prism_agent_events_total{type="tool_call"} 12`,
		`prism_agent_events_total{type="turn_start"} 3 1700000000000`,
		"# HELP prism_exporter_build_info Build info.",
		"# TYPE prism_exporter_build_info gauge",
		`prism_exporter_build_info{version="dev",go_version="go1.26.1"} 1`,
		"# a bare comment",
		"",
	}, "\n")

	exp := metricstest.MustParse(t, body)
	if got := len(exp.FamilyNames()); got != 2 {
		t.Fatalf("parsed %d families, want 2: %v", got, exp.FamilyNames())
	}
	if v, ok := exp.Value("prism_agent_events_total", map[string]string{"type": "tool_call"}); !ok || v != 12 {
		t.Errorf("tool_call value = %v (found=%v), want 12", v, ok)
	}
	if got := exp.Family(t, "prism_exporter_build_info").Type; got != "gauge" {
		t.Errorf("build info type = %q, want gauge", got)
	}
	if got := exp.Family(t, "prism_agent_events_total").Help; got != "Total events." {
		t.Errorf("help = %q, want %q", got, "Total events.")
	}
}

// The histogram seam: the parser must already attribute suffixed
// samples to their declared family, so a later change to the registry is not
// gated on a parser change too.
func TestParse_AttributesHistogramSuffixesToTheirFamily(t *testing.T) {
	body := strings.Join([]string{
		"# TYPE prism_latency_seconds histogram",
		`prism_latency_seconds_bucket{le="0.1"} 1`,
		`prism_latency_seconds_bucket{le="+Inf"} 2`,
		"prism_latency_seconds_sum 0.3",
		"prism_latency_seconds_count 2",
		"",
	}, "\n")

	exp := metricstest.MustParse(t, body)
	f := exp.Family(t, "prism_latency_seconds")
	if len(f.Samples) != 4 {
		t.Fatalf("histogram family has %d samples, want 4: %+v", len(f.Samples), f.Samples)
	}
	if got := len(exp.FamilyNames()); got != 1 {
		t.Errorf("suffixed samples created %d families, want 1: %v", got, exp.FamilyNames())
	}
}

func TestValue_RequiresExactlyOneMatch(t *testing.T) {
	body := strings.Join([]string{
		"# TYPE x_total counter",
		`x_total{a="1",b="1"} 5`,
		`x_total{a="1",b="2"} 7`,
		"",
	}, "\n")
	exp := metricstest.MustParse(t, body)

	if _, ok := exp.Value("x_total", map[string]string{"a": "1"}); ok {
		t.Error("Value matched two samples; it must report only an unambiguous match")
	}
	if v, ok := exp.Value("x_total", map[string]string{"a": "1", "b": "2"}); !ok || v != 7 {
		t.Errorf("Value = %v (found=%v), want 7", v, ok)
	}
	if _, ok := exp.Value("missing_total", nil); ok {
		t.Error("Value found a missing family")
	}
}
