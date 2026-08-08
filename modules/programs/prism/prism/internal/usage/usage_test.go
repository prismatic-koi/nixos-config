package usage

// Tests for the per-account rate-limit snapshot store (issue #2538,
// parent #2537).
//
// Isolation: every test that writes uses a t.TempDir() Store, and every test
// that resolves a path overrides $XDG_STATE_HOME / $XDG_CONFIG_HOME with
// t.Setenv. Nothing here touches the real state or config directory, so the
// nix-sandbox homeless-shelter build (HOME=/homeless-shelter) is unaffected.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

// fullSnapshot mirrors the worked example in issue #2537 verbatim so a change
// to the persisted shape fails loudly here.
func fullSnapshot() Snapshot {
	return Snapshot{
		CapturedAt:          "2026-08-02T23:43:28Z",
		Account:             "work",
		UnifiedStatus:       "allowed_warning",
		RepresentativeClaim: "five_hour",
		UnifiedReset:        i64(1785634800),
		Windows: &Windows{
			FiveHour: &Window{
				Status:             "allowed_warning",
				Utilization:        f64(0.94),
				Reset:              i64(1785634800),
				SurpassedThreshold: f64(0.9),
			},
			SevenDay: &Window{
				Status:      "allowed",
				Utilization: f64(0.42),
				Reset:       i64(1786021200),
			},
		},
		Fallback: &Fallback{Status: "available", Percentage: f64(0.5)},
		Overage:  &Overage{Status: "rejected", DisabledReason: "out_of_credits"},
	}
}

// TestWrite_FormatMatchesIssue2537 pins the exact on-disk JSON shape. Three
// downstream issues read this format; a silent rename here breaks all of them.
func TestWrite_FormatMatchesIssue2537(t *testing.T) {
	dir := t.TempDir()
	if err := NewStore(dir).Write(fullSnapshot()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "work.json"))
	if err != nil {
		t.Fatalf("read work.json: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal work.json (%s): %v", raw, err)
	}

	want := map[string]any{
		"captured_at":          "2026-08-02T23:43:28Z",
		"account":              "work",
		"unified_status":       "allowed_warning",
		"representative_claim": "five_hour",
		"unified_reset":        float64(1785634800),
		"windows": map[string]any{
			"five_hour": map[string]any{
				"status":              "allowed_warning",
				"utilization":         0.94,
				"reset":               float64(1785634800),
				"surpassed_threshold": 0.9,
			},
			"seven_day": map[string]any{
				"status":      "allowed",
				"utilization": 0.42,
				"reset":       float64(1786021200),
			},
		},
		"fallback": map[string]any{"status": "available", "percentage": 0.5},
		"overage": map[string]any{
			"status":          "rejected",
			"disabled_reason": "out_of_credits",
		},
	}

	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("persisted shape mismatch:\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
}

// TestWrite_OrganizationAndWorkspaceIDRoundTrip covers the functional ACs of
// issue #2713: both fields persist to disk and round-trip back through
// ReadAll, and a snapshot that carries neither omits both from the JSON
// rather than writing them as empty strings.
func TestWrite_OrganizationAndWorkspaceIDRoundTrip(t *testing.T) {
	dir := t.TempDir()
	snap := fullSnapshot()
	snap.OrganizationID = "1c5dbea6-0b0b-4750-bf6c-e7d38bc643d6"
	snap.WorkspaceID = "wrkspc_01DU7EeZcQ8gMsz6T4vvtwVD"
	if err := NewStore(dir).Write(snap); err != nil {
		t.Fatalf("Write: %v", err)
	}

	rows, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(rows) != 1 || rows[0].Snapshot == nil {
		t.Fatalf("rows = %+v", rows)
	}
	if got := rows[0].Snapshot.OrganizationID; got != snap.OrganizationID {
		t.Errorf("OrganizationID = %q, want %q", got, snap.OrganizationID)
	}
	if got := rows[0].Snapshot.WorkspaceID; got != snap.WorkspaceID {
		t.Errorf("WorkspaceID = %q, want %q", got, snap.WorkspaceID)
	}
}

