package exporter_test

// The cardinality boundary of #2699 section 6.
//
// agent_events.type is writable from inside a worker sandbox — the sidecar
// persists an unrecognised wire frame verbatim, and the harness pipe socket
// is bind-mounted read-write into the sandbox. So this file proves two
// things: the exposed series count is bounded whatever the database holds,
// and the allowlist does not silently drift away from what prism actually
// writes.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/exporter"
	"github.com/prismatic-koi/prism/internal/session"
)

func TestEventTypeLabel_FoldsUnknownValuesIntoOther(t *testing.T) {
	for _, known := range exporter.KnownEventTypes() {
		if got := exporter.EventTypeLabel(known); got != known {
			t.Errorf("EventTypeLabel(%q) = %q, want it unchanged", known, got)
		}
	}

	for _, unknown := range []string{
		"",
		"definitely_not_an_event_type",
		"tool_call ",                  // trailing space
		"Tool_Call",                   // different case
		strings.Repeat("a", 4096),     // a long value
		"x\ny",                        // an embedded newline
		`injected","evil="1`,          // an exposition-breaking value
		exporter.OtherEventType + "_", // near-miss on the bucket name
	} {
		if got := exporter.EventTypeLabel(unknown); got != exporter.OtherEventType {
			t.Errorf("EventTypeLabel(%q) = %q, want %q", unknown, got, exporter.OtherEventType)
		}
	}
}

func TestMaxAgentEventsSeries_IsTheAllowlistPlusOther(t *testing.T) {
	if want := len(exporter.KnownEventTypes()) + 1; exporter.MaxAgentEventsSeries != want {
		t.Errorf("MaxAgentEventsSeries = %d, want %d", exporter.MaxAgentEventsSeries, want)
	}
	for _, known := range exporter.KnownEventTypes() {
		if known == exporter.OtherEventType {
			t.Errorf("%q is both an allowlisted type and the fold bucket; the bound would be off by one",
				exporter.OtherEventType)
		}
	}
}

// The finding from review round 1: a sandboxed agent writes arbitrary frame
// types through the harness pipe, and each one becomes a permanent series in
// a fleet-wide Prometheus. The series count must stay bounded no matter what
// the table holds.
func TestExporter_SeriesCountIsBoundedAgainstHostileEventTypes(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	const injected = 500
	for i := 0; i < injected; i++ {
		h.writeEvent("injected_type_"+strconv.Itoa(i), 0)
	}
	// A handful of real ones, so the metric still carries signal.
	h.writeEvent("tool_call", 0)
	h.writeEvent("turn_start", 0)

	exp := h.scrape(h.exp)
	family := exp.Family(t, exporter.MetricAgentEventsTotal)

	if len(family.Samples) > exporter.MaxAgentEventsSeries {
		t.Fatalf("%d series after %d injected event types, want at most %d",
			len(family.Samples), injected, exporter.MaxAgentEventsSeries)
	}
	if v, ok := exp.Value(exporter.MetricAgentEventsTotal, map[string]string{"type": exporter.OtherEventType}); !ok || v != injected {
		t.Errorf("type=%q = %v (found=%v), want %d — every injected type must land in the bucket",
			exporter.OtherEventType, v, ok, injected)
	}
	if v, ok := exp.Value(exporter.MetricAgentEventsTotal, map[string]string{"type": "tool_call"}); !ok || v != 1 {
		t.Errorf("tool_call = %v (found=%v), want 1 — the fold must not disturb known types", v, ok)
	}
	for _, s := range family.Samples {
		if strings.HasPrefix(s.Labels["type"], "injected_type_") {
			t.Fatalf("an injected event type reached the exposition as a label: %q", s.Labels["type"])
		}
	}
}

// The bound has to survive the persistence path too, or one injection is
// permanent. A state file holding hostile keys must fold on restore.
func TestExporter_PoisonedStateFileCannotReintroduceSeries(t *testing.T) {
	h := newHarness(t)

	var b strings.Builder
	b.WriteString(`{"version":1,"cursors":{"agent_events":0},"counters":{"` +
		exporter.MetricAgentEventsTotal + `":{`)
	const poisoned = 300
	for i := 0; i < poisoned; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`"[\"poison_` + strconv.Itoa(i) + `\"]":1`)
	}
	b.WriteString(`,"[\"tool_call\"]":7`)
	b.WriteString("}}}")
	if err := os.WriteFile(h.statePath, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	h.start(h.exp)
	exp := h.scrape(h.exp)
	family := exp.Family(t, exporter.MetricAgentEventsTotal)

	if len(family.Samples) > exporter.MaxAgentEventsSeries {
		t.Fatalf("%d series restored from a state file holding %d hostile keys, want at most %d",
			len(family.Samples), poisoned, exporter.MaxAgentEventsSeries)
	}
	// The folded keys are summed, so no count is lost.
	if v, ok := exp.Value(exporter.MetricAgentEventsTotal, map[string]string{"type": exporter.OtherEventType}); !ok || v != poisoned {
		t.Errorf("type=%q = %v (found=%v), want %d — folding must sum, not drop",
			exporter.OtherEventType, v, ok, poisoned)
	}
	if v, ok := exp.Value(exporter.MetricAgentEventsTotal, map[string]string{"type": "tool_call"}); !ok || v != 7 {
		t.Errorf("tool_call = %v (found=%v), want 7 — a known type must restore unchanged", v, ok)
	}

	// And the rewritten state file is clean, so the fold is not re-paid on
	// every subsequent start.
	raw, err := os.ReadFile(h.statePath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(raw), "poison_") {
		t.Errorf("the rewritten state file still carries hostile keys: %s", raw)
	}
}

