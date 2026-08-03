package payload_test

// Tests for the capture-path redactor (issue #2589).
//
// SECURITY: every credential value in this file is synthetic. None is a real
// token, and none is read from the environment of the test process. The
// synthetic values are built so that they match no shape rule unless the test
// is specifically exercising the shape layer.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/payload"
)

// Synthetic values. Long enough to clear MinCredentialValueLen, and shaped so
// that no shape rule claims them — a shape hit would mask a value-layer bug.
const (
	syntheticGitHubToken   = "SYNTHETIC-GITHUB-VALUE-000000000000"
	syntheticAnthropicKey  = "SYNTHETIC-ANTHROPIC-VALUE-111111111"
	syntheticOpenRouterKey = "SYNTHETIC-OPENROUTER-VALUE-22222222"
)

func TestRedact_ValueLayerReplacesWithNamedMarker(t *testing.T) {
	r := payload.NewRedactor(map[string]string{
		"GITHUB_TOKEN": syntheticGitHubToken,
	})

	in := "gh auth token\n" + syntheticGitHubToken + "\nexit 0\n"
	got := r.Redact(in)

	if strings.Contains(got, syntheticGitHubToken) {
		t.Fatal("redacted output still carries the credential value")
	}
	want := "gh auth token\n[redacted:GITHUB_TOKEN]\nexit 0\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRedactionMarker_NamesTheVariable(t *testing.T) {
	if got, want := payload.RedactionMarker("GITHUB_TOKEN"), "[redacted:GITHUB_TOKEN]"; got != want {
		t.Errorf("RedactionMarker: got %q, want %q", got, want)
	}
}

func TestRedact_OutputWithoutCredentialIsUnchangedByteForByte(t *testing.T) {
	r := payload.NewRedactor(map[string]string{
		"GITHUB_TOKEN":      syntheticGitHubToken,
		"ANTHROPIC_API_KEY": syntheticAnthropicKey,
	})

	cases := []string{
		"",
		"ok\n",
		"PASS\nok  github.com/prismatic-koi/prism/internal/db\t0.412s\n",
		strings.Repeat("the quick brown fox jumps over the lazy dog\n", 200),
		`{"type":"tool_result","id":"call_1","success":true,"output":"hello"}`,
		// Near-misses for the shape rules.
		"ghp_short\n",
		"sk-ant\n",
		"AKIA123\n",
		"-----BEGIN CERTIFICATE-----\nnot a private key\n-----END CERTIFICATE-----\n",
	}
	for _, in := range cases {
		if got := r.Redact(in); got != in {
			t.Errorf("Redact(%q) = %q; want the input unchanged", in, got)
		}
	}
}

