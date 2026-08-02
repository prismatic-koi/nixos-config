package sidecar

// Tests for the host-API POST /usage/snapshot endpoint (issue #2538,
// parent #2537).
//
// Isolation: every test sets $XDG_STATE_HOME and $XDG_CONFIG_HOME to a
// t.TempDir(), so nothing is written under the real state or config
// directory and the nix-sandbox homeless-shelter build (HOME=/homeless-
// shelter) never sees a write attempt against $HOME.

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prismatic-koi/prism/internal/usage"
)

// usageTestEnv points the state and config directories at tempdirs and
// returns the resolved usage directory. It does NOT create the accounts
// store — call seedAccountPointer for that.
func usageTestEnv(t *testing.T) string {
	t.Helper()
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	// account.ResolvePaths falls back to $HOME/.pi/agent/auth.json when
	// PI_AUTH_JSON is unset. Pin it so the resolver cannot reach the real
	// home, even though this endpoint only ever reads accounts/current.
	t.Setenv("PI_AUTH_JSON", filepath.Join(t.TempDir(), "auth.json"))
	return filepath.Join(stateHome, "prism", "usage")
}

// seedAccountPointer writes $XDG_CONFIG_HOME/prism/accounts/current with the
// given name, mirroring what `prism account use` produces.
func seedAccountPointer(t *testing.T, name string) {
	t.Helper()
	accountsDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "prism", "accounts")
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatalf("mkdir accounts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(accountsDir, "current"), []byte(name+"\n"), 0o600); err != nil {
		t.Fatalf("write accounts/current: %v", err)
	}
}

// newUsageSidecar builds a worker-role sidecar. Worker role is deliberate:
// the capture hook runs inside worker sandboxes, so the endpoint must be
// reachable without coordinator privilege.
func newUsageSidecar(t *testing.T) *Sidecar {
	t.Helper()
	d := openTestDB(t)
	return newSidecarWithRole(t, "prism-test@usage-worker", "prism-test", "worker", d)
}

// fullBody is the request the extension sends for the worked example in
// issue #2537.
const fullBody = `{
  "unified_status": "allowed_warning",
  "representative_claim": "five_hour",
  "unified_reset": 1785634800,
  "windows": {
    "five_hour": {
      "status": "allowed_warning",
      "utilization": 0.94,
      "reset": 1785634800,
      "surpassed_threshold": 0.9
    },
    "seven_day": {
      "status": "allowed",
      "utilization": 0.42,
      "reset": 1786021200
    }
  },
  "fallback": { "status": "available", "percentage": 0.5 },
  "overage": { "status": "rejected", "disabled_reason": "out_of_credits" }
}`

func readSnapshot(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal %s (%s): %v", path, raw, err)
	}
	return out
}

