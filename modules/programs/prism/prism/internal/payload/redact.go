// Secret redaction for captured event payloads (issue #2589).
//
// Why this file exists
// --------------------
//
// Every frame the harness emits is stored verbatim in prism.db. A command an
// agent runs can print a credential to stdout — `env`, `gh auth token`,
// `curl -v` with an Authorization header, a test that dumps an argv — and the
// value then lands in `agent_events.payload` and stays there until the prune
// job removes it. Nothing in the capture path removed it before this file.
//
// Two layers
// ----------
//
//  1. VALUE matching (primary). The process that captures the frame already
//     holds the literal value of every credential environment variable. An
//     exact match on that value has near-zero false positives: a regex for
//     `ghp_*` guesses, an exact value match knows.
//
//  2. SHAPE matching (secondary). A small set of well-known credential shapes
//     covers the case where the secret is not in the environment of the
//     capturing process. This layer is defence in depth. It never replaces
//     layer 1.
//
// Cost
// ----
//
// Redact runs at most two linear passes over the input:
//
//   - strings.Replacer builds a trie once, at Redactor construction time, and
//     walks the input once. The walk depth is bounded by the longest secret,
//     which is a constant, so the pass is O(len(input)).
//   - The shape layer is one combined RE2 regexp. RE2 is linear in the input
//     with no backtracking.
//
// Neither pass is quadratic in the size of a tool_result payload.
//
// Security rule for this file and its tests
// -----------------------------------------
//
// No function here returns, logs, or embeds a credential value in an error.
// Every test value is synthetic.
package payload

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// MinCredentialValueLen is the shortest environment-variable value the value
// layer treats as a secret.
//
// The guard exists because an empty, whitespace-only, or one-character value
// would otherwise match at nearly every position of ordinary output and shred
// it. Every real credential is far longer than this bound: the shortest token
// prism forwards is a GitHub PAT at 40 characters. A credential shorter than
// the bound is skipped by the value layer and left to the shape layer.
const MinCredentialValueLen = 8

// RedactionMarkerPrefix and RedactionMarkerSuffix bracket the replacement text
// a redacted match is rewritten to. The marker names the environment variable
// (value layer) or the credential shape (shape layer) so the surrounding
// output stays diagnosable.
const (
	RedactionMarkerPrefix = "[redacted:"
	RedactionMarkerSuffix = "]"
)

// RedactionMarker returns the replacement text for a match attributed to name.
func RedactionMarker(name string) string {
	return RedactionMarkerPrefix + name + RedactionMarkerSuffix
}

// Environment-variable name registry.
//
// This registry is the single source of truth for "which environment
// variables carry a credential". internal/container derives its injection
// list from ForwardedCredentialEnvNames, GitHubTokenEnvName,
// GitLabTokenEnvName, and PrismGitHubTokenEnvPrefix, so a credential added
// here is both injected and redacted without a second edit.
//
// internal/payload is a leaf package (stdlib only), which is what lets both
// internal/container and internal/db depend on it without an import cycle.
const (
	// GitHubTokenEnvName is the inherited GitHub token — the final env-var
	// fallback in container.ResolveGitHubToken.
	GitHubTokenEnvName = "GITHUB_TOKEN"

	// GitLabTokenEnvName is the GitLab API token (issue #2668). It is NOT
	// forwarded verbatim from the host environment: container.
	// ResolveGitLabToken resolves it host-side — the sops file named by
	// Config.GitLabTokenPath first, then the inherited env var behind the
	// $(-literal guard — and injects the resulting VALUE, the same shape as
	// GitHubTokenEnvName. It is listed here so the value layer redacts it
	// wherever the capturing process holds it.
	GitLabTokenEnvName = "GITLAB_TOKEN"

	// PrismGitHubTokenEnvPrefix prefixes the per-(account, role) tokens,
	// PRISM_GITHUB_TOKEN_<ACCOUNT>_<ROLE>.
	PrismGitHubTokenEnvPrefix = "PRISM_GITHUB_TOKEN_"
)

