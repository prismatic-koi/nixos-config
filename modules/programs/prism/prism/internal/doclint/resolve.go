package doclint

import (
	"fmt"
	"strings"
)

// resolve attempts to satisfy a token against the source index, returning
// (ok, rule, note). ok=true means the token resolved and no finding should
// be emitted. When ok=false the returned rule and note explain what was
// tried, for debuggability.
func resolve(token string, class tokenClass, idx *sourceIndex, repoRoot string) (bool, string, string) {
	switch class {
	case classFilePath:
		return resolveFilePath(token, idx, repoRoot)
	case classFileWithMember:
		return resolveFileWithMember(token, idx, repoRoot)
	case classBareFilename:
		return resolveBareFilename(token, idx)
	case classDotted:
		return resolveDotted(token, idx)
	case classGoIdent:
		return resolveGoIdent(token, idx)
	case classSnakeCase:
		return resolveSnakeCase(token, idx)
	case classEnvVar:
		return resolveEnvVar(token, idx)
	case classColonToken:
		return resolveColonToken(token, idx)
	default:
		// Skip class — never called; treat as resolved for safety.
		return true, "skip", ""
	}
}

func resolveFilePath(token string, idx *sourceIndex, repoRoot string) (bool, string, string) {
	if idx.filePathExists(token, repoRoot) {
		return true, "file_path", ""
	}
	return false, "file_path", "path does not exist under prism source root or repo root"
}

func resolveBareFilename(token string, idx *sourceIndex) (bool, string, string) {
	if idx.hasBasename(token) {
		return true, "bare_filename", ""
	}
	return false, "bare_filename", "no file with this basename exists under the indexed source tree"
}

func resolveFileWithMember(token string, idx *sourceIndex, repoRoot string) (bool, string, string) {
	m := pathWithMemberRe.FindStringSubmatch(token)
	if m == nil {
		return false, "file_with_member", "regex mismatch (internal error)"
	}
	path, member := m[1], m[2]
	// Path may be a full relative path or a bare basename — both are common
	// in prose. Bare basenames resolve against the walked file index; full
	// relative paths resolve via os.Stat.
	var fileOK bool
	if strings.Contains(path, "/") {
		fileOK = idx.filePathExists(path, repoRoot)
	} else {
		fileOK = idx.hasBasename(path)
	}
	if !fileOK {
		return false, "file_with_member", fmt.Sprintf("file %q does not exist under prism source tree", path)
	}
	if !idx.hasIdent(member) {
		return false, "file_with_member", fmt.Sprintf("identifier %q not found anywhere in indexed source", member)
	}
	return true, "file_with_member", ""
}

func resolveDotted(token string, idx *sourceIndex) (bool, string, string) {
	parts := strings.Split(token, ".")
	// Every segment must appear as an identifier somewhere in the source.
	// This resolves both `Config.MaxMemoryBytes` (Go struct.field) and
	// `agent_status.instance_id` (SQL table.column, which lives in Go
	// string literals whose contents are indexed as identifiers by the
	// loose identRe extraction — table and column names are word-boundary
	// matched in the CREATE TABLE / SELECT strings).
	var missing []string
	for _, p := range parts {
		if !idx.hasIdent(p) {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return true, "dotted", ""
	}
	return false, "dotted", fmt.Sprintf("segments not found in prism .go source: %s", strings.Join(missing, ", "))
}

func resolveGoIdent(token string, idx *sourceIndex) (bool, string, string) {
	if idx.hasIdent(token) {
		return true, "go_ident", ""
	}
	return false, "go_ident", "identifier not found anywhere in prism .go source"
}

func resolveSnakeCase(token string, idx *sourceIndex) (bool, string, string) {
	if idx.hasIdent(token) {
		return true, "snake_case", ""
	}
	return false, "snake_case", "snake_case identifier not found in prism .go source (checked as a Go identifier — includes SQL column/table names embedded in string literals)"
}

func resolveEnvVar(token string, idx *sourceIndex) (bool, string, string) {
	if idx.hasIdent(token) {
		return true, "env_var", ""
	}
	return false, "env_var", "env-var-shape identifier not found in prism .go source"
}

func resolveColonToken(token string, idx *sourceIndex) (bool, string, string) {
	// Split on the first `:`. The prefix is the reason; the suffix is
	// either a placeholder or an example value. If either half fails,
	// annotate which half.
	i := strings.Index(token, ":")
	prefix := token[:i]
	suffix := ""
	if i+1 < len(token) {
		suffix = token[i+1:]
	}

	// Prefix must appear as either an identifier or a string-literal
	// substring in the source (audit reasons like `host_bind` show up as
	// bare Go idents in the format string).
	prefixLiteral := prefix + ":"
	prefixOK := idx.hasIdent(prefix) || idx.hasStringLiteralContaining(prefixLiteral)
	if !prefixOK {
		return false, "colon_token", fmt.Sprintf("audit-reason prefix %q not found as Go ident or in any string literal under prism source", prefix)
	}
	if suffix == "" {
		return true, "colon_token", ""
	}
	// Suffix classification. If the suffix is a placeholder or all-lowercase
	// value or looks like a value not an identifier, pass. If it looks like
	// an identifier (env-var or Go ident), require it to resolve.
	subClass := classify(suffix)
	switch subClass {
	case classSkip:
		// Placeholder or non-identifier — accept.
		return true, "colon_token", ""
	default:
		ok, _, _ := resolve(suffix, subClass, idx, "")
		if ok {
			return true, "colon_token", ""
		}
		return false, "colon_token", fmt.Sprintf("audit-reason value %q not found under prism source (prefix %q resolved)", suffix, prefix)
	}
}
