package container

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// piAuthEnv is one session with a per-session auth dir and a host auth
// file, both under test-owned dirs.
type piAuthEnv struct {
	id       string
	hostAuth string
	agentDir string
	root     string
}

func (e piAuthEnv) sessionAuth() string { return filepath.Join(e.agentDir, "auth.json") }

const piAuthTestHost = `{
  "anthropic": {"type": "oauth", "access": "access-0", "refresh": "refresh-0", "expires": 1000},
  "github-copilot": {"type": "oauth", "access": "c-access", "refresh": "c-refresh", "expires": 2000, "enterpriseUrl": "ghe.example"},
  "openrouter": {"type": "api_key", "key": "sk-or-1"},
  "openai": {"type": "api_key", "key": "!op read secret"}
}`

func newPIAuthEnv(t *testing.T, host string) piAuthEnv {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	setPIAuthTestWaits(t)
	hostDir := filepath.Join(t.TempDir(), ".pi", "agent")
	if err := os.MkdirAll(hostDir, 0o700); err != nil {
		t.Fatal(err)
	}
	hostAuth := filepath.Join(hostDir, "auth.json")
	if host != "" {
		if err := os.WriteFile(hostAuth, []byte(host), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	id := "11111111-2222-3333-4444-555555555555"
	agentDir, err := PreparePIAuthSession(id, hostAuth)
	if err != nil {
		t.Fatalf("PreparePIAuthSession: %v", err)
	}
	root, _ := PIAuthSessionRoot(id)
	return piAuthEnv{id: id, hostAuth: hostAuth, agentDir: agentDir, root: root}
}

func setPIAuthTestWaits(t *testing.T) {
	t.Helper()
	prev := []time.Duration{piAuthSyncLockWait, piAuthPrepareLockWait}
	piAuthSyncLockWait, piAuthPrepareLockWait = 200*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { piAuthSyncLockWait, piAuthPrepareLockWait = prev[0], prev[1] })
}

func readJSONFile(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var m map[string]map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	return m
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeSessionEntry sets one entry of the session file, as pi does.
func (e piAuthEnv) writeSessionEntry(t *testing.T, provider, entry string) {
	t.Helper()
	data, err := os.ReadFile(e.sessionAuth())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m[provider] = json.RawMessage(entry)
	out, _ := json.Marshal(m)
	writeFile(t, e.sessionAuth(), string(out))
}

// removeHostEntry removes one entry of the host file, as a logout on the
// host does.
func (e piAuthEnv) removeHostEntry(t *testing.T, provider string) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(mustRead(t, e.hostAuth)), &m); err != nil {
		t.Fatal(err)
	}
	delete(m, provider)
	out, _ := json.Marshal(m)
	writeFile(t, e.hostAuth, string(out))
}

// loginAgain adds the anthropic entry to the host file n times. Each time,
// a logout on the host removes the entry, and a login in the session adds
// it again. Each add counts as a new provider of the session.
func (e piAuthEnv) loginAgain(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		e.removeHostEntry(t, "anthropic")
		e.sync(t)
		e.writeSessionEntry(t, "anthropic", fmt.Sprintf(`{"type":"oauth","access":"login-%d","refresh":"login-%d-r","expires":%d}`, i, i, 9000+i))
		if res := e.sync(t); !slices.Equal(res.ToHost, []string{"anthropic"}) {
			t.Fatalf("login %d: result = %+v, want anthropic to the host", i, res)
		}
	}
}

func (e piAuthEnv) sync(t *testing.T) PIAuthSyncResult {
	t.Helper()
	res, err := SyncPIAuthSession(e.id, e.hostAuth)
	if err != nil {
		t.Fatalf("SyncPIAuthSession: %v", err)
	}
	return res
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// stringsWithBang returns each string in v that starts with "!".
func stringsWithBang(v any) []string {
	var out []string
	switch x := v.(type) {
	case string:
		if strings.HasPrefix(x, "!") {
			out = append(out, x)
		}
	case map[string]any:
		for _, e := range x {
			out = append(out, stringsWithBang(e)...)
		}
	case []any:
		for _, e := range x {
			out = append(out, stringsWithBang(e)...)
		}
	}
	return out
}

func assertNoCommandValue(t *testing.T, path string) {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(mustRead(t, path)), &v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if bad := stringsWithBang(v); len(bad) > 0 {
		t.Errorf("%s holds a value that starts with \"!\": %v", path, bad)
	}
	if err := CheckPIAuthFile(path); err != nil {
		t.Errorf("CheckPIAuthFile(%s) = %v", path, err)
	}
}

// The auth.json that prism writes for a session holds each host entry,
// less each entry with a value that starts with "!", at any depth.
func TestPreparePIAuthSession_WritesHostFileWithNoCommandValue(t *testing.T) {
	e := newPIAuthEnv(t, `{
  "anthropic": {"type": "oauth", "access": "a", "refresh": "r", "expires": 1},
  "openrouter": {"type": "api_key", "key": "sk-or-1"},
  "openai": {"type": "api_key", "key": "!touch /tmp/planted"},
  "nested": {"type": "api_key", "key": "k", "env": {"A": "!cmd"}},
  "weird": {"type": "oauth", "access": "!x", "refresh": "r", "expires": 1}
}`)
	got := readJSONFile(t, e.sessionAuth())
	var names []string
	for k := range got {
		names = append(names, k)
	}
	slices.Sort(names)
	if want := []string{"anthropic", "openrouter"}; !slices.Equal(names, want) {
		t.Errorf("session entries = %v, want %v", names, want)
	}
	assertNoCommandValue(t, e.sessionAuth())

	for path, want := range map[string]os.FileMode{e.root: 0o700, e.agentDir: 0o700, e.sessionAuth(): 0o600} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
			t.Errorf("mode of %s = (%v, %v), want %v", path, fi, err, want)
		}
	}
	if state := mustRead(t, filepath.Join(e.root, piAuthSyncStateName)); strings.Contains(state, `"a"`) || strings.Contains(state, "sk-or") {
		t.Errorf("the sync state holds a token: %s", state)
	}
}