func TestRedact_MultipleValuesEachGetTheirOwnMarker(t *testing.T) {
	r := payload.NewRedactor(map[string]string{
		"GITHUB_TOKEN":       syntheticGitHubToken,
		"ANTHROPIC_API_KEY":  syntheticAnthropicKey,
		"OPENROUTER_API_KEY": syntheticOpenRouterKey,
	})

	in := fmt.Sprintf("A=%s B=%s C=%s", syntheticAnthropicKey, syntheticGitHubToken, syntheticOpenRouterKey)
	want := "A=[redacted:ANTHROPIC_API_KEY] B=[redacted:GITHUB_TOKEN] C=[redacted:OPENROUTER_API_KEY]"
	if got := r.Redact(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRedact_LongestValueWinsWhenOneValueIsAPrefixOfAnother(t *testing.T) {
	short := "SYNTHETIC-PREFIX-VALUE-AAAA"
	long := short + "-EXTENDED-TAIL"

	r := payload.NewRedactor(map[string]string{
		"SHORT_TOKEN": short,
		"LONG_TOKEN":  long,
	})

	if got, want := r.Redact(long), "[redacted:LONG_TOKEN]"; got != want {
		t.Errorf("long value: got %q, want %q", got, want)
	}
	if got, want := r.Redact(short), "[redacted:SHORT_TOKEN]"; got != want {
		t.Errorf("short value: got %q, want %q", got, want)
	}
}

func TestRedact_DuplicateValuesResolveDeterministically(t *testing.T) {
	shared := "SYNTHETIC-SHARED-VALUE-9999999999"
	r := payload.NewRedactor(map[string]string{
		"ZULU_TOKEN":  shared,
		"ALPHA_TOKEN": shared,
	})
	// The lexicographically first name wins, so repeated construction gives
	// the same marker every time.
	for i := 0; i < 5; i++ {
		if got, want := r.Redact(shared), "[redacted:ALPHA_TOKEN]"; got != want {
			t.Fatalf("iteration %d: got %q, want %q", i, got, want)
		}
	}
	if got := payload.NewRedactor(map[string]string{"ZULU_TOKEN": shared, "ALPHA_TOKEN": shared}).Redact(shared); got != "[redacted:ALPHA_TOKEN]" {
		t.Errorf("second construction: got %q", got)
	}
}

func TestRedact_IsIdempotent(t *testing.T) {
	r := payload.NewRedactor(map[string]string{"GITHUB_TOKEN": syntheticGitHubToken})
	once := r.Redact("token=" + syntheticGitHubToken)
	twice := r.Redact(once)
	if once != twice {
		t.Errorf("second pass changed the output: %q then %q", once, twice)
	}
}

// ---------------------------------------------------------------------------
// Edge cases — over-redaction guards.
// ---------------------------------------------------------------------------

func TestRedact_EmptyShortAndWhitespaceValuesDoNotOverRedact(t *testing.T) {
	// A one-character, empty, or whitespace-only value would match at almost
	// every position of ordinary output. All must be skipped.
	r := payload.NewRedactor(map[string]string{
		"EMPTY_TOKEN":      "",
		"SPACE_TOKEN":      " ",
		"TAB_TOKEN":        "\t",
		"NEWLINE_TOKEN":    "\n",
		"ONE_CHAR_TOKEN":   "a",
		"SHORT_TOKEN":      "abc",
		"BOUNDARY_TOKEN":   strings.Repeat("b", payload.MinCredentialValueLen-1),
		"PADDED_TOKEN":     "   ab   ",
		"EXPANSION_SECRET": "$(cat /run/secrets/token)",
	})

	if got := r.ValueCount(); got != 0 {
		t.Fatalf("ValueCount = %d; want 0 — a value below the guard reached the value layer", got)
	}

	in := "a b c\tabc\n   ab   \nthe quick brown fox\n\n"
	if got := r.Redact(in); got != in {
		t.Errorf("Redact(%q) = %q; want the input unchanged", in, got)
	}
}

func TestRedact_ValueExactlyAtTheMinimumLengthIsRedacted(t *testing.T) {
	value := strings.Repeat("z", payload.MinCredentialValueLen)
	r := payload.NewRedactor(map[string]string{"EDGE_TOKEN": value})
	if got := r.ValueCount(); got != 1 {
		t.Fatalf("ValueCount = %d; want 1", got)
	}
	if got, want := r.Redact("v="+value), "v=[redacted:EDGE_TOKEN]"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRedact_JSONEscapedFormIsAlsoMatched(t *testing.T) {
	// The DB-write control sees a payload that is already JSON. A secret
	// carrying a quote or a backslash appears there in escaped form.
	value := `SYNTHETIC"BACK\SLASH-VALUE-123456`
	r := payload.NewRedactor(map[string]string{"WEIRD_TOKEN": value})

	escaped := `SYNTHETIC\"BACK\\SLASH-VALUE-123456`
	line := `{"output":"` + escaped + `"}`
	got := r.Redact(line)

	if strings.Contains(got, escaped) {
		t.Fatalf("escaped form survived redaction: %q", got)
	}
	if want := `{"output":"[redacted:WEIRD_TOKEN]"}`; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRedact_NilReceiverIsANoOp(t *testing.T) {
	var r *payload.Redactor
	in := "anything at all"
	if got := r.Redact(in); got != in {
		t.Errorf("nil receiver changed the input: %q", got)
	}
	if got := r.ValueCount(); got != 0 {
		t.Errorf("nil receiver ValueCount = %d; want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Shape layer.
// ---------------------------------------------------------------------------

func TestRedact_ShapeLayerRunsWithoutAnyValues(t *testing.T) {
	r := payload.NewShapeOnlyRedactor()
	if got := r.ValueCount(); got != 0 {
		t.Fatalf("ValueCount = %d; want 0", got)
	}

	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			"github classic",
			"token ghp_" + strings.Repeat("A", 36) + " end",
			"token [redacted:github-token] end",
		},
		{
			"github fine-grained",
			"token github_pat_" + strings.Repeat("B", 40) + " end",
			"token [redacted:github-fine-grained-pat] end",
		},
		{
			"anthropic",
			"key sk-ant-" + strings.Repeat("c", 24) + " end",
			"key [redacted:anthropic-api-key] end",
		},
		{
			"openrouter",
			"key sk-or-v1-" + strings.Repeat("d", 32) + " end",
			"key [redacted:openrouter-api-key] end",
		},
		{
			"openai project",
			"key sk-proj-" + strings.Repeat("e", 24) + " end",
			"key [redacted:openai-api-key] end",
		},
		{
			"slack",
			"key xoxb-" + strings.Repeat("1", 12) + " end",
			"key [redacted:slack-token] end",
		},
		{
			"aws access key id",
			"id AKIA" + strings.Repeat("Q", 16) + " end",
			"id [redacted:aws-access-key-id] end",
		},
		{
			"google api key",
			"key AIza" + strings.Repeat("F", 35) + " end",
			"key [redacted:google-api-key] end",
		},
		{
			"atlassian",
			"key ATATT3" + strings.Repeat("G", 50) + " end",
			"key [redacted:atlassian-api-token] end",
		},
		{
			"private key block",
			"head\n-----BEGIN OPENSSH PRIVATE KEY-----\nc3ludGhldGlj\n-----END OPENSSH PRIVATE KEY-----\ntail",
			"head\n[redacted:private-key-block]\ntail",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.Redact(tc.input); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRedact_ShapeLayerIsSecondaryNotAReplacementForValueMatching(t *testing.T) {
	// The synthetic value matches no shape rule. Only the value layer can
	// remove it, which is what makes it the primary control.
	shapeOnly := payload.NewShapeOnlyRedactor()
	if got := shapeOnly.Redact(syntheticGitHubToken); got != syntheticGitHubToken {
		t.Fatalf("shape layer unexpectedly matched the synthetic value: %q", got)
	}

	both := payload.NewRedactor(map[string]string{"GITHUB_TOKEN": syntheticGitHubToken})
	if got, want := both.Redact(syntheticGitHubToken), "[redacted:GITHUB_TOKEN]"; got != want {
		t.Errorf("value layer: got %q, want %q", got, want)
	}

	// And the shape layer still runs when values are configured.
	shaped := "ghp_" + strings.Repeat("A", 36)
	if got, want := both.Redact(shaped), "[redacted:github-token]"; got != want {
		t.Errorf("shape layer with values configured: got %q, want %q", got, want)
	}
}

func TestRedact_ValueLayerWinsOverShapeLayerForTheSameSecret(t *testing.T) {
	// A real token matches a shape too. The value layer runs first, so the
	// marker names the variable — which is the more diagnosable outcome.
	value := "ghp_" + strings.Repeat("A", 36)
	r := payload.NewRedactor(map[string]string{"GITHUB_TOKEN": value})
	if got, want := r.Redact("t="+value), "t=[redacted:GITHUB_TOKEN]"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------------
// Environment-variable name registry.
// ---------------------------------------------------------------------------

func TestIsCredentialEnvName(t *testing.T) {
	credential := []string{
		"GITHUB_TOKEN",
		"ANTHROPIC_API_KEY",
		"OPENROUTER_API_KEY",
		"PRISM_GITHUB_TOKEN_PRISMATIC_KOI_WORKER",
		"SOME_VENDOR_TOKEN",
		"SOME_VENDOR_API_KEY",
		"DB_PASSWORD",
		"AWS_SECRET_ACCESS_KEY",
	}
	for _, name := range credential {
		if !payload.IsCredentialEnvName(name) {
			t.Errorf("IsCredentialEnvName(%q) = false; want true", name)
		}
	}

	// A name that points at a file or a directory names a path, not a
	// secret. Redacting a path corrupts diagnosable output for no gain.
	notCredential := []string{
		"",
		"PATH",
		"HOME",
		"GITHUB_TOKEN_PATH",
		"SOPS_AGE_KEY_FILE",
		"PRISM_SESSION_NAME",
		"XDG_STATE_HOME",
		"_TOKEN",
		"PRISM_GITHUB_TOKEN_",
	}
	for _, name := range notCredential {
		if payload.IsCredentialEnvName(name) {
			t.Errorf("IsCredentialEnvName(%q) = true; want false", name)
		}
	}
}

func TestNewRedactorFromEnviron_ReadsOnlyCredentialNames(t *testing.T) {
	environ := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/agent",
		"GITHUB_TOKEN=" + syntheticGitHubToken,
		"GITHUB_TOKEN_PATH=/run/secrets/github-token",
		"MALFORMED_NO_EQUALS",
	}
	r := payload.NewRedactorFromEnviron(environ)

	if got := r.ValueCount(); got != 1 {
		t.Fatalf("ValueCount = %d; want 1", got)
	}
	in := "PATH=/usr/bin:/bin HOME=/home/agent path=/run/secrets/github-token tok=" + syntheticGitHubToken
	want := "PATH=/usr/bin:/bin HOME=/home/agent path=/run/secrets/github-token tok=[redacted:GITHUB_TOKEN]"
	if got := r.Redact(in); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCredentialEnvNames_AccessorsReturnCopies(t *testing.T) {
	names := payload.CredentialEnvNames()
	if len(names) == 0 {
		t.Fatal("CredentialEnvNames returned an empty list")
	}
	names[0] = "MUTATED"
	if payload.CredentialEnvNames()[0] == "MUTATED" {
		t.Error("CredentialEnvNames leaked its backing array")
	}

	prefixes := payload.CredentialEnvPrefixes()
	prefixes[0] = "MUTATED"
	if payload.CredentialEnvPrefixes()[0] == "MUTATED" {
		t.Error("CredentialEnvPrefixes leaked its backing array")
	}

	suffixes := payload.CredentialEnvNameSuffixes()
	suffixes[0] = "MUTATED"
	if payload.CredentialEnvNameSuffixes()[0] == "MUTATED" {
		t.Error("CredentialEnvNameSuffixes leaked its backing array")
	}
}

func TestCredentialEnvNames_CoversTheForwardedSetAndTheGitHubToken(t *testing.T) {
	names := payload.CredentialEnvNames()
	have := make(map[string]bool, len(names))
	for _, n := range names {
		have[n] = true
	}
	for _, n := range payload.ForwardedCredentialEnvNames {
		if !have[n] {
			t.Errorf("forwarded credential %q is not in CredentialEnvNames", n)
		}
	}
	if !have[payload.GitHubTokenEnvName] {
		t.Errorf("%s is not in CredentialEnvNames", payload.GitHubTokenEnvName)
	}
}

// ---------------------------------------------------------------------------
// Cost.
// ---------------------------------------------------------------------------

// TestRedact_LargePayloadCostIsNotQuadratic bounds the wall-clock cost of a
// single Redact call on a payload far larger than any real tool_result. A
// quadratic implementation on 8 MiB would run for minutes; the linear one
// finishes in well under the budget, so the assertion separates the two
// classes without depending on the speed of the machine.
func TestRedact_LargePayloadCostIsNotQuadratic(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping cost bound in -short mode")
	}
	r := payload.NewRedactor(map[string]string{
		"GITHUB_TOKEN":       syntheticGitHubToken,
		"ANTHROPIC_API_KEY":  syntheticAnthropicKey,
		"OPENROUTER_API_KEY": syntheticOpenRouterKey,
	})

	// 8 MiB of plausible tool output with the secret sprinkled through it.
	var b strings.Builder
	const chunk = "2026-06-01T00:00:00Z level=info msg=\"step complete\" step=42\n"
	for b.Len() < 8<<20 {
		b.WriteString(chunk)
		if b.Len()%1024 < len(chunk) {
			b.WriteString(syntheticGitHubToken)
			b.WriteString("\n")
		}
	}
	in := b.String()

	const budget = 10 * time.Second
	start := time.Now()
	got := r.Redact(in)
	elapsed := time.Since(start)

	if strings.Contains(got, syntheticGitHubToken) {
		t.Error("large payload still carries the credential value")
	}
	if elapsed > budget {
		t.Errorf("Redact of %d bytes took %s, over the %s budget — suspect a super-linear implementation",
			len(in), elapsed, budget)
	}
}

func BenchmarkRedact_NoMatch(b *testing.B) {
	r := payload.NewRedactor(map[string]string{"GITHUB_TOKEN": syntheticGitHubToken})
	in := strings.Repeat("ok  github.com/prismatic-koi/prism/internal/db\t0.412s\n", 2000)
	b.SetBytes(int64(len(in)))
	b.ResetTimer()
	for range b.N {
		_ = r.Redact(in)
	}
}

func BenchmarkRedact_WithMatches(b *testing.B) {
	r := payload.NewRedactor(map[string]string{"GITHUB_TOKEN": syntheticGitHubToken})
	in := strings.Repeat("tok="+syntheticGitHubToken+"\n", 2000)
	b.SetBytes(int64(len(in)))
	b.ResetTimer()
	for range b.N {
		_ = r.Redact(in)
	}
}

// ---------------------------------------------------------------------------
// Shape prefilter — the cost optimisation must never change the result.
// ---------------------------------------------------------------------------

// shapeCorpus is the input set the prefilter is checked against: one positive
// sample per shape, plus near-misses and ordinary output.
func shapeCorpus() []string {
	return []string{
		"",
		"ok\n",
		"PASS\nok\tgithub.com/prismatic-koi/prism/internal/db\t0.412s\n",
		"through the night, right enough, a rough ghost",
		"ghp_short",
		"github_pat_tooshort",
		"sk-ant",
		"AKIA123",
		"-----BEGIN CERTIFICATE-----\nnope\n-----END CERTIFICATE-----",
		"token ghp_" + strings.Repeat("A", 36) + " end",
		"token github_pat_" + strings.Repeat("B", 40) + " end",
		"key sk-ant-" + strings.Repeat("c", 24) + " end",
		"key sk-or-v1-" + strings.Repeat("d", 32) + " end",
		"key sk-proj-" + strings.Repeat("e", 24) + " end",
		"key xoxb-" + strings.Repeat("1", 12) + " end",
		"id AKIA" + strings.Repeat("Q", 16) + " end",
		"key AIza" + strings.Repeat("F", 35) + " end",
		"key ATATT3" + strings.Repeat("G", 50) + " end",
		"head\n-----BEGIN OPENSSH PRIVATE KEY-----\nc3ludGhldGlj\n-----END OPENSSH PRIVATE KEY-----\ntail",
		"two ghp_" + strings.Repeat("A", 36) + " and AIza" + strings.Repeat("F", 35),
	}
}

// TestCredentialShapes_PrefilterNeverChangesTheResult falsifies the prefilter:
// every trigger must be a NECESSARY substring of its pattern, so skipping the
// regexp when no trigger is present must produce exactly the same output as
// running it unconditionally.
func TestCredentialShapes_PrefilterNeverChangesTheResult(t *testing.T) {
	r := payload.NewShapeOnlyRedactor()
	for _, in := range shapeCorpus() {
		withPrefilter := r.Redact(in)
		withoutPrefilter := payload.RedactShapesNoPrefilterForTest(in)
		if withPrefilter != withoutPrefilter {
			t.Errorf("prefilter changed the result for %q:\n  with:    %q\n  without: %q",
				in, withPrefilter, withoutPrefilter)
		}
	}
}

// TestCredentialShapes_EveryPatternRequiresOneOfItsTriggers checks the other
// direction: a payload the shape layer rewrites must have fired the prefilter.
func TestCredentialShapes_EveryPatternRequiresOneOfItsTriggers(t *testing.T) {
	for _, in := range shapeCorpus() {
		if payload.RedactShapesNoPrefilterForTest(in) != in && !payload.ShapeTriggerPresentForTest(in) {
			t.Errorf("a shape matched %q but no trigger is present — the prefilter would skip it", in)
		}
	}
}

func TestCredentialShapeTriggers_EveryShapeDeclaresAtLeastOne(t *testing.T) {
	triggers := payload.CredentialShapeTriggers()
	for _, name := range payload.CredentialShapeNames() {
		if len(triggers[name]) == 0 {
			t.Errorf("shape %q declares no prefilter trigger", name)
		}
		for _, trig := range triggers[name] {
			if trig == "" {
				t.Errorf("shape %q declares an empty trigger", name)
			}
		}
	}
}

// FuzzRedactShapePrefilter is the generalised form of the two tests above: for
// any input at all, the prefiltered shape layer and the unfiltered one must
// agree. The seed corpus runs on every `go test`.
func FuzzRedactShapePrefilter(f *testing.F) {
	for _, s := range shapeCorpus() {
		f.Add(s)
	}
	r := payload.NewShapeOnlyRedactor()
	f.Fuzz(func(t *testing.T, in string) {
		if got, want := r.Redact(in), payload.RedactShapesNoPrefilterForTest(in); got != want {
			t.Errorf("prefilter changed the result for %q: got %q, want %q", in, got, want)
		}
	})
}