// A label value that would break the exposition format must not be able to
// reach it. The fold is the first defence and the escaping is the second;
// this checks the composition end to end.
func TestExporter_HostileEventTypeCannotCorruptTheExposition(t *testing.T) {
	h := newHarness(t)
	h.start(h.exp)

	for _, hostile := range []string{
		`evil"} 999` + "\n" + `prism_injected_total{a="b`,
		"line\nbreak",
		`quote"and\backslash`,
	} {
		h.writeEvent(hostile, 0)
	}

	exp := h.scrape(h.exp) // MustParse fails the test if the body is malformed
	if _, ok := exp.Families["prism_injected_total"]; ok {
		t.Fatal("a hostile event type injected a whole new metric family into the exposition")
	}
	// 2 from #2700 (build_info, agent_events_total) + 6 from #2703's
	// lifecycle and outcome counters + 4 from #2704 (three cost/token
	// counters and prism_account_info) + 4 from #2702's state gauges.
	if got := len(exp.FamilyNames()); got != 16 {
		t.Fatalf("exposition has %d families, want 16: %v", got, exp.FamilyNames())
	}
}

// The allowlist must not drift from what prism actually writes.
//
// Round 2 of review found this test passing while two real event types were
// missing: session.EventSpawnIntent and session.EventSpawnFailed reach
// db.Event.Type through the writeSpawnEvent HELPER, so the field value is a
// parameter and a writer-site scan can never resolve it. The convention pass
// below closes that hole — it looks at the constant declarations themselves,
// not at the writer sites.
//
// The verbatim `default:` path in internal/sidecar stays invisible by design.
// It takes whatever the wire hands it, which is exactly why OtherEventType
// exists.
func TestKnownEventTypes_CoversEveryStaticallyWrittenType(t *testing.T) {
	known := map[string]bool{}
	for _, k := range exporter.KnownEventTypes() {
		known[k] = true
	}

	// A direct assertion on the constants this package can import, so the
	// scan is not the only thing standing between the allowlist and a drift.
	for _, c := range []struct{ name, value string }{
		{"db.SessionReapEventType", db.SessionReapEventType},
		{"session.EventSpawnIntent", session.EventSpawnIntent},
		{"session.EventSpawnFailed", session.EventSpawnFailed},
	} {
		if !known[c.value] {
			t.Errorf("%s (%q) is not in the allowlist", c.name, c.value)
		}
	}

	writers, constants := scanEventTypes(t)
	if len(writers) == 0 {
		t.Fatal("the source scan found no event-type writers; the test would pass vacuously")
	}
	if len(constants) == 0 {
		t.Fatal("the source scan found no event-type constants; the convention pass would pass vacuously")
	}

	for eventType, where := range writers {
		if !known[eventType] {
			t.Errorf("%s writes agent_events.type = %q, which is not in knownEventTypes "+
				"(it would be counted as %q). Add it to eventtypes.go.",
				where, eventType, exporter.OtherEventType)
		}
	}
	for eventType, where := range constants {
		if !known[eventType] {
			t.Errorf("%s declares the event type %q, which is not in knownEventTypes "+
				"(it would be counted as %q). Add it to eventtypes.go.",
				where, eventType, exporter.OtherEventType)
		}
	}
	t.Logf("scanned %d writer-site event types and %d event-type constants", len(writers), len(constants))
}

// The convention pass is the half that catches a constant reaching
// db.Event.Type through a helper parameter. Prove it is not vacuous: it must
// actually find the two constants whose absence round 2 caught.
func TestEventTypeConstantScan_FindsTheHelperForwardedConstants(t *testing.T) {
	_, constants := scanEventTypes(t)
	for _, want := range []string{session.EventSpawnIntent, session.EventSpawnFailed, db.SessionReapEventType} {
		if _, ok := constants[want]; !ok {
			t.Errorf("the constant scan did not find %q; it cannot guard against that type drifting out of the allowlist", want)
		}
	}
	// A writer-site scan alone cannot see the spawn constants, which is the
	// gap the convention pass exists to close.
	writers, _ := scanEventTypes(t)
	if _, ok := writers[session.EventSpawnIntent]; ok {
		t.Logf("note: %q is now reachable from a writer site too", session.EventSpawnIntent)
	}
}

// eventTypeConstNameRe matches the naming convention prism uses for an
// agent_events.type constant: EventSpawnIntent, SessionReapEventType.
var eventTypeConstNameRe = regexp.MustCompile(`^(Event[A-Z]\w*|\w*EventType)$`)