// A missing host file gives an empty object. A file that an earlier sandbox
// of the session put in the agent dir does not stay.
func TestPreparePIAuthSession_FreshDir(t *testing.T) {
	e := newPIAuthEnv(t, "")
	if got := strings.TrimSpace(mustRead(t, e.sessionAuth())); got != "{}" {
		t.Errorf("session auth file = %q, want {}", got)
	}
	writeFile(t, filepath.Join(e.agentDir, "settings.json"), `{"extensions":["/tmp/x.ts"]}`)
	if err := os.Symlink("/etc/passwd", filepath.Join(e.agentDir, "bin")); err != nil {
		t.Fatal(err)
	}
	if _, err := PreparePIAuthSession(e.id, e.hostAuth); err != nil {
		t.Fatalf("PreparePIAuthSession: %v", err)
	}
	entries, _ := os.ReadDir(e.agentDir)
	if len(entries) != 1 || entries[0].Name() != "auth.json" {
		t.Errorf("agent dir after a new prepare = %v, want only auth.json", entries)
	}
}

func TestPIAuthSessionRoot_RefusesBadInstanceID(t *testing.T) {
	for _, id := range []string{"", ".", "..", "../x", "a/b", strings.Repeat("a", 200)} {
		if _, err := PIAuthSessionRoot(id); err == nil {
			t.Errorf("PIAuthSessionRoot(%q) = nil error", id)
		}
		if _, err := PreparePIAuthSession(id, "/nonexistent/auth.json"); err == nil {
			t.Errorf("PreparePIAuthSession(%q) = nil error", id)
		}
	}
}

// An OAuth refresh in the session reaches the host file. Only type, access,
// refresh, and expires are copied. The other fields of the host entry stay,
// and the other host entries do not change.
func TestSyncPIAuthSession_RefreshReachesHost(t *testing.T) {
	e := newPIAuthEnv(t, strings.Replace(piAuthTestHost, `"expires": 1000}`, `"expires": 1000, "hostField": "keep"}`, 1))
	hostBefore := readJSONFile(t, e.hostAuth)
	e.writeSessionEntry(t, "anthropic", `{"type":"oauth","access":"access-1","refresh":"refresh-1","expires":5000,"key":"!evil","extra":{"x":"y"}}`)

	res := e.sync(t)
	if want := []string{"anthropic"}; !slices.Equal(res.ToHost, want) || len(res.Refused) != 0 {
		t.Errorf("ToHost = %v, want %v (refused: %v)", res.ToHost, want, res.Refused)
	}
	host := readJSONFile(t, e.hostAuth)
	if want := map[string]any{"type": "oauth", "access": "access-1", "refresh": "refresh-1", "expires": float64(5000), "hostField": "keep"}; !jsonEqual(host["anthropic"], want) {
		t.Errorf("host anthropic = %v, want %v", host["anthropic"], want)
	}
	for _, p := range []string{"github-copilot", "openai", "openrouter"} {
		if !jsonEqual(host[p], hostBefore[p]) {
			t.Errorf("host %s = %v, want it unchanged: %v", p, host[p], hostBefore[p])
		}
	}
	if fi, err := os.Stat(e.hostAuth); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("host mode = (%v, %v), want 0600", fi, err)
	}
	// The session file now holds the host view: no "!" value, no extra field.
	sess := readJSONFile(t, e.sessionAuth())
	if _, ok := sess["anthropic"]["key"]; ok || sess["anthropic"]["extra"] != nil {
		t.Errorf("session anthropic = %v, want the host entry", sess["anthropic"])
	}
	assertNoCommandValue(t, e.sessionAuth())

	// A second sync with no change does nothing.
	if res := e.sync(t); len(res.ToHost) != 0 || res.ToSession || len(res.Refused) != 0 {
		t.Errorf("second sync = %+v, want no change", res)
	}
}

