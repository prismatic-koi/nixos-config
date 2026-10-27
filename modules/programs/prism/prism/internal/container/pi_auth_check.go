package container

// pi_auth_check.go — refuse to start pi when auth.json holds a command value
// (#3284).
//
// pi 1.4 resolves the key of each `type: "api_key"` entry in auth.json with
// resolveConfigValue (dist/core/resolve-config-value.js). A value that starts
// with "!" runs as a shell command. A sandboxed session must write auth.json
// for the OAuth refresh, so it can plant such a value, and the next pi that
// reads the file runs it. Before each pi start that prism makes, prism reads
// the file on the host and refuses the start if a value starts with "!".
//
// This check does not cover a pi that a person starts in a terminal, or a
// pi that already runs. See docs/invariants/sandbox-pi-agent-dir.md.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// piAuthFileLimit is the largest auth.json that the check reads. A real
// file is a few kB.
const piAuthFileLimit = 4 << 20

// PIHostAuthPath returns the auth.json path that a pi started on the host
// reads: $PI_CODING_AGENT_DIR/auth.json when the env var is set, else
// ~/.pi/agent/auth.json. It returns "" when neither resolves.
func PIHostAuthPath() string {
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return filepath.Join(dir, "auth.json")
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".pi", "agent", "auth.json")
}

// CheckPIAuthFile returns an error when pi can run a value of the auth file
// at path as a shell command, or when the check cannot read the file the way
// pi reads it. A missing or empty file is valid. The error names the file
// and the provider, and never holds a value from the file.
//
// The check is wider than pi: it refuses a string that starts with "!"
// anywhere in a provider entry, not only in the key of an api_key entry. It
// fails closed: a file that Go cannot decode as one JSON object is refused,
// because JSON.parse in pi can accept a file that Go does not.
func CheckPIAuthFile(path string) error {
	if path == "" {
		return nil
	}
	err := checkPIAuthFileOnce(path)
	if errors.Is(err, errPIAuthUndecodable) {
		// pi writes auth.json in place under its lock, and this read takes
		// no lock. Read once more before a refusal.
		piAuthRetryWait()
		err = checkPIAuthFileOnce(path)
	}
	return err
}

// errPIAuthUndecodable marks a file that Go cannot decode as one JSON object.
var errPIAuthUndecodable = errors.New("not one JSON object that prism can check")

// piAuthRetryWait is the wait before the second read of an undecodable
// file. Tests replace it.
var piAuthRetryWait = func() { time.Sleep(200 * time.Millisecond) }

func checkPIAuthFileOnce(path string) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("pi auth file %q: %w", path, err)
	}
	defer f.Close()
	if fi, statErr := f.Stat(); statErr != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("pi auth file %q is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, piAuthFileLimit+1))
	if err != nil {
		return fmt.Errorf("pi auth file %q: %w", path, err)
	}
	if len(data) > piAuthFileLimit {
		return fmt.Errorf("pi auth file %q is larger than %d bytes", path, piAuthFileLimit)
	}
	// pi strips a byte order mark before it parses (stripBom).
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if len(data) == 0 {
		return nil
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil || top == nil {
		return fmt.Errorf("pi auth file %q is %w", path, errPIAuthUndecodable)
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("pi auth file %q is %w", path, errPIAuthUndecodable)
	}

	providers := make([]string, 0, len(top))
	for p := range top {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	for _, p := range providers {
		if hasCommandValue(top[p]) {
			return fmt.Errorf("pi auth file %q: the entry of provider %q has a value that starts with \"!\". "+
				"pi runs such a value as a shell command, so prism does not start pi. "+
				"A sandboxed session can have put it there: report this to the user, "+
				"and do not edit the file", path, p)
		}
	}
	return nil
}

// hasCommandValue reports whether v holds a string that starts with "!", at
// any depth.
func hasCommandValue(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.HasPrefix(x, "!")
	case map[string]any:
		for _, e := range x {
			if hasCommandValue(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if hasCommandValue(e) {
				return true
			}
		}
	}
	return false
}
