package adf_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/prismatic-koi/atlassian/internal/adf"
)

func doc(t *testing.T, content string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(`{"type":"doc","version":1,"content":`+content+`}`), &v); err != nil {
		t.Fatalf("parse doc: %v", err)
	}
	return v
}

func TestParagraph(t *testing.T) {
	input := doc(t, `[{"type":"paragraph","content":[{"type":"text","text":"Hello world"}]}]`)
	got := adf.Render(input)
	if !strings.Contains(got, "Hello world") {
		t.Errorf("paragraph: expected 'Hello world' in %q", got)
	}
}

func TestHeadings(t *testing.T) {
	for level := 1; level <= 6; level++ {
		input := doc(t, fmt.Sprintf(`[{"type":"heading","attrs":{"level":%d},"content":[{"type":"text","text":"Title"}]}]`, level))
		got := adf.Render(input)
		prefix := strings.Repeat("#", level) + " Title"
		if !strings.HasPrefix(got, prefix) {
			t.Errorf("h%d: expected prefix %q got %q", level, prefix, got)
		}
	}
}

func TestBulletList(t *testing.T) {
	input := doc(t, `[{"type":"bulletList","content":[
		{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"item one"}]}]},
		{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"item two"}]}]}
	]}]`)
	got := adf.Render(input)
	if !strings.Contains(got, "- item one") {
		t.Errorf("bullet list: expected '- item one' in %q", got)
	}
	if !strings.Contains(got, "- item two") {
		t.Errorf("bullet list: expected '- item two' in %q", got)
	}
}

func TestOrderedList(t *testing.T) {
	input := doc(t, `[{"type":"orderedList","content":[
		{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"first"}]}]},
		{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"second"}]}]}
	]}]`)
	got := adf.Render(input)
	if !strings.Contains(got, "1. first") {
		t.Errorf("ordered list: expected '1. first' in %q", got)
	}
	if !strings.Contains(got, "2. second") {
		t.Errorf("ordered list: expected '2. second' in %q", got)
	}
}

func TestCodeBlock(t *testing.T) {
	input := doc(t, `[{"type":"codeBlock","attrs":{"language":"go"},"content":[{"type":"text","text":"fmt.Println(\"hi\")"}]}]`)
	got := adf.Render(input)
	if !strings.Contains(got, "```go") {
		t.Errorf("code block: expected ```go in %q", got)
	}
	if !strings.Contains(got, "fmt.Println") {
		t.Errorf("code block: expected code content in %q", got)
	}
	if !strings.Contains(got, "```") {
		t.Errorf("code block: expected closing ``` in %q", got)
	}
}

func TestInlineCode(t *testing.T) {
	input := doc(t, `[{"type":"paragraph","content":[{"type":"text","text":"foo","marks":[{"type":"code"}]}]}]`)
	got := adf.Render(input)
	if !strings.Contains(got, "`foo`") {
		t.Errorf("inline code: expected '`foo`' in %q", got)
	}
}

func TestBold(t *testing.T) {
	input := doc(t, `[{"type":"paragraph","content":[{"type":"text","text":"bold","marks":[{"type":"strong"}]}]}]`)
	got := adf.Render(input)
	if !strings.Contains(got, "**bold**") {
		t.Errorf("bold: expected '**bold**' in %q", got)
	}
}

func TestItalic(t *testing.T) {
	input := doc(t, `[{"type":"paragraph","content":[{"type":"text","text":"italic","marks":[{"type":"em"}]}]}]`)
	got := adf.Render(input)
	if !strings.Contains(got, "_italic_") {
		t.Errorf("italic: expected '_italic_' in %q", got)
	}
}

func TestLink(t *testing.T) {
	input := doc(t, `[{"type":"paragraph","content":[{"type":"text","text":"click","marks":[{"type":"link","attrs":{"href":"https://example.com"}}]}]}]`)
	got := adf.Render(input)
	if !strings.Contains(got, "[click](https://example.com)") {
		t.Errorf("link: expected markdown link in %q", got)
	}
}

func TestHardBreak(t *testing.T) {
	input := doc(t, `[{"type":"paragraph","content":[{"type":"text","text":"line1"},{"type":"hardBreak"},{"type":"text","text":"line2"}]}]`)
	got := adf.Render(input)
	if !strings.Contains(got, "line1") || !strings.Contains(got, "line2") {
		t.Errorf("hardBreak: expected both lines in %q", got)
	}
	if !strings.Contains(got, "  \n") {
		t.Errorf("hardBreak: expected '  \\n' in %q", got)
	}
}

func TestBlockquote(t *testing.T) {
	input := doc(t, `[{"type":"blockquote","content":[{"type":"paragraph","content":[{"type":"text","text":"quoted text"}]}]}]`)
	got := adf.Render(input)
	if !strings.Contains(got, "> quoted text") {
		t.Errorf("blockquote: expected '> quoted text' in %q", got)
	}
}

func TestTable(t *testing.T) {
	input := doc(t, `[{"type":"table","content":[
		{"type":"tableRow","content":[
			{"type":"tableHeader","content":[{"type":"paragraph","content":[{"type":"text","text":"Name"}]}]},
			{"type":"tableHeader","content":[{"type":"paragraph","content":[{"type":"text","text":"Value"}]}]}
		]},
		{"type":"tableRow","content":[
			{"type":"tableCell","content":[{"type":"paragraph","content":[{"type":"text","text":"foo"}]}]},
			{"type":"tableCell","content":[{"type":"paragraph","content":[{"type":"text","text":"bar"}]}]}
		]}
	]}]`)
	got := adf.Render(input)
	if !strings.Contains(got, "| Name |") {
		t.Errorf("table: expected '| Name |' in %q", got)
	}
	if !strings.Contains(got, "| --- |") {
		t.Errorf("table: expected separator in %q", got)
	}
	if !strings.Contains(got, "| foo |") {
		t.Errorf("table: expected data row in %q", got)
	}
}

func TestUnknownNodeType(t *testing.T) {
	input := doc(t, `[{"type":"weirdFutureNode","content":[]}]`)
	got := adf.Render(input)
	if !strings.Contains(got, "<!-- unrendered: weirdFutureNode -->") {
		t.Errorf("unknown: expected comment in %q", got)
	}
}

func TestUnknownNodeTypeDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("unknown node panicked: %v", r)
		}
	}()
	input := doc(t, `[{"type":"unknownComplexNode","attrs":{"x":1},"content":[{"type":"text","text":"hi"}]}]`)
	_ = adf.Render(input)
}

func TestEmptyDoc(t *testing.T) {
	input := doc(t, `[]`)
	got := adf.Render(input)
	if got != "" {
		t.Errorf("empty doc: expected empty string, got %q", got)
	}
}

func TestNilInput(t *testing.T) {
	got := adf.Render(nil)
	if got != "" {
		t.Errorf("nil input: expected empty string, got %q", got)
	}
}