// scanEventTypes walks the prism source and returns two maps of event type to
// the file:line that produced it.
//
// writers covers the writer sites:
//
//   - writeEvent("<type>", ...) / writeEvent(someConst, ...) — the helper in
//     internal/sidecar and its siblings.
//   - db.Event{... Type: <value> ...} — every other writer. Inside package db
//     the type is spelled `Event`, so that form is accepted only for files
//     under internal/db.
//
// An identifier or qualified identifier at either site is resolved against
// the package-level string constants of the tree.
//
// constants covers every package-level string constant whose NAME follows the
// event-type convention, wherever it is declared. This is the pass that
// catches a constant reaching db.Event.Type through a helper PARAMETER, which
// no writer-site scan can follow.
func scanEventTypes(t *testing.T) (writers, constants map[string]string) {
	t.Helper()
	writers = map[string]string{}
	constants = map[string]string{}
	fset := token.NewFileSet()

	// go test runs with the package directory as the working directory, so
	// the module root is two levels up.
	moduleRoot := filepath.Join("..", "..")
	roots := []string{filepath.Join(moduleRoot, "cmd"), filepath.Join(moduleRoot, "internal")}
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("cannot reach %s from the package directory: %v", root, err)
		}
	}

	type fileEntry struct {
		path string
		ast  *ast.File
	}
	var files []fileEntry
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, parseErr := parser.ParseFile(fset, path, nil, 0)
			if parseErr != nil {
				return nil // not our job to police unparseable files
			}
			files = append(files, fileEntry{path: path, ast: f})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}

	// Pass 1 — every package-level string constant, keyed two ways so an
	// unqualified identifier and a package-qualified one both resolve.
	// exporter's own package is skipped: it declares OtherEventType, which is
	// the fold bucket rather than a writable event type.
	byDir := map[string]map[string]string{} // package dir -> const -> value
	byPkg := map[string]map[string]string{} // package name -> const -> value
	for _, fe := range files {
		dir := filepath.Dir(fe.path)
		pkg := fe.ast.Name.Name
		isExporterPkg := pkg == "exporter"
		for _, decl := range fe.ast.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					v, ok := stringLit(vs.Values[i])
					if !ok {
						continue
					}
					if byDir[dir] == nil {
						byDir[dir] = map[string]string{}
					}
					if byPkg[pkg] == nil {
						byPkg[pkg] = map[string]string{}
					}
					byDir[dir][name.Name] = v
					byPkg[pkg][name.Name] = v
					if !isExporterPkg && eventTypeConstNameRe.MatchString(name.Name) {
						constants[v] = position(fset, name.Pos())
					}
				}
			}
		}
	}

	// resolve turns a Type: value or a writeEvent first argument into the
	// event-type string, following a constant identifier when it can.
	resolve := func(dir string, e ast.Expr) (string, bool) {
		if v, ok := stringLit(e); ok {
			return v, true
		}
		switch id := e.(type) {
		case *ast.Ident:
			if v, ok := byDir[dir][id.Name]; ok {
				return v, true
			}
		case *ast.SelectorExpr:
			pkg, ok := id.X.(*ast.Ident)
			if !ok {
				return "", false
			}
			if v, ok := byPkg[pkg.Name][id.Sel.Name]; ok {
				return v, true
			}
		}
		return "", false
	}

	// Pass 2 — the writer sites.
	for _, fe := range files {
		dir := filepath.Dir(fe.path)
		inPackageDB := strings.Contains(filepath.ToSlash(fe.path), "/internal/db/")
		ast.Inspect(fe.ast, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				if sel, ok := node.Fun.(*ast.SelectorExpr); ok &&
					sel.Sel.Name == "writeEvent" && len(node.Args) > 0 {
					if v, ok := resolve(dir, node.Args[0]); ok {
						writers[v] = position(fset, node.Pos())
					}
				}
			case *ast.CompositeLit:
				if !isDBEventLiteral(node, inPackageDB) {
					return true
				}
				for _, elt := range node.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := kv.Key.(*ast.Ident)
					if !ok || key.Name != "Type" {
						continue
					}
					if v, ok := resolve(dir, kv.Value); ok {
						writers[v] = position(fset, kv.Pos())
					}
				}
			}
			return true
		})
	}
	return writers, constants
}

// isDBEventLiteral reports whether lit is a db.Event composite literal — or,
// inside package db, a bare Event literal. Scoping this way keeps unrelated
// Event types elsewhere in the tree out of the scan.
func isDBEventLiteral(lit *ast.CompositeLit, inPackageDB bool) bool {
	switch typ := lit.Type.(type) {
	case *ast.SelectorExpr:
		pkg, ok := typ.X.(*ast.Ident)
		return ok && pkg.Name == "db" && typ.Sel.Name == "Event"
	case *ast.Ident:
		return inPackageDB && typ.Name == "Event"
	}
	return false
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return v, true
}

func position(fset *token.FileSet, p token.Pos) string {
	pos := fset.Position(p)
	return filepath.ToSlash(pos.Filename) + ":" + strconv.Itoa(pos.Line)
}
