// Package adf renders Atlassian Document Format (ADF) JSON into Markdown.
//
// Only the node types emitted in practice by Jira Cloud and Confluence Cloud
// are handled; unknown types render as HTML comments so the surrounding output
// is not broken.
package adf

import (
	"fmt"
	"strings"
)

// Render converts an ADF node (decoded from JSON into map[string]any) into
// a Markdown string. Top-level call should pass the "doc" node.
func Render(node any) string {
	var sb strings.Builder
	renderNode(&sb, node, 0, false, false)
	return strings.TrimRight(sb.String(), "\n")
}

func renderNode(sb *strings.Builder, node any, listDepth int, inOrderedList bool, inTableCell bool) {
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	nodeType, _ := m["type"].(string)

	switch nodeType {
	case "doc":
		renderChildren(sb, m, listDepth, false, false)

	case "paragraph":
		renderChildren(sb, m, listDepth, false, false)
		if !inTableCell {
			sb.WriteString("\n\n")
		}

	case "heading":
		level := 1
		if attrs, ok := m["attrs"].(map[string]any); ok {
			if l, ok := attrs["level"].(float64); ok {
				level = int(l)
			}
		}
		sb.WriteString(strings.Repeat("#", level))
		sb.WriteString(" ")
		renderInlineChildren(sb, m)
		sb.WriteString("\n\n")

	case "bulletList":
		renderList(sb, m, listDepth, false)

	case "orderedList":
		renderList(sb, m, listDepth, true)

	case "listItem":
		renderChildren(sb, m, listDepth, false, false)

	case "codeBlock":
		lang := ""
		if attrs, ok := m["attrs"].(map[string]any); ok {
			if l, ok := attrs["language"].(string); ok {
				lang = l
			}
		}
		sb.WriteString("```")
		sb.WriteString(lang)
		sb.WriteString("\n")
		renderInlineChildren(sb, m)
		sb.WriteString("\n```\n\n")

	case "blockquote":
		var inner strings.Builder
		renderChildren(&inner, m, 0, false, false)
		lines := strings.Split(strings.TrimRight(inner.String(), "\n"), "\n")
		for _, line := range lines {
			sb.WriteString("> ")
			sb.WriteString(line)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")

	case "rule":
		sb.WriteString("---\n\n")

	case "hardBreak":
		sb.WriteString("  \n")

	case "table":
		renderTable(sb, m)

	case "tableRow":
		// handled inside renderTable

	case "tableCell", "tableHeader":
		// handled inside renderTable

	case "text":
		text, _ := m["text"].(string)
		marks, _ := m["marks"].([]any)
		text = applyMarks(text, marks)
		sb.WriteString(text)

	case "inlineCard":
		// render as a link if URL is available
		if attrs, ok := m["attrs"].(map[string]any); ok {
			if url, ok := attrs["url"].(string); ok {
				sb.WriteString("[")
				sb.WriteString(url)
				sb.WriteString("](")
				sb.WriteString(url)
				sb.WriteString(")")
				return
			}
		}
		sb.WriteString("<!-- unrendered: inlineCard -->")

	case "mention":
		if attrs, ok := m["attrs"].(map[string]any); ok {
			if name, ok := attrs["text"].(string); ok {
				sb.WriteString("@")
				sb.WriteString(name)
				return
			}
			if id, ok := attrs["id"].(string); ok {
				sb.WriteString("@")
				sb.WriteString(id)
				return
			}
		}
		sb.WriteString("<!-- unrendered: mention -->")

	case "emoji":
		if attrs, ok := m["attrs"].(map[string]any); ok {
			if text, ok := attrs["text"].(string); ok {
				sb.WriteString(text)
				return
			}
		}
		sb.WriteString("<!-- unrendered: emoji -->")

	case "mediaSingle", "media", "mediaGroup":
		sb.WriteString("<!-- unrendered: ")
		sb.WriteString(nodeType)
		sb.WriteString(" -->\n\n")

	case "panel":
		renderChildren(sb, m, listDepth, false, false)

	case "expand":
		if attrs, ok := m["attrs"].(map[string]any); ok {
			if title, ok := attrs["title"].(string); ok {
				sb.WriteString("**")
				sb.WriteString(title)
				sb.WriteString("**\n\n")
			}
		}
		renderChildren(sb, m, listDepth, false, false)

	default:
		if nodeType == "" {
			return
		}
		sb.WriteString("<!-- unrendered: ")
		sb.WriteString(nodeType)
		sb.WriteString(" -->")
	}
}

// renderChildren renders all content children of a node.
func renderChildren(sb *strings.Builder, m map[string]any, listDepth int, inOrderedList bool, inTableCell bool) {
	content, _ := m["content"].([]any)
	for _, child := range content {
		renderNode(sb, child, listDepth, inOrderedList, inTableCell)
	}
}

// renderInlineChildren renders content children without block wrappers
// (used for headings and code blocks where we want plain text).
func renderInlineChildren(sb *strings.Builder, m map[string]any) {
	content, _ := m["content"].([]any)
	for _, child := range content {
		if cm, ok := child.(map[string]any); ok {
			if cm["type"] == "text" {
				text, _ := cm["text"].(string)
				marks, _ := cm["marks"].([]any)
				sb.WriteString(applyMarks(text, marks))
			} else if cm["type"] == "hardBreak" {
				sb.WriteString("  \n")
			}
			// Other inline types are ignored for simplicity in headings/code blocks
		}
	}
}

func renderList(sb *strings.Builder, m map[string]any, depth int, ordered bool) {
	content, _ := m["content"].([]any)
	indent := strings.Repeat("  ", depth)
	for i, child := range content {
		cm, ok := child.(map[string]any)
		if !ok {
			continue
		}
		if ordered {
			sb.WriteString(fmt.Sprintf("%s%d. ", indent, i+1))
		} else {
			sb.WriteString(indent + "- ")
		}
		// Render listItem content inline, handling nested lists
		renderListItemContent(sb, cm, depth)
	}
	if depth == 0 {
		sb.WriteString("\n")
	}
}

// renderListItemContent renders the content of a list item. Paragraphs are
// inline (no double newline), nested lists are indented.
func renderListItemContent(sb *strings.Builder, m map[string]any, depth int) {
	content, _ := m["content"].([]any)
	first := true
	for _, child := range content {
		cm, ok := child.(map[string]any)
		if !ok {
			continue
		}
		childType, _ := cm["type"].(string)
		switch childType {
		case "paragraph":
			if !first {
				sb.WriteString(strings.Repeat("  ", depth+1))
			}
			renderInlineChildren(sb, cm)
			sb.WriteString("\n")
		case "bulletList":
			renderList(sb, cm, depth+1, false)
		case "orderedList":
			renderList(sb, cm, depth+1, true)
		default:
			var inner strings.Builder
			renderNode(&inner, child, depth+1, false, false)
			sb.WriteString(inner.String())
		}
		first = false
	}
}

func renderTable(sb *strings.Builder, m map[string]any) {
	rows, _ := m["content"].([]any)
	for rowIdx, row := range rows {
		rowMap, ok := row.(map[string]any)
		if !ok {
			continue
		}
		cells, _ := rowMap["content"].([]any)
		sb.WriteString("|")
		for _, cell := range cells {
			cellMap, ok := cell.(map[string]any)
			if !ok {
				sb.WriteString(" |")
				continue
			}
			var cellSB strings.Builder
			renderChildren(&cellSB, cellMap, 0, false, true)
			cellText := strings.TrimSpace(cellSB.String())
			cellText = strings.ReplaceAll(cellText, "\n", " ")
			sb.WriteString(" ")
			sb.WriteString(cellText)
			sb.WriteString(" |")
		}
		sb.WriteString("\n")
		// After first row (header), add separator
		if rowIdx == 0 {
			sb.WriteString("|")
			for range cells {
				sb.WriteString(" --- |")
			}
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n")
}

func applyMarks(text string, marks []any) string {
	for _, mark := range marks {
		mm, ok := mark.(map[string]any)
		if !ok {
			continue
		}
		markType, _ := mm["type"].(string)
		switch markType {
		case "strong":
			text = "**" + text + "**"
		case "em":
			text = "_" + text + "_"
		case "code":
			text = "`" + text + "`"
		case "link":
			attrs, _ := mm["attrs"].(map[string]any)
			href, _ := attrs["href"].(string)
			text = "[" + text + "](" + href + ")"
		case "strike":
			text = "~~" + text + "~~"
		case "underline":
			// No native Markdown underline; use HTML
			text = "<u>" + text + "</u>"
		case "subsup":
			// subscript/superscript — skip, render as plain text
		case "textColor":
			// color info — skip, render as plain text
		case "backgroundColor":
			// background color — skip, render as plain text
		}
	}
	return text
}