// ForwardedCredentialEnvNames are the external-tool credentials that
// internal/container forwards verbatim from the host environment into every
// agent sandbox. Treat it as read-only; callers that need to mutate the
// result must copy it first.
var ForwardedCredentialEnvNames = []string{
	"ANTHROPIC_API_KEY",
	"OPENROUTER_API_KEY",
}

// otherCredentialEnvNames are names internal/container does not forward but
// that can still be present in the environment of a host-mode agent, or of
// the sidecar itself. Redacting a value that was never going to appear costs
// nothing; missing one costs a leaked credential.
var otherCredentialEnvNames = []string{
	"ANTHROPIC_AUTH_TOKEN",
	"ATLASSIAN_API_TOKEN",
	"AWS_SECRET_ACCESS_KEY",
	"AWS_SESSION_TOKEN",
	"CACHIX_AUTH_TOKEN",
	"DEEPSEEK_API_KEY",
	"GEMINI_API_KEY",
	"GH_TOKEN",
	"GOOGLE_API_KEY",
	"GRAFANA_API_KEY",
	"NOTION_API_KEY",
	"NPM_TOKEN",
	"OPENAI_API_KEY",
	"SLACK_TOKEN",
}

// credentialEnvPrefixes match a whole family of names by prefix.
var credentialEnvPrefixes = []string{
	PrismGitHubTokenEnvPrefix,
}

// credentialEnvNameSuffixes are a name-shape heuristic so a credential
// nobody listed is still caught. A name that ends in one of these carries a
// secret by convention.
//
// Every suffix here ends the name. `GITHUB_TOKEN_PATH` and
// `SOPS_AGE_KEY_FILE` therefore do NOT match — they name a file, not a
// secret, and redacting a path would corrupt diagnosable output for no gain.
// Sorted by byte order so the list is trivially diffable against the pi
// extension's copy.
var credentialEnvNameSuffixes = []string{
	"_ACCESS_KEY",
	"_APIKEY",
	"_API_KEY",
	"_CREDENTIALS",
	"_PASSWD",
	"_PASSWORD",
	"_PRIVATE_KEY",
	"_SECRET",
	"_SECRET_KEY",
	"_TOKEN",
}

// credentialEnvNameList is the sorted, de-duplicated union of the exact
// names, computed once at package init.
var credentialEnvNameList = func() []string {
	out := make([]string, 0, len(ForwardedCredentialEnvNames)+len(otherCredentialEnvNames)+2)
	out = append(out, ForwardedCredentialEnvNames...)
	out = append(out, GitHubTokenEnvName, GitLabTokenEnvName)
	out = append(out, otherCredentialEnvNames...)
	sort.Strings(out)
	return slices.Compact(out)
}()

// credentialEnvNameSet is credentialEnvNameList as a membership set.
var credentialEnvNameSet = func() map[string]bool {
	m := make(map[string]bool, len(credentialEnvNameList))
	for _, n := range credentialEnvNameList {
		m[n] = true
	}
	return m
}()

// CredentialEnvNames returns a copy of the exact environment-variable names
// the value layer reads, sorted. Prefix and suffix matches are not included —
// use IsCredentialEnvName for a full membership test.
func CredentialEnvNames() []string {
	return slices.Clone(credentialEnvNameList)
}

// CredentialEnvPrefixes returns a copy of the name prefixes that mark a whole
// family of environment variables as credential-carrying.
func CredentialEnvPrefixes() []string { return slices.Clone(credentialEnvPrefixes) }

// CredentialEnvNameSuffixes returns a copy of the name-shape heuristic
// suffixes. See credentialEnvNameSuffixes for why a `_PATH` or `_FILE` name
// is deliberately not covered.
func CredentialEnvNameSuffixes() []string { return slices.Clone(credentialEnvNameSuffixes) }

