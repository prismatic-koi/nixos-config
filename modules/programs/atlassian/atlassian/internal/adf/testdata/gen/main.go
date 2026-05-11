//go:build ignore

// gen generates golden ADF JSON files for the builder tests.
// Run from the repo root:
//   go run ./internal/adf/testdata/gen/main.go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/prismatic-koi/atlassian/internal/adf"
)

func main() {
	_, file, _, _ := runtime.Caller(0)
	// testdata dir is two levels up from this file
	dir := filepath.Join(filepath.Dir(file), "..")

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
		doc, err := adf.Build([]byte(tc.src))
		if err != nil {
			fmt.Fprintf(os.Stderr, "SKIP %s: %v\n", tc.name, err)
			continue
		}
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "ERROR %s: %v\n", tc.name, err)
			continue
		}
		path := filepath.Join(dir, tc.name+".golden.json")
		if err := os.WriteFile(path, append(b, '\n'), 0644); err != nil {
			fmt.Fprintf(os.Stderr, "WRITE %s: %v\n", tc.name, err)
			continue
		}
		fmt.Printf("wrote %s\n", path)
	}
}
