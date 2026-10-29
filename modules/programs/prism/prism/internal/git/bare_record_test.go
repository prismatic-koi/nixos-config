package git

// bare_record_test.go — OpenBare runs host git only in a .bare that prism
// made (issue #3333).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// recordBareForTest records gitDir, a .bare that a fixture made with plain
// git, as a .bare that prism made.
func recordBareForTest(t *testing.T, gitDir string) {
	t.Helper()
	if err := recordBare(gitDir); err != nil {
		t.Fatalf("record %s: %v", gitDir, err)
	}
}

// useBareRecord makes OpenBare use the bare repo record for the rest of t,
// as on the host, with a new state dir and no session start records. It
// returns the path of the record file.
func useBareRecord(t *testing.T) string {
	t.Helper()
	t.Setenv("PRISM_HOST_API", "")
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	prev := allowUnrecordedBare.Swap(false)
	t.Cleanup(func() { allowUnrecordedBare.Store(prev) })
	setSessionStarts(t)
	return filepath.Join(state, "prism", bareRecordName)
}

// setSessionStarts makes the session start records of prism.db be starts
// for the rest of t. internal/db sets the real reader, and this package
// cannot import internal/db.
func setSessionStarts(t *testing.T, starts ...SessionStart) {
	t.Helper()
	prev := sessionStartSource.Load()
	read := func(string) ([]SessionStart, error) { return starts, nil }
	sessionStartSource.Store(&read)
	t.Cleanup(func() { sessionStartSource.Store(prev) })
}

// prismLayout makes root in the prism layout with plain git: root/.bare and
// a .git file that names it.
func prismLayout(t *testing.T, origin, root string) {
	t.Helper()
	if out, err := exec.Command("git", "clone", "-q", "--bare", origin, filepath.Join(root, ".bare")).CombinedOutput(); err != nil {
		t.Fatalf("clone %s: %v\n%s", root, err, out)
	}
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: ./.bare\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// plainClone makes root a plain clone of origin.
func plainClone(t *testing.T, origin, root string) {
	t.Helper()
	if out, err := exec.Command("git", "clone", "-q", origin, root).CombinedOutput(); err != nil {
		t.Fatalf("clone %s: %v\n%s", root, err, out)
	}
}

// readRecordPaths returns the path of each entry in the record file.
func readRecordPaths(t *testing.T, file string) []string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	var rec bareRecordFile
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("parse the record: %v", err)
	}
	var paths []string
	for _, e := range rec.Repos {
		paths = append(paths, e.Path)
	}
	return paths
}

