package cmd

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/prismatic-koi/prism/internal/session"
)

// startHostAPIStub starts a stand-in for the host-API socket of the pi
// session sessionName. handle gets the path and the body of each request and
// returns the HTTP status of the answer.
//
// The socket path comes from XDG_STATE_HOME, so set XDG_STATE_HOME before
// the call. Use a short dir (stateguardtest.ShortTempDir): the path must fit
// in sun_path.
func startHostAPIStub(t *testing.T, sessionName string, handle func(path string, body []byte) int) {
	t.Helper()
	sockPath, err := session.SidecarHostAPIPath(sessionName)
	if err != nil {
		t.Fatalf("host-API socket path of %q: %v", sessionName, err)
	}
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen on %s: %v", sockPath, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.WriteHeader(handle(r.URL.Path, body))
		_, _ = w.Write([]byte(`{}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}
