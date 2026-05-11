package adf_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/atlassian/internal/adf"
)

// buildMust calls Build and fatals the test if it returns an error.
func buildMust(t *testing.T, md string) map[string]any {
	t.Helper()
	doc, err := adf.Build([]byte(md))
	if err != nil {
		t.Fatalf("Build(%q): %v", md, err)
	}
	return doc
}

// docContent returns the top-level content array from an ADF doc.
func docContent(t *testing.T, doc map[string]any) []any {
	t.Helper()
	c, ok := doc["content"].([]any)
	if !ok {
		t.Fatalf("doc has no content array: %v", doc)
	}
	return c
}

// firstNode returns the first node in the doc content.
func firstNode(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	c := docContent(t, doc)
	if len(c) == 0 {
		t.Fatal("doc content is empty")
	}
	m, ok := c[0].(map[string]any)
	if !ok {
		t.Fatalf("first node is not a map: %T", c[0])
	}
	return m
}

// nodeType returns the "type" field of an ADF node map.
func nodeType(n map[string]any) string {
	s, _ := n["type"].(string)
	return s
}

// nodeContent returns the "content" array of an ADF node.
func nodeContent(t *testing.T, n map[string]any) []any {
	t.Helper()
	c, ok := n["content"].([]any)
	if !ok {
		t.Fatalf("node %q has no content array", nodeType(n))
	}
	return c
}

// toMap asserts an element is a map[string]any.
func toMap(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("expected map[string]any, got %T: %v", v, v)
	}
	return m
}

// marshalJSON round-trips through JSON to normalise types (e.g. int → float64).
func marshalJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

// ---- Per-construct tests ----

func TestBuild_Paragraph(t *testing.T) {
	doc := buildMust(t, "Hello world\n")
	node := firstNode(t, doc)
	if nodeType(node) != "paragraph" {
		t.Fatalf("expected paragraph, got %q", nodeType(node))
	}
	content := nodeContent(t, node)
	if len(content) == 0 {
		t.Fatal("paragraph has no content")
	}
	text := toMap(t, content[0])
	if text["type"] != "text" || text["text"] != "Hello world" {
		t.Errorf("unexpected text node: %v", text)
	}
}

func TestBuild_HeadingH1(t *testing.T) {
	doc := buildMust(t, "# Title\n")
	node := firstNode(t, doc)
	if nodeType(node) != "heading" {
		t.Fatalf("expected heading, got %q", nodeType(node))
	}
	attrs := toMap(t, node["attrs"])
	gotLevel, _ := attrs["level"].(int)
	if gotLevel != 1 {
		t.Errorf("expected level 1, got %v (%T)", attrs["level"], attrs["level"])
	}
	content := nodeContent(t, node)
	text := toMap(t, content[0])
	if text["text"] != "Title" {
		t.Errorf("expected 'Title', got %q", text["text"])
	}
}

func TestBuild_HeadingsH1toH6(t *testing.T) {
	for level := 1; level <= 6; level++ {
		md := strings.Repeat("#", level) + " Level\n"
		doc := buildMust(t, md)
		node := firstNode(t, doc)
		if nodeType(node) != "heading" {
			t.Errorf("h%d: expected heading, got %q", level, nodeType(node))
			continue
		}
		attrs := toMap(t, node["attrs"])
		gotLevel, _ := attrs["level"].(int)
		if gotLevel != level {
			t.Errorf("h%d: expected level %d, got %d", level, level, gotLevel)
		}
	}
}

func TestBuild_BulletList(t *testing.T) {
	doc := buildMust(t, "- item one\n- item two\n")
	node := firstNode(t, doc)
	if nodeType(node) != "bulletList" {
		t.Fatalf("expected bulletList, got %q", nodeType(node))
	}
	items := nodeContent(t, node)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	for i, item := range items {
		im := toMap(t, item)
		if im["type"] != "listItem" {
			t.Errorf("item %d: expected listItem, got %q", i, im["type"])
		}
	}
}