// IsCredentialEnvName reports whether an environment-variable name is
// expected to carry a credential value.
func IsCredentialEnvName(name string) bool {
	if name == "" {
		return false
	}
	if credentialEnvNameSet[name] {
		return true
	}
	for _, p := range credentialEnvPrefixes {
		if strings.HasPrefix(name, p) && len(name) > len(p) {
			return true
		}
	}
	for _, s := range credentialEnvNameSuffixes {
		if strings.HasSuffix(name, s) && len(name) > len(s) {
			return true
		}
	}
	return false
}

// Shape layer.
//
// Each shape is anchored on a distinctive issuer prefix so ordinary output
// does not match. A shape with a generic body (a bare base64 run, a JWT) is
// deliberately absent: the false-positive rate would corrupt more output than
// the rule protects.
type credentialShape struct {
	name    string
	pattern string
	// triggers are literal substrings of which at least one MUST be
	// present for the pattern to have any chance of matching. They drive
	// the prefilter — see shapeTriggerPresent.
	triggers []string
	// anchored matches the whole of a candidate string. It is used to
	// attribute a combined-regexp match back to the shape that produced it.
	anchored *regexp.Regexp
}

var credentialShapes = func() []credentialShape {
	raw := []struct {
		name, pattern string
		triggers      []string
	}{
		// A PEM private-key block. First, because its body can contain
		// text that a later shape would otherwise match piecemeal.
		//
		// `[\s\S]` rather than `(?s).` so the pattern string is byte-
		// identical to the JavaScript one in the pi extension.
		{
			"private-key-block",
			`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`,
			[]string{"-----BEGIN "},
		},
		// GitHub fine-grained PAT. Before the classic prefixes because
		// `github_pat_` shares no prefix with them but is the longer name.
		{
			"github-fine-grained-pat",
			`github_pat_[A-Za-z0-9_]{40,255}`,
			[]string{"github_pat_"},
		},
		// GitHub classic PAT / OAuth / user / server / refresh token.
		{
			"github-token",
			`gh[pousr]_[A-Za-z0-9]{36,255}`,
			[]string{"ghp_", "gho_", "ghu_", "ghs_", "ghr_"},
		},
		// GitLab personal / project / group access token. `glpat-` is the
		// issuer prefix GitLab puts on every PAT-class token, so the rule is
		// anchored the same way as the GitHub ones. A GitLab OAuth or CI job
		// token has a different prefix and is deliberately not covered here —
		// the value layer catches whatever the capturing process holds.
		{
			"gitlab-pat",
			`glpat-[A-Za-z0-9_-]{20,255}`,
			[]string{"glpat-"},
		},
		{"anthropic-api-key", `sk-ant-[A-Za-z0-9_-]{24,512}`, []string{"sk-ant-"}},
		{"openrouter-api-key", `sk-or-v1-[A-Za-z0-9]{32,512}`, []string{"sk-or-v1-"}},
		{"openai-api-key", `sk-proj-[A-Za-z0-9_-]{24,512}`, []string{"sk-proj-"}},
		{
			"slack-token",
			`xox[abprs]-[A-Za-z0-9-]{12,255}`,
			[]string{"xoxa-", "xoxb-", "xoxp-", "xoxr-", "xoxs-"},
		},
		{"aws-access-key-id", `\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`, []string{"AKIA", "ASIA"}},
		{"google-api-key", `\bAIza[0-9A-Za-z_-]{35}\b`, []string{"AIza"}},
		{"atlassian-api-token", `ATATT3[A-Za-z0-9_=.-]{50,512}`, []string{"ATATT3"}},
	}
	out := make([]credentialShape, 0, len(raw))
	for _, r := range raw {
		out = append(out, credentialShape{
			name:     r.name,
			pattern:  r.pattern,
			triggers: r.triggers,
			anchored: regexp.MustCompile(`\A(?:` + r.pattern + `)\z`),
		})
	}
	return out
}()

// shapeTriggers is the flattened trigger set, computed once.
var shapeTriggers = func() []string {
	var out []string
	for _, s := range credentialShapes {
		out = append(out, s.triggers...)
	}
	return out
}()

