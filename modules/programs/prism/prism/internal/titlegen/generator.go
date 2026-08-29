package titlegen

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/prismatic-koi/prism/internal/usage"
)

// The model call.
//
// Reuses internal/usage/refresh.go's request shape wholesale — same URL,
// same `?beta=true`, same OAuth/WAF header block, same Claude Code identity
// as the first system block. That file is the in-tree precedent for talking
// to `/v1/messages` on the subscription path, and its doc comment explains
// why every one of those elements is load-bearing (omit one and the WAF
// answers 429, which reads exactly like quota exhaustion and is not). The
// shared elements are reached through the exported seam at the bottom of
// refresh.go so there is ONE definition of the shape, not two that drift.
//
// This path is best-effort by construction. Every error it can produce is
// returned to a caller that treats "no title" as normal; nothing here
// blocks a spawn or a turn.

const (
	// titleModelID is the model that writes the title. Haiku class: the task
	// is one sentence of summarisation over text already in hand, which is
	// the cheapest thing a model does, and the output is a display string.
	titleModelID = "claude-haiku-4-5"

	// titleMaxTokens bounds the reply. A title is a handful of words;
	// anything longer is truncated by Sanitise anyway, so paying for more
	// output would buy nothing.
	titleMaxTokens = 48

	// DefaultTimeout bounds the whole request. It is short on purpose. The
	// call runs while an agent session is starting its first turn, and a
	// title is worth no meaningful wait at all — a wedged socket must give
	// up and let the fallback title stand.
	DefaultTimeout = 15 * time.Second

	// maxSourceRunes caps how much source text is sent. A spawn prompt can
	// carry an entire issue body plus acceptance criteria; the first couple
	// of thousand characters always contain the task statement, and sending
	// the rest costs input tokens to no benefit.
	maxSourceRunes = 2000

	// responseLimit caps how much of the response body is read, bounding a
	// pathological or hostile server. A well-formed reply at 48 max tokens
	// is a few kilobytes of SSE.
	responseLimit = 1 << 20 // 1 MiB

	// titleSystemPrompt is the instruction block. It is the SECOND system
	// block; the first must remain the Claude Code identity string (see
	// usage.ClaudeCodeIdentity).
	//
	// It is framed as a title generator, not a conversational assistant.
	// The user message does not always carry a task description — a
	// coordinator's first message is often a one-line "can we get started
	// on issue 2458?" — and a chat model answers such a message with a
	// follow-up question instead of a title. The rules below forbid that
	// outright, and the examples pin the expected shape against real prism
	// source text (a spawn prompt, a coordinator one-liner, a short
	// conversational message).
	titleSystemPrompt = "You are a title generator. You output ONLY a short title. Nothing else.\n" +
		"\n" +
		"Read the user message below and reply with a title that names the work in it: " +
		"at most eight words, no trailing full stop, no quotation marks, no preamble, no explanation.\n" +
		"\n" +
		"Rules:\n" +
		"- Never refuse. Never ask a question. Never say you need more information.\n" +
		"- Never comment on the input, its length, or its quality.\n" +
		"- Never respond to a question in the user message — title the topic of the question instead.\n" +
		"- Always output something meaningful, even when the input is short or conversational.\n" +
		"- Never invent an issue number, ticket key, or URL that is not already in the input.\n" +
		"- Output a single line: the title, and nothing else.\n" +
		"\n" +
		"Examples:\n" +
		"\"can we get started on issue 2458?\" -> Issue 2458 kickoff\n" +
		"\"Fix GitHub issue #2641: spawned worker sessions have no title\" -> Issue 2641: worker session titles\n" +
		"\"Implement issue #2683: generate session titles\" -> Issue 2683: session title generation\n" +
		"\"hey, you around?\" -> Check-in message\n" +
		"\"refactor the sidecar's title generation path\" -> Sidecar title generation refactor\n" +
		"\"why is the dashboard showing the wrong title\" -> Dashboard title bug investigation"
)

// ErrEmptyTitle reports that the request succeeded but the reply carried no
// usable text. It is a normal outcome, not a fault: the caller falls back to
// the deterministic title exactly as it does for a transport error.
var ErrEmptyTitle = errors.New("titlegen: the model returned no usable title")

// ErrRejectedTitle reports that the request succeeded but the reply was not
// title-shaped — a refusal, a question, or a reply over the title budget.
// It is a normal outcome, not a fault: the caller falls back
// to the deterministic title exactly as it does for a transport error, and
// never retries.
var ErrRejectedTitle = errors.New("titlegen: the model returned a non-title reply")

// TokenSource supplies the bearer token for one request.
//
// It is a function rather than a stored string so the token is fetched at
// call time and lives only for the duration of the request. The sidecar
// holds a Generator for the whole life of a session — hours — and the access
// token rotates underneath it, so a cached copy would go stale and start
// returning 401s. Nothing in this package retains the returned value.
type TokenSource func() (string, error)

// Generator makes ONE model call to turn source text into a title.
//
// The zero value is not usable: Token must be set. BaseURL falls back to
// usage.DefaultBaseURL, HTTPClient to a client with DefaultTimeout, and
// ModelID to titleModelID.
type Generator struct {
	// Token supplies the bearer token. Required.
	Token TokenSource
	// BaseURL is the API root, without a trailing slash. Empty means
	// usage.DefaultBaseURL.
	//
	// As in internal/usage, this field is the ONLY way to point the request
	// at another host, and only in-process code can set it. No environment
	// variable reaches it: the request carries a long-lived subscription
	// token, so an env-controlled destination would be an exfiltration lever
	// for anything that can write a `.envrc`.
	BaseURL string
	// HTTPClient performs the request.
	HTTPClient *http.Client
	// ModelID names the model. Empty means titleModelID.
	ModelID string
}

