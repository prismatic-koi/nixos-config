// Package metricstest parses the Prometheus text exposition format so tests
// can assert that an exporter's output is well formed, rather than assert on
// substrings of it (issue #2700).
//
// It is a test-support package, in the shape of internal/sidecar/sidecartest.
// Nothing in the production build imports it.
//
// The grammar it accepts is the one Prometheus documents for the 0.0.4 text
// format:
//
//	# HELP <name> <help text>
//	# TYPE <name> <counter|gauge|histogram|summary|untyped>
//	<name>[{<label>="<value>"[,...]}] <value>[ <timestamp>]
//
// It is deliberately strict. A parser that shrugs at a malformed line would
// let the exporter ship output that Prometheus rejects, which is the exact
// failure the test exists to catch.
package metricstest

import (
	"bufio"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var (
	metricNameRe = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)
	labelNameRe  = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

	validTypes = map[string]bool{
		"counter":   true,
		"gauge":     true,
		"histogram": true,
		"summary":   true,
		"untyped":   true,
	}
)

// Sample is one parsed time series.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// Label returns the value of one label and whether it is present.
func (s Sample) Label(name string) (string, bool) {
	v, ok := s.Labels[name]
	return v, ok
}

// Family is one parsed metric family: its declared type, its help text, and
// its samples.
type Family struct {
	Name    string
	Type    string
	Help    string
	Samples []Sample
}

// Exposition is a whole parsed /metrics body.
type Exposition struct {
	Families map[string]*Family
}

// Family returns the named family, failing the test when it is absent.
func (e *Exposition) Family(t testing.TB, name string) *Family {
	t.Helper()
	f, ok := e.Families[name]
	if !ok {
		t.Fatalf("metricstest: no metric family %q in exposition; have %v", name, e.FamilyNames())
	}
	return f
}