// shapeTriggerPresent reports whether any shape could possibly match s.
//
// It exists for cost, not correctness. Running the combined regexp over a
// large payload costs roughly ten times as much as the literal substring
// scans below, and the overwhelmingly common case is a payload with no
// credential shape in it at all. Every trigger is a NECESSARY substring of
// its pattern, so a false negative here is impossible;
// TestCredentialShapes_EveryPatternRequiresOneOfItsTriggers pins that.
func shapeTriggerPresent(s string) bool {
	for _, t := range shapeTriggers {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
}

// combinedShapeRE matches any credential shape in a single linear pass.
// Go's regexp uses leftmost-first alternation, so the order of
// credentialShapes decides which rule wins an overlap.
var combinedShapeRE = func() *regexp.Regexp {
	parts := make([]string, 0, len(credentialShapes))
	for _, s := range credentialShapes {
		parts = append(parts, `(?:`+s.pattern+`)`)
	}
	return regexp.MustCompile(strings.Join(parts, "|"))
}()

// CredentialShapeTriggers returns the literal prefilter substrings keyed by
// shape name. At least one entry must be present in the input for the shape
// to match.
func CredentialShapeTriggers() map[string][]string {
	out := make(map[string][]string, len(credentialShapes))
	for _, s := range credentialShapes {
		out[s.name] = slices.Clone(s.triggers)
	}
	return out
}

// CredentialShapeNames returns the names of the shape rules, in match order.
// The order is load-bearing: alternation is leftmost-first, so it decides
// which rule wins an overlapping match.
func CredentialShapeNames() []string {
	out := make([]string, 0, len(credentialShapes))
	for _, s := range credentialShapes {
		out = append(out, s.name)
	}
	return out
}

// CredentialShapePatterns returns the shape rules keyed by name. The pattern
// strings are written to be valid and equivalent in both RE2 and the
// JavaScript regexp dialect, so the pi extension can carry them verbatim and
// the parity test can compare them directly.
func CredentialShapePatterns() map[string]string {
	out := make(map[string]string, len(credentialShapes))
	for _, s := range credentialShapes {
		out[s.name] = s.pattern
	}
	return out
}

// shapeNameFor attributes a combined-regexp match back to a shape name.
// It returns "credential" when no individual rule claims the match, which
// cannot happen for a match the combined regexp produced but keeps the
// function total.
func shapeNameFor(match string) string {
	for _, s := range credentialShapes {
		if s.anchored.MatchString(match) {
			return s.name
		}
	}
	return "credential"
}

// Redactor rewrites credential values found in captured text.
//
// Build one with NewRedactor, NewRedactorFromEnviron, or NewEnvRedactor. The
// zero value redacts nothing, and a nil *Redactor is safe: its Redact returns
// the input unchanged, which lets a caller treat "no redactor configured" as a
// no-op without a nil check at every call site.
//
// A Redactor is immutable after construction and safe for concurrent use.
type Redactor struct {
	// values rewrites every known literal credential value. nil when no
	// value passed the length and content guards.
	values *strings.Replacer
	// valueCount is the number of distinct credential values the value
	// layer covers. Exposed for tests and diagnostics; never the values
	// themselves.
	valueCount int
	// shapes is the combined shape regexp, or nil when the shape layer is
	// switched off.
	shapes *regexp.Regexp
}

// NewRedactor builds a Redactor from an explicit name-to-value map. Both
// layers are enabled.
//
// Values are filtered before they reach the value layer:
//
//   - a value shorter than MinCredentialValueLen is skipped;
//   - a value that is empty or whitespace-only is skipped;
//   - a value that looks like an unexpanded shell literal (`$(…)`) is
//     skipped — it is not a secret, it is a propagation bug (see #2348).
//
// When two names carry the same value, the marker names the
// lexicographically first of them, so the output is deterministic.
func NewRedactor(secrets map[string]string) *Redactor {
	return newRedactor(secrets)
}

// NewRedactorFromEnviron builds a Redactor from an `os.Environ`-shaped slice
// of `NAME=VALUE` strings. Only names that IsCredentialEnvName accepts are
// read.
func NewRedactorFromEnviron(environ []string) *Redactor {
	secrets := make(map[string]string, 8)
	for _, kv := range environ {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || !IsCredentialEnvName(name) {
			continue
		}
		secrets[name] = value
	}
	return NewRedactor(secrets)
}

// NewEnvRedactor builds a Redactor from the current process environment.
func NewEnvRedactor() *Redactor {
	return NewRedactorFromEnviron(os.Environ())
}

// NewShapeOnlyRedactor builds a Redactor with the shape layer only. It exists
// for tests and for callers that must prove the shape layer stands on its own;
// production callers use NewEnvRedactor so the value layer runs first.
func NewShapeOnlyRedactor() *Redactor {
	return newRedactor(nil)
}

// newRedactor builds a Redactor with the shape layer always enabled and the
// value layer populated from secrets. The shape layer is not optional: it is
// the floor the whole control rests on when the process holds no credential
// values.
func newRedactor(secrets map[string]string) *Redactor {
	r := &Redactor{shapes: combinedShapeRE}

	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)

	type rule struct{ pattern, marker string }
	var rules []rule
	seenValue := make(map[string]bool, len(names))
	seenPattern := make(map[string]bool, len(names)*2)

	for _, name := range names {
		value := secrets[name]
		if !usableSecretValue(value) {
			continue
		}
		if seenValue[value] {
			continue
		}
		seenValue[value] = true
		r.valueCount++

		marker := RedactionMarker(name)
		for _, pattern := range matchForms(value) {
			if pattern == "" || seenPattern[pattern] {
				continue
			}
			seenPattern[pattern] = true
			rules = append(rules, rule{pattern: pattern, marker: marker})
		}
	}

	if len(rules) == 0 {
		return r
	}

	// Longest pattern first so a secret that is a prefix of another secret
	// never wins the match. strings.Replacer resolves a tie at the same
	// input position by argument order.
	sort.SliceStable(rules, func(i, j int) bool {
		if len(rules[i].pattern) != len(rules[j].pattern) {
			return len(rules[i].pattern) > len(rules[j].pattern)
		}
		return rules[i].pattern < rules[j].pattern
	})

	pairs := make([]string, 0, len(rules)*2)
	for _, rl := range rules {
		pairs = append(pairs, rl.pattern, rl.marker)
	}
	r.values = strings.NewReplacer(pairs...)
	return r
}

