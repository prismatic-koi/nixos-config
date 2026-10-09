package prismcontainer

// The Containerfile transport check.
//
// libimage reads an image reference that starts with "<transport>:" from
// that transport, and several transports read a path on the host:
// tarball: takes any tar archive as a layer, oci-archive:, docker-archive:,
// oci:, and dir: read image archives and layouts, and atomic: reads the
// kubeconfig of the host. A build pulls an image for FROM, for COPY
// --from=, and for RUN --mount=from=. The check refuses a Containerfile
// that can name a transport other than a registry in any of these places,
// before podman runs. It works the same on every platform.
//
// The check matches more than buildah does. A match that buildah does not
// make only refuses a build, so the check takes the wider reading wherever
// the two can differ:
//
//   - It reads every physical line and every line that continuation
//     joins, with and without comment lines.
//   - It expands only $VAR, ${VAR}, ${VAR:-word}, ${VAR-word},
//     ${VAR:+word}, and ${VAR+word}, and refuses every other use of "$".
//   - A variable has every value that the Containerfile, a --build-arg,
//     or a platform argument gives it, and the empty value.
//   - A --mount or --from flag must hold no variable at all: a base image
//     can set an ENV of any name, and the check cannot see it.
//
// An ONBUILD trigger of a base image runs instructions that are not in the
// Containerfile, so this check cannot see them. On Linux the signature
// policy of the build (ScopeExecutor) refuses those transports too.

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
)

// buildRefusedTransports are the transport names that a build must not
// use. "docker" is not in the list: "docker://" is a registry reference,
// and "docker:<tag>" is the Docker Hub image "docker".
func buildRefusedTransports() []string {
	var names []string
	for name := range imageTransports {
		if name != "docker" {
			names = append(names, name)
		}
	}
	return names
}

// platformArgs are the ARG names that buildah sets itself.
var platformArgs = []string{
	"TARGETPLATFORM", "TARGETOS", "TARGETARCH", "TARGETVARIANT",
	"BUILDPLATFORM", "BUILDOS", "BUILDARCH", "BUILDVARIANT",
}

var platformArgValues = []string{"linux/amd64", "linux/arm64", "linux", "amd64", "arm64", "v8"}

// maxExpansions limits the number of values that one word or one variable
// can expand to.
const maxExpansions = 256

// maxVarDepth limits how deep a variable value can name other variables.
const maxVarDepth = 8

// unexpandable is the value of a variable that the check cannot expand. A
// FROM line that uses the variable is refused.
const unexpandable = "\x00unexpandable"

var (
	escapeDirective = regexp.MustCompile(`(?i)^\s*#\s*escape\s*=\s*(\S*)`)
	varName         = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)
)

func refuseFile(line int, format string, args ...any) error {
	return fmt.Errorf("the Containerfile cannot be built: line %d: %s", line, fmt.Sprintf(format, args...))
}

