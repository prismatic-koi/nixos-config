package prismcontainer

// The Containerfile transport check.
//
// libimage reads an image reference that starts with "<transport>:" from
// that transport, and several transports read a path on the host:
// tarball: takes any tar archive as a layer, oci-archive:, docker-archive:,
// oci:, and dir: read image archives and layouts, and atomic: reads the
// kubeconfig of the host. A build pulls an image for FROM, for COPY or ADD
// --from=, and for RUN --mount=from=. Before podman runs, the check refuses
// a Containerfile that can name a transport other than a registry in any of
// these places. It works the same on every platform.
//
// The check does not expand or unquote a reference. Buildah expands ARG
// and ENV values, quotes, and escapes before it reads a reference, and a
// copy of that word processing in prism can always differ from buildah in
// some case. Thus an image reference must be a literal token: only the
// characters of an image reference, with no "$", quote, or backslash.
// Then nothing can build a transport prefix that the check does not see.
//
// Each line is read in two ways, and both must pass:
//
//   - The parser view copies how the Dockerfile parser of buildah splits a
//     line (splitCommand and extractBuilderFlags in imagebuilder): the
//     keyword at ASCII whitespace, then the flags byte by byte. The parser
//     reads the bytes 0x85 and 0xA0 as spaces, also inside a UTF-8
//     character, and it lower-cases the keyword with strings.ToLower, so
//     "ONBUİLD" is ONBUILD. This view finds every flag that buildah finds.
//   - The word view splits at whitespace runes and keeps quotes and
//     backslashes. The literal rule uses it.
//
// In both views, a flag that holds a byte above 0x7F is refused, so that
// the two ways to split a line cannot give different flags.
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

var (
	escapeDirective = regexp.MustCompile(`(?i)^\s*#\s*escape\s*=\s*(\S*)`)
	// literalReference is an image reference, a stage name, or a stage
	// index, written literally.
	literalReference = regexp.MustCompile(`^(docker://)?[A-Za-z0-9._/:@-]+$`)
	// literalFlagName is the name part of a flag, written literally.
	literalFlagName = regexp.MustCompile(`^--[A-Za-z0-9-]+$`)
	// parserWhitespace is tokenWhitespace of the Dockerfile parser.
	parserWhitespace = regexp.MustCompile(`[\t\v\f\r ]+`)
)

func refuseFile(line int, format string, args ...any) error {
	return fmt.Errorf("the Containerfile cannot be built: line %d: %s", line, fmt.Sprintf(format, args...))
}

// literalHint ends each refusal of a reference that is not literal.
const literalHint = "Write the image literally: no ARG or other variable, quote, or backslash in FROM, --from, or --mount"

