package titlegen

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseBody renders a minimal Anthropic streaming response carrying the given
// text deltas, plus the surrounding frame types a real response includes.
func sseBody(deltas ...string) string {
	var b strings.Builder
	b.WriteString("event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	b.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0}\n\n")
	for _, d := range deltas {
		payload, _ := json.Marshal(map[string]any{
			"type":  "content_block_delta",
			"index": 0,
			"delta": map[string]string{"type": "text_delta", "text": d},
		})
		b.WriteString("event: content_block_delta\ndata: " + string(payload) + "\n\n")
	}
	b.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return b.String()
}

// newTestGenerator points a Generator at a local server. BaseURL is the only
// seam that can redirect the request, and it is in-process by design — no
// environment variable reaches it, because the request carries a bearer
// token.
func newTestGenerator(t *testing.T, h http.HandlerFunc) *Generator {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Generator{
		BaseURL:    srv.URL,
		Token:      func() (string, error) { return "test-token", nil },
		HTTPClient: srv.Client(),
	}
}

// TestGenerateTitle_HappyPath verifies the streamed deltas are reassembled
// into one title.
func TestGenerateTitle_HappyPath(t *testing.T) {
	g := newTestGenerator(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, sseBody("Generate ", "session ", "titles"))
	})
	got, err := g.GenerateTitle(context.Background(), "Implement issue #2683: generate session titles")
	if err != nil {
		t.Fatalf("GenerateTitle: %v", err)
	}
	if got != "Generate session titles" {
		t.Errorf("GenerateTitle = %q, want %q", got, "Generate session titles")
	}
}

// TestGenerateTitle_RequestShape pins the three WAF-critical elements the
// internal/usage doc comment describes. Omitting any of them returns a 429
// that reads exactly like quota exhaustion and is not (#2537), so each is
// asserted explicitly rather than assumed.
func TestGenerateTitle_RequestShape(t *testing.T) {
	type captured struct {
		query  string
		header http.Header
		body   map[string]any
	}
	var got captured
	g := newTestGenerator(t, func(w http.ResponseWriter, r *http.Request) {
		got.query = r.URL.RawQuery
		got.header = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		_, _ = io.WriteString(w, sseBody("A title"))
	})
	if _, err := g.GenerateTitle(context.Background(), "some task"); err != nil {
		t.Fatalf("GenerateTitle: %v", err)
	}

	// Element 1: ?beta=true on /v1/messages.
	if !strings.Contains(got.query, "beta=true") {
		t.Errorf("query = %q, want it to carry beta=true", got.query)
	}

	// Element 2: the fingerprint header block.
	for _, h := range []string{
		"authorization", "anthropic-version", "anthropic-beta",
		"anthropic-dangerous-direct-browser-access", "x-app", "user-agent",
		"x-stainless-arch", "x-stainless-lang", "x-stainless-os",
		"x-stainless-package-version", "x-stainless-runtime",
	} {
		if got.header.Get(h) == "" {
			t.Errorf("request is missing header %q", h)
		}
	}
	if want := "Bearer test-token"; got.header.Get("authorization") != want {
		t.Errorf("authorization = %q, want %q", got.header.Get("authorization"), want)
	}
	if ua := got.header.Get("user-agent"); !strings.HasPrefix(ua, "claude-cli/") {
		t.Errorf("user-agent = %q, want a claude-cli/<version> prefix", ua)
	}

	// Element 3: the Claude Code identity as the FIRST system block, with an
	// ephemeral cache_control. The instruction block must come after it —
	// demoting the identity is what triggers the misleading 429.
	system, ok := got.body["system"].([]any)
	if !ok || len(system) < 2 {
		t.Fatalf("body.system = %#v, want at least two blocks", got.body["system"])
	}
	first, _ := system[0].(map[string]any)
	if first["text"] != "You are Claude Code, Anthropic's official CLI for Claude." {
		t.Errorf("first system block = %#v, want the Claude Code identity", first)
	}
	if _, hasCache := first["cache_control"]; !hasCache {
		t.Error("first system block is missing cache_control")
	}

	// `stream: true` mirrors pi's OAuth path, which is always SSE.
	if got.body["stream"] != true {
		t.Errorf("body.stream = %#v, want true", got.body["stream"])
	}
	if got.body["model"] == "" || got.body["model"] == nil {
		t.Error("body.model is empty")
	}
}