func TestBuild_OrderedList(t *testing.T) {
	doc := buildMust(t, "1. first\n2. second\n")
	node := firstNode(t, doc)
	if nodeType(node) != "orderedList" {
		t.Fatalf("expected orderedList, got %q", nodeType(node))
	}
	items := nodeContent(t, node)
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
}

func TestBuild_NestedList(t *testing.T) {
	md := "- parent\n  - child one\n  - child two\n"
	doc := buildMust(t, md)
	node := firstNode(t, doc)
	if nodeType(node) != "bulletList" {
		t.Fatalf("expected bulletList, got %q", nodeType(node))
	}
	items := nodeContent(t, node)
	if len(items) == 0 {
		t.Fatal("no items")
	}
	// The first list item should contain a nested list somewhere
	parentItem := toMap(t, items[0])
	content := nodeContent(t, parentItem)
	found := false
	for _, c := range content {
		cm := toMap(t, c)
		if cm["type"] == "bulletList" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected nested bulletList inside first listItem; got: %v", marshalJSON(t, parentItem))
	}
}

func TestBuild_FencedCodeBlock(t *testing.T) {
	md := "```go\nfmt.Println(\"hi\")\n```\n"
	doc := buildMust(t, md)
	node := firstNode(t, doc)
	if nodeType(node) != "codeBlock" {
		t.Fatalf("expected codeBlock, got %q", nodeType(node))
	}
	attrs := toMap(t, node["attrs"])
	if attrs["language"] != "go" {
		t.Errorf("expected language='go', got %q", attrs["language"])
	}
	content := nodeContent(t, node)
	if len(content) == 0 {
		t.Fatal("codeBlock has no content")
	}
	text := toMap(t, content[0])
	if !strings.Contains(text["text"].(string), "fmt.Println") {
		t.Errorf("expected code text, got %q", text["text"])
	}
}

func TestBuild_FencedCodeBlockNoLang(t *testing.T) {
	md := "```\nsome code\n```\n"
	doc := buildMust(t, md)
	node := firstNode(t, doc)
	if nodeType(node) != "codeBlock" {
		t.Fatalf("expected codeBlock, got %q", nodeType(node))
	}
	attrs := toMap(t, node["attrs"])
	if attrs["language"] != "" {
		t.Errorf("expected empty language, got %q", attrs["language"])
	}
}

func TestBuild_InlineCode(t *testing.T) {
	doc := buildMust(t, "Use `foo` here.\n")
	node := firstNode(t, doc)
	content := nodeContent(t, node)
	// Find the inline code node
	found := false
	for _, c := range content {
		cm := toMap(t, c)
		if cm["type"] != "text" {
			continue
		}
		marks, _ := cm["marks"].([]any)
		for _, mark := range marks {
			mm := toMap(t, mark)
			if mm["type"] == "code" {
				found = true
				if cm["text"] != "foo" {
					t.Errorf("inline code text: expected 'foo', got %q", cm["text"])
				}
			}
		}
	}
	if !found {
		t.Errorf("no inline code node found in %v", marshalJSON(t, node))
	}
}