// FamilyNames returns every family name, sorted.
func (e *Exposition) FamilyNames() []string {
	names := make([]string, 0, len(e.Families))
	for n := range e.Families {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Value returns the value of the single sample of family name whose labels
// match every entry in match, and whether exactly one such sample exists.
func (e *Exposition) Value(name string, match map[string]string) (float64, bool) {
	f, ok := e.Families[name]
	if !ok {
		return 0, false
	}
	var (
		found float64
		count int
	)
	for _, s := range f.Samples {
		matched := true
		for k, v := range match {
			if s.Labels[k] != v {
				matched = false
				break
			}
		}
		if matched {
			found = s.Value
			count++
		}
	}
	if count != 1 {
		return 0, false
	}
	return found, true
}

// MustParse parses body and fails the test on any grammar violation.
func MustParse(t testing.TB, body string) *Exposition {
	t.Helper()
	e, err := Parse(body)
	if err != nil {
		t.Fatalf("metricstest: exposition does not parse: %v\n--- body ---\n%s", err, body)
	}
	return e
}

// Parse parses body in the Prometheus text exposition format.
func Parse(body string) (*Exposition, error) {
	out := &Exposition{Families: make(map[string]*Family)}
	// A duplicate HELP or TYPE line for one family is rejected below, via the
	// non-empty checks in parseComment. Prometheus rejects it too. Interleaved
	// families are accepted: the format permits them and no exporter this
	// parser serves emits them.
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		trimmed := strings.TrimRight(line, "\r")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			if err := parseComment(out, trimmed); err != nil {
				return nil, fmt.Errorf("line %d: %w", lineNo, err)
			}
			continue
		}
		if err := parseSample(out, trimmed); err != nil {
			return nil, fmt.Errorf("line %d: %w", lineNo, err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func parseComment(out *Exposition, line string) error {
	rest := strings.TrimSpace(strings.TrimPrefix(line, "#"))
	switch {
	case strings.HasPrefix(rest, "HELP "):
		name, help, _ := strings.Cut(strings.TrimPrefix(rest, "HELP "), " ")
		if !metricNameRe.MatchString(name) {
			return fmt.Errorf("HELP for invalid metric name %q", name)
		}
		f := out.family(name)
		if f.Help != "" {
			return fmt.Errorf("duplicate HELP for %q", name)
		}
		f.Help = help
		return nil
	case strings.HasPrefix(rest, "TYPE "):
		name, typ, ok := strings.Cut(strings.TrimPrefix(rest, "TYPE "), " ")
		if !ok {
			return fmt.Errorf("TYPE line for %q has no type", name)
		}
		if !metricNameRe.MatchString(name) {
			return fmt.Errorf("TYPE for invalid metric name %q", name)
		}
		typ = strings.TrimSpace(typ)
		if !validTypes[typ] {
			return fmt.Errorf("metric %q has unknown type %q", name, typ)
		}
		f := out.family(name)
		if f.Type != "" {
			return fmt.Errorf("duplicate TYPE for %q", name)
		}
		f.Type = typ
		return nil
	default:
		// A bare "# ..." comment is legal and carries no meaning.
		return nil
	}
}

func parseSample(out *Exposition, line string) error {
	name, rest, err := splitSampleName(line)
	if err != nil {
		return err
	}
	if !metricNameRe.MatchString(name) {
		return fmt.Errorf("invalid metric name %q", name)
	}

	labels := map[string]string{}
	rest = strings.TrimSpace(rest)
	if strings.HasPrefix(rest, "{") {
		closing := strings.LastIndex(rest, "}")
		if closing < 0 {
			return fmt.Errorf("metric %q: label set is not closed", name)
		}
		labels, err = parseLabels(rest[1:closing])
		if err != nil {
			return fmt.Errorf("metric %q: %w", name, err)
		}
		rest = strings.TrimSpace(rest[closing+1:])
	}

	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return fmt.Errorf("metric %q: no value", name)
	}
	if len(fields) > 2 {
		return fmt.Errorf("metric %q: trailing content after value: %q", name, rest)
	}
	value, err := parseValue(fields[0])
	if err != nil {
		return fmt.Errorf("metric %q: %w", name, err)
	}
	if len(fields) == 2 {
		if _, err := strconv.ParseInt(fields[1], 10, 64); err != nil {
			return fmt.Errorf("metric %q: invalid timestamp %q", name, fields[1])
		}
	}

	// Attribute the sample to its family. A histogram or summary emits
	// samples under a suffixed name, so fall back to the suffix-stripped
	// family when the exact name has no declaration.
	familyName := name
	if _, declared := out.Families[name]; !declared {
		if base, ok := suffixBase(out, name); ok {
			familyName = base
		}
	}
	f := out.family(familyName)
	f.Samples = append(f.Samples, Sample{Name: name, Labels: labels, Value: value})
	return nil
}

// splitSampleName splits the leading metric name off a sample line.
func splitSampleName(line string) (string, string, error) {
	for i, r := range line {
		if r == '{' || r == ' ' || r == '\t' {
			return line[:i], line[i:], nil
		}
	}
	return "", "", fmt.Errorf("sample line %q has no value", line)
}

func suffixBase(out *Exposition, name string) (string, bool) {
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if !strings.HasSuffix(name, suffix) {
			continue
		}
		base := strings.TrimSuffix(name, suffix)
		if f, ok := out.Families[base]; ok && (f.Type == "histogram" || f.Type == "summary") {
			return base, true
		}
	}
	return "", false
}

func parseLabels(s string) (map[string]string, error) {
	labels := map[string]string{}
	i := 0
	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == ',') {
			i++
		}
		if i >= len(s) {
			break
		}
		start := i
		for i < len(s) && s[i] != '=' {
			i++
		}
		if i >= len(s) {
			return nil, fmt.Errorf("label %q has no value", strings.TrimSpace(s[start:]))
		}
		name := strings.TrimSpace(s[start:i])
		if !labelNameRe.MatchString(name) {
			return nil, fmt.Errorf("invalid label name %q", name)
		}
		i++ // consume '='
		if i >= len(s) || s[i] != '"' {
			return nil, fmt.Errorf("label %q value is not quoted", name)
		}
		i++ // consume opening quote
		var value strings.Builder
		closed := false
		for i < len(s) {
			c := s[i]
			if c == '\\' {
				if i+1 >= len(s) {
					return nil, fmt.Errorf("label %q value ends in a dangling escape", name)
				}
				switch s[i+1] {
				case '\\':
					value.WriteByte('\\')
				case '"':
					value.WriteByte('"')
				case 'n':
					value.WriteByte('\n')
				default:
					return nil, fmt.Errorf("label %q value has invalid escape \\%c", name, s[i+1])
				}
				i += 2
				continue
			}
			if c == '"' {
				closed = true
				i++
				break
			}
			if c == '\n' {
				return nil, fmt.Errorf("label %q value contains a raw newline", name)
			}
			value.WriteByte(c)
			i++
		}
		if !closed {
			return nil, fmt.Errorf("label %q value is not terminated", name)
		}
		if _, dup := labels[name]; dup {
			return nil, fmt.Errorf("duplicate label %q", name)
		}
		labels[name] = value.String()
	}
	return labels, nil
}

func parseValue(s string) (float64, error) {
	switch s {
	case "+Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	case "NaN":
		return math.NaN(), nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	return v, nil
}

func (e *Exposition) family(name string) *Family {
	f, ok := e.Families[name]
	if !ok {
		f = &Family{Name: name}
		e.Families[name] = f
	}
	return f
}