func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// A value that a session writes reaches the host only as an OAuth entry
// with a string access, a string refresh, and a finite number expires.
// Each other value is refused with a reason, and the session file gets the
// host value again.
func TestSyncPIAuthSession_RefusesEveryOtherValue(t *testing.T) {
	apiKeyHost := strings.Replace(piAuthTestHost, `"anthropic": {"type": "oauth", "access": "access-0", "refresh": "refresh-0", "expires": 1000}`,
		`"anthropic": {"type": "api_key", "key": "sk-ant-1"}`, 1)
	if apiKeyHost == piAuthTestHost {
		t.Fatal("the fixture with an anthropic api key is the default fixture")
	}
	cases := map[string]struct{ provider, entry, host string }{
		"command api key":    {"openrouter", `{"type":"api_key","key":"!touch /tmp/planted"}`, ""},
		"new command key":    {"evil", `{"type":"api_key","key":"!touch /tmp/planted"}`, ""},
		"api key over oauth": {"anthropic", `{"type":"api_key","key":"sk-plain"}`, ""},
		"expires string":     {"anthropic", `{"type":"oauth","access":"a","refresh":"r","expires":"5000"}`, ""},
		"expires infinite":   {"anthropic", `{"type":"oauth","access":"a","refresh":"r","expires":1e400}`, ""},
		"access number":      {"anthropic", `{"type":"oauth","access":5,"refresh":"r","expires":5000}`, ""},
		"access null":        {"anthropic", `{"type":"oauth","access":null,"refresh":"r","expires":5000}`, ""},
		"no refresh":         {"anthropic", `{"type":"oauth","access":"a","expires":5000}`, ""},
		"empty access":       {"anthropic", `{"type":"oauth","access":"","refresh":"r","expires":5000}`, ""},
		"command access":     {"anthropic", `{"type":"oauth","access":"!touch /tmp/x","refresh":"r","expires":5000}`, ""},
		"not an object":      {"anthropic", `"!touch /tmp/x"`, ""},
		"oauth over api key": {"anthropic", `{"type":"oauth","access":"a","refresh":"r","expires":5000}`, apiKeyHost},
		"other provider":     {"github-copilot", `{"type":"oauth","access":"a","refresh":"r","expires":5000}`, ""},
		"bad provider name":  {"../x", `{"type":"oauth","access":"a","refresh":"r","expires":5000}`, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			host := tc.host
			if host == "" {
				host = piAuthTestHost
			}
			e := newPIAuthEnv(t, host)
			hostBefore := mustRead(t, e.hostAuth)
			e.writeSessionEntry(t, tc.provider, tc.entry)

			res := e.sync(t)
			if len(res.ToHost) != 0 {
				t.Errorf("ToHost = %v, want none", res.ToHost)
			}
			if len(res.Refused) != 1 {
				t.Errorf("Refused = %q, want one reason", res.Refused)
			}
			if got := mustRead(t, e.hostAuth); got != hostBefore {
				t.Errorf("host file changed:\n%s", got)
			}
			if !res.ToSession {
				t.Errorf("the session file did not get the host value again")
			}
			assertNoCommandValue(t, e.sessionAuth())
			// The refusal is reported once. The state does not record a
			// name that is not a provider name, so that one is reported
			// again.
			e.writeSessionEntry(t, tc.provider, tc.entry)
			wantRefused := 0
			if tc.provider == "../x" {
				wantRefused = 1
			}
			if res := e.sync(t); len(res.Refused) != wantRefused || len(res.ToHost) != 0 {
				t.Errorf("second sync = %+v, want %d refusals and no copy", res, wantRefused)
			}
		})
	}
}

// A session file that is a symlink, is not a regular file, is larger than
// the limit, or does not parse changes nothing on the host. The sync
// returns the reason, and does not follow the symlink.
func TestSyncPIAuthSession_HostileSessionFile(t *testing.T) {
	newToken := `{"anthropic":{"type":"oauth","access":"stolen","refresh":"stolen","expires":9999}}`
	cases := map[string]struct {
		plant  func(t *testing.T, path string)
		reason string
	}{
		"symlink": {func(t *testing.T, path string) {
			target := filepath.Join(t.TempDir(), "target.json")
			writeFile(t, target, newToken)
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}, "is a symlink"},
		"fifo": {func(t *testing.T, path string) {
			if err := syscall.Mkfifo(path, 0o600); err != nil {
				t.Skipf("mkfifo: %v", err)
			}
		}, "is not a regular file"},
		"dir": {func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}, "is not a regular file"},
		"too large": {func(t *testing.T, path string) {
			big := `{"anthropic":{"type":"oauth","access":"` + strings.Repeat("a", piAuthSessionFileLimit) + `","refresh":"r","expires":9999}}`
			writeFile(t, path, big)
		}, "is larger than"},
		"not json": {func(t *testing.T, path string) {
			writeFile(t, path, `{"anthropic":`)
		}, "is not one JSON object"},
		"two objects": {func(t *testing.T, path string) {
			writeFile(t, path, newToken+newToken)
		}, "is not one JSON object"},
		"array": {func(t *testing.T, path string) {
			writeFile(t, path, `[`+newToken+`]`)
		}, "is not one JSON object"},
		"null": {func(t *testing.T, path string) {
			writeFile(t, path, `null`)
		}, "is not one JSON object"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newPIAuthEnv(t, piAuthTestHost)
			hostBefore := mustRead(t, e.hostAuth)
			if err := os.Remove(e.sessionAuth()); err != nil {
				t.Fatal(err)
			}
			tc.plant(t, e.sessionAuth())
			sessBefore, _ := os.Lstat(e.sessionAuth())

			done := make(chan PIAuthSyncResult, 1)
			go func() {
				res, err := SyncPIAuthSession(e.id, e.hostAuth)
				if err != nil {
					t.Errorf("SyncPIAuthSession: %v", err)
				}
				done <- res
			}()
			var res PIAuthSyncResult
			select {
			case res = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the sync blocked on the session file")
			}

			if len(res.Refused) != 1 || !strings.Contains(res.Refused[0], tc.reason) {
				t.Errorf("Refused = %q, want one reason with %q", res.Refused, tc.reason)
			}
			if strings.Contains(strings.Join(res.Refused, " "), "stolen") {
				t.Errorf("the reason holds a value from the file: %q", res.Refused)
			}
			if len(res.ToHost) != 0 || res.ToSession {
				t.Errorf("result = %+v, want no copy", res)
			}
			if got := mustRead(t, e.hostAuth); got != hostBefore {
				t.Errorf("host file changed:\n%s", got)
			}
			if after, _ := os.Lstat(e.sessionAuth()); after.Mode() != sessBefore.Mode() || after.ModTime() != sessBefore.ModTime() {
				t.Errorf("session file changed: %v -> %v", sessBefore, after)
			}
		})
	}
}