// TestGenerateTitle_SanitisesModelOutput covers the security AC from the
// title side. The model is a remote party, so its output is untrusted text
// like any other; it lands in a column rendered verbatim by the dashboard.
func TestGenerateTitle_SanitisesModelOutput(t *testing.T) {
	g := newTestGenerator(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, sseBody("\x1b]0;pwned\x07Real ", "title\x00 here"))
	})
	got, err := g.GenerateTitle(context.Background(), "task")
	if err != nil {
		t.Fatalf("GenerateTitle: %v", err)
	}
	for _, r := range got {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("GenerateTitle returned %q, which carries control byte %#x", got, r)
		}
	}
	if !strings.Contains(got, "Real title here") {
		t.Errorf("GenerateTitle = %q, want it to retain the legitimate text", got)
	}
}

// TestGenerateTitle_TruncatesToMaxTitleRunes verifies a chatty model cannot
// produce a title wider than the dashboard column budget.
func TestGenerateTitle_TruncatesToMaxTitleRunes(t *testing.T) {
	long := strings.Repeat("very long title ", 40)
	g := newTestGenerator(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, sseBody(long))
	})
	got, err := g.GenerateTitle(context.Background(), "task")
	if err != nil {
		t.Fatalf("GenerateTitle: %v", err)
	}
	if n := len([]rune(got)); n > MaxTitleRunes {
		t.Errorf("GenerateTitle returned %d runes, want <= %d", n, MaxTitleRunes)
	}
}

// TestGenerateTitle_StripsWrappingQuotes — models often quote a requested
// title, which looks wrong in a fixed-width column.
func TestGenerateTitle_StripsWrappingQuotes(t *testing.T) {
	for _, in := range []string{`"Fix the login bug"`, `'Fix the login bug'`, "`Fix the login bug`"} {
		g := newTestGenerator(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, sseBody(in))
		})
		got, err := g.GenerateTitle(context.Background(), "task")
		if err != nil {
			t.Fatalf("GenerateTitle(%q): %v", in, err)
		}
		if got != "Fix the login bug" {
			t.Errorf("GenerateTitle(%q) = %q, want %q", in, got, "Fix the login bug")
		}
	}
}

