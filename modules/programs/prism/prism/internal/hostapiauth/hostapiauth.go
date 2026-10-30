// Package hostapiauth holds the credential of the host API (#3379).
//
// Each sidecar writes a new random secret to hostapi.secret, in the run dir
// that holds its hostapi.sock, and refuses each host-API request that does
// not carry that secret in the Header field. A client finds the secret file
// next to the socket that it dials. The sandbox of a session can read its
// own run dir only (the bwrap bind, the section-6 rule of the sandbox-exec
// profile), so a session that reaches the socket of another sidecar does
// not have the secret of that sidecar.
//
// Without the check, the socket that a client reached decided its identity:
// a session that put a symlink to the hostapi.sock of a coordinator at its
// own hostapi.sock got the coordinator role.
//
// The package is a leaf: the sidecar, the prism CLI, and promptdelivery all
// import it.
package hostapiauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/prismatic-koi/prism/internal/rundir"
)

// Header is the request header that carries the secret. The pi extension
// sends the same name (pi/extensions/anthropic-oauth/host-api-auth.ts).
const Header = "X-Prism-Host-API-Secret"

// SecretFileName is the name of the secret file in the run dir. The pi
// extension uses the same name.
const SecretFileName = "hostapi.secret"

// secretBytes is the length of the random secret, before hex encoding.
const secretBytes = 32

// maxSecretFileBytes bounds the read of a secret file. The sandbox of the
// session can write the file, so the host must not read a file of any size.
const maxSecretFileBytes = 4096

// SecretPath returns the path of the secret file of the host-API socket at
// sockPath: hostapi.secret in the dir of sockPath. It does not resolve a
// symlink, so a session whose hostapi.sock is a symlink to another socket
// still gets its own secret path.
func SecretPath(sockPath string) string {
	return filepath.Join(filepath.Dir(sockPath), SecretFileName)
}

// WriteNewSecret makes a new random secret, writes it hex-encoded to path
// with mode 0600, and returns it.
//
// rundir.WriteFile replaces a symlink or a file that the sandbox put at path
// and does not follow it. The sidecar keeps the returned value and never
// reads the file back, so a change to the file cannot change the secret that
// the sidecar expects.
func WriteNewSecret(path string) (string, error) {
	raw := make([]byte, secretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate host-API secret: %w", err)
	}
	secret := hex.EncodeToString(raw)
	if err := rundir.WriteFile(path, []byte(secret)); err != nil {
		return "", fmt.Errorf("write host-API secret: %w", err)
	}
	return secret, nil
}

// ReadSecret reads the secret file at path. It refuses a symlink at path, a
// dir of path that is a symlink, and a path that is not a regular file. It
// returns an error for an empty file.
func ReadSecret(path string) (string, error) {
	if err := rundir.RefuseSymlinkDir(filepath.Dir(path)); err != nil {
		return "", err
	}
	f, err := rundir.OpenRead(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretFileBytes))
	if err != nil {
		return "", fmt.Errorf("read host-API secret %s: %w", path, err)
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return "", fmt.Errorf("host-API secret %s is empty", path)
	}
	return secret, nil
}

// Matches reports whether got equals want. The compare takes the same time
// for each got of a given length. An empty want matches nothing, so a
// sidecar that has no secret refuses every request.
func Matches(got, want string) bool {
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// DialUnix dials the host-API socket at sockPath. It refuses a sockPath
// that is a symlink, or whose dir is a symlink, and the error names the
// path. See rundir.RefuseSymlink for the limit of this check.
func DialUnix(ctx context.Context, d *net.Dialer, sockPath string) (net.Conn, error) {
	if err := rundir.RefuseSymlink(sockPath); err != nil {
		return nil, fmt.Errorf("host-API socket: %w", err)
	}
	return d.DialContext(ctx, "unix", sockPath)
}

// WithSecret returns a RoundTripper that sets Header on each request to the
// secret in secretPath, and then sends the request with base. It reads the
// file for each request, so a client gets the new secret of a sidecar that
// restarted.
//
// If the read fails, the request goes without Header and the sidecar refuses
// it with 401. That error names the cause, so the client does not stop here.
func WithSecret(base http.RoundTripper, secretPath string) http.RoundTripper {
	return &secretTransport{base: base, secretPath: secretPath}
}

type secretTransport struct {
	base       http.RoundTripper
	secretPath string
}

func (t *secretTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	secret, err := ReadSecret(t.secretPath)
	if err != nil {
		return t.base.RoundTrip(req)
	}
	// A RoundTripper must not change the request of the caller.
	out := req.Clone(req.Context())
	out.Header.Set(Header, secret)
	return t.base.RoundTrip(out)
}

// UnixTransport returns the transport of a host-API client for the socket
// at sockPath: dial is the dial function of the client (nil for DialUnix
// with a zero Dialer), and each request carries the secret of the socket.
func UnixTransport(sockPath string, dial func(ctx context.Context) (net.Conn, error)) http.RoundTripper {
	if dial == nil {
		dial = func(ctx context.Context) (net.Conn, error) {
			return DialUnix(ctx, &net.Dialer{}, sockPath)
		}
	}
	base := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx)
		},
	}
	return WithSecret(base, SecretPath(sockPath))
}
