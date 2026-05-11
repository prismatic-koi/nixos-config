package adf

// Build converts Markdown text into an ADF document (as a map[string]any
// suitable for JSON encoding).
//
// The supported subset mirrors what Render() understands:
//   - Paragraphs
//   - Headings h1–h6
//   - Bullet lists, ordered lists (with nesting)
//   - Fenced code blocks with language
//   - Inline code, bold, italic, links
//   - Hard line breaks
//   - Blockquotes
//   - Tables (GFM table syntax)
//
// Any Markdown construct outside this set causes a non-zero exit via a
// returned UnsupportedError.
//
// Parsing is done by goldmark; the AST is walked and ADF nodes are emitted
// manually so the mapping stays auditable and the supported subset is precise.

import (
	"fmt"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

// UnsupportedError is returned when an unsupported Markdown construct is
// encountered. It names the construct type and the approximate line number.
type UnsupportedError struct {
	Construct string
	Line      int
}

func (e *UnsupportedError) Error() string {
	if e.Line > 0 {
		return fmt.Sprintf("unsupported markdown construct %q at line %d", e.Construct, e.Line)
	}
	return fmt.Sprintf("unsupported markdown construct %q", e.Construct)
}

// Build parses src as Markdown and returns an ADF document or an error.
// Returns UnsupportedError for any construct outside the supported subset.
// Returns a plain error for empty input.
func Build(src []byte) (map[string]any, error) {
	if len(strings.TrimSpace(string(src))) == 0 {
		return nil, fmt.Errorf("empty body: no content to send")
	}

	// Use goldmark with GFM table extension.
	md := goldmark.New(
		goldmark.WithExtensions(extension.Table),
	)

	reader := text.NewReader(src)
	doc := md.Parser().Parse(reader)

	b := &builder{src: src}
	content, err := b.walkBlock(doc)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"version": 1,
		"type":    "doc",
		"content": content,
	}, nil
}

type builder struct {
	src []byte
}

// lineOf returns the 1-based line number for a node using its segment/lines.
// Returns 0 if the line number cannot be determined safely.
func lineOf(n ast.Node, src []byte) (result int) {
	defer func() {
		if recover() != nil {
			result = 0
		}
	}()
	type hasLines interface {
		Lines() *text.Segments
	}
	hl, ok := n.(hasLines)
	if !ok {
		return 0
	}
	lines := hl.Lines()
	if lines == nil || lines.Len() == 0 {
		return 0
	}
	seg := lines.At(0)
	// Count newlines before seg.Start
	line := 1
	for i := 0; i < seg.Start && i < len(src); i++ {
		if src[i] == '\n' {
			line++
		}
	}
	return line
}

// walkBlock walks block-level children of n and returns ADF nodes.
func (b *builder) walkBlock(n ast.Node) ([]any, error) {
	var out []any
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		nodes, err := b.blockNode(child)
		if err != nil {
			return nil, err
		}
		out = append(out, nodes...)
	}
	return out, nil
}

