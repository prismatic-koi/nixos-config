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
// Buildah starts an instruction at a physical line and joins the
// continuation lines that follow it. A heredoc body is read raw and starts
// no instruction. Prism does not track heredocs. It reads an instruction
// from every physical line instead, joined with its continuation lines as
// the parser joins them (instructionTexts). The check refuses an escape
// character other than "\", so the joins agree. Each instruction that
// buildah parses is then one of the texts that prism checks. A heredoc body line
// is checked too, which can refuse a line that buildah does not read as an
// instruction.
//
// Each text is read in two ways, and both must pass:
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
	"unicode/utf8"
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
	// escapeDirective matches a line after leading whitespace is removed.
	escapeDirective = regexp.MustCompile(`(?i)^#\s*escape\s*=\s*(\S*)`)
	// literalReference is an image reference, a stage name, or a stage
	// index, written literally.
	literalReference = regexp.MustCompile(`^(docker://)?[A-Za-z0-9._/:@-]+$`)
	// literalFlagName is the name part of a flag, written literally.
	literalFlagName = regexp.MustCompile(`^--[A-Za-z0-9_-]+$`)
	// parserWhitespace is tokenWhitespace of the Dockerfile parser.
	parserWhitespace = regexp.MustCompile(`[\t\v\f\r ]+`)
)

func refuseFile(line int, format string, args ...any) error {
	return fmt.Errorf("the Containerfile cannot be built: line %d: %s", line, fmt.Sprintf(format, args...))
}

// literalHint ends each refusal of a reference that is not literal.
const literalHint = "Write the image literally: no ARG or other variable, quote, or backslash in FROM, --from, or --mount. " + restructureHint

// restructureHint ends each refusal that a physical line can cause. The
// check reads an instruction from every physical line, also from a line of
// shell or SQL text in a RUN continuation or a heredoc.
const restructureHint = "If the line is not an instruction (for example shell or SQL text that starts with FROM, COPY, ADD, or RUN), restructure the text so that the line does not start that way"

// maxContinuationLines limits one instruction and its continuation lines.
// The check reads an instruction from each line of a chain, so its cost
// grows with the square of the chain length.
const maxContinuationLines = 200

// maxFromWords is the number of words after the flags of FROM that the
// check reads: the image, AS, and the stage name. Buildah refuses more.
const maxFromWords = 3