// TestWrite_OrganizationAndWorkspaceIDOmittedWhenAbsent covers the edge-case
// AC: a snapshot with neither field set must not write them as
// present-and-empty.
func TestWrite_OrganizationAndWorkspaceIDOmittedWhenAbsent(t *testing.T) {
	dir := t.TempDir()
	if err := NewStore(dir).Write(fullSnapshot()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "work.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"organization_id", "workspace_id"} {
		if _, present := got[key]; present {
			t.Errorf("absent field %q must be omitted, but it is present in %s", key, raw)
		}
	}
}

// TestReadAll_PreExistingFileWithNoOrgFieldsLoadsCleanly covers the edge-case
// AC: a snapshot file written before this change (no org/workspace fields)
// must load without error and report both fields as absent, not error out.
func TestReadAll_PreExistingFileWithNoOrgFieldsLoadsCleanly(t *testing.T) {
	dir := t.TempDir()
	// Deliberately hand-written, mirroring a pre-#2713 on-disk file: no
	// organization_id / workspace_id keys at all.
	preExisting := `{
		"captured_at": "2026-08-02T23:43:28Z",
		"account": "work",
		"unified_status": "allowed_warning"
	}`
	if err := os.WriteFile(filepath.Join(dir, "work.json"), []byte(preExisting), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	rows, err := ReadAll(dir)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(rows) != 1 || rows[0].Snapshot == nil {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].ReadErr != nil {
		t.Errorf("ReadErr = %v, want nil for a valid pre-existing file", rows[0].ReadErr)
	}
	if rows[0].Snapshot.OrganizationID != "" {
		t.Errorf("OrganizationID = %q, want absent", rows[0].Snapshot.OrganizationID)
	}
	if rows[0].Snapshot.WorkspaceID != "" {
		t.Errorf("WorkspaceID = %q, want absent", rows[0].Snapshot.WorkspaceID)
	}
}

// TestWrite_UtilizationStaysRawFraction guards the "do NOT multiply by 100"
// rule: the display legs (#2540) scale, the store does not.
func TestWrite_UtilizationStaysRawFraction(t *testing.T) {
	dir := t.TempDir()
	snap := fullSnapshot()
	if err := NewStore(dir).Write(snap); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "work.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(raw), `"utilization": 0.94`) {
		t.Errorf("expected the raw fraction 0.94 on disk, got:\n%s", raw)
	}
	if strings.Contains(string(raw), "94.0") || strings.Contains(string(raw), `"utilization": 94`) {
		t.Errorf("utilization looks scaled to a percentage:\n%s", raw)
	}
}

// TestWrite_AbsentFieldsAreOmitted is the "a reader can tell not-present from
// zero" guarantee. A nil pointer must vanish from the JSON; an explicit zero
// must survive.
func TestWrite_AbsentFieldsAreOmitted(t *testing.T) {
	dir := t.TempDir()
	snap := Snapshot{
		CapturedAt: "2026-08-02T23:43:28Z",
		Account:    "work",
		Windows: &Windows{
			// Utilization explicitly zero; every sibling absent.
			FiveHour: &Window{Utilization: f64(0)},
		},
	}
	if err := NewStore(dir).Write(snap); err != nil {
		t.Fatalf("Write: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "work.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"unified_status", "representative_claim", "unified_reset", "fallback", "overage"} {
		if _, present := got[key]; present {
			t.Errorf("absent field %q must be omitted, but it is present in %s", key, raw)
		}
	}

	windows, _ := got["windows"].(map[string]any)
	fiveHour, _ := windows["five_hour"].(map[string]any)
	util, present := fiveHour["utilization"]
	if !present {
		t.Fatalf("an explicit zero utilization must be persisted, got %s", raw)
	}
	if util != float64(0) {
		t.Errorf("utilization = %v, want 0", util)
	}
	if _, present := fiveHour["status"]; present {
		t.Errorf("absent window status must be omitted, got %s", raw)
	}
	if _, present := windows["seven_day"]; present {
		t.Errorf("absent seven_day window must be omitted, got %s", raw)
	}
}

// TestWrite_CurrentJSONMirrorsAccountFile covers the AC: current.json carries
// the active account name and the same snapshot object.
func TestWrite_CurrentJSONMirrorsAccountFile(t *testing.T) {
	dir := t.TempDir()
	if err := NewStore(dir).Write(fullSnapshot()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	perAccount, err := os.ReadFile(filepath.Join(dir, "work.json"))
	if err != nil {
		t.Fatalf("read work.json: %v", err)
	}
	current, err := os.ReadFile(filepath.Join(dir, CurrentFileName))
	if err != nil {
		t.Fatalf("read current.json: %v", err)
	}
	if string(perAccount) != string(current) {
		t.Errorf("current.json must be a byte-identical copy:\n work.json: %s\ncurrent.json: %s", perAccount, current)
	}

	var got Snapshot
	if err := json.Unmarshal(current, &got); err != nil {
		t.Fatalf("unmarshal current.json: %v", err)
	}
	if got.Account != "work" {
		t.Errorf("current.json account = %q, want %q", got.Account, "work")
	}
}

// TestWrite_FileAndDirModes covers the security AC: files 0600, directory 0700.
func TestWrite_FileAndDirModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	// Start from a directory that does not exist so Write must create it.
	// The already-exists path is covered by
	// TestWrite_TightensPreExistingLooseDirMode.
	dir := filepath.Join(t.TempDir(), "usage")
	if err := NewStore(dir).Write(fullSnapshot()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("usage dir mode = %04o, want 0700", got)
	}

	for _, name := range []string{"work.json", CurrentFileName} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", name, got)
		}
	}
}

