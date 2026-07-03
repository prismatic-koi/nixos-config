package doclint

import (
	"regexp"
	"strings"
)

// tokenClass is the classification of a backticked token. Only tokens that
// classify as one of the identifier-shaped classes are checked; everything
// else is skipped (see the comments in classify below for the specific skip
// rules).
type tokenClass int

const (
	classSkip           tokenClass = iota
	classFilePath                  // relative path with at least one `/`
	classFileWithMember            // `path/to/file.go::Identifier`
	classBareFilename              // `proxy_test.go` — no directory prefix, but has a known extension
	classDotted                    // `Foo.Bar` or `agent_status.instance_id`
	classGoIdent                   // `hostConfig`, `checkHostConfig`, `NewIsolated`
	classSnakeCase                 // `agent_max_open_files_soft`
	classEnvVar                    // `CONTAINER_HOST`, `SYS_ADMIN`
	classColonToken                // `host_bind:<path>`, `cap_add:SYS_ADMIN`
)

func (c tokenClass) String() string {
	switch c {
	case classFilePath:
		return "file_path"
	case classFileWithMember:
		return "file_with_member"
	case classBareFilename:
		return "bare_filename"
	case classDotted:
		return "dotted"
	case classGoIdent:
		return "go_ident"
	case classSnakeCase:
		return "snake_case"
	case classEnvVar:
		return "env_var"
	case classColonToken:
		return "colon_token"
	default:
		return "skip"
	}
}

var (
	goIdentRe        = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	pathRe           = regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_./-]*[A-Za-z0-9_]$`)
	envVarRe         = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	snakeCaseWordRe  = regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)+$`)
	pathWithMemberRe = regexp.MustCompile(`^([^:]+)::([A-Za-z_][A-Za-z0-9_]*)$`)

	// knownFileExtensions is the allowlist of RHS extensions that convert a
	// dotted `foo.ext` token from a struct-field reference into a
	// bare-filename lookup. Deliberately narrow: extensions common in this
	// repo (Go/Nix/JS/TS/SQL/YAML/JSON/TOML/Markdown). Additions should be
	// low-risk (unambiguously a file extension in this project's docs).
	knownFileExtensions = map[string]bool{
		"go":   true,
		"nix":  true,
		"md":   true,
		"sql":  true,
		"json": true,
		"yaml": true,
		"yml":  true,
		"toml": true,
		"ts":   true,
		"tsx":  true,
		"js":   true,
		"jsx":  true,
		"sh":   true,
		"log":  true,
		"conf": true,
		"txt":  true,
	}
)