func TestBuild_Bold(t *testing.T) {
	doc := buildMust(t, "**bold text**\n")
	node := firstNode(t, doc)
	content := nodeContent(t, node)
	found := false
	for _, c := range content {
		cm := toMap(t, c)
		if cm["type"] != "text" {
			continue
		}
		marks, _ := cm["marks"].([]any)
		for _, mark := range marks {
			mm := toMap(t, mark)
			if mm["type"] == "strong" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no strong mark found in %v", marshalJSON(t, node))
	}
}

func TestBuild_Italic(t *testing.T) {
	doc := buildMust(t, "_italic text_\n")
	node := firstNode(t, doc)
	content := nodeContent(t, node)
	found := false
	for _, c := range content {
		cm := toMap(t, c)
		if cm["type"] != "text" {
			continue
		}
		marks, _ := cm["marks"].([]any)
		for _, mark := range marks {
			mm := toMap(t, mark)
			if mm["type"] == "em" {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no em mark found in %v", marshalJSON(t, node))
	}
}

func TestBuild_Link(t *testing.T) {
	doc := buildMust(t, "[click here](https://example.com)\n")
	node := firstNode(t, doc)
	content := nodeContent(t, node)
	found := false
	for _, c := range content {
		cm := toMap(t, c)
		if cm["type"] != "text" {
			continue
		}
		marks, _ := cm["marks"].([]any)
		for _, mark := range marks {
			mm := toMap(t, mark)
			if mm["type"] == "link" {
				attrs := toMap(t, mm["attrs"])
				if attrs["href"] == "https://example.com" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("no link mark found in %v", marshalJSON(t, node))
	}
}

func TestBuild_HardBreak(t *testing.T) {
	// Two trailing spaces force a hard break in Markdown
	doc := buildMust(t, "line one  \nline two\n")
	node := firstNode(t, doc)
	content := nodeContent(t, node)
	found := false
	for _, c := range content {
		cm := toMap(t, c)
		if cm["type"] == "hardBreak" {
			found = true
		}
	}
	if !found {
		t.Errorf("no hardBreak node found in %v", marshalJSON(t, node))
	}
}

func TestBuild_Blockquote(t *testing.T) {
	doc := buildMust(t, "> quoted text\n")
	node := firstNode(t, doc)
	if nodeType(node) != "blockquote" {
		t.Fatalf("expected blockquote, got %q", nodeType(node))
	}
	inner := nodeContent(t, node)
	if len(inner) == 0 {
		t.Fatal("blockquote has no content")
	}
	para := toMap(t, inner[0])
	if para["type"] != "paragraph" {
		t.Errorf("expected paragraph inside blockquote, got %q", para["type"])
	}
}

func TestBuild_Table(t *testing.T) {
	md := "| Name | Value |\n|------|-------|\n| foo  | bar   |\n"
	doc := buildMust(t, md)
	node := firstNode(t, doc)
	if nodeType(node) != "table" {
		t.Fatalf("expected table, got %q: %v", nodeType(node), marshalJSON(t, doc))
	}
	rows := nodeContent(t, node)
	if len(rows) < 2 {
		t.Fatalf("expected at least 2 rows (header + data), got %d", len(rows))
	}
	// First row should have tableHeader cells
	headerRow := toMap(t, rows[0])
	if headerRow["type"] != "tableRow" {
		t.Errorf("expected tableRow for header, got %q", headerRow["type"])
	}
	headerCells := nodeContent(t, headerRow)
	if len(headerCells) == 0 {
		t.Fatal("header row has no cells")
	}
	firstCell := toMap(t, headerCells[0])
	if firstCell["type"] != "tableHeader" {
		t.Errorf("expected tableHeader, got %q", firstCell["type"])
	}
	// Second row should have tableCell cells
	dataRow := toMap(t, rows[1])
	dataCells := nodeContent(t, dataRow)
	firstDataCell := toMap(t, dataCells[0])
	if firstDataCell["type"] != "tableCell" {
		t.Errorf("expected tableCell, got %q", firstDataCell["type"])
	}
}

// ---- Unsupported construct tests ----

func TestBuild_RejectedTaskList(t *testing.T) {
	_, err := adf.Build([]byte("- [ ] unchecked item\n- [x] checked item\n"))
	if err == nil {
		t.Fatal("expected error for task list")
	}
	unsup, ok := err.(*adf.UnsupportedError)
	if !ok {
		t.Fatalf("expected *UnsupportedError, got %T: %v", err, err)
	}
	if !strings.Contains(unsup.Construct, "task") && !strings.Contains(unsup.Construct, "Task") {
		t.Errorf("expected 'task' in construct name, got %q", unsup.Construct)
	}
}

func TestBuild_RejectedStrikethrough(t *testing.T) {
	_, err := adf.Build([]byte("~~strikethrough text~~\n"))
	if err == nil {
		t.Fatal("expected error for strikethrough")
	}
	unsup, ok := err.(*adf.UnsupportedError)
	if !ok {
		t.Fatalf("expected *UnsupportedError, got %T: %v", err, err)
	}
	if !strings.Contains(unsup.Construct, "strikethrough") {
		t.Errorf("expected 'strikethrough' in construct name, got %q", unsup.Construct)
	}
}

func TestBuild_RejectedHTMLBlock(t *testing.T) {
	_, err := adf.Build([]byte("<div>raw html</div>\n"))
	if err == nil {
		t.Fatal("expected error for raw HTML block")
	}
	unsup, ok := err.(*adf.UnsupportedError)
	if !ok {
		t.Fatalf("expected *UnsupportedError, got %T: %v", err, err)
	}
	if !strings.Contains(unsup.Construct, "HTML") && !strings.Contains(unsup.Construct, "html") {
		t.Errorf("expected 'HTML' in construct name, got %q", unsup.Construct)
	}
}

func TestBuild_RejectedRawHTMLInline(t *testing.T) {
	_, err := adf.Build([]byte("text with <b>inline html</b> here\n"))
	if err == nil {
		t.Fatal("expected error for inline raw HTML")
	}
	_, ok := err.(*adf.UnsupportedError)
	if !ok {
		t.Fatalf("expected *UnsupportedError, got %T: %v", err, err)
	}
}

func TestBuild_RejectedImage(t *testing.T) {
	_, err := adf.Build([]byte("![alt text](image.png)\n"))
	if err == nil {
		t.Fatal("expected error for image")
	}
	unsup, ok := err.(*adf.UnsupportedError)
	if !ok {
		t.Fatalf("expected *UnsupportedError, got %T: %v", err, err)
	}
	if !strings.Contains(unsup.Construct, "image") && !strings.Contains(unsup.Construct, "Image") {
		t.Errorf("expected 'image' in construct name, got %q", unsup.Construct)
	}
}

func TestBuild_EmptyBody(t *testing.T) {
	_, err := adf.Build([]byte(""))
	if err == nil {
		t.Fatal("expected error for empty body")
	}
	if !strings.Contains(err.Error(), "empty body") {
		t.Errorf("expected 'empty body' in error, got %q", err.Error())
	}
}

func TestBuild_WhitespaceOnlyBody(t *testing.T) {
	_, err := adf.Build([]byte("   \n\t\n"))
	if err == nil {
		t.Fatal("expected error for whitespace-only body")
	}
	if !strings.Contains(err.Error(), "empty body") {
		t.Errorf("expected 'empty body' in error, got %q", err.Error())
	}
}

func TestBuild_UnsupportedErrorHasLineNumber(t *testing.T) {
	src := "paragraph one\n\n<div>html</div>\n"
	_, err := adf.Build([]byte(src))
	if err == nil {
		t.Fatal("expected error for raw HTML block")
	}
	unsup, ok := err.(*adf.UnsupportedError)
	if !ok {
		t.Fatalf("expected *UnsupportedError, got %T: %v", err, err)
	}
	if unsup.Line == 0 {
		t.Error("expected non-zero line number in UnsupportedError")
	}
}

// ---- Doc structure validation ----

func TestBuild_DocHasVersionAndType(t *testing.T) {
	doc := buildMust(t, "hello\n")
	if doc["version"] != 1 {
		t.Errorf("expected version=1, got %v", doc["version"])
	}
	if doc["type"] != "doc" {
		t.Errorf("expected type='doc', got %v", doc["type"])
	}
}

// ---- Round-trip test: Markdown → ADF → Markdown ----

func TestBuild_RoundTrip(t *testing.T) {
	// A representative document using all supported constructs.
	// The round-trip through Render() should produce equivalent Markdown
	// (up to whitespace normalisation).
	md := `# Round Trip

This is a paragraph with **bold**, _italic_, and ` + "`code`" + `.

## Links and Lists

Here is a [link](https://example.com).

- item one
- item two
  - nested item

1. first
2. second

` + "```go" + `
fmt.Println("hello")
` + "```" + `

> a blockquote paragraph

| Col A | Col B |
|-------|-------|
| foo   | bar   |
`
	adfDoc, err := adf.Build([]byte(md))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	rendered := adf.Render(adfDoc)
	if rendered == "" {
		t.Fatal("Render returned empty string")
	}

	// Normalise: collapse multiple blank lines, trim trailing whitespace on each line.
	normalise := func(s string) string {
		lines := strings.Split(s, "\n")
		var out []string
		for _, l := range lines {
			out = append(out, strings.TrimRight(l, " \t"))
		}
		// Collapse 3+ consecutive blank lines to 2
		result := strings.Join(out, "\n")
		for strings.Contains(result, "\n\n\n") {
			result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
		}
		return strings.TrimSpace(result)
	}

	origNorm := normalise(md)
	renderedNorm := normalise(rendered)

	// Check key content is present (we don't require bit-for-bit equality
	// because ADF→MD rendering may differ in minor whitespace).
	checks := []string{
		"# Round Trip",
		"**bold**",
		"_italic_",
		"`code`",
		"[link](https://example.com)",
		"- item one",
		"- item two",
		"1. first",
		"2. second",
		"```go",
		`fmt.Println("hello")`,
		"> a blockquote",
		"| Col A |",
		"| foo |",
	}

	_ = origNorm // used for documentation; not compared bit-for-bit

	for _, check := range checks {
		if !strings.Contains(renderedNorm, check) {
			t.Errorf("round-trip: expected %q in rendered output\nRendered:\n%s", check, renderedNorm)
		}
	}
}

// ---- Golden file tests ----
//
// Each golden file in testdata/<name>.golden.json captures the exact ADF JSON
// that Build() must produce for the corresponding markdown input.
// Run `go run ./internal/adf/testdata/gen/main.go` to regenerate them after
// intentional changes to the builder.

// normaliseJSON round-trips through JSON to produce a canonical byte form
// (sorted keys, consistent spacing) for comparison.
func normaliseJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	// Unmarshal back to any to lose type distinctions (int vs float64), then
	// re-marshal through MarshalIndent for a stable canonical form.
	var generic any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	out, err := json.MarshalIndent(generic, "", "  ")
	if err != nil {
		t.Fatalf("json.MarshalIndent: %v", err)
	}
	return out
}

// readGolden reads testdata/<name>.golden.json relative to this test file.
func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	path := filepath.Join("testdata", name+".golden.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden file %s: %v", path, err)
	}
	// Normalise the golden file through the same JSON round-trip.
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("parse golden file %s: %v", path, err)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("re-marshal golden %s: %v", path, err)
	}
	return out
}

// TestBuild_GoldenFiles verifies the exact ADF JSON for each supported
// markdown construct by comparing against the golden files in testdata/.
func TestBuild_GoldenFiles(t *testing.T) {
	cases := []struct {
		name string
		src  string
	}{
		{"paragraph", "Hello world\n"},
		{"heading-h1", "# Title\n"},
		{"heading-h2", "## Subtitle\n"},
		{"bullet-list", "- item one\n- item two\n"},
		{"ordered-list", "1. first\n2. second\n"},
		{"nested-list", "- parent\n  - child one\n  - child two\n"},
		{"fenced-code-block", "```go\nfmt.Println(\"hi\")\n```\n"},
		{"inline-code", "Use `foo` here.\n"},
		{"bold", "**bold text**\n"},
		{"italic", "_italic text_\n"},
		{"link", "[click here](https://example.com)\n"},
		{"hard-break", "line one  \nline two\n"},
		{"blockquote", "> quoted text\n"},
		{"table", "| Name | Value |\n|------|-------|\n| foo  | bar   |\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := buildMust(t, tc.src)
			got := normaliseJSON(t, doc)
			want := readGolden(t, tc.name)
			if string(got) != string(want) {
				t.Errorf("ADF mismatch for %q:\ngot:\n%s\nwant:\n%s", tc.name, got, want)
			}
		})
	}
}