// TestWrite_TightensPreExistingLooseDirMode proves the chmod is unconditional:
// a usage/ directory that already exists with a loose mode is tightened.
func TestWrite_TightensPreExistingLooseDirMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	dir := filepath.Join(t.TempDir(), "usage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("pre-create dir: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("pre-chmod dir: %v", err)
	}

	if err := NewStore(dir).Write(fullSnapshot()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("pre-existing usage dir mode = %04o, want 0700", got)
	}
}

// TestWrite_LeavesNoTempFiles proves the atomic write cleans up after itself:
// after two writes the directory holds exactly the two snapshot files.
func TestWrite_LeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Write(fullSnapshot()); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	if err := store.Write(fullSnapshot()); err != nil {
		t.Fatalf("Write 2: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("tempfile left behind: %s", e.Name())
		}
	}
	if len(names) != 2 {
		t.Errorf("directory holds %v, want exactly [current.json work.json]", names)
	}
}

// TestWrite_SecondWriteReplacesFirst proves the rename overwrites rather than
// appending or failing on an existing file.
func TestWrite_SecondWriteReplacesFirst(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	first := fullSnapshot()
	if err := store.Write(first); err != nil {
		t.Fatalf("Write 1: %v", err)
	}
	second := fullSnapshot()
	second.UnifiedStatus = "allowed"
	second.Windows.FiveHour.Utilization = f64(0.11)
	if err := store.Write(second); err != nil {
		t.Fatalf("Write 2: %v", err)
	}

	var got Snapshot
	raw, err := os.ReadFile(filepath.Join(dir, "work.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.UnifiedStatus != "allowed" {
		t.Errorf("unified_status = %q, want the second write's value %q", got.UnifiedStatus, "allowed")
	}
	if got.Windows == nil || got.Windows.FiveHour == nil || got.Windows.FiveHour.Utilization == nil ||
		*got.Windows.FiveHour.Utilization != 0.11 {
		t.Errorf("five_hour utilization not replaced by the second write: %s", raw)
	}
}

func TestSanitizeAccountName(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"work", "work"},
		{"personal", "personal"},
		{"work-prod", "work-prod"},
		{"work.prod", "work.prod"},
		{"  work  ", "work"},
		// Rejected shapes fall back to "unknown".
		{"", UnknownAccount},
		{"   ", UnknownAccount},
		{"..", UnknownAccount},
		{".", UnknownAccount},
		{".hidden", UnknownAccount},
		{"../escape", UnknownAccount},
		{"with/slash", UnknownAccount},
		{`with\backslash`, UnknownAccount},
		{"current", UnknownAccount},
		{"nul\x00byte", UnknownAccount},
		{"new\nline", UnknownAccount},
		{strings.Repeat("a", maxAccountNameLen+1), UnknownAccount},
		// Exactly at the limit is allowed.
		{strings.Repeat("a", maxAccountNameLen), strings.Repeat("a", maxAccountNameLen)},
	}
	for _, tc := range cases {
		if got := SanitizeAccountName(tc.in); got != tc.want {
			t.Errorf("SanitizeAccountName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestWrite_SanitisesAccountName proves a path-traversing account name cannot
// escape the usage directory, and that the persisted `account` field agrees
// with the filename.
func TestWrite_SanitisesAccountName(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "usage")

	snap := fullSnapshot()
	snap.Account = "../escape"
	if err := NewStore(dir).Write(snap); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, err := os.Stat(filepath.Join(root, "escape.json")); err == nil {
		t.Fatalf("path traversal escaped the usage directory: %s exists", filepath.Join(root, "escape.json"))
	}
	raw, err := os.ReadFile(filepath.Join(dir, UnknownAccount+".json"))
	if err != nil {
		t.Fatalf("expected the snapshot under %s.json: %v", UnknownAccount, err)
	}
	var got Snapshot
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Account != UnknownAccount {
		t.Errorf("persisted account = %q, want %q — the field must agree with the filename", got.Account, UnknownAccount)
	}
}

func TestWrite_EmptyDirIsAnError(t *testing.T) {
	if err := NewStore("").Write(fullSnapshot()); err == nil {
		t.Error("Write with an empty Dir must return an error")
	}
}

func TestDefaultDir_PrefersXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-state-fixture")
	got, err := DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir: %v", err)
	}
	want := filepath.Join("/tmp/xdg-state-fixture", "prism", "usage")
	if got != want {
		t.Errorf("DefaultDir() = %q, want %q", got, want)
	}
}