// usableSecretValue reports whether a value is safe to feed to the value
// layer. See NewRedactor for the rules.
func usableSecretValue(v string) bool {
	if len(v) < MinCredentialValueLen {
		return false
	}
	if strings.TrimSpace(v) == "" {
		return false
	}
	if len(strings.TrimSpace(v)) < MinCredentialValueLen {
		return false
	}
	// An unexpanded `$(cat …)` value is a propagation bug, not a secret
	// (issue #2348). Redacting it would hide the bug.
	if strings.Contains(v, "$(") {
		return false
	}
	return true
}

// matchForms returns the textual forms a secret can take in captured content.
//
// The DB-write layer sees a payload that is already JSON, so a secret that
// contains a character JSON escapes (a quote, a backslash, a control
// character) appears there in escaped form. Registering both forms lets the
// DB layer catch it without parsing the payload.
func matchForms(value string) []string {
	forms := []string{value}
	if encoded, err := json.Marshal(value); err == nil && len(encoded) >= 2 {
		// Strip the surrounding quotes json.Marshal adds.
		inner := string(encoded[1 : len(encoded)-1])
		if inner != value {
			forms = append(forms, inner)
		}
	}
	return forms
}

// Redact returns s with every credential value and every known credential
// shape replaced by a marker naming what was removed.
//
// A nil receiver returns s unchanged.
func (r *Redactor) Redact(s string) string {
	if r == nil || s == "" {
		return s
	}
	if r.values != nil {
		s = r.values.Replace(s)
	}
	if r.shapes != nil && shapeTriggerPresent(s) {
		s = r.shapes.ReplaceAllStringFunc(s, func(m string) string {
			return RedactionMarker(shapeNameFor(m))
		})
	}
	return s
}

