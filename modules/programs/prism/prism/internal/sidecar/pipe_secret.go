package sidecar

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// pipeSecretBytes is the length of the random harness pipe secret, before
// hex encoding.
const pipeSecretBytes = 32

// pipeSecretRedacted replaces the secret of a hello frame in the frame
// archive.
const pipeSecretRedacted = "[redacted]"

// pipeRefusedCode is the code of the error frame that the sidecar sends to a
// peer whose first frame does not carry the pipe secret.
const pipeRefusedCode = "unauthorized"

// writeHarnessPipeSecret makes a new random secret, writes it hex-encoded to
// path with mode 0600, and returns it.
//
// The sandbox of the session can write the dir of path. So the secret goes
// to a new temp file in that dir (random name, O_EXCL), and a rename puts it
// at path. The rename replaces a symlink or a file that the sandbox put at
// path. It does not follow it. The function also refuses a dir that is a
// symlink: through it, the write can replace the secret file of another
// session.
//
// The sidecar keeps the returned value and never reads the file back. Thus a
// change to the file in the sandbox cannot change the secret that the
// sidecar expects.
func writeHarnessPipeSecret(path string) (string, error) {
	raw := make([]byte, pipeSecretBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate harness pipe secret: %w", err)
	}
	secret := hex.EncodeToString(raw)

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("harness pipe secret dir: %w", err)
	}
	if info, err := os.Lstat(dir); err != nil {
		return "", fmt.Errorf("harness pipe secret dir: %w", err)
	} else if !info.IsDir() {
		return "", fmt.Errorf("harness pipe secret dir %s is not a directory (mode %v)", dir, info.Mode())
	}
	tmp, err := os.CreateTemp(dir, ".pipe.secret-*")
	if err != nil {
		return "", fmt.Errorf("harness pipe secret temp file: %w", err)
	}
	tmpPath := tmp.Name()
	_, writeErr := tmp.WriteString(secret)
	closeErr := tmp.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr == nil {
		writeErr = os.Rename(tmpPath, path)
	}
	if writeErr != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write harness pipe secret %s: %w", path, writeErr)
	}
	return secret, nil
}

// pipeSecretMatches reports whether got equals want. The compare takes the
// same time for each got of a given length. An empty want matches nothing,
// so a sidecar with no secret refuses every peer.
func pipeSecretMatches(got, want string) bool {
	if want == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// redactHelloSecret returns the hello frame for the frame archive, with the
// value of "secret" replaced by pipeSecretRedacted. The archive is readable
// with `prism logs`, so it must not hold a live secret. It returns nil when
// line is not a JSON object.
func redactHelloSecret(line []byte) []byte {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil || fields == nil {
		return nil
	}
	if _, ok := fields["secret"]; ok {
		fields["secret"] = json.RawMessage(`"` + pipeSecretRedacted + `"`)
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return out
}

// refusePipePeer sends an unauthorized error frame to a peer that did not
// show the pipe secret, and closes the connection. The frame is not
// archived: the peer is not the agent of this session, so the exchange is
// not part of its harness traffic.
func refusePipePeer(conn net.Conn, reason string) {
	b, _ := json.Marshal(struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Type: "error", Code: pipeRefusedCode, Message: reason})
	b = append(b, '\n')
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = conn.Write(b)
	_ = conn.Close()
}