func mustResolve(t *testing.T, p string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func hasPath(paths []string, p string) bool {
	want, err := filepath.EvalSymlinks(p)
	if err != nil {
		want = p
	}
	for _, q := range paths {
		if q == want {
			return true
		}
	}
	return false
}

// plantBare makes <root>/.bare with plain git, as a sandboxed session of a
// plain clone can do in its own root. The .bare has a smudge filter that
// info/attributes selects, and a config hook for post-checkout with a name
// that prism does not know. A hook.post-checkout.command entry defeats the
// pin hook.post-checkout.enabled = false (#3349). It replaces the .git dir of root with a
// .git file, so the root has the layout of a prism project. Each payload
// appends one line to marker.
func plantBare(t *testing.T, origin, root, marker string) string {
	t.Helper()
	bareDir := filepath.Join(root, ".bare")
	if out, err := exec.Command("git", "clone", "-q", "--bare", origin, bareDir).CombinedOutput(); err != nil {
		t.Fatalf("plant .bare: %v\n%s", err, out)
	}
	cfg, err := os.OpenFile(filepath.Join(bareDir, "config"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(cfg, "[filter \"evil\"]\n\tsmudge = \"echo smudge >> '%s'; cat\"\n", marker)
	fmt.Fprintf(cfg, "[hook \"friendly\"]\n\tevent = post-checkout\n\tcommand = \"echo hook >> '%s'\"\n", marker)
	fmt.Fprintf(cfg, "[hook \"post-checkout\"]\n\tcommand = true\n")
	if err := cfg.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bareDir, "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bareDir, "info", "attributes"), []byte("* filter=evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: ./.bare\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return bareDir
}

func takeMarker(marker string) string {
	data, _ := os.ReadFile(marker)
	_ = os.Remove(marker)
	return strings.TrimSpace(string(data))
}

// TestOpenBare_RefusesBareThatPrismDidNotMake is AC 1 of #3333. A sandboxed
// root session of a plain clone in ~/code can write its root. It makes a
// .bare there with a smudge filter that info/attributes selects and a config
// hook. The control shows that a worktree add with the env of a pinned Repo
// runs both payloads: neither GIT_ATTR_SOURCE nor the command-line config
// stops them. A later `prism spawn` runs that worktree add on the host. So
// OpenBare and each host git command for the root must refuse it.
func TestOpenBare_RefusesBareThatPrismDidNotMake(t *testing.T) {
	isolateHostGitConfig(t)
	useBareRecord(t)
	code := filepath.Join(t.TempDir(), "code")
	if err := os.MkdirAll(code, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, []string{code}, nil)
	origin := newOrigin(t)

	made := filepath.Join(code, "made")
	if err := CloneWorktree(origin, made, func(string) {}); err != nil {
		t.Fatalf("CloneWorktree: %v", err)
	}

	plain := filepath.Join(code, "plain")
	if out, err := exec.Command("git", "clone", "-q", origin, plain).CombinedOutput(); err != nil {
		t.Fatalf("clone plain: %v\n%s", err, out)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	bareDir := plantBare(t, origin, plain, marker)

	control := Repo{gitDir: bareDir, commonDir: bareDir, dir: bareDir}
	_, _ = control.CombinedOutput("worktree", "add", filepath.Join(t.TempDir(), "control"), "main")
	got := takeMarker(marker)
	if !strings.Contains(got, "smudge") || !strings.Contains(got, "hook") {
		t.Fatalf("control: a pinned worktree add in the planted root ran %q, want the smudge filter and the hook; the plant does not work", got)
	}

	if _, err := OpenBare(plain); !errors.Is(err, ErrNotPrismBare) {
		t.Errorf("OpenBare(%s) = %v, want ErrNotPrismBare", plain, err)
	}
	if _, err := CreateWorktree(plain, "spawned"); !errors.Is(err, ErrNotPrismBare) {
		t.Errorf("CreateWorktree = %v, want ErrNotPrismBare", err)
	}
	if err := FetchRemote(plain); err == nil {
		t.Error("FetchRemote: want a refusal")
	}
	if wts := Worktrees(plain); wts != nil {
		t.Errorf("Worktrees = %v, want nil", wts)
	}
	if _, err := OpenDir(plain); !errors.Is(err, ErrNotPrismBare) {
		t.Errorf("OpenDir = %v, want ErrNotPrismBare", err)
	}
	if _, err := EnvForDir(plain, nil); !errors.Is(err, ErrNotPrismBare) {
		t.Errorf("EnvForDir = %v, want ErrNotPrismBare", err)
	}
	if _, err := Stat(plain); err == nil {
		t.Error("Stat: want a refusal")
	}
	if got := takeMarker(marker); got != "" {
		t.Errorf("a host git command for the planted root ran the payload: %q", got)
	}

	if r, err := OpenBare(made); err != nil {
		t.Errorf("OpenBare(%s), a root that prism made: %v", made, err)
	} else if branch, err := r.Output("symbolic-ref", "--short", "HEAD"); err != nil || branch != "main" {
		t.Errorf("git in OpenBare(%s): HEAD = %q, %v; want main", made, branch, err)
	}
}

// TestBareRecord_StartsFromSessionStarts (#3364): with no record file, prism
// starts the record from the session start records of prism.db, not from the
// .bare dirs on disk. A .bare goes in the record only if a session started in
// a worktree of its root, and no session other than a meta-session started
// in the root or above it. A root whose .git is a directory is a plain
// clone, and the start skips its .bare. A .bare made after the start is not
// in the record.
func TestBareRecord_StartsFromSessionStarts(t *testing.T) {
	isolateHostGitConfig(t)
	recordFile := useBareRecord(t)
	base := t.TempDir()
	code := filepath.Join(base, "code")
	if err := os.MkdirAll(code, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, []string{code}, nil)
	origin := newOrigin(t)

	// Projects in the prism layout, made with plain git.
	used := filepath.Join(code, "used")
	idle := filepath.Join(code, "idle")
	converted := filepath.Join(code, "converted")
	inBare := filepath.Join(code, "inbare")
	for _, root := range []string{used, idle, converted, inBare} {
		prismLayout(t, origin, root)
	}
	// A plain clone with a .bare next to its .git dir.
	plain := filepath.Join(code, "plain")
	plainClone(t, origin, plain)
	if out, err := exec.Command("git", "clone", "-q", "--bare", origin, filepath.Join(plain, ".bare")).CombinedOutput(); err != nil {
		t.Fatalf("plant plain/.bare: %v\n%s", err, out)
	}
	setSessionStarts(t,
		SessionStart{Name: "used@main", Worktree: filepath.Join(used, "main")},
		// A meta-session runs no agent, so its dir above the roots does
		// not count.
		SessionStart{Name: "scratchpad", Worktree: base, Open: true},
		// The root session of a plain clone ran in converted. It could
		// write the root, so the worktree session after it does not count.
		SessionStart{Name: "converted", Worktree: converted},
		SessionStart{Name: "converted@main", Worktree: filepath.Join(converted, "main")},
		// A dir in .bare is not a worktree.
		SessionStart{Name: "inbare@x", Worktree: filepath.Join(inBare, ".bare", "worktrees", "x")},
		SessionStart{Name: "plain@main", Worktree: filepath.Join(plain, "main")},
	)

	if _, err := os.Stat(recordFile); !os.IsNotExist(err) {
		t.Fatalf("the record exists before the first OpenBare: %v", err)
	}
	if _, err := OpenBare(used); err != nil {
		t.Errorf("OpenBare(%s): %v", used, err)
	}
	paths := readRecordPaths(t, recordFile)
	if !hasPath(paths, filepath.Join(used, ".bare")) || len(paths) != 1 {
		t.Errorf("record = %v, want only %s/.bare", paths, used)
	}
	for _, root := range []string{idle, converted, inBare, plain} {
		_, err := OpenBare(root)
		if !errors.Is(err, ErrNotPrismBare) {
			t.Errorf("OpenBare(%s) = %v, want ErrNotPrismBare", root, err)
			continue
		}
		if want := "prism confirm-bare " + mustResolve(t, root); !strings.Contains(err.Error(), want) {
			t.Errorf("OpenBare(%s) = %v, want the step %q", root, err, want)
		}
	}

	late := filepath.Join(code, "late")
	marker := filepath.Join(t.TempDir(), "marker")
	plainClone(t, origin, late)
	plantBare(t, origin, late, marker)
	if _, err := OpenBare(late); !errors.Is(err, ErrNotPrismBare) {
		t.Errorf("OpenBare(%s) = %v, want ErrNotPrismBare", late, err)
	}
	if got := takeMarker(marker); got != "" {
		t.Errorf("OpenBare ran the payload: %q", got)
	}
}

// TestBareRecord_StartRefusesBarePlantedBefore is AC 1 of #3364. The
// sandboxed root session of a plain clone plants a .bare in its root before
// the record exists, and replaces its .git dir with a .git file. The control
// shows that a pinned worktree add in that .bare runs the payload. The start
// of the record must not record the plant, so OpenBare and CreateWorktree,
// the step of /spawn, refuse it and no payload runs.
//
// Each case has a start record of a worktree session in the root, as from a
// /spawn before the update. In the first case the root session runs. In the
// second case it ended. In the third case the root session started in the
// project location, a dir above the root. In the fourth case its start dir
// is a symlink to the root, so only the compare by file identity finds it. In
// the fifth case prism.db has no record of the root session, and no start
// record of a worktree session.
func TestBareRecord_StartRefusesBarePlantedBefore(t *testing.T) {
	for _, tc := range []struct {
		name   string
		starts func(t *testing.T, code, root string) []SessionStart
	}{
		{"root session runs", func(_ *testing.T, _, root string) []SessionStart {
			return []SessionStart{
				{Name: "plain", Worktree: root, Open: true},
				{Name: "plain@spawned", Worktree: filepath.Join(root, "spawned")},
			}
		}},
		{"root session ended", func(_ *testing.T, _, root string) []SessionStart {
			return []SessionStart{
				{Name: "plain", Worktree: root},
				{Name: "plain@spawned", Worktree: filepath.Join(root, "spawned")},
			}
		}},
		{"session above the root", func(_ *testing.T, code, root string) []SessionStart {
			return []SessionStart{
				{Name: "code", Worktree: code, Open: true},
				{Name: "plain@spawned", Worktree: filepath.Join(root, "spawned")},
			}
		}},
		{"root session through a symlink", func(t *testing.T, _, root string) []SessionStart {
			alias := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(root, alias); err != nil {
				t.Fatal(err)
			}
			return []SessionStart{
				{Name: "plain", Worktree: alias, Open: true},
				{Name: "plain@spawned", Worktree: filepath.Join(root, "spawned")},
			}
		}},
		{"no start record", func(*testing.T, string, string) []SessionStart { return nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateHostGitConfig(t)
			recordFile := useBareRecord(t)
			code := filepath.Join(t.TempDir(), "code")
			if err := os.MkdirAll(code, 0o700); err != nil {
				t.Fatal(err)
			}
			writeProjectConfig(t, []string{code}, nil)
			origin := newOrigin(t)
			root := filepath.Join(code, "plain")
			plainClone(t, origin, root)
			marker := filepath.Join(t.TempDir(), "marker")
			bareDir := plantBare(t, origin, root, marker)
			setSessionStarts(t, tc.starts(t, code, root)...)

			control := Repo{gitDir: bareDir, commonDir: bareDir, dir: bareDir}
			_, _ = control.CombinedOutput("worktree", "add", filepath.Join(t.TempDir(), "control"), "main")
			if got := takeMarker(marker); !strings.Contains(got, "smudge") {
				t.Fatalf("control: a pinned worktree add in the plant ran %q, want the smudge filter; the plant does not work", got)
			}

			if _, err := os.Stat(recordFile); !os.IsNotExist(err) {
				t.Fatalf("the record exists before the first OpenBare: %v", err)
			}
			if _, err := OpenBare(root); !errors.Is(err, ErrNotPrismBare) {
				t.Errorf("OpenBare(%s) = %v, want ErrNotPrismBare", root, err)
			}
			if paths := readRecordPaths(t, recordFile); hasPath(paths, bareDir) {
				t.Errorf("record = %v, want no entry for the plant %s", paths, bareDir)
			}
			if _, err := CreateWorktree(root, "spawned2"); !errors.Is(err, ErrNotPrismBare) {
				t.Errorf("CreateWorktree = %v, want ErrNotPrismBare", err)
			}
			if got := takeMarker(marker); got != "" {
				t.Errorf("a host git command for the planted root ran the payload: %q", got)
			}
		})
	}
}

// TestBareRecord_RestartsAnUnmarkedRecord (#3364): the start of #3363 wrote
// a record from the .bare dirs on disk, with no start mark. Such a record
// can hold a plant. prism treats it as no record and starts the record again
// from the session start records. The control shows that the old record
// accepts the plant.
func TestBareRecord_RestartsAnUnmarkedRecord(t *testing.T) {
	isolateHostGitConfig(t)
	recordFile := useBareRecord(t)
	code := filepath.Join(t.TempDir(), "code")
	if err := os.MkdirAll(code, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, []string{code}, nil)
	origin := newOrigin(t)
	used := filepath.Join(code, "used")
	prismLayout(t, origin, used)
	planted := filepath.Join(code, "plain")
	plainClone(t, origin, planted)
	marker := filepath.Join(t.TempDir(), "marker")
	bareDir := plantBare(t, origin, planted, marker)
	setSessionStarts(t,
		SessionStart{Name: "used@main", Worktree: filepath.Join(used, "main")},
		SessionStart{Name: "plain", Worktree: planted, Open: true},
	)

	var old bareRecordFile
	for _, gitDir := range []string{filepath.Join(used, ".bare"), bareDir} {
		resolved := mustResolve(t, gitDir)
		info, err := os.Lstat(resolved)
		if err != nil {
			t.Fatal(err)
		}
		ino, _ := inodeOf(info)
		old.Repos = append(old.Repos, bareRecordEntry{Path: resolved, Ino: ino})
	}
	data, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(recordFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(mustResolve(t, bareDir))
	if err != nil {
		t.Fatal(err)
	}
	if ino, _ := inodeOf(info); !old.matches(info, ino) {
		t.Fatal("control: the old record does not accept the plant")
	}

	if _, err := OpenBare(planted); !errors.Is(err, ErrNotPrismBare) {
		t.Errorf("OpenBare(%s) with an unmarked record that holds it = %v, want ErrNotPrismBare", planted, err)
	}
	if _, err := CreateWorktree(planted, "spawned"); !errors.Is(err, ErrNotPrismBare) {
		t.Errorf("CreateWorktree = %v, want ErrNotPrismBare", err)
	}
	if got := takeMarker(marker); got != "" {
		t.Errorf("a host git command for the planted root ran the payload: %q", got)
	}
	if _, err := OpenBare(used); err != nil {
		t.Errorf("OpenBare(%s): %v", used, err)
	}
	raw, err := os.ReadFile(recordFile)
	if err != nil {
		t.Fatal(err)
	}
	var rec bareRecordFile
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Start != bareRecordStart {
		t.Errorf("record start = %q, want %q", rec.Start, bareRecordStart)
	}
	if paths := readRecordPaths(t, recordFile); hasPath(paths, bareDir) || !hasPath(paths, filepath.Join(used, ".bare")) {
		t.Errorf("record = %v, want %s/.bare and no entry for the plant", paths, used)
	}
}

// TestBareRecord_StartNeedsTheReader: with no reader of the session start
// records, prism does not start the record. It refuses, and it writes no
// record file, so a later prism command with the reader starts the record.
func TestBareRecord_StartNeedsTheReader(t *testing.T) {
	isolateHostGitConfig(t)
	recordFile := useBareRecord(t)
	code := filepath.Join(t.TempDir(), "code")
	if err := os.MkdirAll(code, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, []string{code}, nil)
	root := filepath.Join(code, "repo")
	prismLayout(t, newOrigin(t), root)

	prev := sessionStartSource.Swap(nil)
	_, err := OpenBare(root)
	sessionStartSource.Store(prev)
	if err == nil {
		t.Fatal("OpenBare with no reader: want an error")
	}
	if _, err := os.Stat(recordFile); !os.IsNotExist(err) {
		t.Errorf("OpenBare with no reader wrote the record: %v", err)
	}

	setSessionStarts(t, SessionStart{Name: "repo@main", Worktree: filepath.Join(root, "main")})
	if _, err := OpenBare(root); err != nil {
		t.Errorf("OpenBare with the reader: %v", err)
	}
}

// TestConfirmBare: the owner adds a .bare that the start of the record did
// not get. ConfirmBare refuses a plain clone, a root outside the project
// roots, a root in which a session runs, and a sandbox.
func TestConfirmBare(t *testing.T) {
	isolateHostGitConfig(t)
	recordFile := useBareRecord(t)
	base := t.TempDir()
	code := filepath.Join(base, "code")
	if err := os.MkdirAll(code, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, []string{code}, nil)
	origin := newOrigin(t)

	idle := filepath.Join(code, "idle")
	prismLayout(t, origin, idle)
	busy := filepath.Join(code, "busy")
	prismLayout(t, origin, busy)
	plain := filepath.Join(code, "plain")
	plainClone(t, origin, plain)
	if out, err := exec.Command("git", "clone", "-q", "--bare", origin, filepath.Join(plain, ".bare")).CombinedOutput(); err != nil {
		t.Fatalf("plant plain/.bare: %v\n%s", err, out)
	}
	outside := filepath.Join(base, "npm", "x")
	prismLayout(t, origin, outside)
	raw := filepath.Join(code, "raw.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", raw).CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v\n%s", err, out)
	}
	busyStart := SessionStart{Name: "busy", Worktree: busy, Open: true}
	setSessionStarts(t, busyStart)

	if _, err := OpenBare(idle); !errors.Is(err, ErrNotPrismBare) {
		t.Fatalf("OpenBare(%s) before the confirmation = %v, want ErrNotPrismBare", idle, err)
	}
	gitDir, err := ConfirmBare(idle)
	if err != nil {
		t.Fatalf("ConfirmBare(%s): %v", idle, err)
	}
	if want := mustResolve(t, filepath.Join(idle, ".bare")); gitDir != want {
		t.Errorf("ConfirmBare(%s) = %q, want %q", idle, gitDir, want)
	}
	if _, err := OpenBare(idle); err != nil {
		t.Errorf("OpenBare(%s) after the confirmation: %v", idle, err)
	}

	if _, err := ConfirmBare(busy); err == nil || !strings.Contains(err.Error(), "the session busy runs in") {
		t.Errorf("ConfirmBare(%s) while a session runs in the root = %v, want a refusal that names the session", busy, err)
	}
	if _, err := ConfirmBare(plain); err == nil || !strings.Contains(err.Error(), "plain clone") {
		t.Errorf("ConfirmBare(%s), a plain clone = %v, want a refusal", plain, err)
	}
	if _, err := ConfirmBare(raw); err == nil || !strings.Contains(err.Error(), "bare git dir") {
		t.Errorf("ConfirmBare(%s), a raw bare git dir = %v, want a refusal", raw, err)
	}
	if _, err := ConfirmBare(outside); !errors.Is(err, ErrNotProjectRoot) {
		t.Errorf("ConfirmBare(%s) = %v, want ErrNotProjectRoot", outside, err)
	}
	paths := readRecordPaths(t, recordFile)
	for _, root := range []string{busy, plain, outside} {
		if hasPath(paths, filepath.Join(root, ".bare")) {
			t.Errorf("record = %v, want no entry for %s", paths, root)
		}
	}
	if hasPath(paths, raw) {
		t.Errorf("record = %v, want no entry for %s", paths, raw)
	}

	busyStart.Open = false
	setSessionStarts(t, busyStart)
	if _, err := ConfirmBare(busy); err != nil {
		t.Errorf("ConfirmBare(%s) after the session ended: %v", busy, err)
	}

	t.Setenv("PRISM_HOST_API", "unix:///nonexistent/host-api.sock")
	if _, err := ConfirmBare(idle); !errors.Is(err, ErrConfirmInSandbox) {
		t.Errorf("ConfirmBare in a sandbox = %v, want ErrConfirmInSandbox", err)
	}
}

// TestBareRecord_ReplacedBareRefused: the record holds the inode of each
// .bare. A .bare with another inode at a recorded path is not the dir that
// prism made. OpenBare refuses it and removes the stale entry, so a later
// .bare at that path is refused too, also the original dir.
func TestBareRecord_ReplacedBareRefused(t *testing.T) {
	isolateHostGitConfig(t)
	recordFile := useBareRecord(t)
	code := filepath.Join(t.TempDir(), "code")
	if err := os.MkdirAll(code, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, []string{code}, nil)
	origin := newOrigin(t)
	root := filepath.Join(code, "repo")
	if err := CloneWorktree(origin, root, func(string) {}); err != nil {
		t.Fatalf("CloneWorktree: %v", err)
	}
	if _, err := OpenBare(root); err != nil {
		t.Fatalf("OpenBare: %v", err)
	}

	bareDir := filepath.Join(root, ".bare")
	kept := filepath.Join(t.TempDir(), "kept")
	if err := os.Rename(bareDir, kept); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-R", kept, bareDir).CombinedOutput(); err != nil {
		t.Fatalf("copy .bare: %v\n%s", err, out)
	}
	if _, err := OpenBare(root); !errors.Is(err, ErrNotPrismBare) {
		t.Errorf("OpenBare with a copied .bare = %v, want ErrNotPrismBare", err)
	}
	if paths := readRecordPaths(t, recordFile); hasPath(paths, bareDir) {
		t.Errorf("record = %v, want the stale entry removed", paths)
	}

	if err := os.RemoveAll(bareDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(kept, bareDir); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBare(root); !errors.Is(err, ErrNotPrismBare) {
		t.Errorf("OpenBare after the stale entry was removed = %v, want ErrNotPrismBare", err)
	}
}

// TestBareRecord_ConvertRecordsAndRollbackForgets: prism convert records the
// .bare that it makes. A rollback removes the entry before it removes the
// .bare, because a new dir can get the inode of the removed one.
func TestBareRecord_ConvertRecordsAndRollbackForgets(t *testing.T) {
	isolateHostGitConfig(t)
	recordFile := useBareRecord(t)
	code := filepath.Join(t.TempDir(), "code")
	if err := os.MkdirAll(code, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, []string{code}, nil)

	failed := filepath.Join(code, "failed")
	if err := os.MkdirAll(failed, 0o700); err != nil {
		t.Fatal(err)
	}
	initRepoWithRemote(t, failed, filepath.Join(t.TempDir(), "remote.git"), "main")
	var convErr error
	withStepHook(func(step string) error {
		if step == "mkdir-worktrees-dir" {
			if paths := readRecordPaths(t, recordFile); !hasPath(paths, filepath.Join(failed, ".bare")) {
				t.Errorf("record during the conversion = %v, want %s/.bare", paths, failed)
			}
			return errInjected
		}
		return nil
	}, func() {
		_, convErr = ConvertToBare(failed, func(string) {})
	})
	if !errors.Is(convErr, errInjected) {
		t.Fatalf("ConvertToBare = %v, want the injected failure", convErr)
	}
	if paths := readRecordPaths(t, recordFile); len(paths) != 0 {
		t.Errorf("record after the rollback = %v, want no entry", paths)
	}

	converted := filepath.Join(code, "converted")
	if err := os.MkdirAll(converted, 0o700); err != nil {
		t.Fatal(err)
	}
	initRepoWithRemote(t, converted, filepath.Join(t.TempDir(), "remote.git"), "main")
	if _, err := ConvertToBare(converted, func(string) {}); err != nil {
		t.Fatalf("ConvertToBare: %v", err)
	}
	if _, err := OpenBare(converted); err != nil {
		t.Errorf("OpenBare(%s) after prism convert: %v", converted, err)
	}
}

// TestBareRecord_NotUsedInSandbox: a sandbox cannot read the prism state
// dir. In a sandbox, git runs with the rights of the session, so OpenBare
// does not use the record there.
func TestBareRecord_NotUsedInSandbox(t *testing.T) {
	isolateHostGitConfig(t)
	recordFile := useBareRecord(t)
	t.Setenv("PRISM_HOST_API", "unix:///nonexistent/host-api.sock")
	root := t.TempDir()
	writeProjectConfig(t, nil, []string{root})
	if out, err := exec.Command("git", "clone", "-q", "--bare", newOrigin(t), filepath.Join(root, ".bare")).CombinedOutput(); err != nil {
		t.Fatalf("clone .bare: %v\n%s", err, out)
	}
	if _, err := OpenBare(root); err != nil {
		t.Errorf("OpenBare in a sandbox: %v", err)
	}
	if _, err := os.Stat(recordFile); !os.IsNotExist(err) {
		t.Errorf("OpenBare in a sandbox wrote the record: %v", err)
	}
}

// TestBareRecord_NoBareMadeInSandbox: a sandbox cannot write the record, so
// a .bare that CloneWorktree or ConvertToBare made there is refused on the
// host. Both refuse in a sandbox and change nothing.
func TestBareRecord_NoBareMadeInSandbox(t *testing.T) {
	isolateHostGitConfig(t)
	useBareRecord(t)
	t.Setenv("PRISM_HOST_API", "unix:///nonexistent/host-api.sock")
	code := filepath.Join(t.TempDir(), "code")
	if err := os.MkdirAll(code, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, []string{code}, nil)

	target := filepath.Join(code, "cloned")
	if err := CloneWorktree(newOrigin(t), target, func(string) {}); !errors.Is(err, ErrBareInSandbox) {
		t.Errorf("CloneWorktree in a sandbox = %v, want ErrBareInSandbox", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("CloneWorktree in a sandbox made %s: %v", target, err)
	}

	plain := filepath.Join(code, "plain")
	if err := os.MkdirAll(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	initRepoWithRemote(t, plain, filepath.Join(t.TempDir(), "remote.git"), "main")
	if _, err := ConvertToBare(plain, func(string) {}); !errors.Is(err, ErrBareInSandbox) {
		t.Errorf("ConvertToBare in a sandbox = %v, want ErrBareInSandbox", err)
	}
	if info, err := os.Stat(filepath.Join(plain, ".git")); err != nil || !info.IsDir() {
		t.Errorf("ConvertToBare in a sandbox changed .git: %v", err)
	}
	if _, err := os.Stat(filepath.Join(plain, ".bare")); !os.IsNotExist(err) {
		t.Errorf("ConvertToBare in a sandbox made .bare: %v", err)
	}
}

// TestOpenBare_ChecksTheResolvedRoot is AC 2 of #3333. OpenBare resolves the
// root one time, and the checks and GIT_DIR use that path. A root in a
// project location that is a symlink to a dir outside the project roots is
// refused: the dir that git uses is not a project root. A root reached
// through a symlinked parent gets a GIT_DIR with no symlink in it.
func TestOpenBare_ChecksTheResolvedRoot(t *testing.T) {
	isolateHostGitConfig(t)
	useBareRecord(t)
	base := t.TempDir()
	code := filepath.Join(base, "code")
	if err := os.MkdirAll(code, 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, []string{code}, nil)
	origin := newOrigin(t)
	root := filepath.Join(code, "repo")
	if err := CloneWorktree(origin, root, func(string) {}); err != nil {
		t.Fatalf("CloneWorktree: %v", err)
	}

	outside := filepath.Join(base, "npm", "x")
	if out, err := exec.Command("git", "clone", "-q", "--bare", origin, filepath.Join(outside, ".bare")).CombinedOutput(); err != nil {
		t.Fatalf("clone outside: %v\n%s", err, out)
	}
	link := filepath.Join(code, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenBare(link); !errors.Is(err, ErrNotProjectRoot) {
		t.Errorf("OpenBare(%s -> %s) = %v, want ErrNotProjectRoot", link, outside, err)
	}

	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(base, alias); err != nil {
		t.Fatal(err)
	}
	r, err := OpenBare(filepath.Join(alias, "code", "repo"))
	if err != nil {
		t.Fatalf("OpenBare through the symlink: %v", err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(root, ".bare"))
	if err != nil {
		t.Fatal(err)
	}
	if r.GitDir() != want || r.CommonDir() != want {
		t.Errorf("OpenBare through the symlink: GitDir = %q, CommonDir = %q; want the resolved path %q", r.GitDir(), r.CommonDir(), want)
	}
}