// classify decides which tokenClass a stripped token belongs to. A token
// classified as classSkip is never checked.
//
// The rules are deliberately conservative — high precision beats high
// recall for a lint that lands green on the whole current tree. When
// unsure, prefer classSkip.
func classify(t string) tokenClass {
	if t == "" {
		return classSkip
	}
	// Placeholders like `<sessionName>`, `<path>`, or English phrases
	// wrapped in angle brackets are never identifiers. There is one
	// exception: audit-reason-with-placeholder-value tokens of the shape
	// `prefix:<placeholder>` (e.g. `host_bind:<path>`,
	// `unknown_field:<json error>`). Those are real colon-tokens whose
	// prefix should still resolve; the resolver handles the placeholder
	// value by skipping it.
	if strings.ContainsAny(t, "<>") {
		if strings.HasPrefix(t, "<") {
			return classSkip
		}
		i := strings.Index(t, ":")
		if i <= 0 {
			return classSkip
		}
		prefix := t[:i]
		if strings.ContainsAny(prefix, "<>") {
			return classSkip
		}
		return classColonToken
	}
	// Multi-word tokens (e.g. `case "bind"`, `-- do X`) are never single
	// identifiers.
	if strings.ContainsAny(t, " \t") {
		return classSkip
	}
	// Curly-brace group notation like `{bind, volume, tmpfs}` — not an
	// identifier.
	if strings.ContainsAny(t, "{}") {
		return classSkip
	}
	// Parens or brackets are also groupy notation.
	if strings.ContainsAny(t, "()[]") {
		return classSkip
	}
	// Quoted content is prose or a value, not an identifier.
	if strings.ContainsAny(t, `"'`) {
		return classSkip
	}
	// Assignments like `containers_enabled=1`, `HOME=/homeless-shelter` —
	// checking the LHS is worth doing but complicates the rule matrix;
	// keep them out of MVP scope. The relevant column name will typically
	// also appear elsewhere in the docs on its own.
	if strings.Contains(t, "=") {
		return classSkip
	}
	// URLs and command-line flags are not identifiers.
	if strings.Contains(t, "://") {
		return classSkip
	}
	if strings.HasPrefix(t, "--") || (strings.HasPrefix(t, "-") && len(t) == 2) {
		return classSkip
	}
	// Shell / env references like `$HOME`, `$XDG_STATE_HOME/prism/run/`.
	// Only the bare `$HOME` form ends up worth resolving, and it is a
	// well-known env var — better handled by env-var resolution below,
	// so strip the leading `$` and let it fall through if it survives.
	if strings.HasPrefix(t, "$") {
		return classSkip
	}
	// Tilde-home references (`~/.config/prism/agents/`) — path shape but
	// not resolvable against the source tree without $HOME. Skip.
	if strings.HasPrefix(t, "~") {
		return classSkip
	}
	// GitHub issue / PR shorthand: `#1234 §4` etc.
	if strings.HasPrefix(t, "#") {
		return classSkip
	}
	// Comma-lists, wildcards, glob patterns.
	if strings.ContainsAny(t, ",*|?&\\") {
		return classSkip
	}

	// `path/to/file::Ident` — file with member.
	if pathWithMemberRe.MatchString(t) {
		return classFileWithMember
	}
	// Path-shaped: has a `/`.
	if strings.Contains(t, "/") {
		if pathRe.MatchString(t) {
			return classFilePath
		}
		return classSkip
	}
	// Colon-token: audit-log reason like `host_bind:<path>` or
	// `cap_add:SYS_ADMIN`.
	if strings.Contains(t, ":") {
		return classColonToken
	}
	// Dotted or bare-filename: single-dot tokens with a recognised file
	// extension on the RHS (`proxy_test.go`, `pi.nix`, `flake.nix`) go
	// through the file-basename resolver rather than being split into
	// unrelated identifier segments.
	if strings.Contains(t, ".") {
		parts := strings.Split(t, ".")
		for _, p := range parts {
			if p == "" || !goIdentRe.MatchString(p) {
				return classSkip
			}
		}
		if len(parts) == 2 && knownFileExtensions[parts[1]] {
			return classBareFilename
		}
		return classDotted
	}
	// Env-var-shape: all uppercase (with optional digits/underscore). At
	// least 3 chars so we don't false-positive on acronyms like `OK`, `IO`.
	if envVarRe.MatchString(t) && len(t) >= 3 {
		return classEnvVar
	}
	// Snake_case identifier: `agent_max_open_files_soft`. Requires at least
	// one underscore between lowercase words.
	if snakeCaseWordRe.MatchString(t) {
		return classSnakeCase
	}
	// Go identifier with a mixed-case shape (contains both lowercase and
	// uppercase letters). Pure lowercase words without underscores are
	// English prose (`allow`, `bwrap`, `bind`, `build`) and are skipped.
	if goIdentRe.MatchString(t) && hasBothCases(t) {
		return classGoIdent
	}
	return classSkip
}

func hasBothCases(s string) bool {
	hasUpper := false
	hasLower := false
	for _, r := range s {
		switch {
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		case r >= 'a' && r <= 'z':
			hasLower = true
		}
	}
	return hasUpper && hasLower
}
