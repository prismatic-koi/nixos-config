package cmd

// checkin_tools.go — payload-parsing and result-summary helpers used by the
// various checkin rendering paths (turns, raw, review).

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/prismatic-koi/prism/internal/payload"
)

// extractMessageID returns the messageId field from a payload JSON string.
func extractMessageID(raw string) string {
	var p payload.MsgUser // any struct with MessageID works here
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return ""
	}
	return p.MessageID
}

// extractBashCommand extracts the command string from bash tool args.
// The args may be a plain string (the command itself) or a JSON object
// with a "command" or "cmd" field.
func extractBashCommand(args string) string {
	// Try JSON object first.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &obj); err == nil {
		for _, key := range []string{"command", "cmd"} {
			if raw, ok := obj[key]; ok {
				var s string
				if err := json.Unmarshal(raw, &s); err == nil {
					return s
				}
			}
		}
	}
	// Try plain JSON string.
	var s string
	if err := json.Unmarshal([]byte(args), &s); err == nil {
		return s
	}
	// Fall back to raw args.
	return args
}

// extractStringField extracts a string value from a JSON object by trying
// each key in order. Returns the raw string if none match or if args is not JSON.
func extractStringField(args string, keys ...string) string {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &obj); err != nil {
		// Not a JSON object — try plain JSON string.
		var s string
		if err2 := json.Unmarshal([]byte(args), &s); err2 == nil {
			return s
		}
		return args
	}
	for _, key := range keys {
		if raw, ok := obj[key]; ok {
			var s string
			if err := json.Unmarshal(raw, &s); err == nil {
				return s
			}
		}
	}
	return args
}

// toolKeyArg extracts the key argument for the one-liner display per tool type.
// For bash: first ~80 chars of the command string.
// For read/edit/write: the file path.
// For task: the description.
// For glob/grep: the pattern.
// For todowrite: empty string (tool name alone is sufficient).
// For unknown tools: first ~80 chars of args.
func toolKeyArg(tool, args string) string {
	switch tool {
	case "bash", "Bash":
		// Args for bash is typically the command string directly.
		cmd := extractBashCommand(args)
		if len([]rune(cmd)) > 80 {
			runes := []rune(cmd)
			return string(runes[:80]) + "..."
		}
		return cmd

	case "read", "Read":
		return extractStringField(args, "filePath", "path", "file_path")

	case "edit", "Edit":
		return extractStringField(args, "filePath", "path", "file_path")

	case "write", "Write":
		return extractStringField(args, "filePath", "path", "file_path")

	case "task", "Task":
		desc := extractStringField(args, "description", "desc", "prompt")
		if len([]rune(desc)) > 80 {
			runes := []rune(desc)
			return string(runes[:80]) + "..."
		}
		return desc

	case "glob", "Glob":
		return extractStringField(args, "pattern", "glob")

	case "grep", "Grep":
		return extractStringField(args, "pattern", "regex", "query")

	case "todowrite", "TodoWrite", "Todowrite":
		return ""

	default:
		// Generic: first ~80 chars of raw args.
		if len([]rune(args)) > 80 {
			runes := []rune(args)
			return string(runes[:80]) + "..."
		}
		return args
	}
}

// toolResultSummary produces a one-line result summary for the given tool and
// raw result string.
func toolResultSummary(tool, result string) string {
	switch tool {
	case "bash", "Bash":
		return bashResultSummary(result)

	case "read", "Read":
		// Count lines in result.
		if result == "" {
			return "✓ (0 lines)"
		}
		n := strings.Count(result, "\n") + 1
		// If result ends with a trailing newline, don't count the empty last line.
		if strings.HasSuffix(result, "\n") && n > 1 {
			n--
		}
		return fmt.Sprintf("✓ (%d lines)", n)

	case "edit", "Edit":
		if isErrorResult(result) {
			return "✗"
		}
		return "✓"

	case "write", "Write":
		if isErrorResult(result) {
			return "✗"
		}
		return "✓"

	case "task", "Task":
		if isErrorResult(result) {
			return "✗"
		}
		return "✓"

	case "glob", "Glob":
		return matchCountSummary(result)

	case "grep", "Grep":
		return matchCountSummary(result)

	case "todowrite", "TodoWrite", "Todowrite":
		return "✓"

	default:
		// Generic: first meaningful line or ✓ if empty.
		if result == "" {
			return "✓"
		}
		line := firstMeaningfulLine(result)
		if len([]rune(line)) > 60 {
			runes := []rune(line)
			return string(runes[:60]) + "..."
		}
		return line
	}
}

// bashResultSummary extracts a one-line summary from a bash tool result.
// Returns ✓ for empty output, ✗ + first stderr line for errors, or the
// first meaningful output line otherwise.
func bashResultSummary(result string) string {
	if result == "" {
		return "✓"
	}
	// Check for common error indicators in the result.
	lower := strings.ToLower(result)
	isErr := strings.Contains(lower, "error:") ||
		strings.Contains(lower, "command not found") ||
		strings.Contains(lower, "exit status") ||
		strings.Contains(lower, "permission denied") ||
		strings.Contains(lower, "no such file")

	line := firstMeaningfulLine(result)
	if isErr {
		if len([]rune(line)) > 60 {
			runes := []rune(line)
			return "✗ " + string(runes[:60]) + "..."
		}
		return "✗ " + line
	}
	if len([]rune(line)) > 60 {
		runes := []rune(line)
		return string(runes[:60]) + "..."
	}
	return line
}

// isErrorResult returns true if the result string looks like an error.
// Uses conservative heuristics to avoid false positives — e.g. a file named
// "error_handler.go" or a commit message containing "error" should not trigger
// this. We require the error marker to appear at the start of the result, at
// the start of a line, or as part of a well-known error pattern.
func isErrorResult(result string) bool {
	if strings.Contains(result, "✗") {
		return true
	}
	lower := strings.ToLower(result)
	// "Error:" at the beginning of the result or after a newline.
	if strings.HasPrefix(lower, "error") ||
		strings.Contains(lower, "\nerror") {
		return true
	}
	// Explicit failure patterns.
	return strings.Contains(lower, "failed:") ||
		strings.Contains(lower, "failed\n") ||
		strings.HasSuffix(lower, "failed")
}

// matchCountSummary returns "N matches" or "no matches" from a glob/grep result.
func matchCountSummary(result string) string {
	if result == "" {
		return "no matches"
	}
	// Count non-empty lines as matches.
	count := 0
	for _, line := range strings.Split(result, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	if count == 0 {
		return "no matches"
	}
	if count == 1 {
		return "1 match"
	}
	return fmt.Sprintf("%d matches", count)
}

// firstMeaningfulLine returns the first non-empty, non-whitespace line from s.
func firstMeaningfulLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			return trimmed
		}
	}
	return s
}

// turnLabel builds the "[agent · model]" label for a turn header.
// Returns an empty string when both agent and model are absent.
func turnLabel(agent, model string) string {
	if agent == "" && model == "" {
		return ""
	}
	if agent == "" {
		return model
	}
	if model == "" {
		return agent
	}
	return agent + " · " + model
}