// RedactJSON redacts a JSON document, one scalar at a time.
//
// Use this, NOT Redact, for anything that is stored as JSON.
//
// Why it exists
// -------------
//
// Redact matches over flat text. The private-key-block shape has a
// `[\s\S]*?` body, so on a serialised JSON document a dangling
// `-----BEGIN … PRIVATE KEY-----` in one field and an `-----END …-----` in a
// later field make one match consume the JSON structure between them. Two
// failure modes follow, both silent and both permanent once the row is
// written:
//
//	{"a":"-----BEGIN A PRIVATE KEY-----","b":[1,"-----END A PRIVATE KEY-----"]}
//	  -> {"a":"[redacted:private-key-block]"]}          invalid JSON
//
//	{"args":{"a":"-----BEGIN K PRIVATE KEY-----","z":"-----END K PRIVATE KEY-----"}}
//	  -> {"args":{"a":"[redacted:private-key-block]"}}   field z silently gone
//
// RedactJSON parses the document and redacts each string scalar and each
// object key on its own, so no pattern can reach past the value it matched
// in. The pi extension's redactFrame does the same thing for the same
// reason.
//
// Guarantees
// ----------
//
//   - A document with no credential in it is returned BYTE FOR BYTE. Nothing
//     is re-encoded unless a redaction actually fired.
//   - A document that is not a single JSON value is treated as free text and
//     passed to Redact. There are no JSON delimiters to protect in that case,
//     and a spanning match is then the correct behaviour.
//   - Re-encoding preserves numbers exactly (json.Number, so a large int64
//     does not round-trip through float64) and does not escape `<`, `>`, or
//     `&`, so the output differs from the input only where a credential was.
//     Object key ORDER is not preserved on a re-encode, because Go marshals a
//     map with sorted keys. That is a cosmetic change to a document that was
//     going to change anyway.
//
// Recursion is bounded by encoding/json, which refuses a document nested
// deeper than its own limit before this function ever sees the value.
func (r *Redactor) RedactJSON(doc string) string {
	if r == nil || doc == "" {
		return doc
	}

	dec := json.NewDecoder(strings.NewReader(doc))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || dec.More() {
		return r.Redact(doc)
	}

	redacted, changed := r.redactJSONValue(v)
	if !changed {
		return doc
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(redacted); err != nil {
		// Unreachable for a value that came out of Decode. Fall back to the
		// flat pass rather than store the document with the credential in it.
		return r.Redact(doc)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// redactJSONValue walks a decoded JSON value, redacting every string scalar
// and every object key. It reports whether anything changed so the caller can
// skip the re-encode and return the input unchanged.
func (r *Redactor) redactJSONValue(v any) (any, bool) {
	switch t := v.(type) {
	case string:
		s := r.Redact(t)
		return s, s != t

	case []any:
		changed := false
		for i, e := range t {
			ne, c := r.redactJSONValue(e)
			if c {
				t[i] = ne
				changed = true
			}
		}
		return t, changed

	case map[string]any:
		changed := false
		var renamedFrom, renamedTo []string
		for k, e := range t {
			if ne, c := r.redactJSONValue(e); c {
				t[k] = ne
				changed = true
			}
			if nk := r.Redact(k); nk != k {
				renamedFrom = append(renamedFrom, k)
				renamedTo = append(renamedTo, nk)
			}
		}
		// Apply key renames after the range: adding a key to a map that is
		// being ranged over is unspecified in Go. Two keys that redact to the
		// same marker collapse into one — a key that IS a credential is
		// pathological, and losing one is better than storing it.
		for i, from := range renamedFrom {
			t[renamedTo[i]] = t[from]
			delete(t, from)
			changed = true
		}
		return t, changed

	default:
		// json.Number, bool, nil — no free text to redact.
		return v, false
	}
}

// ValueCount reports how many distinct credential values the value layer
// covers. It never exposes a value. Zero means the value layer is inactive
// and only the shape layer runs.
func (r *Redactor) ValueCount() int {
	if r == nil {
		return 0
	}
	return r.valueCount
}