// checkContainerfile refuses a Containerfile that can make podman read an
// image from a transport other than a registry.
func checkContainerfile(data []byte, buildArgs []string) error {
	// The Dockerfile parser drops a byte order mark at the start.
	text := strings.TrimPrefix(string(data), "\ufeff")
	physical := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, line := range physical {
		if m := escapeDirective.FindStringSubmatch(line); m != nil && m[1] != `\` {
			return refuseFile(i+1, "the escape directive %q is not supported: remove it, so that \\ is the escape character", m[1])
		}
	}

	var lines []joinedLine
	for i, line := range physical {
		lines = append(lines, joinedLine{i + 1, line})
	}
	lines = append(lines, joinContinuations(physical, true)...)
	lines = append(lines, joinContinuations(physical, false)...)

	lookup := newVarResolver(lines, buildArgs).lookup
	for _, l := range lines {
		if err := checkInstruction(l.line, l.text, lookup); err != nil {
			return err
		}
	}
	return nil
}

type joinedLine struct {
	line int
	text string
}

// joinContinuations joins each line that ends in "\" with the next line.
// With skipComments, a comment line or an empty line inside a joined line
// is left out, as the Dockerfile parser does.
func joinContinuations(physical []string, skipComments bool) []joinedLine {
	var out []joinedLine
	var cur strings.Builder
	start := 0
	open := false
	for i, line := range physical {
		trimmed := strings.TrimSpace(line)
		if open && skipComments && (trimmed == "" || strings.HasPrefix(trimmed, "#")) {
			continue
		}
		if !open {
			start = i + 1
		}
		body := strings.TrimRight(line, " \t")
		if strings.HasSuffix(body, `\`) {
			cur.WriteString(strings.TrimSuffix(body, `\`))
			open = true
			continue
		}
		cur.WriteString(body)
		out = append(out, joinedLine{start, cur.String()})
		cur.Reset()
		open = false
	}
	if open {
		out = append(out, joinedLine{start, cur.String()})
	}
	return out
}

// varResolver gives every value that a variable can have in a FROM line.
type varResolver struct {
	raw      map[string][]string
	resolved map[string][]string
	active   map[string]bool
}

// newVarResolver collects the ARG and ENV values of the Containerfile, the
// --build-arg values, and the platform values. Each raw value is a word
// that expandWord reads.
func newVarResolver(lines []joinedLine, buildArgs []string) *varResolver {
	r := &varResolver{raw: map[string][]string{}, resolved: map[string][]string{}, active: map[string]bool{}}
	add := func(name, value string) { r.raw[name] = append(r.raw[name], value) }
	for _, a := range buildArgs {
		k, v, _ := strings.Cut(a, "=")
		add(k, literalWord(v))
		if strings.Contains(v, "$") {
			add(k, v)
		}
	}
	for _, name := range platformArgs {
		for _, v := range platformArgValues {
			add(name, literalWord(v))
		}
	}
	for _, l := range lines {
		words := rawWords(l.text)
		if len(words) < 2 {
			continue
		}
		kind := strings.ToUpper(words[0])
		if kind != "ARG" && kind != "ENV" {
			continue
		}
		for _, w := range words[1:] {
			if name, value, ok := strings.Cut(w, "="); ok {
				add(name, value)
			}
		}
		if kind == "ENV" && !strings.Contains(words[1], "=") && len(words) > 2 {
			// ENV NAME value: the value is the rest of the line.
			add(words[1], strings.Join(words[2:], " "))
		}
	}
	return r
}

func (r *varResolver) lookup(name string) []string {
	if v, ok := r.resolved[name]; ok {
		return v
	}
	raw, ok := r.raw[name]
	if !ok {
		return []string{""}
	}
	if r.active[name] || len(r.active) >= maxVarDepth {
		return []string{unexpandable}
	}
	r.active[name] = true
	values := []string{""}
	seen := map[string]bool{"": true}
	for _, w := range raw {
		expanded, err := expandWord(w, r.lookup)
		if err != nil {
			expanded = []string{unexpandable}
		}
		for _, e := range expanded {
			if !seen[e] {
				seen[e] = true
				values = append(values, e)
			}
		}
	}
	if len(values) > maxExpansions {
		values = []string{unexpandable}
	}
	delete(r.active, name)
	r.resolved[name] = values
	return values
}

// literalWord quotes s so that expandWord returns it unchanged.
func literalWord(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// checkInstruction checks one line. A FROM line has every word checked. In
// every other line, the flag words at the start (after ONBUILD, when it is
// there) have each --from and from= value checked.
func checkInstruction(line int, text string, lookup func(string) []string) error {
	words := rawWords(text)
	if len(words) == 0 || strings.HasPrefix(words[0], "#") {
		return nil
	}
	if strings.EqualFold(words[0], "ONBUILD") {
		words = words[1:]
	}
	if len(words) == 0 {
		return nil
	}
	if strings.EqualFold(words[0], "FROM") {
		for _, w := range words[1:] {
			values, err := expandWord(w, lookup)
			if err != nil {
				return refuseFile(line, "prism cannot check the FROM word %q: %v", w, err)
			}
			for _, v := range values {
				if err := checkReference(line, v); err != nil {
					return err
				}
			}
		}
		return nil
	}
	// A physical line inside a joined line can start with a flag.
	flags := words
	if !strings.HasPrefix(words[0], "--") {
		flags = words[1:]
	}
	for i, w := range flags {
		if !strings.HasPrefix(w, "--") {
			break
		}
		lower := strings.ToLower(w)
		if !strings.HasPrefix(lower, "--mount") && !strings.Contains(lower, "from") {
			continue
		}
		values, err := expandWord(w, nil)
		if err != nil {
			return refuseFile(line, "prism cannot check the flag %q: %v. Give --mount and --from with no variable", w, err)
		}
		for _, v := range values {
			if strings.EqualFold(v, "--from") && i+1 < len(flags) {
				next, err := expandWord(flags[i+1], nil)
				if err != nil {
					return refuseFile(line, "prism cannot check the --from value %q: %v. Give the value with no variable", flags[i+1], err)
				}
				for _, n := range next {
					if err := checkReference(line, n); err != nil {
						return err
					}
				}
			}
			if err := checkFromValues(line, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkFromValues checks each from= value in one expanded flag word, up to
// the next ",".
func checkFromValues(line int, word string) error {
	lower := strings.ToLower(word)
	for i := 0; ; {
		at := strings.Index(lower[i:], "from=")
		if at < 0 {
			return nil
		}
		start := i + at + len("from=")
		value, _, _ := strings.Cut(word[start:], ",")
		if err := checkReference(line, value); err != nil {
			return err
		}
		i = start
	}
}

// checkReference refuses an image reference that names a transport other
// than a registry.
func checkReference(line int, ref string) error {
	if strings.Contains(ref, unexpandable) {
		return refuseFile(line, "a variable in a FROM line has a value that prism cannot check. In ARG, ENV, and --build-arg values, use no \"$\" other than $VAR and ${VAR...}")
	}
	for _, t := range buildRefusedTransports() {
		if strings.HasPrefix(ref, t+":") {
			return refuseFile(line, "the image source %q uses the %q transport. Prism builds only from registry images, local images, and build stages", ref, t)
		}
	}
	return nil
}

// rawWords splits text at whitespace outside quotes, with no expansion.
// A backslash keeps the next character in the word.
func rawWords(text string) []string {
	var words []string
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for _, r := range text {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			cur.WriteRune(r)
			escaped = true
			inWord = true
		case quote != 0:
			cur.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			cur.WriteRune(r)
			quote = r
			inWord = true
		case unicode.IsSpace(r):
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// expandWord removes the quotes of one word and expands its variables
// through lookup. It returns every value that the word can have. A nil
// lookup makes any variable an error.
func expandWord(word string, lookup func(string) []string) ([]string, error) {
	results := []string{""}
	appendLit := func(s string) {
		for i := range results {
			results[i] += s
		}
	}
	appendAll := func(values []string) error {
		var next []string
		for _, r := range results {
			for _, v := range values {
				next = append(next, r+v)
			}
		}
		if len(next) > maxExpansions {
			return fmt.Errorf("the word has more than %d possible values", maxExpansions)
		}
		results = next
		return nil
	}

	runes := []rune(word)
	var quote rune
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				appendLit(string(r))
			}
		case r == '\\':
			if i+1 >= len(runes) {
				appendLit(`\`)
				continue
			}
			n := runes[i+1]
			if quote == '"' && n != '"' && n != '$' && n != '\\' {
				appendLit(`\`)
				continue
			}
			appendLit(string(n))
			i++
		case r == '"':
			if quote == '"' {
				quote = 0
			} else {
				quote = '"'
			}
		case r == '\'' && quote == 0:
			quote = '\''
		case r == '$':
			if lookup == nil {
				return nil, fmt.Errorf("it holds a variable")
			}
			values, consumed, err := expandVar(string(runes[i+1:]), lookup)
			if err != nil {
				return nil, err
			}
			if err := appendAll(values); err != nil {
				return nil, err
			}
			i += consumed
		default:
			appendLit(string(r))
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("a quote is not closed")
	}
	return results, nil
}

// expandVar expands the variable reference that follows a "$" in s. It
// returns the values and the number of runes it read.
func expandVar(s string, lookup func(string) []string) ([]string, int, error) {
	if name := varName.FindString(s); name != "" {
		return lookup(name), len([]rune(name)), nil
	}
	if !strings.HasPrefix(s, "{") {
		return nil, 0, fmt.Errorf("\"$\" is not followed by a variable name")
	}
	name := varName.FindString(s[1:])
	if name == "" {
		return nil, 0, fmt.Errorf("the form \"${%s\" is not supported", firstRunes(s[1:], 8))
	}
	after := s[1+len(name):]
	if strings.HasPrefix(after, "}") {
		return lookup(name), len([]rune(s[:len(name)+2])), nil
	}
	op := ""
	for _, o := range []string{":-", ":+", "-", "+"} {
		if strings.HasPrefix(after, o) {
			op = o
			break
		}
	}
	if op == "" {
		return nil, 0, fmt.Errorf("the form \"${%s%s\" is not supported: use $VAR, ${VAR}, ${VAR:-word}, ${VAR-word}, ${VAR:+word}, or ${VAR+word}", name, firstRunes(after, 2))
	}
	wordStart := 1 + len(name) + len(op)
	end := matchingBrace(s, wordStart)
	if end < 0 {
		return nil, 0, fmt.Errorf("\"${%s\" is not closed", name)
	}
	alt, err := expandWord(s[wordStart:end], lookup)
	if err != nil {
		return nil, 0, err
	}
	consumed := len([]rune(s[:end+1]))
	// The check does not know if the variable is set, so it takes both
	// results of the operator.
	if op == ":-" || op == "-" {
		return append(append([]string{}, lookup(name)...), alt...), consumed, nil
	}
	return append([]string{""}, alt...), consumed, nil
}

// matchingBrace returns the index of the "}" that closes the "${" before
// start, with nested "${...}" skipped.
func matchingBrace(s string, start int) int {
	depth := 0
	for i := start; i < len(s); i++ {
		switch {
		case s[i] == '\\':
			i++
		case strings.HasPrefix(s[i:], "${"):
			depth++
			i++
		case s[i] == '}':
			if depth == 0 {
				return i
			}
			depth--
		}
	}
	return -1
}

func firstRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		r = r[:n]
	}
	return string(r)
}
