package promptdelivery_test

import (
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/prismatic-koi/prism/internal/db"
	"github.com/prismatic-koi/prism/internal/promptdelivery"
	"github.com/prismatic-koi/prism/internal/session"
	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

func TestErrorBodySnippet(t *testing.T) {
	limit := promptdelivery.ErrorBodyLimit
	// "é" is 2 bytes. 511 ASCII bytes then "é" put the rune across the cut
	// at byte 512.
	splitRune := strings.Repeat("a", limit-1) + "é" + "tail"

	tests := []struct {
		name string
		in   string
		want string // exact; empty means check with fn
		fn   func(t *testing.T, got string)
	}{
		{name: "empty", in: "", want: "(empty body)"},
		{name: "whitespace only", in: " \n\t \r\n", want: "(empty body)"},
		{name: "short", in: `{"error":"bad deliver_as"}`, want: `{"error":"bad deliver_as"}`},
		{name: "multi-line", in: "line one\nline two\r\n\tline three\n", want: "line one line two line three"},
		{name: "over limit", in: strings.Repeat("x", limit*4), fn: func(t *testing.T, got string) {
			if want := strings.Repeat("x", limit) + "..."; got != want {
				t.Errorf("got len %d, want %d bytes of x plus ellipsis", len(got), len(want))
			}
		}},
		{name: "exactly at limit", in: strings.Repeat("x", limit), want: strings.Repeat("x", limit)},
		{name: "rune split at cut", in: splitRune, fn: func(t *testing.T, got string) {
			if !utf8.ValidString(got) {
				t.Errorf("output is not valid UTF-8")
			}
			if strings.ContainsRune(got, utf8.RuneError) {
				t.Errorf("output holds a replacement rune: the cut split a rune")
			}
			if want := strings.Repeat("a", limit-1) + "..."; got != want {
				t.Errorf("got len %d, want the %d bytes before the rune plus ellipsis", len(got), limit-1)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := promptdelivery.ErrorBodySnippet(strings.NewReader(tc.in))
			if strings.ContainsAny(got, "\r\n") {
				t.Errorf("output is not one line: %q", got)
			}
			if len(got) > limit+len("...") {
				t.Errorf("output length %d exceeds bound", len(got))
			}
			if tc.fn != nil {
				tc.fn(t, got)
			} else if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDeliverToSession_Non2xxErrorIncludesBody: a sidecar that answers 4xx
// with a body must have that body in the error the caller sees.
func TestDeliverToSession_Non2xxErrorIncludesBody(t *testing.T) {
	xdgTmp := stateguardtest.ShortTempDir(t, "prism-pd-4xx-")
	t.Setenv("XDG_STATE_HOME", xdgTmp)

	sessionName := "myrepo@feature"
	sockPath, err := session.SidecarHostAPIPath(sessionName)
	if err != nil {
		t.Fatalf("resolve socket path: %v", err)
	}
	if err := os.MkdirAll(sockPath[:strings.LastIndex(sockPath, "/")], 0o700); err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unknown deliver_as \"bogus\"", http.StatusBadRequest)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	pi := "pi"
	status := &db.Status{SessionName: sessionName, Harness: &pi}
	err = promptdelivery.DeliverToSession(sessionName, status, "hello", nil, "", "bogus")
	if err == nil {
		t.Fatal("DeliverToSession returned nil for a 400 response")
	}
	for _, want := range []string{"HTTP 400", `unknown deliver_as "bogus"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "\n") {
		t.Errorf("error is not one line: %q", err.Error())
	}
}