// checkContainerfile refuses a Containerfile that can make podman read an
// image from a transport other than a registry.
func checkContainerfile(data []byte) error {
	// The Dockerfile parser drops a byte order mark at the start.
	text := strings.TrimPrefix(string(data), "\ufeff")
	physical := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, line := range physical {
		if m := escapeDirective.FindStringSubmatch(line); m != nil && m[1] != `\` {
			return refuseFile(i+1, "the escape directive %q is not supported: remove it, so that \\ is the escape character", m[1])
		}
	}
	for _, l := range append(joinContinuations(physical, true), joinContinuations(physical, false)...) {
		if err := checkInstruction(l.line, l.text, true); err != nil {
			return err
		}
	}
	// A physical line is read too, in case buildah joins lines in a way
	// that the joins above do not. Most physical lines inside a joined
	// line are not instructions (shell code, SQL), so only a definite
	// transport reference is refused here.
	for i, line := range physical {
		if err := checkInstruction(i+1, line, false); err != nil {
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
			// A comment line ends at its own end, also with a "\" there.
			if strings.HasPrefix(trimmed, "#") {
				out = append(out, joinedLine{start, line})
				continue
			}
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

// checkInstruction checks one line in the parser view and in the word
// view. With strict false, only a literal transport reference is refused.
func checkInstruction(line int, text string, strict bool) error {
	if err := checkWordView(line, text, strict); err != nil {
		return err
	}
	return checkParserView(line, text, strict)
}

// checkWordView checks one line split at whitespace runes. In a FROM line,
// the first word that is not a flag is the image, and it must be literal.
// The later words (AS and the stage name, or the text of a heredoc line
// that starts with "from") get the transport check only. In every other
// line, the flag words at the start (after ONBUILD, when it is there) are
// read: the value of --from, and the from= values of --mount.
func checkWordView(line int, text string, strict bool) error {
	words := rawWords(text)
	if len(words) == 0 || strings.HasPrefix(words[0], "#") {
		return nil
	}
	if strings.ToLower(words[0]) == "onbuild" {
		words = words[1:]
	}
	if len(words) == 0 {
		return nil
	}
	if strings.ToLower(words[0]) == "from" {
		image := true
		for _, w := range words[1:] {
			if strings.HasPrefix(w, "--") {
				if strict && hasNonASCII(w) {
					return refuseNonASCIIFlag(line, w)
				}
				continue
			}
			if err := checkReference(line, w, strict && image); err != nil {
				return err
			}
			image = false
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
		name, value, hasValue := strings.Cut(w, "=")
		if !literalFlagName.MatchString(name) {
			if strict {
				return refuseFile(line, "the flag %q has a name that is not literal. Write flag names literally", w)
			}
			continue
		}
		if strict && hasNonASCII(w) {
			return refuseNonASCIIFlag(line, w)
		}
		switch asciiLower(name) {
		case "--from":
			if !hasValue {
				if i+1 >= len(flags) {
					continue
				}
				value = flags[i+1]
			}
			if err := checkReference(line, value, strict); err != nil {
				return err
			}
		case "--mount":
			if err := checkMount(line, value, strict); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkParserView checks one line as the Dockerfile parser of buildah
// splits it. The parser has already removed the quotes and backslashes of
// a flag, so the literal rule does not apply to a flag here. The word view
// applies it.
func checkParserView(line int, text string, strict bool) error {
	cmd, flags, args := splitParserLine(text)
	var allFlags []string
	allFlags = append(allFlags, flags...)
	if cmd == "onbuild" {
		cmd, flags, args = splitParserLine(args)
		allFlags = append(allFlags, flags...)
	}
	for _, f := range allFlags {
		if strict && hasNonASCII(f) {
			return refuseNonASCIIFlag(line, f)
		}
		name, value, _ := strings.Cut(f, "=")
		switch strings.ToLower(name) {
		case "--from":
			if err := checkReference(line, value, strict); err != nil {
				return err
			}
		case "--mount":
			if err := checkMount(line, value, strict); err != nil {
				return err
			}
		}
	}
	if cmd == "from" {
		for i, w := range parserWhitespace.Split(args, -1) {
			if w == "" {
				continue
			}
			if err := checkReference(line, w, strict && i == 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// splitParserLine splits one line as splitCommand of the Dockerfile parser
// does: the keyword (lower case), the flags, and the rest.
func splitParserLine(text string) (string, []string, string) {
	parts := parserWhitespace.Split(strings.TrimSpace(text), 2)
	cmd := strings.ToLower(parts[0])
	if len(parts) < 2 {
		return cmd, nil, ""
	}
	args, flags := extractParserFlags(parts[1])
	return cmd, flags, strings.TrimSpace(args)
}

// extractParserFlags is a copy of extractBuilderFlags of the Dockerfile
// parser (openshift/imagebuilder, dockerfile/parser/split_command.go). It
// reads the line byte by byte, as the parser does. Do not change it to
// read runes: then it no longer finds the flags that buildah finds.
func extractParserFlags(line string) (string, []string) {
	const (
		inSpaces = iota
		inWord
		inQuote
	)
	var words []string
	phase := inSpaces
	word := ""
	quote := '\000'
	blankOK := false
	var ch rune
	for pos := 0; pos <= len(line); pos++ {
		if pos != len(line) {
			ch = rune(line[pos])
		}
		if phase == inSpaces {
			if pos == len(line) {
				break
			}
			if unicode.IsSpace(ch) {
				continue
			}
			if ch != '-' || pos+1 == len(line) || rune(line[pos+1]) != '-' {
				return line[pos:], words
			}
			phase = inWord
		}
		if (phase == inWord || phase == inQuote) && pos == len(line) {
			if word != "--" && (blankOK || len(word) > 0) {
				words = append(words, word)
			}
			break
		}
		if phase == inWord {
			if unicode.IsSpace(ch) {
				phase = inSpaces
				if word == "--" {
					return line[pos:], words
				}
				if blankOK || len(word) > 0 {
					words = append(words, word)
				}
				word = ""
				blankOK = false
				continue
			}
			if ch == '\'' || ch == '"' {
				quote = ch
				blankOK = true
				phase = inQuote
				continue
			}
			if ch == '\\' {
				if pos+1 == len(line) {
					continue
				}
				pos++
				ch = rune(line[pos])
			}
			word += string(ch)
			continue
		}
		if phase == inQuote {
			if ch == quote {
				phase = inWord
				continue
			}
			if ch == '\\' {
				if pos+1 == len(line) {
					phase = inWord
					continue
				}
				pos++
				ch = rune(line[pos])
			}
			word += string(ch)
		}
	}
	return "", words
}

func hasNonASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 0x7f {
			return true
		}
	}
	return false
}

func refuseNonASCIIFlag(line int, flag string) error {
	return refuseFile(line, "the flag %q holds a character that is not ASCII. Write flags in ASCII only", flag)
}

// checkMount checks the from= values of one --mount value. A "$", a quote,
// or a backslash anywhere in the value can build a key or a separator, so
// it is refused.
func checkMount(line int, value string, strict bool) error {
	if strings.ContainsAny(value, "$\"'\\") {
		if strict {
			return refuseFile(line, "the --mount value %q holds a variable, a quote, or a backslash. %s", value, literalHint)
		}
		return nil
	}
	for _, field := range strings.Split(value, ",") {
		key, ref, ok := strings.Cut(field, "=")
		if ok && asciiLower(strings.TrimSpace(key)) == "from" {
			if err := checkReference(line, ref, strict); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkReference refuses an image reference that is not literal, or that
// names a transport other than a registry. With strict false, only the
// second is refused.
func checkReference(line int, ref string, strict bool) error {
	for _, t := range buildRefusedTransports() {
		if strings.HasPrefix(ref, t+":") {
			return refuseFile(line, "the image source %q uses the %q transport. Prism builds only from registry images, local images, and build stages", ref, t)
		}
	}
	if strict && !literalReference.MatchString(ref) {
		return refuseFile(line, "the image reference %q is not literal. %s", ref, literalHint)
	}
	return nil
}

// asciiLower lower-cases the ASCII letters of s and keeps every other
// byte. The result has the same length as s, byte for byte.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
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