// The no-op proof of the symlink case: a read that follows the symlink
// gives the host a new token. So the symlink test can fail.
func TestSyncPIAuthSession_SymlinkTargetHasNewToken(t *testing.T) {
	target := filepath.Join(t.TempDir(), "target.json")
	writeFile(t, target, `{"anthropic":{"type":"oauth","access":"stolen","refresh":"stolen","expires":9999}}`)
	f, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	file, err := readPIAuthOpenFile(f, target, "session auth file", piAuthSessionFileLimit)
	if err != nil {
		t.Fatalf("read of the target: %v", err)
	}
	if _, reason := piAuthOAuthEntry("anthropic", file.entries["anthropic"]); reason != "" {
		t.Fatalf("the target entry is not a valid OAuth entry (%s), so the symlink test proves nothing", reason)
	}
}

// pi writes an old snapshot of the file back after a refresh. A value that
// a sync already saw does not undo a newer host value.
func TestSyncPIAuthSession_OldSnapshotDoesNotUndoHost(t *testing.T) {
	e := newPIAuthEnv(t, piAuthTestHost)
	oldAnthropic := `{"type":"oauth","access":"access-0","refresh":"refresh-0","expires":1000}`

	// Another session refreshed anthropic, and the sync copies it here.
	writeFile(t, e.hostAuth, strings.Replace(piAuthTestHost, `"access": "access-0", "refresh": "refresh-0", "expires": 1000`,
		`"access": "a-new", "refresh": "a-new-r", "expires": 4000`, 1))
	if res := e.sync(t); !res.ToSession {
		t.Fatalf("the host change did not reach the session: %+v", res)
	}
	// pi writes its old snapshot back.
	e.writeSessionEntry(t, "anthropic", oldAnthropic)

	res := e.sync(t)
	if len(res.ToHost) != 0 {
		t.Errorf("ToHost = %v, want none: the old value undid the host value", res.ToHost)
	}
	if got := readJSONFile(t, e.hostAuth)["anthropic"]["access"]; got != "a-new" {
		t.Errorf("host anthropic access = %v, want a-new", got)
	}
	if got := readJSONFile(t, e.sessionAuth())["anthropic"]["access"]; got != "a-new" {
		t.Errorf("session anthropic access = %v, want a-new", got)
	}
}