// TestGenerateTitle_ErrorPaths verifies every remote failure is reported as
// a plain error the caller can fall back from. None of them may panic, and
// none may return a title.
func TestGenerateTitle_ErrorPaths(t *testing.T) {
	t.Run("non-200 status", func(t *testing.T) {
		g := newTestGenerator(t, func(w http.ResponseWriter, r *http.Request) {
			// 429 is the interesting one: it is what a WAF rejection looks
			// like, and it must not be retried or treated as fatal.
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"rate limited"}`)
		})
		got, err := g.GenerateTitle(context.Background(), "task")
		if err == nil {
			t.Fatalf("GenerateTitle = %q, want an error on HTTP 429", got)
		}
		if got != "" {
			t.Errorf("GenerateTitle returned %q alongside an error, want \"\"", got)
		}
	})

	t.Run("401 unauthenticated", func(t *testing.T) {
		g := newTestGenerator(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
		if _, err := g.GenerateTitle(context.Background(), "task"); err == nil {
			t.Fatal("GenerateTitle returned nil error on HTTP 401")
		}
	})

	t.Run("empty stream", func(t *testing.T) {
		g := newTestGenerator(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, sseBody())
		})
		_, err := g.GenerateTitle(context.Background(), "task")
		if !errors.Is(err, ErrEmptyTitle) {
			t.Errorf("err = %v, want ErrEmptyTitle", err)
		}
	})

	t.Run("malformed sse is skipped, not fatal", func(t *testing.T) {
		g := newTestGenerator(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, "data: {not json}\n\n"+sseBody("Good title"))
		})
		got, err := g.GenerateTitle(context.Background(), "task")
		if err != nil {
			t.Fatalf("GenerateTitle: %v", err)
		}
		if got != "Good title" {
			t.Errorf("GenerateTitle = %q, want %q", got, "Good title")
		}
	})

	t.Run("token source failure makes no request", func(t *testing.T) {
		called := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))
		t.Cleanup(srv.Close)
		g := &Generator{
			BaseURL:    srv.URL,
			HTTPClient: srv.Client(),
			Token:      func() (string, error) { return "", errors.New("no credentials on this host") },
		}
		if _, err := g.GenerateTitle(context.Background(), "task"); err == nil {
			t.Fatal("GenerateTitle returned nil error when the token source failed")
		}
		if called {
			t.Error("a request was sent despite the token source failing")
		}
	})

	t.Run("no token source configured", func(t *testing.T) {
		g := &Generator{}
		if _, err := g.GenerateTitle(context.Background(), "task"); err == nil {
			t.Fatal("GenerateTitle returned nil error with no token source")
		}
	})

	t.Run("blank source text makes no request", func(t *testing.T) {
		called := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			called = true
		}))
		t.Cleanup(srv.Close)
		g := &Generator{
			BaseURL:    srv.URL,
			HTTPClient: srv.Client(),
			Token:      func() (string, error) { return "t", nil },
		}
		if _, err := g.GenerateTitle(context.Background(), "   \n\t "); !errors.Is(err, ErrEmptyTitle) {
			t.Errorf("err = %v, want ErrEmptyTitle", err)
		}
		if called {
			t.Error("a request was sent for blank source text")
		}
	})
}

// TestGenerateTitle_ContextCancellationIsNotFatal verifies a slow server
// gives up rather than hanging. The caller runs this on a session's first
// turn, so a wedged socket must never hold a turn open.
func TestGenerateTitle_ContextCancellationIsNotFatal(t *testing.T) {
	release := make(chan struct{})
	g := newTestGenerator(t, func(w http.ResponseWriter, r *http.Request) {
		<-release
	})
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	got, err := g.GenerateTitle(ctx, "task")
	if err == nil {
		t.Fatalf("GenerateTitle = %q, want a timeout error", got)
	}
	if got != "" {
		t.Errorf("GenerateTitle returned %q alongside a timeout error", got)
	}
}

// TestGenerateTitle_SendsExactlyOneRequest — there is no retry. A retry
// would double the quota cost of a display string, on the first turn of
// every eligible session.
func TestGenerateTitle_SendsExactlyOneRequest(t *testing.T) {
	var requests int
	g := newTestGenerator(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusInternalServerError)
	})
	_, _ = g.GenerateTitle(context.Background(), "task")
	if requests != 1 {
		t.Errorf("request count = %d, want exactly 1 (no retry)", requests)
	}
}

// TestGenerateTitle_CapsSourceText verifies a very large spawn prompt is
// truncated before it is sent. A prompt can carry an entire issue body plus
// acceptance criteria; sending all of it costs input tokens for no benefit.
func TestGenerateTitle_CapsSourceText(t *testing.T) {
	var sentLen int
	g := newTestGenerator(t, func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) > 0 && len(body.Messages[0].Content) > 0 {
			sentLen = len([]rune(body.Messages[0].Content[0].Text))
		}
		_, _ = io.WriteString(w, sseBody("Title"))
	})
	huge := strings.Repeat("a very long spawn prompt line. ", 5000)
	if _, err := g.GenerateTitle(context.Background(), huge); err != nil {
		t.Fatalf("GenerateTitle: %v", err)
	}
	if sentLen > maxSourceRunes {
		t.Errorf("sent %d runes of source text, want <= %d", sentLen, maxSourceRunes)
	}
	if sentLen == 0 {
		t.Error("sent no source text at all")
	}
}
