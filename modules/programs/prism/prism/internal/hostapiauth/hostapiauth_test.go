package hostapiauth

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/rundir"
	"github.com/prismatic-koi/prism/internal/stateguard/stateguardtest"
)

func TestSecretPath_IsInTheDirOfTheSocket(t *testing.T) {
	got := SecretPath("/state/prism/run/abc/hostapi.sock")
	if want := "/state/prism/run/abc/hostapi.secret"; got != want {
		t.Errorf("SecretPath = %q, want %q", got, want)
	}
}

func TestWriteNewSecret_WritesReadableSecretWithMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "abc", SecretFileName)
	secret, err := WriteNewSecret(path)
	if err != nil {
		t.Fatalf("WriteNewSecret: %v", err)
	}
	if len(secret) != 2*secretBytes {
		t.Errorf("the secret has %d hex chars, want %d", len(secret), 2*secretBytes)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("the secret file has mode %v, want 0600", info.Mode().Perm())
	}
	got, err := ReadSecret(path)
	if err != nil || got != secret {
		t.Errorf("ReadSecret after WriteNewSecret: match=%v err=%v", got == secret, err)
	}
	again, err := WriteNewSecret(path)
	if err != nil {
		t.Fatal(err)
	}
	if again == secret {
		t.Error("two calls of WriteNewSecret gave the same secret")
	}
}

func TestReadSecret_RefusesSymlinkAndEmptyFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "other-secret")
	if err := os.WriteFile(target, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, SecretFileName)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadSecret(link); err == nil {
		t.Errorf("ReadSecret followed a symlink and read %d bytes", len(got))
	}

	empty := filepath.Join(t.TempDir(), SecretFileName)
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSecret(empty); err == nil {
		t.Error("ReadSecret accepted an empty secret file")
	}
}

func TestMatches(t *testing.T) {
	cases := []struct {
		got, want string
		match     bool
	}{
		{"abc", "abc", true},
		{"abd", "abc", false},
		{"", "abc", false},
		{"", "", false},
		{"abc", "", false},
	}
	for _, c := range cases {
		if Matches(c.got, c.want) != c.match {
			t.Errorf("Matches(%q, %q) = %v, want %v", c.got, c.want, !c.match, c.match)
		}
	}
}

// listen binds a Unix socket in a short dir (sun_path is 104 bytes on
// Darwin) and serves h. It returns the socket path.
func listen(t *testing.T, h http.Handler) string {
	t.Helper()
	dir := filepath.Join(stateguardtest.ShortTempDir(t, "hapi"), "run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "hostapi.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// headerEcho answers 200 with "secret" in the body when the request carries
// Header, and 204 when it does not. It never writes the value.
func headerEcho(want string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get(Header)
		switch {
		case got == "":
			w.WriteHeader(http.StatusNoContent)
		case got == want:
			_, _ = w.Write([]byte("secret"))
		default:
			w.WriteHeader(http.StatusForbidden)
		}
	})
}

func TestUnixTransport_SendsTheSecretOfTheSocket(t *testing.T) {
	var want string
	sock := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headerEcho(want).ServeHTTP(w, r)
	}))
	secret, err := WriteNewSecret(SecretPath(sock))
	if err != nil {
		t.Fatal(err)
	}
	want = secret
	client := &http.Client{Transport: UnixTransport(sock, nil), Timeout: 5 * time.Second}
	req, _ := http.NewRequest(http.MethodGet, "http://prism-hostapi/x", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200 (the request did not carry the secret of the socket)", resp.StatusCode)
	}
	if req.Header.Get(Header) != "" {
		t.Error("the transport set the header on the request of the caller")
	}

	// The transport reads the file for each request.
	rotated, err := WriteNewSecret(SecretPath(sock))
	if err != nil {
		t.Fatal(err)
	}
	want = rotated
	resp, err = client.Get("http://prism-hostapi/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d after a new secret, want 200", resp.StatusCode)
	}
}

func TestUnixTransport_NoSecretFileSendsNoHeader(t *testing.T) {
	sock := listen(t, headerEcho("unused"))
	client := &http.Client{Transport: UnixTransport(sock, nil), Timeout: 5 * time.Second}
	resp, err := client.Get("http://prism-hostapi/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status %d, want 204 (no header without a secret file)", resp.StatusCode)
	}
}

// TestDialUnix_RefusesSymlinkedSocket: a hostapi.sock that is a symlink to
// another live socket is refused, and the dial does not reach that socket.
func TestDialUnix_RefusesSymlinkedSocket(t *testing.T) {
	reached := make(chan struct{}, 1)
	other := listen(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- struct{}{}
	}))
	ownDir := filepath.Join(stateguardtest.ShortTempDir(t, "hapi"), "own")
	if err := os.MkdirAll(ownDir, 0o700); err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(ownDir, "hostapi.sock")
	if err := os.Symlink(other, own); err != nil {
		t.Fatal(err)
	}

	_, err := DialUnix(context.Background(), &net.Dialer{}, own)
	if !errors.Is(err, rundir.ErrSymlink) {
		t.Fatalf("DialUnix of a symlinked socket: %v, want an error that wraps rundir.ErrSymlink", err)
	}
	if !strings.Contains(err.Error(), own) {
		t.Errorf("the error %q does not name the path %s", err, own)
	}

	client := &http.Client{Transport: UnixTransport(own, nil), Timeout: 2 * time.Second}
	if resp, err := client.Get("http://prism-hostapi/x"); err == nil {
		resp.Body.Close()
		t.Error("a client of UnixTransport reached the socket through a symlink")
	}
	select {
	case <-reached:
		t.Error("the other socket got a request through the symlink")
	default:
	}
}