// The sync copies only the anthropic entry from a session file to the host
// file (#3314). A github-copilot entry with a planted proxy-ep host, or a
// valid OAuth entry of any other provider, changes nothing in the host
// file. The sync reports the reason once, and the session file gets the
// host value again.
func TestSyncPIAuthSession_OtherProviderDoesNotReachHost(t *testing.T) {
	planted := `{"type":"oauth","access":"tid=x;exp=9999999999;proxy-ep=proxy.attacker.example;","refresh":"r","expires":9999999999999}`
	noCopilot := strings.Replace(piAuthTestHost,
		`  "github-copilot": {"type": "oauth", "access": "c-access", "refresh": "c-refresh", "expires": 2000, "enterpriseUrl": "ghe.example"},
`, "", 1)
	if strings.Contains(noCopilot, "github-copilot") {
		t.Fatal("the fixture with no copilot entry still has one")
	}
	cases := map[string]struct{ host, provider, entry string }{
		"copilot over the host entry": {piAuthTestHost, "github-copilot", planted},
		"new copilot entry":           {noCopilot, "github-copilot", planted},
		"new oauth provider":          {piAuthTestHost, "openai-codex", `{"type":"oauth","access":"a","refresh":"r","expires":5000}`},
		"oauth over api key":          {piAuthTestHost, "openrouter", `{"type":"oauth","access":"a","refresh":"r","expires":5000}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newPIAuthEnv(t, tc.host)
			hostBefore := mustRead(t, e.hostAuth)
			sessHostView := readJSONFile(t, e.sessionAuth())[tc.provider]
			e.writeSessionEntry(t, tc.provider, tc.entry)

			res := e.sync(t)
			if len(res.ToHost) != 0 {
				t.Errorf("ToHost = %v, want none", res.ToHost)
			}
			want := fmt.Sprintf(`provider %q: the sync copies only the "anthropic" entry to the host file`, tc.provider)
			if !slices.Equal(res.Refused, []string{want}) {
				t.Errorf("Refused = %q, want [%q]", res.Refused, want)
			}
			if got := mustRead(t, e.hostAuth); got != hostBefore {
				t.Errorf("host file changed:\n%s", got)
			}
			if !res.ToSession {
				t.Errorf("the session file did not get the host value again")
			}
			if got := readJSONFile(t, e.sessionAuth())[tc.provider]; !jsonEqual(got, sessHostView) {
				t.Errorf("session %s = %v, want the host view %v", tc.provider, got, sessHostView)
			}

			// The same value again is not reported again.
			e.writeSessionEntry(t, tc.provider, tc.entry)
			if res := e.sync(t); len(res.Refused) != 0 || len(res.ToHost) != 0 {
				t.Errorf("second sync = %+v, want no refusal and no copy", res)
			}
			if got := mustRead(t, e.hostAuth); got != hostBefore {
				t.Errorf("host file changed after the second sync:\n%s", got)
			}
		})
	}
}

// An Anthropic refresh in one session reaches the host file, and the sync
// of another session brings the new token to that session.
func TestSyncPIAuthSession_RefreshReachesOtherSession(t *testing.T) {
	a := newPIAuthEnv(t, piAuthTestHost)
	bID := "66666666-7777-8888-9999-000000000000"
	bDir, err := PreparePIAuthSession(bID, a.hostAuth)
	if err != nil {
		t.Fatalf("PreparePIAuthSession: %v", err)
	}
	bRoot, _ := PIAuthSessionRoot(bID)
	b := piAuthEnv{id: bID, hostAuth: a.hostAuth, agentDir: bDir, root: bRoot}

	a.writeSessionEntry(t, "anthropic", `{"type":"oauth","access":"access-a","refresh":"refresh-a","expires":5000}`)
	if res := a.sync(t); !slices.Equal(res.ToHost, []string{"anthropic"}) {
		t.Fatalf("sync of session a = %+v, want anthropic to the host", res)
	}
	if res := b.sync(t); !res.ToSession || len(res.ToHost) != 0 {
		t.Errorf("sync of session b = %+v, want a write of the session file only", res)
	}
	if got := readJSONFile(t, b.sessionAuth())["anthropic"]; got["access"] != "access-a" || got["refresh"] != "refresh-a" {
		t.Errorf("session b anthropic = %v, want the token of session a", got)
	}
}

// When the host entry and the session entry both changed, the host value
// wins, and the session value does not reach the host later.
func TestSyncPIAuthSession_HostWinsAConflict(t *testing.T) {
	e := newPIAuthEnv(t, piAuthTestHost)
	// prism account use replaces the host file with a rename.
	other := strings.Replace(piAuthTestHost, `"access": "access-0", "refresh": "refresh-0"`, `"access": "acct2", "refresh": "acct2-r"`, 1)
	tmp := e.hostAuth + ".new"
	writeFile(t, tmp, other)
	if err := os.Rename(tmp, e.hostAuth); err != nil {
		t.Fatal(err)
	}
	// The session refreshed the old account at the same time.
	e.writeSessionEntry(t, "anthropic", `{"type":"oauth","access":"acct1-new","refresh":"acct1-new-r","expires":7000}`)

	res := e.sync(t)
	if len(res.ToHost) != 0 || len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "host value wins") {
		t.Errorf("result = %+v, want one refusal: the host value wins", res)
	}
	if got := readJSONFile(t, e.sessionAuth())["anthropic"]["access"]; got != "acct2" {
		t.Errorf("session anthropic access = %v, want acct2", got)
	}
	e.writeSessionEntry(t, "anthropic", `{"type":"oauth","access":"acct1-new","refresh":"acct1-new-r","expires":7000}`)
	e.sync(t)
	if got := readJSONFile(t, e.hostAuth)["anthropic"]["access"]; got != "acct2" {
		t.Errorf("host anthropic access = %v, want acct2: the refused value reached the host later", got)
	}
}

// prism account use, a refresh of another session, or a login on the host
// reaches the session file with no restart, for each provider. A "!" value
// on the host does not.
func TestSyncPIAuthSession_HostChangeReachesSession(t *testing.T) {
	e := newPIAuthEnv(t, piAuthTestHost)
	other := strings.Replace(piAuthTestHost, `"access": "access-0"`, `"access": "acct2"`, 1)
	other = strings.Replace(other, `"access": "c-access"`, `"access": "c-new"`, 1)
	other = strings.Replace(other, `"sk-or-1"`, `"!curl evil"`, 1)
	other = strings.Replace(other, `{
`, `{
  "openai-codex": {"type": "oauth", "access": "x-access", "refresh": "x-refresh", "expires": 3000},
  "groq": {"type": "api_key", "key": "gsk-1"},
`, 1)
	tmp := e.hostAuth + ".new"
	writeFile(t, tmp, other)
	if err := os.Rename(tmp, e.hostAuth); err != nil {
		t.Fatal(err)
	}
	res := e.sync(t)
	if !res.ToSession || len(res.ToHost) != 0 {
		t.Errorf("result = %+v, want a write of the session file only", res)
	}
	sess := readJSONFile(t, e.sessionAuth())
	for p, want := range map[string][2]string{
		"anthropic":      {"access", "acct2"},
		"github-copilot": {"access", "c-new"},
		"openai-codex":   {"access", "x-access"},
		"groq":           {"key", "gsk-1"},
	} {
		if sess[p][want[0]] != want[1] {
			t.Errorf("session %s = %v, want %s %s", p, sess[p], want[0], want[1])
		}
	}
	if _, ok := sess["openrouter"]; ok {
		t.Errorf("session has the host entry with a \"!\" value: %v", sess["openrouter"])
	}
	assertNoCommandValue(t, e.sessionAuth())
}

// A login in the session reaches the host, as before #3295.
func TestSyncPIAuthSession_LoginReachesHost(t *testing.T) {
	e := newPIAuthEnv(t, "")
	writeFile(t, e.sessionAuth(), `{"anthropic":{"type":"oauth","access":"a","refresh":"r","expires":1}}`)
	if res := e.sync(t); !slices.Equal(res.ToHost, []string{"anthropic"}) {
		t.Fatalf("result = %+v, want anthropic to the host", res)
	}
	if got := readJSONFile(t, e.hostAuth)["anthropic"]["access"]; got != "a" {
		t.Errorf("host anthropic access = %v, want a", got)
	}
}

// A host auth.json that the user made a symlink stays a symlink.
func TestSyncPIAuthSession_HostSymlinkStays(t *testing.T) {
	e := newPIAuthEnv(t, piAuthTestHost)
	real := filepath.Join(t.TempDir(), "real-auth.json")
	writeFile(t, real, mustRead(t, e.hostAuth))
	if err := os.Remove(e.hostAuth); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, e.hostAuth); err != nil {
		t.Fatal(err)
	}
	e.writeSessionEntry(t, "anthropic", `{"type":"oauth","access":"access-1","refresh":"refresh-1","expires":5000}`)
	e.sync(t)
	if fi, err := os.Lstat(e.hostAuth); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("host auth.json = (%v, %v), want the symlink", fi, err)
	}
	if got := readJSONFile(t, real)["anthropic"]["access"]; got != "access-1" {
		t.Errorf("symlink target anthropic access = %v, want access-1", got)
	}
}

func TestSyncPIAuthSession_NoAgentDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if _, err := SyncPIAuthSession("no-such-session", filepath.Join(t.TempDir(), "auth.json")); !errors.Is(err, ErrNoPIAuthSession) {
		t.Errorf("SyncPIAuthSession = %v, want ErrNoPIAuthSession", err)
	}
}

// A host-mode pi holds the lock of the host file while it waits for the
// shared refresh lock, which the sync or its caller holds. So the sync does
// not wait for the host lock: it copies the new token at once, and the host
// pi then reads it (#3295 review). A file of zero bytes under a held lock
// is a write of pi in progress: the sync changes nothing, and the value
// stays new for the next sync.
func TestSyncPIAuthSession_DoesNotWaitForHostLock(t *testing.T) {
	e := newPIAuthEnv(t, piAuthTestHost)
	if err := os.Mkdir(e.hostAuth+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	e.writeSessionEntry(t, "anthropic", `{"type":"oauth","access":"access-1","refresh":"refresh-1","expires":5000}`)
	started := time.Now()
	res := e.sync(t)
	if took := time.Since(started); took > 500*time.Millisecond {
		t.Errorf("the sync waited %v for the lock of the host file", took)
	}
	if !slices.Equal(res.ToHost, []string{"anthropic"}) {
		t.Errorf("result = %+v, want anthropic to the host", res)
	}
	if got := readJSONFile(t, e.hostAuth)["anthropic"]["access"]; got != "access-1" {
		t.Errorf("host anthropic access = %v, want access-1", got)
	}
	if _, err := os.Stat(e.hostAuth + ".lock"); err != nil {
		t.Errorf("the sync removed the lock of the host pi: %v", err)
	}

	// pi truncated the file and did not yet write it.
	writeFile(t, e.hostAuth, "")
	e.writeSessionEntry(t, "anthropic", `{"type":"oauth","access":"access-2","refresh":"refresh-2","expires":6000}`)
	sessBefore := mustRead(t, e.sessionAuth())
	if _, err := SyncPIAuthSession(e.id, e.hostAuth); err == nil {
		t.Errorf("SyncPIAuthSession = nil error for an empty host file under a held lock")
	}
	if got := mustRead(t, e.hostAuth); got != "" {
		t.Errorf("host file = %q, want it unchanged", got)
	}
	if got := mustRead(t, e.sessionAuth()); got != sessBefore {
		t.Errorf("session file changed: %s", got)
	}

	// pi wrote the file and released the lock. The value is still new.
	writeFile(t, e.hostAuth, `{"anthropic":{"type":"oauth","access":"access-1","refresh":"refresh-1","expires":5000}}`)
	if err := os.Remove(e.hostAuth + ".lock"); err != nil {
		t.Fatal(err)
	}
	if res := e.sync(t); !slices.Equal(res.ToHost, []string{"anthropic"}) {
		t.Errorf("result = %+v, want anthropic to the host", res)
	}
	if got := readJSONFile(t, e.hostAuth)["anthropic"]["access"]; got != "access-2" {
		t.Errorf("host anthropic access = %v, want access-2", got)
	}
}

// A session cannot make the host file grow without a limit: no entry of
// another provider reaches it, the session adds the anthropic entry at most
// piAuthMaxNewProviders times in its life, a token of more than
// piAuthMaxTokenLen bytes is refused, and no write makes the host file
// larger than piAuthHostWriteLimit. An update of an existing entry still
// works after the limit.
func TestSyncPIAuthSession_LimitsHostGrowth(t *testing.T) {
	e := newPIAuthEnv(t, piAuthTestHost)
	hostBefore := mustRead(t, e.hostAuth)
	entry := `{"type":"oauth","access":"a","refresh":"r","expires":1}`
	m := map[string]json.RawMessage{}
	for i := 0; i < 500; i++ {
		m[fmt.Sprintf("p%04d", i)] = json.RawMessage(entry)
	}
	data, _ := json.Marshal(m)
	writeFile(t, e.sessionAuth(), string(data))

	if res := e.sync(t); len(res.ToHost) != 0 {
		t.Errorf("ToHost = %v, want none", res.ToHost)
	}
	if got := mustRead(t, e.hostAuth); got != hostBefore {
		t.Errorf("host file changed:\n%s", got)
	}

	e.loginAgain(t, piAuthMaxNewProviders)

	// The count is kept across a new start of the session.
	if _, err := PreparePIAuthSession(e.id, e.hostAuth); err != nil {
		t.Fatal(err)
	}
	e.removeHostEntry(t, "anthropic")
	e.sync(t)
	e.writeSessionEntry(t, "anthropic", entry)
	if res := e.sync(t); len(res.ToHost) != 0 || len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "already added") {
		t.Errorf("after a restart result = %+v, want the new entry refused", res)
	}
	if _, ok := readJSONFile(t, e.hostAuth)["anthropic"]; ok {
		t.Errorf("the host file has the anthropic entry after the limit")
	}

	// A login on the host gives the entry back. Then a long token is
	// refused.
	writeFile(t, e.hostAuth, piAuthTestHost)
	e.sync(t)
	long := strings.Repeat("a", piAuthMaxTokenLen+1)
	e.writeSessionEntry(t, "anthropic", `{"type":"oauth","access":"`+long+`","refresh":"r","expires":7000}`)
	if res := e.sync(t); len(res.ToHost) != 0 || len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "longer than") {
		t.Errorf("result = %+v, want the long token refused", res)
	}

	// An update of an existing entry works.
	e.writeSessionEntry(t, "anthropic", `{"type":"oauth","access":"access-9","refresh":"refresh-9","expires":9000}`)
	if res := e.sync(t); !slices.Equal(res.ToHost, []string{"anthropic"}) {
		t.Errorf("result = %+v, want anthropic to the host", res)
	}
	if fi, err := os.Stat(e.hostAuth); err != nil || fi.Size() > piAuthHostWriteLimit {
		t.Errorf("host file = (%v, %v), want at most %d bytes", fi, err, piAuthHostWriteLimit)
	}
}

// No write makes the host file larger than piAuthHostWriteLimit.
func TestSyncPIAuthSession_HostWriteLimit(t *testing.T) {
	big := `{"pad":{"type":"api_key","key":"` + strings.Repeat("k", piAuthHostWriteLimit-200) + `"},` +
		`"anthropic":{"type":"oauth","access":"access-0","refresh":"refresh-0","expires":1000}}`
	e := newPIAuthEnv(t, big)
	before := mustRead(t, e.hostAuth)
	long := strings.Repeat("a", 1000)
	e.writeSessionEntry(t, "anthropic", `{"type":"oauth","access":"`+long+`","refresh":"refresh-1","expires":5000}`)
	res := e.sync(t)
	if len(res.ToHost) != 0 || len(res.Refused) != 1 || !strings.Contains(res.Refused[0], "larger than") {
		t.Errorf("result = %+v, want the write refused", res)
	}
	if got := mustRead(t, e.hostAuth); got != before {
		t.Errorf("host file changed")
	}
}

// A session cannot reset its own sync state. The reset sequence of the
// review of #3312: use up the count of new providers, then write many
// entries so that the state grows past its read limit, then add a provider
// again. The state stays small, and the add is refused.
func TestSyncPIAuthSession_SessionCannotResetState(t *testing.T) {
	e := newPIAuthEnv(t, piAuthTestHost)
	e.loginAgain(t, piAuthMaxNewProviders)

	for round := 0; round < 3; round++ {
		m := map[string]json.RawMessage{}
		for i := 0; i < 1000; i++ {
			name := fmt.Sprintf("x%d-%d-%s", round, i, strings.Repeat("n", 1000))
			if i%2 == 0 {
				name = fmt.Sprintf("k%d-%d", round, i)
			}
			m[name] = json.RawMessage(`{"type":"api_key","key":"` + strings.Repeat("v", 20) + `"}`)
		}
		data, _ := json.Marshal(m)
		writeFile(t, e.sessionAuth(), string(data))
		e.sync(t)
	}
	statePath := filepath.Join(e.root, piAuthSyncStateName)
	if fi, err := os.Stat(statePath); err != nil || fi.Size() > 256<<10 {
		t.Errorf("sync state = (%v, %v), want at most 256 KiB", fi, err)
	}
	if _, err := readPIAuthSyncState(e.root); err != nil {
		t.Errorf("readPIAuthSyncState: %v", err)
	}

	e.removeHostEntry(t, "anthropic")
	e.sync(t)
	e.writeSessionEntry(t, "anthropic", `{"type":"oauth","access":"a","refresh":"r","expires":1}`)
	if res := e.sync(t); len(res.ToHost) != 0 {
		t.Errorf("ToHost = %v after the state flood, want none", res.ToHost)
	}
	if _, ok := readJSONFile(t, e.hostAuth)["anthropic"]; ok {
		t.Errorf("the host file has the anthropic entry after the state flood")
	}
}

// A sync state that cannot be read fails closed: the session adds no new
// provider.
func TestSyncPIAuthSession_BadStateFailsClosed(t *testing.T) {
	e := newPIAuthEnv(t, piAuthTestHost)
	e.removeHostEntry(t, "anthropic")
	e.sync(t)
	writeFile(t, filepath.Join(e.root, piAuthSyncStateName), `{"providers":`)
	e.writeSessionEntry(t, "anthropic", `{"type":"oauth","access":"a","refresh":"r","expires":1}`)
	res := e.sync(t)
	if len(res.ToHost) != 0 {
		t.Errorf("ToHost = %v with a bad state, want none", res.ToHost)
	}
	if !strings.Contains(strings.Join(res.Refused, " "), "cannot be read") {
		t.Errorf("Refused = %q, want the state reason", res.Refused)
	}
	st, err := readPIAuthSyncState(e.root)
	if err != nil || st.NewProviders != piAuthMaxNewProviders {
		t.Errorf("state after the sync = (%+v, %v), want NewProviders %d", st, err, piAuthMaxNewProviders)
	}
}

func TestRemovePIAuthSession(t *testing.T) {
	e := newPIAuthEnv(t, piAuthTestHost)
	RemovePIAuthSession(e.id)
	if _, err := os.Lstat(e.root); !os.IsNotExist(err) {
		t.Errorf("per-session auth dir after RemovePIAuthSession: %v", err)
	}
	RemovePIAuthSession(e.id)
	RemovePIAuthSession("../escape")
}

func TestLinkPIAgentDirEntries(t *testing.T) {
	hostDir, _ := piAgentDirFixture(t)
	writeFile(t, filepath.Join(hostDir, piAuthTmpPrefix+"x"), "{}")
	agentDir := t.TempDir()
	writeFile(t, filepath.Join(agentDir, "auth.json"), "{}")
	for i := 0; i < 2; i++ {
		if err := LinkPIAgentDirEntries(hostDir, agentDir); err != nil {
			t.Fatalf("LinkPIAgentDirEntries: %v", err)
		}
	}
	entries, _ := os.ReadDir(agentDir)
	var got []string
	for _, en := range entries {
		got = append(got, en.Name())
		if en.Name() == "auth.json" {
			if fi, _ := os.Lstat(filepath.Join(agentDir, en.Name())); !fi.Mode().IsRegular() {
				t.Errorf("auth.json is not the regular session file")
			}
			continue
		}
		if target, err := os.Readlink(filepath.Join(agentDir, en.Name())); err != nil || target != filepath.Join(hostDir, en.Name()) {
			t.Errorf("%s -> (%q, %v), want %s", en.Name(), target, err, filepath.Join(hostDir, en.Name()))
		}
	}
	want := []string{"auth.json", "bin", "extensions", "models-store.json", "notion-mcp-client.json", "notion-mcp-oauth.json", "sessions", "settings.json", "skills", "themes", "trust.json"}
	if !slices.Equal(got, want) {
		t.Errorf("agent dir = %v, want %v", got, want)
	}
}

// The Go form of the shared refresh lock is the form of refresh-lock.ts:
// a dir with an owner file that holds id, pid, and ts.
func TestTakePIRefreshLock(t *testing.T) {
	lockDir := t.TempDir()
	release, err := TakePIRefreshLock(lockDir, 0)
	if err != nil {
		t.Fatalf("TakePIRefreshLock: %v", err)
	}
	path := filepath.Join(lockDir, piRefreshLockName)
	var owner map[string]any
	if err := json.Unmarshal([]byte(mustRead(t, filepath.Join(path, "owner"))), &owner); err != nil {
		t.Fatal(err)
	}
	if _, ok := owner["id"].(string); !ok || owner["pid"] == nil || owner["ts"] == nil {
		t.Errorf("owner = %v, want id, pid, and ts", owner)
	}
	if _, err := TakePIRefreshLock(lockDir, 50*time.Millisecond); !errors.Is(err, ErrPIRefreshLockHeld) {
		t.Errorf("second TakePIRefreshLock = %v, want ErrPIRefreshLockHeld", err)
	}
	release()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("lock dir after release: %v", err)
	}

	// A lock dir of a dead holder is removed.
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(path, "owner"), `{"id":"dead","pid":1,"ts":0}`)
	old := time.Now().Add(-2 * piRefreshLockStale)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	release, err = TakePIRefreshLock(lockDir, 0)
	if err != nil {
		t.Fatalf("TakePIRefreshLock past a stale lock: %v", err)
	}
	// A release does not remove the lock of another holder.
	writeFile(t, filepath.Join(path, "owner"), `{"id":"other","pid":1,"ts":0}`)
	release()
	if _, err := os.Lstat(path); err != nil {
		t.Errorf("release removed the lock of another holder: %v", err)
	}
}

// A session cannot fill the sidecar log: one sync returns at most
// piAuthMaxRefused reasons, and a long provider name is cut.
func TestSyncPIAuthSession_CapsReasons(t *testing.T) {
	e := newPIAuthEnv(t, piAuthTestHost)
	m := map[string]json.RawMessage{}
	for i := 0; i < 3*piAuthMaxRefused; i++ {
		m[strings.Repeat("x", 1000)+string(rune('a'+i%26))+strings.Repeat("y", i)] = json.RawMessage(`{"type":"oauth","access":"a","refresh":"r","expires":1}`)
	}
	data, _ := json.Marshal(m)
	writeFile(t, e.sessionAuth(), string(data))

	res := e.sync(t)
	if len(res.Refused) != piAuthMaxRefused+1 {
		t.Errorf("len(Refused) = %d, want %d", len(res.Refused), piAuthMaxRefused+1)
	}
	for _, r := range res.Refused {
		if len(r) > 200 {
			t.Errorf("reason of %d bytes: %.80s...", len(r), r)
		}
	}
}