func TestDefaultDir_FallsBackToHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/tmp/home-fixture")
	got, err := DefaultDir()
	if err != nil {
		t.Fatalf("DefaultDir: %v", err)
	}
	want := filepath.Join("/tmp/home-fixture", ".local", "state", "prism", "usage")
	if got != want {
		t.Errorf("DefaultDir() = %q, want %q", got, want)
	}
}

// TestDirForHome_PrefersXDGStateHome pins the resolution ORDER of the helper
// the sandbox mount builders share with DefaultDir (issue #2572): when
// $XDG_STATE_HOME is set it wins outright and the home argument is ignored.
// A regression here would make the sandbox bind a different directory from
// the one the writer and pi/extensions/prism.ts resolve.
func TestDirForHome_PrefersXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-state-fixture")
	got := DirForHome("/tmp/home-fixture")
	want := filepath.Join("/tmp/xdg-state-fixture", "prism", "usage")
	if got != want {
		t.Errorf("DirForHome() = %q, want %q", got, want)
	}
}

func TestDirForHome_FallsBackToHomeArgument(t *testing.T) {
	// The home ARGUMENT wins over $HOME: container callers resolve the host
	// home once and pass it down, and their fixtures drive it directly.
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/tmp/env-home-must-not-be-used")
	got := DirForHome("/tmp/home-fixture")
	want := filepath.Join("/tmp/home-fixture", ".local", "state", "prism", "usage")
	if got != want {
		t.Errorf("DirForHome() = %q, want %q", got, want)
	}
}

// TestDirForHome_UnresolvableReturnsEmpty covers the caller contract: with
// neither $XDG_STATE_HOME nor a home argument there is nothing to resolve,
// and the empty string tells the sandbox mount builders to skip the entry
// rather than emit a bind rooted at "/".
func TestDirForHome_UnresolvableReturnsEmpty(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	if got := DirForHome(""); got != "" {
		t.Errorf("DirForHome(\"\") = %q, want \"\"", got)
	}
}