// blockNode converts a single block AST node to one or more ADF nodes.
func (b *builder) blockNode(n ast.Node) ([]any, error) {
	switch n.Kind() {
	case ast.KindParagraph:
		inline, err := b.inlineContent(n)
		if err != nil {
			return nil, err
		}
		return []any{map[string]any{"type": "paragraph", "content": inline}}, nil

	case ast.KindHeading:
		h := n.(*ast.Heading)
		inline, err := b.inlineContent(n)
		if err != nil {
			return nil, err
		}
		return []any{map[string]any{
			"type":    "heading",
			"attrs":   map[string]any{"level": h.Level},
			"content": inline,
		}}, nil

	case ast.KindFencedCodeBlock:
		cb := n.(*ast.FencedCodeBlock)
		lang := ""
		if cb.Info != nil {
			info := string(cb.Info.Segment.Value(b.src))
			// Trim any trailing options (e.g. "go linenums" → "go")
			lang = strings.Fields(info)[0]
		}
		var code strings.Builder
		for i := 0; i < cb.Lines().Len(); i++ {
			line := cb.Lines().At(i)
			code.Write(line.Value(b.src))
		}
		// Remove trailing newline added by goldmark
		codeStr := strings.TrimSuffix(code.String(), "\n")
		codeContent := []any{}
		if codeStr != "" {
			codeContent = []any{map[string]any{"type": "text", "text": codeStr}}
		}
		return []any{map[string]any{
			"type":    "codeBlock",
			"attrs":   map[string]any{"language": lang},
			"content": codeContent,
		}}, nil

	case ast.KindCodeBlock:
		// Indented code block (no language)
		var code strings.Builder
		for i := 0; i < n.Lines().Len(); i++ {
			line := n.Lines().At(i)
			code.Write(line.Value(b.src))
		}
		codeStr := strings.TrimSuffix(code.String(), "\n")
		codeContent := []any{}
		if codeStr != "" {
			codeContent = []any{map[string]any{"type": "text", "text": codeStr}}
		}
		return []any{map[string]any{
			"type":    "codeBlock",
			"attrs":   map[string]any{"language": ""},
			"content": codeContent,
		}}, nil

	case ast.KindBlockquote:
		children, err := b.walkBlock(n)
		if err != nil {
			return nil, err
		}
		return []any{map[string]any{"type": "blockquote", "content": children}}, nil

	case ast.KindList:
		lst := n.(*ast.List)
		items, err := b.listItems(n)
		if err != nil {
			return nil, err
		}
		listType := "bulletList"
		if lst.IsOrdered() {
			listType = "orderedList"
		}
		return []any{map[string]any{"type": listType, "content": items}}, nil

	case ast.KindThematicBreak:
		return []any{map[string]any{"type": "rule"}}, nil

	case extast.KindTable:
		tbl, err := b.buildTable(n)
		if err != nil {
			return nil, err
		}
		return []any{tbl}, nil

	case ast.KindHTMLBlock:
		line := lineOf(n, b.src)
		return nil, &UnsupportedError{Construct: "raw HTML block", Line: line}

	default:
		line := lineOf(n, b.src)
		return nil, &UnsupportedError{Construct: n.Kind().String(), Line: line}
	}
}

// listItems builds ADF listItem nodes from list children.
func (b *builder) listItems(n ast.Node) ([]any, error) {
	var items []any
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		if child.Kind() != ast.KindListItem {
			continue
		}
		content, err := b.listItemContent(child)
		if err != nil {
			return nil, err
		}
		items = append(items, map[string]any{"type": "listItem", "content": content})
	}
	return items, nil
}

// listItemContent renders the content of a single list item.
// The first child that is a TextBlock/Paragraph becomes inline paragraph content;
// subsequent children (nested lists, etc.) are appended.
func (b *builder) listItemContent(n ast.Node) ([]any, error) {
	var out []any
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		switch child.Kind() {
		case ast.KindTextBlock:
			inline, err := b.inlineContent(child)
			if err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"type": "paragraph", "content": inline})
		case ast.KindParagraph:
			inline, err := b.inlineContent(child)
			if err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"type": "paragraph", "content": inline})
		case ast.KindList:
			nested, err := b.blockNode(child)
			if err != nil {
				return nil, err
			}
			out = append(out, nested...)
		default:
			nested, err := b.blockNode(child)
			if err != nil {
				return nil, err
			}
			out = append(out, nested...)
		}
	}
	return out, nil
}

// inlineContent converts the inline children of a block node into ADF text/inline nodes.
func (b *builder) inlineContent(n ast.Node) ([]any, error) {
	var out []any
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		nodes, err := b.inlineNode(child)
		if err != nil {
			return nil, err
		}
		out = append(out, nodes...)
	}
	return out, nil
}