// TestHostAPI_UsageSnapshot_WritesBothFiles covers the three functional ACs:
// the per-account file, the current.json copy, and the documented format.
func TestHostAPI_UsageSnapshot_WritesBothFiles(t *testing.T) {
	usageDir := usageTestEnv(t)
	seedAccountPointer(t, "work")
	sc := newUsageSidecar(t)

	rr := doHostAPI(t, sc, http.MethodPost, "/usage/snapshot", fullBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	var resp map[string]string
	decodeJSONBody(t, rr, &resp)
	if resp["account"] != "work" {
		t.Errorf("response account = %q, want %q", resp["account"], "work")
	}

	perAccount := readSnapshot(t, filepath.Join(usageDir, "work.json"))
	for _, key := range []string{
		"captured_at", "account", "unified_status", "representative_claim",
		"unified_reset", "windows", "fallback", "overage",
	} {
		if _, present := perAccount[key]; !present {
			t.Errorf("work.json is missing the documented key %q", key)
		}
	}
	if perAccount["account"] != "work" {
		t.Errorf("work.json account = %v, want %q", perAccount["account"], "work")
	}
	if perAccount["unified_status"] != "allowed_warning" {
		t.Errorf("unified_status = %v, want allowed_warning", perAccount["unified_status"])
	}
	if perAccount["representative_claim"] != "five_hour" {
		t.Errorf("representative_claim = %v, want five_hour", perAccount["representative_claim"])
	}

	windows, ok := perAccount["windows"].(map[string]any)
	if !ok {
		t.Fatalf("windows is not an object: %v", perAccount["windows"])
	}
	fiveHour, ok := windows["five_hour"].(map[string]any)
	if !ok {
		t.Fatalf("windows.five_hour is not an object: %v", windows["five_hour"])
	}
	// AC: utilization is stored as the RAW fraction.
	if fiveHour["utilization"] != 0.94 {
		t.Errorf("five_hour utilization = %v, want the raw fraction 0.94", fiveHour["utilization"])
	}
	// AC: reset fields are integer unix seconds.
	if fiveHour["reset"] != float64(1785634800) {
		t.Errorf("five_hour reset = %v, want 1785634800", fiveHour["reset"])
	}
	sevenDay, ok := windows["seven_day"].(map[string]any)
	if !ok {
		t.Fatalf("windows.seven_day is not an object: %v", windows["seven_day"])
	}
	if sevenDay["utilization"] != 0.42 {
		t.Errorf("seven_day utilization = %v, want 0.42", sevenDay["utilization"])
	}

	// current.json must be a byte-identical copy of the per-account file.
	rawAccount, err := os.ReadFile(filepath.Join(usageDir, "work.json"))
	if err != nil {
		t.Fatalf("read work.json: %v", err)
	}
	rawCurrent, err := os.ReadFile(filepath.Join(usageDir, usage.CurrentFileName))
	if err != nil {
		t.Fatalf("read current.json: %v", err)
	}
	if string(rawAccount) != string(rawCurrent) {
		t.Errorf("current.json must carry the same object:\n work.json: %s\ncurrent.json: %s", rawAccount, rawCurrent)
	}
}

// TestHostAPI_UsageSnapshot_CapturedAtIsSetHostSide proves the sidecar stamps
// captured_at rather than accepting it from the caller.
func TestHostAPI_UsageSnapshot_CapturedAtIsSetHostSide(t *testing.T) {
	usageDir := usageTestEnv(t)
	seedAccountPointer(t, "work")
	sc := newUsageSidecar(t)

	rr := doHostAPI(t, sc, http.MethodPost, "/usage/snapshot", fullBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	snap := readSnapshot(t, filepath.Join(usageDir, "work.json"))
	capturedAt, _ := snap["captured_at"].(string)
	if capturedAt == "" {
		t.Fatalf("captured_at is missing or empty: %v", snap)
	}
	if !strings.HasSuffix(capturedAt, "Z") || len(capturedAt) != len("2026-08-02T23:43:28Z") {
		t.Errorf("captured_at = %q, want RFC3339 UTC with second resolution", capturedAt)
	}
}

// TestHostAPI_UsageSnapshot_AccountResolvedHostSide covers the AC: the account
// name comes from accounts/current at write time, never from the caller.
func TestHostAPI_UsageSnapshot_AccountResolvedHostSide(t *testing.T) {
	usageDir := usageTestEnv(t)
	seedAccountPointer(t, "personal")
	sc := newUsageSidecar(t)

	rr := doHostAPI(t, sc, http.MethodPost, "/usage/snapshot", fullBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	if _, err := os.Stat(filepath.Join(usageDir, "personal.json")); err != nil {
		t.Errorf("expected personal.json to exist: %v", err)
	}
	snap := readSnapshot(t, filepath.Join(usageDir, usage.CurrentFileName))
	if snap["account"] != "personal" {
		t.Errorf("current.json account = %v, want personal", snap["account"])
	}
}

// TestHostAPI_UsageSnapshot_CallerCannotSupplyAccount is the security half of
// the previous test: a body that names an account is rejected outright rather
// than persisted under the caller's chosen name.
func TestHostAPI_UsageSnapshot_CallerCannotSupplyAccount(t *testing.T) {
	usageDir := usageTestEnv(t)
	seedAccountPointer(t, "work")
	sc := newUsageSidecar(t)

	body := `{"account":"attacker","unified_status":"allowed"}`
	rr := doHostAPI(t, sc, http.MethodPost, "/usage/snapshot", body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(filepath.Join(usageDir, "attacker.json")); err == nil {
		t.Error("a caller-supplied account name must not produce a file")
	}
}

// TestHostAPI_UsageSnapshot_RejectsUnknownField covers the security AC: an
// unknown field is rejected rather than persisted. The nested case matters
// most — DisallowUnknownFields must reach inside `windows`.
func TestHostAPI_UsageSnapshot_RejectsUnknownField(t *testing.T) {
	usageDir := usageTestEnv(t)
	seedAccountPointer(t, "work")
	sc := newUsageSidecar(t)

	cases := map[string]string{
		"top level":         `{"unified_status":"allowed","authorization":"Bearer sk-ant-secret"}`,
		"captured_at":       `{"unified_status":"allowed","captured_at":"1999-01-01T00:00:00Z"}`,
		"inside windows":    `{"windows":{"five_hour":{"utilization":0.5},"one_year":{"utilization":0.1}}}`,
		"inside a window":   `{"windows":{"five_hour":{"utilization":0.5,"secret":"x"}}}`,
		"inside fallback":   `{"fallback":{"status":"available","token":"sk-ant-secret"}}`,
		"non-unified field": `{"unified_status":"allowed","anthropic_ratelimit_requests_limit":40}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			rr := doHostAPI(t, sc, http.MethodPost, "/usage/snapshot", body)
			if rr.Code != http.StatusBadRequest {
				t.Errorf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
			}
			if !strings.Contains(rr.Body.String(), "unknown field") {
				t.Errorf("error body = %q, want it to mention \"unknown field\"", rr.Body.String())
			}
		})
	}

	// No file may have been created by any of the rejected bodies.
	if entries, err := os.ReadDir(usageDir); err == nil && len(entries) > 0 {
		t.Errorf("rejected requests must persist nothing, found %d entries", len(entries))
	}
}

// TestHostAPI_UsageSnapshot_NoAccountStore covers the AC: with no account
// store, the sidecar persists under "unknown" rather than failing.
func TestHostAPI_UsageSnapshot_NoAccountStore(t *testing.T) {
	usageDir := usageTestEnv(t)
	// Deliberately do NOT seed the accounts directory.
	sc := newUsageSidecar(t)

	rr := doHostAPI(t, sc, http.MethodPost, "/usage/snapshot", fullBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	snap := readSnapshot(t, filepath.Join(usageDir, usage.UnknownAccount+".json"))
	if snap["account"] != usage.UnknownAccount {
		t.Errorf("account = %v, want %q", snap["account"], usage.UnknownAccount)
	}
	current := readSnapshot(t, filepath.Join(usageDir, usage.CurrentFileName))
	if current["account"] != usage.UnknownAccount {
		t.Errorf("current.json account = %v, want %q", current["account"], usage.UnknownAccount)
	}
}

// TestHostAPI_UsageSnapshot_OmitsAbsentFields covers the "absent headers are
// omitted, not zero-filled" rule end to end through the endpoint.
func TestHostAPI_UsageSnapshot_OmitsAbsentFields(t *testing.T) {
	usageDir := usageTestEnv(t)
	seedAccountPointer(t, "work")
	sc := newUsageSidecar(t)

	// Only the 5h window reported anything, and only its utilization.
	body := `{"windows":{"five_hour":{"utilization":0}}}`
	rr := doHostAPI(t, sc, http.MethodPost, "/usage/snapshot", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	snap := readSnapshot(t, filepath.Join(usageDir, "work.json"))
	for _, key := range []string{"unified_status", "representative_claim", "unified_reset", "fallback", "overage"} {
		if _, present := snap[key]; present {
			t.Errorf("absent field %q must be omitted, got %v", key, snap[key])
		}
	}
	windows, _ := snap["windows"].(map[string]any)
	if _, present := windows["seven_day"]; present {
		t.Errorf("absent seven_day window must be omitted, got %v", windows["seven_day"])
	}
	fiveHour, _ := windows["five_hour"].(map[string]any)
	// An explicit zero must survive — that is the whole point of the rule.
	if got, present := fiveHour["utilization"]; !present || got != float64(0) {
		t.Errorf("five_hour utilization = %v (present=%v), want an explicit 0", got, present)
	}
	if _, present := fiveHour["status"]; present {
		t.Errorf("absent window status must be omitted, got %v", fiveHour["status"])
	}
}

// TestHostAPI_UsageSnapshot_RejectsEmptyBody proves an information-free
// request cannot clobber a good snapshot.
func TestHostAPI_UsageSnapshot_RejectsEmptyBody(t *testing.T) {
	usageDir := usageTestEnv(t)
	seedAccountPointer(t, "work")
	sc := newUsageSidecar(t)

	// Seed a good snapshot first.
	rr := doHostAPI(t, sc, http.MethodPost, "/usage/snapshot", fullBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("seed: status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}
	before, err := os.ReadFile(filepath.Join(usageDir, "work.json"))
	if err != nil {
		t.Fatalf("read seed snapshot: %v", err)
	}

	for _, body := range []string{`{}`, `{"windows":{}}`, `{"windows":{"five_hour":{}},"fallback":{},"overage":{}}`} {
		rr := doHostAPI(t, sc, http.MethodPost, "/usage/snapshot", body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("POST %s: status = %d, body = %q, want 400", body, rr.Code, rr.Body.String())
		}
	}

	after, err := os.ReadFile(filepath.Join(usageDir, "work.json"))
	if err != nil {
		t.Fatalf("re-read snapshot: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("an empty request must leave the existing snapshot byte-identical:\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestHostAPI_UsageSnapshot_RejectsFractionalReset proves the strict *int64
// decode: a fractional reset is refused rather than silently truncated on the
// server. The extension truncates before it sends, so this can only be a bug
// in a producer.
func TestHostAPI_UsageSnapshot_RejectsFractionalReset(t *testing.T) {
	usageTestEnv(t)
	seedAccountPointer(t, "work")
	sc := newUsageSidecar(t)

	rr := doHostAPI(t, sc, http.MethodPost, "/usage/snapshot",
		`{"unified_reset":1785634800.5}`)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, body = %q, want 400", rr.Code, rr.Body.String())
	}
}

// TestHostAPI_UsageSnapshot_MethodNotAllowed pins the method guard.
func TestHostAPI_UsageSnapshot_MethodNotAllowed(t *testing.T) {
	usageTestEnv(t)
	sc := newUsageSidecar(t)

	rr := doHostAPI(t, sc, http.MethodGet, "/usage/snapshot", "")
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /usage/snapshot: status = %d, want 405", rr.Code)
	}
}

// TestHostAPI_UsageSnapshot_WorkerAndCoordinatorBothAllowed pins the role
// policy: every session runs pi on the OAuth path, so every session is a
// legitimate producer.
func TestHostAPI_UsageSnapshot_WorkerAndCoordinatorBothAllowed(t *testing.T) {
	for _, role := range []string{"worker", "coordinator"} {
		t.Run(role, func(t *testing.T) {
			usageTestEnv(t)
			seedAccountPointer(t, "work")
			d := openTestDB(t)
			name := "prism-test@usage-" + role
			if role == "coordinator" {
				name = "prism-test@main"
			}
			sc := newSidecarWithRole(t, name, "prism-test", role, d)

			rr := doHostAPI(t, sc, http.MethodPost, "/usage/snapshot", fullBody)
			if rr.Code != http.StatusOK {
				t.Errorf("%s: status = %d, body = %q, want 200", role, rr.Code, rr.Body.String())
			}
		})
	}
}

// TestHostAPI_UsageSnapshot_FileModes covers the security AC at the endpoint
// level: 0600 files inside a 0700 directory.
func TestHostAPI_UsageSnapshot_FileModes(t *testing.T) {
	usageDir := usageTestEnv(t)
	seedAccountPointer(t, "work")
	sc := newUsageSidecar(t)

	rr := doHostAPI(t, sc, http.MethodPost, "/usage/snapshot", fullBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	di, err := os.Stat(usageDir)
	if err != nil {
		t.Fatalf("stat usage dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("usage dir mode = %04o, want 0700", got)
	}
	for _, name := range []string{"work.json", usage.CurrentFileName} {
		fi, err := os.Stat(filepath.Join(usageDir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", name, got)
		}
	}
}

// TestHostAPI_UsageSnapshot_NoCredentialInFilesOrLog covers the security AC:
// nothing credential-shaped reaches either snapshot file or the sidecar log.
// The request body deliberately carries only legal fields — the point is that
// the *output* is clean, and the unknown-field test above covers the case
// where a caller tries to smuggle a token in.
func TestHostAPI_UsageSnapshot_NoCredentialInFilesOrLog(t *testing.T) {
	usageDir := usageTestEnv(t)
	seedAccountPointer(t, "work")

	var logBuf strings.Builder
	d := openTestDB(t)
	sc := newSidecarWithRole(t, "prism-test@usage-worker", "prism-test", "worker", d)
	sc.cfg.Logger = log.New(&logBuf, "", 0)

	rr := doHostAPI(t, sc, http.MethodPost, "/usage/snapshot", fullBody)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200", rr.Code, rr.Body.String())
	}

	forbidden := []string{"authorization", "bearer", "sk-ant", "access_token", "refresh_token"}
	for _, name := range []string{"work.json", usage.CurrentFileName} {
		raw, err := os.ReadFile(filepath.Join(usageDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		lower := strings.ToLower(string(raw))
		for _, f := range forbidden {
			if strings.Contains(lower, f) {
				t.Errorf("%s contains the credential-shaped substring %q: %s", name, f, raw)
			}
		}
	}

	logLower := strings.ToLower(logBuf.String())
	for _, f := range forbidden {
		if strings.Contains(logLower, f) {
			t.Errorf("sidecar log contains the credential-shaped substring %q: %s", f, logBuf.String())
		}
	}
	// The log must still record the write, otherwise the assertion above is
	// vacuous.
	if !strings.Contains(logBuf.String(), "/usage/snapshot") {
		t.Errorf("expected a /usage/snapshot log line, got: %s", logBuf.String())
	}
}

// TestHostAPI_UsageSnapshot_AcceptsTheRefreshPayload pins the wire contract
// between the active refresh (issue #2541) and this endpoint.
//
// The refresh marshals a usage.SnapshotPayload and POSTs it here. The handler
// decodes with DisallowUnknownFields, so ONE extra or renamed field on that
// struct rejects the whole request and the refresh silently stops persisting.
// Neither package's own tests can catch that: this is the only place the two
// halves meet.
func TestHostAPI_UsageSnapshot_AcceptsTheRefreshPayload(t *testing.T) {
	usageDir := usageTestEnv(t)
	seedAccountPointer(t, "work")
	sc := newUsageSidecar(t)

	h := http.Header{}
	h.Set("anthropic-ratelimit-unified-status", "allowed_warning")
	h.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
	h.Set("anthropic-ratelimit-unified-reset", "1785634800")
	h.Set("anthropic-ratelimit-unified-5h-status", "allowed_warning")
	h.Set("anthropic-ratelimit-unified-5h-utilization", "0.94")
	h.Set("anthropic-ratelimit-unified-5h-reset", "1785634800")
	h.Set("anthropic-ratelimit-unified-5h-surpassed-threshold", "0.9")
	h.Set("anthropic-ratelimit-unified-7d-status", "allowed")
	h.Set("anthropic-ratelimit-unified-7d-utilization", "0.42")
	h.Set("anthropic-ratelimit-unified-7d-reset", "1786021200")
	h.Set("anthropic-ratelimit-unified-fallback", "available")
	h.Set("anthropic-ratelimit-unified-fallback-percentage", "0.5")
	h.Set("anthropic-ratelimit-unified-overage-status", "rejected")
	h.Set("anthropic-ratelimit-unified-overage-disabled-reason", "out_of_credits")

	payload := usage.ParseRateLimitHeaders(h)
	if payload == nil {
		t.Fatal("ParseRateLimitHeaders returned nil for the full header set")
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	rr := doHostAPI(t, sc, http.MethodPost, "/usage/snapshot", string(raw))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %q, want 200 — the refresh payload no longer matches the endpoint schema",
			rr.Code, rr.Body.String())
	}

	snap := readSnapshot(t, filepath.Join(usageDir, "work.json"))
	if snap["account"] != "work" {
		t.Errorf("account = %v, want work (resolved host-side, not from the payload)", snap["account"])
	}
	if snap["unified_status"] != "allowed_warning" {
		t.Errorf("unified_status = %v, want allowed_warning", snap["unified_status"])
	}
	windows, ok := snap["windows"].(map[string]any)
	if !ok {
		t.Fatalf("windows is not an object: %v", snap["windows"])
	}
	fiveHour, ok := windows["five_hour"].(map[string]any)
	if !ok {
		t.Fatalf("windows.five_hour is not an object: %v", windows["five_hour"])
	}
	if fiveHour["utilization"] != 0.94 {
		t.Errorf("five_hour utilization = %v, want 0.94", fiveHour["utilization"])
	}
	if fiveHour["reset"] != float64(1785634800) {
		t.Errorf("five_hour reset = %v, want an integer 1785634800", fiveHour["reset"])
	}
}