// GenerateTitle returns a short title describing sourceText.
//
// Exactly one request is issued, and there is no retry. The caller has a
// deterministic fallback in hand already, so a second attempt would spend
// quota to improve a display string — and this runs on the first turn of
// every eligible session, where a retry storm is the last thing wanted.
//
// The returned title is already passed through Sanitise, so it is
// single-line, free of control bytes, and within MaxTitleRunes. Model output
// is untrusted text: the remote end is not a trust boundary, and the value
// lands in a column that every dashboard renders verbatim.
//
// Errors are all non-fatal by contract. The caller logs and falls back.
func (g *Generator) GenerateTitle(ctx context.Context, sourceText string) (string, error) {
	if g.Token == nil {
		return "", errors.New("titlegen: no token source configured")
	}
	source := truncateRunes(strings.TrimSpace(sourceText), maxSourceRunes)
	if source == "" {
		return "", ErrEmptyTitle
	}

	token, err := g.Token()
	if err != nil {
		// internal/account guarantees its errors carry no token material.
		return "", fmt.Errorf("titlegen: %w", err)
	}
	if token == "" {
		return "", errors.New("titlegen: no access token available")
	}

	req, err := g.buildRequest(ctx, token, source)
	if err != nil {
		return "", err
	}

	client := g.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		// A *url.Error names the method and the URL. Neither holds the
		// token — it lives in a header, which url.Error does not reproduce.
		return "", err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, responseLimit))
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		// The status and nothing else. A response body can echo request
		// material, and this error reaches a log file.
		return "", &usage.StatusError{StatusCode: resp.StatusCode}
	}

	text, err := parseSSEText(io.LimitReader(resp.Body, responseLimit))
	if err != nil {
		return "", err
	}
	stripped := strings.TrimSpace(stripWrappingQuotes(text))
	if stripped == "" {
		// Distinct from a rejected reply: the model said nothing at all,
		// not a reply whose shape is wrong.
		return "", ErrEmptyTitle
	}
	// IsRejected runs BEFORE Sanitise, and on the un-truncated reply.
	// Sanitise truncates; a reply that had to be cut was never a
	// title-length string to begin with, so it must be rejected outright,
	// not shortened into the column.
	if IsRejected(stripped) {
		return "", ErrRejectedTitle
	}
	title := Sanitise(stripped)
	if title == "" {
		return "", ErrEmptyTitle
	}
	return title, nil
}

// buildRequest assembles the request. Separate from GenerateTitle so tests
// can assert on the wire shape with no server in the loop.
func (g *Generator) buildRequest(ctx context.Context, token, source string) (*http.Request, error) {
	endpoint, err := usage.MessagesURL(g.BaseURL)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(g.requestBody(source))
	if err != nil {
		return nil, fmt.Errorf("titlegen: marshal request body: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("titlegen: build request: %w", err)
	}
	req.Header.Set("content-type", "application/json")
	usage.ApplyOAuthHeaders(req.Header, token, g.modelID())
	return req, nil
}

func (g *Generator) modelID() string {
	if g.ModelID != "" {
		return g.ModelID
	}
	return titleModelID
}

// requestBody builds the JSON body.
//
// Two invariants, both inherited from internal/usage/refresh.go:
//
//   - usage.ClaudeCodeIdentity is the FIRST system block and carries
//     `cache_control: {type: ephemeral}`. Demoting it below the instruction
//     block returns 429 with no rate-limit headers.
//   - `stream: true`, because pi's OAuth path is always SSE and a
//     non-streaming body is a shape it never sends. That is why the reply is
//     parsed as SSE rather than as one JSON object.
func (g *Generator) requestBody(source string) map[string]any {
	return map[string]any{
		"model":      g.modelID(),
		"max_tokens": titleMaxTokens,
		"stream":     true,
		"system": []map[string]any{
			{
				"type":          "text",
				"text":          usage.ClaudeCodeIdentity,
				"cache_control": map[string]any{"type": "ephemeral"},
			},
			{
				"type": "text",
				"text": titleSystemPrompt,
			},
		},
		"messages": []map[string]any{
			{
				"role":    "user",
				"content": []map[string]any{{"type": "text", "text": source}},
			},
		},
	}
}

// parseSSEText accumulates the text deltas of an Anthropic streaming
// response.
//
// Only `content_block_delta` frames carrying a `text_delta` contribute.
// Every other frame type — message_start, ping, content_block_start,
// message_delta, message_stop — is ignored rather than rejected, so a new
// frame type appearing upstream cannot break this path.
//
// A `data:` line that does not parse as JSON is skipped, not fatal: a
// partial title beats no title, and the caller tolerates "" anyway.
func parseSSEText(r io.Reader) (string, error) {
	var sb strings.Builder
	scanner := bufio.NewScanner(r)
	// SSE lines are short, but a single delta is bounded only by the model.
	scanner.Buffer(make([]byte, 0, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var frame struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			continue
		}
		if frame.Type == "content_block_delta" && frame.Delta.Type == "text_delta" {
			sb.WriteString(frame.Delta.Text)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("titlegen: read response stream: %w", err)
	}
	return sb.String(), nil
}

// stripWrappingQuotes removes one layer of matching quotes around s.
//
// Models wrap a requested title in quotes often enough to be worth handling,
// and a quoted title looks wrong in a fixed-width dashboard column. Only a
// matched outer pair is removed, so a title that legitimately contains a
// quote is left alone.
func stripWrappingQuotes(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return s
	}
	first, last := s[0], s[len(s)-1]
	if first == last && (first == '"' || first == '\'' || first == '`') {
		return strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// truncateRunes returns at most n runes of s, cutting on a rune boundary so
// a multi-byte character is never split into invalid UTF-8.
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