// checkContainerfile refuses a Containerfile that can make podman read an
// image from a transport other than a registry.
func checkContainerfile(data []byte) error {
	// The Dockerfile parser drops a byte order mark at the start.
	text := strings.TrimPrefix(string(data), "\ufeff")
	physical := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i, line := range physical {
		// The parser removes leading unicode.IsSpace characters before it
		// reads a directive, \v and U+00A0 included.
		if m := escapeDirective.FindStringSubmatch(strings.TrimLeftFunc(line, unicode.IsSpace)); m != nil && m[1] != `\` {
			return refuseFile(i+1, "the escape directive %q is not supported: remove it, so that \\ is the escape character", m[1])
		}
	}
	for _, skipComments := range []bool{true, false} {
		texts, err := instructionTexts(physical, skipComments)
		if err != nil {
			return err
		}
		for _, t := range texts {
			if err := checkInstruction(t.line, t.text); err != nil {
				return err
			}
		}
	}
	return nil
}

type joinedLine struct {
	line int
	text string
}

// instructionTexts returns an instruction text for each physical line
// that can start an instruction: the line joined with its continuation
// lines, with the continuation backslashes removed. This is how the
// Dockerfile parser joins lines (Parse in imagebuilder):
//
//   - A line that is empty or a comment, after its leading whitespace is
//     removed, starts no instruction.
//   - A line continues when it ends in "\" and spaces or tabs.
//   - With skipComments, an empty line and a comment line inside the
//     chain are left out, as the parser does. Without it, they are kept.
//     That second reading only adds texts to check.
//
// The text of a line inside a chain is a suffix of the text of the chain,
// so the texts share one string.
func instructionTexts(physical []string, skipComments bool) ([]joinedLine, error) {
	var out []joinedLine
	var chain strings.Builder
	type start struct{ line, off int }
	var starts []start
	chainStart := -1
	flush := func() {
		text := chain.String()
		for _, st := range starts {
			out = append(out, joinedLine{st.line, text[st.off:]})
		}
		chain.Reset()
		starts = starts[:0]
		chainStart = -1
	}
	for i, line := range physical {
		trimmed := strings.TrimSpace(line)
		blank := trimmed == "" || strings.HasPrefix(trimmed, "#")
		if chainStart < 0 {
			if blank {
				continue
			}
			chainStart = i
		} else if skipComments && blank {
			continue
		}
		if i-chainStart >= maxContinuationLines {
			return nil, refuseFile(chainStart+1, "the instruction continues over more than %d lines. Split it into smaller instructions", maxContinuationLines)
		}
		starts = append(starts, start{i + 1, chain.Len()})
		body := strings.TrimRight(line, " \t")
		if strings.HasSuffix(body, `\`) {
			chain.WriteString(strings.TrimSuffix(body, `\`))
			continue
		}
		chain.WriteString(line)
		flush()
	}
	if chainStart >= 0 {
		flush()
	}
	return out, nil
}

// checkInstruction checks one instruction text in the parser view and in
// the word view.
func checkInstruction(line int, text string) error {
	if err := checkWordView(line, text); err != nil {
		return err
	}
	return checkParserView(line, text)
}

// imageInstructions are the instructions with flags that can name an
// image: COPY and ADD --from, and RUN --mount from=. FROM is checked on its
// own. Buildah ignores an instruction that it does not know, and the other
// instructions pull no image, so their flags are not read.
var imageInstructions = map[string]bool{"copy": true, "add": true, "run": true}

// checkWordView checks one instruction text split at whitespace runes. In
// FROM, the first word that is not a flag is the image, and it must be
// literal. The next words (AS and the stage name, or the text of a heredoc
// line that starts with "from") get the transport check only. In COPY,
// ADD, and RUN, the flags at the start are read: the value of --from, and
// the from= values of --mount. The scan stops after the words that the
// check needs, so a long instruction costs little.
func checkWordView(line int, text string) error {
	ws := &wordScanner{text: text}
	keyword, ok := ws.next()
	if !ok || strings.HasPrefix(keyword, "#") {
		return nil
	}
	if strings.ToLower(keyword) == "onbuild" {
		// ONBUILD has flags of its own before the instruction.
		for keyword, ok = ws.next(); ok && strings.HasPrefix(keyword, "--"); keyword, ok = ws.next() {
		}
		if !ok {
			return nil
		}
	}
	keyword = strings.ToLower(keyword)
	if keyword == "from" {
		words := 0
		for w, ok := ws.next(); ok && words < maxFromWords; w, ok = ws.next() {
			if strings.HasPrefix(w, "--") {
				if hasNonASCII(w) {
					return refuseNonASCIIFlag(line, w)
				}
				continue
			}
			if err := checkReference(line, w, words == 0); err != nil {
				return err
			}
			words++
		}
		return nil
	}
	if !imageInstructions[keyword] {
		return nil
	}
	for w, ok := ws.next(); ok && strings.HasPrefix(w, "--") && w != "--"; w, ok = ws.next() {
		name, value, hasValue := strings.Cut(w, "=")
		if !literalFlagName.MatchString(name) {
			return refuseFile(line, "the flag %q has a name that is not literal. Write flag names literally. %s", w, restructureHint)
		}
		if hasNonASCII(w) {
			return refuseNonASCIIFlag(line, w)
		}
		switch asciiLower(name) {
		case "--from":
			if !hasValue {
				if value, ok = ws.next(); !ok {
					return nil
				}
			}
			if err := checkReference(line, value, true); err != nil {
				return err
			}
		case "--mount":
			if err := checkMount(line, value); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkParserView checks one instruction text as the Dockerfile parser of
// buildah splits it. The parser view finds every flag that buildah finds,
// so it is a complete check of the flags by itself.
//
// imagebuilder runs ProcessWord on each flag after it extracts the flag,
// so a "$", a quote, or a backslash in a flag can still change it. Thus
// the name of a flag must be literal here too: ProcessWord then leaves the
// name as it is, and a later "=" from an expansion comes after the name.
// The values of --from and --mount get the literal rule in checkReference
// and checkMount.
func checkParserView(line int, text string) error {
	cmd, rest := splitParserKeyword(text)
	if cmd == "onbuild" {
		// ONBUILD has flags of its own before the instruction.
		args, _ := extractParserFlags(rest)
		cmd, rest = splitParserKeyword(args)
	}
	if cmd != "from" && !imageInstructions[cmd] {
		return nil
	}
	args, flags := extractParserFlags(rest)
	args = strings.TrimSpace(args)
	for _, f := range flags {
		if hasNonASCII(f) {
			return refuseNonASCIIFlag(line, f)
		}
		name, value, _ := strings.Cut(f, "=")
		if !literalFlagName.MatchString(name) {
			return refuseFile(line, "the flag %q has a name that is not literal. Write flag names literally. %s", f, restructureHint)
		}
		switch strings.ToLower(name) {
		case "--from":
			if err := checkReference(line, value, true); err != nil {
				return err
			}
		case "--mount":
			if err := checkMount(line, value); err != nil {
				return err
			}
		}
	}
	if cmd == "from" {
		words := parserWhitespace.Split(args, maxFromWords+1)
		for i, w := range words {
			if i == maxFromWords || w == "" {
				break
			}
			if err := checkReference(line, w, i == 0); err != nil {
				return err
			}
		}
	}
	return nil
}

// splitParserKeyword splits one line as splitCommand of the Dockerfile
// parser does: the keyword (lower case) and the rest, which holds the
// flags and the arguments.
func splitParserKeyword(text string) (string, string) {
	parts := parserWhitespace.Split(strings.TrimSpace(text), 2)
	cmd := strings.ToLower(parts[0])
	if len(parts) < 2 {
		return cmd, ""
	}
	return cmd, parts[1]
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
	var word strings.Builder
	takeWord := func() string {
		w := word.String()
		word.Reset()
		return w
	}
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
			if w := takeWord(); w != "--" && (blankOK || len(w) > 0) {
				words = append(words, w)
			}
			break
		}
		if phase == inWord {
			if unicode.IsSpace(ch) {
				phase = inSpaces
				w := takeWord()
				if w == "--" {
					return line[pos:], words
				}
				if blankOK || len(w) > 0 {
					words = append(words, w)
				}
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
			word.WriteRune(ch)
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
			word.WriteRune(ch)
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
	return refuseFile(line, "the flag %q holds a character that is not ASCII. Write flags in ASCII only. %s", flag, restructureHint)
}

// checkMount checks the from= values of one --mount value. A "$", a quote,
// or a backslash anywhere in the value can build a key or a separator, so
// it is refused.
func checkMount(line int, value string) error {
	if strings.ContainsAny(value, "$\"'\\") {
		return refuseFile(line, "the --mount value %q holds a variable, a quote, or a backslash. %s", value, literalHint)
	}
	for _, field := range strings.Split(value, ",") {
		key, ref, ok := strings.Cut(field, "=")
		if ok && asciiLower(strings.TrimSpace(key)) == "from" {
			if err := checkReference(line, ref, true); err != nil {
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

// wordScanner splits text at whitespace runes outside quotes, with no
// expansion. A backslash keeps the next character in the word. It returns
// one word at a time, so a caller can stop early.
type wordScanner struct {
	text string
	pos  int
}

func (s *wordScanner) next() (string, bool) {
	var cur strings.Builder
	inWord := false
	var quote rune
	escaped := false
	for s.pos < len(s.text) {
		r, size := utf8.DecodeRuneInString(s.text[s.pos:])
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
				return cur.String(), true
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
		s.pos += size
	}
	return cur.String(), inWord
}