// TestDirForHome_MatchesDefaultDir proves the two entry points cannot drift:
// DefaultDir must be DirForHome(os.UserHomeDir()) for both resolution
// branches. prism.ts's usageSnapshotPath() mirrors the same two branches.
func TestDirForHome_MatchesDefaultDir(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stateEnv string
	}{
		{"xdg-state-home-set", "/tmp/xdg-state-fixture"},
		{"xdg-state-home-empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", tc.stateEnv)
			t.Setenv("HOME", "/tmp/home-fixture")
			home, err := os.UserHomeDir()
			if err != nil {
				t.Fatalf("UserHomeDir: %v", err)
			}
			want, err := DefaultDir()
			if err != nil {
				t.Fatalf("DefaultDir: %v", err)
			}
			if got := DirForHome(home); got != want {
				t.Errorf("DirForHome(%q) = %q, DefaultDir() = %q — the two must agree", home, got, want)
			}
		})
	}
}

// TestCurrentAccountName_NoAccountStore covers the AC: when no account store
// exists, the resolver reports "unknown" rather than failing.
func TestCurrentAccountName_NoAccountStore(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PI_AUTH_JSON", filepath.Join(t.TempDir(), "auth.json"))
	if got := CurrentAccountName(); got != UnknownAccount {
		t.Errorf("CurrentAccountName() = %q, want %q", got, UnknownAccount)
	}
}

func TestCurrentAccountName_ReadsPointerFile(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("PI_AUTH_JSON", filepath.Join(t.TempDir(), "auth.json"))

	accountsDir := filepath.Join(cfg, "prism", "accounts")
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatalf("mkdir accounts: %v", err)
	}
	// The pointer file is written with a trailing newline by
	// internal/account; the resolver must trim it.
	if err := os.WriteFile(filepath.Join(accountsDir, "current"), []byte("work\n"), 0o600); err != nil {
		t.Fatalf("write current: %v", err)
	}

	if got := CurrentAccountName(); got != "work" {
		t.Errorf("CurrentAccountName() = %q, want %q", got, "work")
	}
}

// TestCurrentAccountName_MalformedPointerFile is the defence-in-depth case:
// a hand-edited pointer file must not steer a write out of the usage dir.
func TestCurrentAccountName_MalformedPointerFile(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("PI_AUTH_JSON", filepath.Join(t.TempDir(), "auth.json"))

	accountsDir := filepath.Join(cfg, "prism", "accounts")
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatalf("mkdir accounts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(accountsDir, "current"), []byte("../../escape\n"), 0o600); err != nil {
		t.Fatalf("write current: %v", err)
	}

	if got := CurrentAccountName(); got != UnknownAccount {
		t.Errorf("CurrentAccountName() = %q, want %q", got, UnknownAccount)
	}
}

// TestCurrentAccountName_EmptyPointerFile covers a pointer file that exists
// but holds only whitespace.
func TestCurrentAccountName_EmptyPointerFile(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("PI_AUTH_JSON", filepath.Join(t.TempDir(), "auth.json"))

	accountsDir := filepath.Join(cfg, "prism", "accounts")
	if err := os.MkdirAll(accountsDir, 0o700); err != nil {
		t.Fatalf("mkdir accounts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(accountsDir, "current"), []byte("  \n"), 0o600); err != nil {
		t.Fatalf("write current: %v", err)
	}

	if got := CurrentAccountName(); got != UnknownAccount {
		t.Errorf("CurrentAccountName() = %q, want %q", got, UnknownAccount)
	}
}

func TestFormatCapturedAt(t *testing.T) {
	at := time.Date(2026, 8, 2, 23, 43, 28, 500_000_000, time.FixedZone("NZST", 12*3600))
	if got, want := FormatCapturedAt(at), "2026-08-02T11:43:28Z"; got != want {
		t.Errorf("FormatCapturedAt = %q, want %q (UTC, second resolution)", got, want)
	}
}

// TestSnapshot_NoTokenShapedFields is a structural guard for the security AC:
// the persisted object must carry no credential-shaped key. It marshals a
// fully-populated snapshot and asserts none of the forbidden keys appear.
func TestSnapshot_NoTokenShapedFields(t *testing.T) {
	raw, err := json.Marshal(fullSnapshot())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	lower := strings.ToLower(string(raw))
	for _, forbidden := range []string{"authorization", "bearer", "token", "access", "refresh", "api_key", "apikey"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("persisted snapshot contains the credential-shaped substring %q: %s", forbidden, raw)
		}
	}
}