// inlineNode converts a single inline AST node.
func (b *builder) inlineNode(n ast.Node) ([]any, error) {
	switch n.Kind() {
	case ast.KindText:
		t := n.(*ast.Text)
		text := string(t.Segment.Value(b.src))
		nodes := []any{map[string]any{"type": "text", "text": text}}
		if t.HardLineBreak() {
			nodes = append(nodes, map[string]any{"type": "hardBreak"})
		} else if t.SoftLineBreak() {
			// Soft line break becomes a space in rendered output — join with a space
			nodes = append(nodes, map[string]any{"type": "text", "text": " "})
		}
		return nodes, nil

	case ast.KindCodeSpan:
		code := string(n.(*ast.CodeSpan).Text(b.src))
		return []any{map[string]any{
			"type":  "text",
			"text":  code,
			"marks": []any{map[string]any{"type": "code"}},
		}}, nil

	case ast.KindEmphasis:
		em := n.(*ast.Emphasis)
		inner, err := b.inlineContent(n)
		if err != nil {
			return nil, err
		}
		markType := "em"
		if em.Level == 2 {
			markType = "strong"
		}
		// Apply the mark to each inner text node
		return applyMarkToNodes(inner, map[string]any{"type": markType}), nil

	case ast.KindLink:
		lnk := n.(*ast.Link)
		inner, err := b.inlineContent(n)
		if err != nil {
			return nil, err
		}
		href := string(lnk.Destination)
		mark := map[string]any{
			"type":  "link",
			"attrs": map[string]any{"href": href},
		}
		return applyMarkToNodes(inner, mark), nil

	case ast.KindAutoLink:
		al := n.(*ast.AutoLink)
		url := string(al.URL(b.src))
		return []any{map[string]any{
			"type":  "text",
			"text":  url,
			"marks": []any{map[string]any{"type": "link", "attrs": map[string]any{"href": url}}},
		}}, nil

	case ast.KindRawHTML:
		line := lineOf(n, b.src)
		return nil, &UnsupportedError{Construct: "raw HTML", Line: line}

	case ast.KindImage:
		line := lineOf(n, b.src)
		return nil, &UnsupportedError{Construct: "image", Line: line}

	case ast.KindString:
		// goldmark internal string node (e.g. from entity expansion)
		text := string(n.(*ast.String).Value)
		return []any{map[string]any{"type": "text", "text": text}}, nil

	default:
		line := lineOf(n, b.src)
		return nil, &UnsupportedError{Construct: n.Kind().String(), Line: line}
	}
}

// applyMarkToNodes applies a mark to all leaf text nodes in a slice of ADF nodes.
func applyMarkToNodes(nodes []any, mark map[string]any) []any {
	out := make([]any, 0, len(nodes))
	for _, n := range nodes {
		nm, ok := n.(map[string]any)
		if !ok {
			out = append(out, n)
			continue
		}
		if nm["type"] == "text" {
			// Deep copy with mark added
			newNode := make(map[string]any, len(nm)+1)
			for k, v := range nm {
				newNode[k] = v
			}
			existing, _ := newNode["marks"].([]any)
			newNode["marks"] = append(existing, mark)
			out = append(out, newNode)
		} else {
			out = append(out, n)
		}
	}
	return out
}

// buildTable converts a GFM table node into an ADF table node.
func (b *builder) buildTable(n ast.Node) (map[string]any, error) {
	var rows []any
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		switch child.Kind() {
		case extast.KindTableHeader:
			row, err := b.buildTableRow(child, true)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		case extast.KindTableRow:
			row, err := b.buildTableRow(child, false)
			if err != nil {
				return nil, err
			}
			rows = append(rows, row)
		}
	}
	return map[string]any{"type": "table", "content": rows}, nil
}

// buildTableRow converts a table header/row node into an ADF tableRow.
func (b *builder) buildTableRow(n ast.Node, isHeader bool) (map[string]any, error) {
	var cells []any
	cellType := "tableCell"
	if isHeader {
		cellType = "tableHeader"
	}
	for child := n.FirstChild(); child != nil; child = child.NextSibling() {
		if child.Kind() != extast.KindTableCell {
			continue
		}
		inline, err := b.inlineContent(child)
		if err != nil {
			return nil, err
		}
		para := map[string]any{"type": "paragraph", "content": inline}
		cells = append(cells, map[string]any{
			"type":    cellType,
			"content": []any{para},
		})
	}
	return map[string]any{"type": "tableRow", "content": cells}, nil
}
