package prismcontainer_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
	"github.com/prismatic-koi/prism/internal/prismcontainer"
	"github.com/prismatic-koi/prism/internal/prismcontainer/prismcontainertest"
)

func buildDeps(f *prismcontainertest.Fake) prismcontainer.Deps {
	d := deps(f)
	d.BuildExecutor = prismcontainer.PlainExecutor{Runner: f}
	return d
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stagedView is what podman sees during a build: the Containerfile it got
// and every entry of the context it got.
type stagedView struct {
	fileBase    string
	fileContent string
	entries     map[string]string
	contextDir  string
}

// captureStaged makes the fake build record the copy that podman reads.
// The copy is removed after the build, so it is read during the build.
func captureStaged(t *testing.T, f *prismcontainertest.Fake, view *stagedView) {
	t.Helper()
	f.OnBuild = func(_ context.Context, out io.Writer, args []string) (int, error) {
		file := optionValue(args, "--file")
		if len(file) != 1 {
			return 125, fmt.Errorf("--file = %q", file)
		}
		data, err := os.ReadFile(file[0])
		if err != nil {
			return 125, err
		}
		view.fileBase = filepath.Base(file[0])
		view.fileContent = string(data)
		view.contextDir = args[len(args)-1]
		view.entries = map[string]string{}
		err = filepath.WalkDir(view.contextDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(view.contextDir, p)
			switch {
			case d.Type()&fs.ModeSymlink != 0:
				target, _ := os.Readlink(p)
				view.entries[rel] = "link:" + target
			case d.IsDir():
				view.entries[rel] = "dir"
			default:
				b, _ := os.ReadFile(p)
				view.entries[rel] = "file:" + string(b)
			}
			return nil
		})
		if err != nil {
			return 125, err
		}
		_, _ = io.WriteString(out, "STEP 1/1: FROM alpine\n")
		return 0, nil
	}
}

func TestBuild_Success(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM alpine\n")
	f := &prismcontainertest.Fake{}
	f.OnBuild = func(_ context.Context, out io.Writer, _ []string) (int, error) {
		_, _ = io.WriteString(out, "STEP 1/1: FROM alpine\nCOMMIT\n")
		return 0, nil
	}

	res := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{})

	if res.ExitCode != 0 || res.Message != "" {
		t.Fatalf("result = %+v, want exit 0 and no message", res)
	}
	token := container.InstanceTokenForID(testInstanceID)
	want := regexp.MustCompile(`^localhost/prism-` + token + `-prism-test-container-run-[0-9a-f]{8}$`)
	if !want.MatchString(res.Image) {
		t.Errorf("image = %q, want it to match %s", res.Image, want)
	}
	if string(res.Output) != "STEP 1/1: FROM alpine\nCOMMIT\n" {
		t.Errorf("output = %q", res.Output)
	}
	var stdout, stderr bytes.Buffer
	prismcontainer.WriteBuildResult(&stdout, &stderr, res)
	if stdout.String() != res.Image+"\n" {
		t.Errorf("stdout = %q, want the image name only", stdout.String())
	}
	if !strings.Contains(stderr.String(), "COMMIT") {
		t.Errorf("stderr = %q, want the build output", stderr.String())
	}
	if tags := optionValue(f.BuildCalls()[0], "--tag"); !slices.Equal(tags, []string{res.Image}) {
		t.Errorf("--tag = %q, want [%s]", tags, res.Image)
	}
	stage, _ := container.PrismContainerBuildStageDirPath()
	if left, _ := os.ReadDir(stage); len(left) != 0 {
		t.Errorf("build copy left after the build: %v", left)
	}
	lockDir, _ := container.PrismContainerBuildLockDirPath()
	if left, _ := os.ReadDir(lockDir); len(left) != 0 {
		t.Errorf("build marker left after the build: %v", left)
	}
}

// TestBuild_ImageWorksWithRun: the printed image name is a local name that
// `prism container run` uses as it is, with no pull.
func TestBuild_ImageWorksWithRun(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM alpine\n")
	f := &prismcontainertest.Fake{}
	built := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{Tag: "app:v1"})
	if built.ExitCode != 0 {
		t.Fatalf("build: %+v", built)
	}

	res := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: built.Image, Command: []string{"true"}})
	if res.ExitCode != 0 {
		t.Fatalf("run of the built image: %+v", res)
	}
	for _, call := range f.Calls() {
		if call[0] == "pull" {
			t.Errorf("run pulled the built image: %q", call)
		}
	}
	runs := f.RunCalls()
	if len(runs) != 1 || !slices.Contains(runs[0], built.Image) {
		t.Errorf("run argv %q does not name the built image %s", runs, built.Image)
	}
}

func TestBuild_FileAndContext(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM root-file\n")
	writeFile(t, c.Worktree, "root.txt", "root")
	writeFile(t, c.Worktree, "app/Containerfile", "FROM app-default\n")
	writeFile(t, c.Worktree, "app/main.go", "package main")
	writeFile(t, c.Worktree, "app/sub/data", "data")
	writeFile(t, c.Worktree, "docker/App.containerfile", "FROM chosen\n")

	cases := []struct {
		name        string
		req         prismcontainer.BuildRequest
		wantBase    string
		wantContent string
		wantEntry   string
		notEntry    string
	}{
		{"defaults", prismcontainer.BuildRequest{}, "Containerfile", "FROM root-file\n", "root.txt", ""},
		{"context", prismcontainer.BuildRequest{Context: "app"}, "Containerfile", "FROM app-default\n", "sub/data", "root.txt"},
		{"file", prismcontainer.BuildRequest{File: "docker/App.containerfile"}, "App.containerfile", "FROM chosen\n", "app/main.go", ""},
		{"both", prismcontainer.BuildRequest{Context: "app/", File: "./docker/App.containerfile"}, "App.containerfile", "FROM chosen\n", "main.go", "root.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &prismcontainertest.Fake{}
			var view stagedView
			captureStaged(t, f, &view)
			res := prismcontainer.Build(context.Background(), buildDeps(f), c, tc.req)
			if res.ExitCode != 0 {
				t.Fatalf("build: %+v", res)
			}
			if view.fileBase != tc.wantBase || view.fileContent != tc.wantContent {
				t.Errorf("Containerfile = %s %q, want %s %q", view.fileBase, view.fileContent, tc.wantBase, tc.wantContent)
			}
			if _, ok := view.entries[tc.wantEntry]; !ok {
				t.Errorf("context lacks %s: %v", tc.wantEntry, view.entries)
			}
			if _, ok := view.entries[tc.notEntry]; tc.notEntry != "" && ok {
				t.Errorf("context holds %s, which is outside CONTEXT: %v", tc.notEntry, view.entries)
			}
			if strings.HasPrefix(view.contextDir, c.Worktree) {
				t.Errorf("podman got a worktree path %s, want the copy", view.contextDir)
			}
		})
	}
}

func TestBuild_BuildArgAndTag(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM alpine\n")
	f := &prismcontainertest.Fake{}
	res := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{
		BuildArgs: []string{"VERSION=1.2", "EMPTY=", "X=a=b"},
		Tag:       "myapp:v2",
	})
	if res.ExitCode != 0 {
		t.Fatalf("build: %+v", res)
	}
	token := container.InstanceTokenForID(testInstanceID)
	if want := "localhost/prism-" + token + "-prism-test-container-run-myapp:v2"; res.Image != want {
		t.Errorf("image = %q, want %q", res.Image, want)
	}
	args := f.BuildCalls()[0]
	if got := optionValue(args, "--build-arg"); !slices.Equal(got, []string{"VERSION=1.2", "EMPTY=", "X=a=b"}) {
		t.Errorf("--build-arg = %q", got)
	}
}

func TestBuild_FailurePrintsOutput(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM alpine\nRUN false\n")
	f := &prismcontainertest.Fake{}
	f.OnBuild = func(_ context.Context, out io.Writer, _ []string) (int, error) {
		_, _ = io.WriteString(out, "STEP 2/2: RUN false\nError: building at STEP \"RUN false\": exit status 1\n")
		return 1, nil
	}
	res := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{})
	if res.ExitCode != 1 || res.Image != "" || !strings.Contains(res.Message, "the build failed") {
		t.Fatalf("result = %+v, want exit 1, no image, and a failure message", res)
	}
	var stdout, stderr bytes.Buffer
	prismcontainer.WriteBuildResult(&stdout, &stderr, res)
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty after a failed build", stdout.String())
	}
	if !strings.Contains(stderr.String(), "exit status 1") || !strings.Contains(stderr.String(), "the build failed") {
		t.Errorf("stderr = %q, want the build output and the failure", stderr.String())
	}
	if imgs := f.Images(); len(imgs) != 0 {
		t.Errorf("a failed build left images: %v", imgs)
	}
}

// TestBuild_PathsOutsideWorktreeRefused: a context or Containerfile that
// resolves outside the worktree is refused before podman runs.
func TestBuild_PathsOutsideWorktreeRefused(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM alpine\n")
	outside := prismcontainertest.RealTempDir(t)
	writeFile(t, outside, "Containerfile", "FROM outside\n")
	writeFile(t, outside, "secret", "s3cr3t")
	for link, target := range map[string]string{
		"ctxlink":   outside,
		"filelink":  filepath.Join(outside, "Containerfile"),
		"rel/ctxup": "../../" + filepath.Base(outside),
	} {
		p := filepath.Join(c.Worktree, link)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, c.Worktree, "sub/Containerfile", "FROM alpine\n")
	if err := os.Symlink(filepath.Join(outside, "Containerfile"), filepath.Join(c.Worktree, "sub", "Dockerfile.out")); err != nil {
		t.Fatal(err)
	}

	cases := map[string]prismcontainer.BuildRequest{
		"dotdot context":     {Context: "../x"},
		"absolute context":   {Context: outside},
		"symlink context":    {Context: "ctxlink"},
		"symlink context up": {Context: "rel/ctxup"},
		"dotdot file":        {File: "../Containerfile"},
		"absolute file":      {File: filepath.Join(outside, "Containerfile")},
		"symlink file":       {File: "filelink"},
		"symlink file in ok": {Context: "sub", File: "sub/Dockerfile.out"},
		"missing context":    {Context: "nope"},
		"context is a file":  {Context: "Containerfile"},
		"file is a dir":      {File: "sub"},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			f := &prismcontainertest.Fake{}
			res := prismcontainer.Build(context.Background(), buildDeps(f), c, req)
			if res.ExitCode != prismcontainer.ExitRefused || !strings.HasPrefix(res.Message, "refused: ") {
				t.Errorf("result = %+v, want a refusal", res)
			}
			if calls := f.Calls(); len(calls) != 0 {
				t.Errorf("podman called before the refusal: %q", calls)
			}
		})
	}
}

// TestBuild_SymlinksOutsideNotCopied: a symlink in the context whose target
// is outside the worktree does not put the target into the copy that
// podman reads, so it cannot reach the image.
func TestBuild_SymlinksOutsideNotCopied(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM alpine\nCOPY . /src\n")
	writeFile(t, c.Worktree, "data.txt", "inside")
	outside := prismcontainertest.RealTempDir(t)
	writeFile(t, outside, "secret", "s3cr3t-outside")
	links := map[string]string{
		"abs-file":   filepath.Join(outside, "secret"),
		"abs-dir":    outside,
		"rel-up":     "../../../../../../../../" + strings.TrimPrefix(filepath.Join(outside, "secret"), "/"),
		"sub/via-up": "../..",
		"ok-link":    "data.txt",
		"sub/ok-up":  "../data.txt",
		"dangling":   "missing.txt",
	}
	for link, target := range links {
		p := filepath.Join(c.Worktree, link)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.Symlink(target, p); err != nil {
			t.Fatal(err)
		}
	}
	f := &prismcontainertest.Fake{}
	var view stagedView
	captureStaged(t, f, &view)

	res := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{})
	if res.ExitCode != 0 {
		t.Fatalf("build: %+v", res)
	}
	for _, gone := range []string{"abs-file", "abs-dir", "rel-up", "sub/via-up"} {
		if e, ok := view.entries[gone]; ok {
			t.Errorf("copy holds %s (%s), which points outside the context", gone, e)
		}
		if !strings.Contains(res.Message, gone) {
			t.Errorf("message does not name the dropped symlink %s: %q", gone, res.Message)
		}
	}
	for link, want := range map[string]string{"ok-link": "link:data.txt", "sub/ok-up": "link:../data.txt", "dangling": "link:missing.txt"} {
		if got := view.entries[link]; got != want {
			t.Errorf("copy entry %s = %q, want %q", link, got, want)
		}
	}
	for rel, e := range view.entries {
		if strings.Contains(e, "s3cr3t-outside") {
			t.Errorf("copy entry %s holds the outside file", rel)
		}
	}
}

// TestBuild_ContextSwappedToSymlinkDuringCopy: a context directory that
// the agent replaces with a symlink to an outside path is never read
// through the symlink.
func TestBuild_ContextSwappedToSymlinkDuringCopy(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM alpine\n")
	writeFile(t, c.Worktree, "app/file", "inside")
	outside := prismcontainertest.RealTempDir(t)
	writeFile(t, outside, "file", "s3cr3t-outside")

	// The fake podman ps runs after the path checks and before the copy:
	// swap the context there.
	f := &prismcontainertest.Fake{}
	swapped := false
	f.OnBuild = func(context.Context, io.Writer, []string) (int, error) { return 0, nil }
	swapper := &swapRunner{Fake: f, before: func() {
		if swapped {
			return
		}
		swapped = true
		if err := os.Rename(filepath.Join(c.Worktree, "app"), filepath.Join(c.Worktree, "app.moved")); err != nil {
			t.Errorf("rename: %v", err)
		}
		if err := os.Symlink(outside, filepath.Join(c.Worktree, "app")); err != nil {
			t.Errorf("symlink: %v", err)
		}
	}}
	d := buildDeps(f)
	d.Runner = swapper
	d.BuildExecutor = prismcontainer.PlainExecutor{Runner: swapper}

	res := prismcontainer.Build(context.Background(), d, c, prismcontainer.BuildRequest{Context: "app", File: "Containerfile"})
	if !swapped {
		t.Fatal("the swap did not run")
	}
	if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, "changed while prism copied it") {
		t.Errorf("result = %+v, want a refusal that the context changed", res)
	}
	if len(f.BuildCalls()) != 0 {
		t.Errorf("podman build ran after the swap")
	}
}

// swapRunner runs before on the first podman call.
type swapRunner struct {
	*prismcontainertest.Fake
	before func()
}

func (s *swapRunner) Run(ctx context.Context, stdout, stderr io.Writer, args ...string) (int, error) {
	s.before()
	return s.Fake.Run(ctx, stdout, stderr, args...)
}

func TestBuild_IgnoreFile(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM alpine\n")
	writeFile(t, c.Worktree, ".containerignore", "# comment\n/target/\nnode_modules\n**/cache\n*.log\n.containerignore\n")
	writeFile(t, c.Worktree, "target/big", "x")
	writeFile(t, c.Worktree, "node_modules/m/index.js", "x")
	writeFile(t, c.Worktree, "pkg/cache/blob", "x")
	writeFile(t, c.Worktree, "pkg/main.go", "x")
	writeFile(t, c.Worktree, "app.log", "x")
	f := &prismcontainertest.Fake{}
	var view stagedView
	captureStaged(t, f, &view)
	if res := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{}); res.ExitCode != 0 {
		t.Fatalf("build: %+v", res)
	}
	for _, gone := range []string{"target", "node_modules", "pkg/cache"} {
		if _, ok := view.entries[gone]; ok {
			t.Errorf("copy holds %s, which a literal pattern excludes", gone)
		}
	}
	// Podman applies the patterns that the copy does not, so it needs the
	// ignore file and the paths those patterns name.
	for _, kept := range []string{".containerignore", "app.log", "pkg/main.go"} {
		if _, ok := view.entries[kept]; !ok {
			t.Errorf("copy lacks %s", kept)
		}
	}

	// A "!" exception can keep a path below an excluded directory, so the
	// copy then applies no pattern.
	writeFile(t, c.Worktree, ".containerignore", "target\n!target/keep\n")
	writeFile(t, c.Worktree, "target/keep", "k")
	view = stagedView{}
	if res := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{}); res.ExitCode != 0 {
		t.Fatalf("build: %+v", res)
	}
	if _, ok := view.entries["target/keep"]; !ok {
		t.Errorf("copy lacks target/keep, which a \"!\" exception keeps: %v", view.entries)
	}
}

func TestBuild_ContextTooLargeRefused(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM alpine\n")
	big, err := os.Create(filepath.Join(c.Worktree, "sparse"))
	if err != nil {
		t.Fatal(err)
	}
	if err := big.Truncate(prismcontainer.MaxContextBytes + 1); err != nil {
		t.Fatal(err)
	}
	big.Close()
	f := &prismcontainertest.Fake{}
	res := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{})
	if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, "more than 4 GiB") {
		t.Errorf("result = %+v, want a refusal naming the size", res)
	}
	if len(f.BuildCalls()) != 0 {
		t.Errorf("podman build ran")
	}
}

// TestBuild_ArgvFixedOptions checks the fixed options and that no agent
// input adds a forbidden option, for inputs that look like options.
func TestBuild_ArgvFixedOptions(t *testing.T) {
	requests := []prismcontainer.BuildRequest{
		{},
		{BuildArgs: []string{"A=--volume=/:/host", "B=--secret=id=x,src=/etc/shadow", "C=--network=host", "D=--device=/dev/kvm", "E=--cap-add=ALL"}},
		{Context: "--network=host", File: "--network=host/Containerfile"},
		{Tag: "volume", BuildArgs: []string{"V=-v /:/x"}},
	}
	for i, req := range requests {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			c := newCaller(t)
			writeFile(t, c.Worktree, "Containerfile", "FROM alpine\n")
			writeFile(t, c.Worktree, "--network=host/Containerfile", "FROM alpine\n")
			f := &prismcontainertest.Fake{}
			if res := prismcontainer.Build(context.Background(), buildDeps(f), c, req); res.ExitCode != 0 {
				t.Fatalf("build: %+v", res)
			}
			args := f.BuildCalls()[0]
			if args[0] != "build" {
				t.Fatalf("argv = %q", args)
			}
			want := map[string]string{
				"--memory":       prismcontainer.MemoryLimit,
				"--memory-swap":  prismcontainer.MemoryLimit,
				"--cpu-period":   "100000",
				"--cpu-quota":    "200000",
				"--security-opt": "no-new-privileges",
				"--label":        prismcontainer.LabelInstanceID + "=" + testInstanceID,
				"--layer-label":  prismcontainer.LabelInstanceID + "=" + testInstanceID,
				"--ulimit":       "nproc=1024:1024",
			}
			for flag, val := range want {
				if got := optionValue(args, flag); !slices.Equal(got, []string{val}) {
					t.Errorf("%s = %q, want exactly [%q]", flag, got, val)
				}
			}
			for _, o := range args[:len(args)-1] {
				for _, bad := range []string{"--volume", "-v", "--secret", "--network", "--net", "--device", "--cap-add", "--privileged", "--ssh", "--build-context", "--build-arg-file", "--env", "--pid", "--ipc", "--userns", "--isolation", "--runtime"} {
					if o == bad || strings.HasPrefix(o, bad+"=") {
						t.Errorf("argv holds %q: %q", o, args)
					}
				}
			}
			for _, a := range optionValue(args, "--build-arg") {
				if !slices.Contains(req.BuildArgs, a) {
					t.Errorf("--build-arg %q is not an agent KEY=VALUE input", a)
				}
			}
			stage, _ := container.PrismContainerBuildStageDirPath()
			for _, p := range append(optionValue(args, "--file"), args[len(args)-1]) {
				if !strings.HasPrefix(p, stage+string(filepath.Separator)) {
					t.Errorf("path %q is not in the build copy dir %s", p, stage)
				}
			}
		})
	}
}

func TestBuild_RequestRefusals(t *testing.T) {
	cases := map[string]prismcontainer.BuildRequest{
		"build-arg no =":   {BuildArgs: []string{"HOME"}},
		"build-arg key":    {BuildArgs: []string{"A-B=1"}},
		"tag upper":        {Tag: "MyApp"},
		"tag slash":        {Tag: "a/b"},
		"tag dash":         {Tag: "-x"},
		"tag colon only":   {Tag: "app:"},
		"tag digest":       {Tag: "app@sha256:" + strings.Repeat("a", 64)},
		"tag transport":    {Tag: "oci-archive:/tmp/x"},
		"tag long name":    {Tag: strings.Repeat("a", 65)},
		"tag long tag":     {Tag: "a:" + strings.Repeat("t", 129)},
		"tag space":        {Tag: "a b"},
		"timeout over 60m": {TimeoutSeconds: 3601},
		"timeout negative": {TimeoutSeconds: -1},
		"nul in context":   {Context: "a\x00b"},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			c := newCaller(t)
			writeFile(t, c.Worktree, "Containerfile", "FROM alpine\n")
			f := &prismcontainertest.Fake{}
			res := prismcontainer.Build(context.Background(), buildDeps(f), c, req)
			if res.ExitCode != prismcontainer.ExitRefused || !strings.HasPrefix(res.Message, "refused: ") {
				t.Errorf("result = %+v, want a refusal", res)
			}
			if calls := f.Calls(); len(calls) != 0 {
				t.Errorf("podman called: %q", calls)
			}
			lines := readAudit(t, testInstanceID)
			if len(lines) != 1 || lines[0]["decision"] != prismcontainer.DecisionRefused || lines[0]["command"] != "build" {
				t.Errorf("audit = %v, want one refused build line", lines)
			}
		})
	}
}

func TestBuild_ValidTags(t *testing.T) {
	for _, tag := range []string{"a", "my-app", "my_app.v2", "a__b", "app:v1.2-rc_1", "app:LATEST", strings.Repeat("a", 64) + ":" + strings.Repeat("t", 128)} {
		c := newCaller(t)
		writeFile(t, c.Worktree, "Containerfile", "FROM alpine\n")
		f := &prismcontainertest.Fake{}
		if res := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{Tag: tag}); res.ExitCode != 0 {
			t.Errorf("tag %q: %+v", tag, res)
		}
	}
}

// TestBuild_ImageNameIsAValidReference: the session name is folded so the
// image name is a valid reference for any session name.
func TestBuild_ImageNameIsAValidReference(t *testing.T) {
	ref := regexp.MustCompile(`^localhost/[a-z0-9]+(?:(?:[._]|__|[-]+)[a-z0-9]+)*(?::[\w][\w.-]{0,127})?$`)
	for _, session := range []string{"Repo@Feat/X_-y", "nixos-config@main~review-1-review-qa", "a.b@c+d#e", strings.Repeat("Long-", 40)} {
		c := newCaller(t)
		c.SessionName = session
		writeFile(t, c.Worktree, "Containerfile", "FROM alpine\n")
		f := &prismcontainertest.Fake{}
		res := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{Tag: "app:v1"})
		if res.ExitCode != 0 || !ref.MatchString(res.Image) || len(strings.SplitN(res.Image, ":", 2)[0]) > 255 {
			t.Errorf("session %q: image %q is not a valid reference (%+v)", session, res.Image, res)
		}
	}
}

func TestBuild_DefaultTimeout(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM alpine\n")
	f := &prismcontainertest.Fake{}
	var deadline time.Time
	f.OnBuild = func(ctx context.Context, _ io.Writer, _ []string) (int, error) {
		deadline, _ = ctx.Deadline()
		return 0, nil
	}
	start := time.Now()
	prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{})
	if got := deadline.Sub(start); got < 29*time.Minute || got > 30*time.Minute+time.Second {
		t.Errorf("build deadline in %s, want 30m", got)
	}
	lines := readAudit(t, testInstanceID)
	if lines[0]["timeout_seconds"] != float64(1800) {
		t.Errorf("audit timeout_seconds = %v, want 1800", lines[0]["timeout_seconds"])
	}
}

func TestBuild_Timeout(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM alpine\n")
	f := &prismcontainertest.Fake{}
	f.OnBuild = func(ctx context.Context, _ io.Writer, _ []string) (int, error) {
		<-ctx.Done()
		return -1, ctx.Err()
	}
	res := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{TimeoutSeconds: 1})
	if res.ExitCode != prismcontainer.ExitTimeout || res.Message != "the --timeout of 1s expired, so prism stopped the build" {
		t.Errorf("result = %+v, want exit %d and the timeout message", res, prismcontainer.ExitTimeout)
	}
	lockDir, _ := container.PrismContainerBuildLockDirPath()
	if left, _ := os.ReadDir(lockDir); len(left) != 0 {
		t.Errorf("build marker left after the timeout: %v", left)
	}
}

func TestBuild_LimitRefusals(t *testing.T) {
	cases := map[string]func(f *prismcontainertest.Fake){
		"session container": func(f *prismcontainertest.Fake) {
			f.Add(prismcontainertest.Container{Name: "x", State: "running", Labels: map[string]string{prismcontainer.LabelInstanceID: testInstanceID}})
		},
		"host": func(f *prismcontainertest.Fake) {
			for i := range 4 {
				f.Add(prismcontainertest.Container{Name: fmt.Sprint(i), State: "running",
					Labels: map[string]string{prismcontainer.LabelInstanceID: fmt.Sprintf("00000000-0000-4000-8000-00000000000%d", i)}})
			}
		},
	}
	wants := map[string]string{"session container": "per-session limit is 1", "host": "host-wide limit is 4"}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			c := newCaller(t)
			writeFile(t, c.Worktree, "Containerfile", "FROM alpine\n")
			f := &prismcontainertest.Fake{}
			setup(f)
			res := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{})
			if res.ExitCode != prismcontainer.ExitRefused || !strings.Contains(res.Message, wants[name]) {
				t.Errorf("result = %+v, want a refusal naming %q", res, wants[name])
			}
			if len(f.BuildCalls()) != 0 {
				t.Errorf("podman build ran")
			}
		})
	}
}

// TestBuild_CountsTowardLimits: a running build counts toward the
// per-session limit of run and build, and toward the host-wide limit of
// another session.
func TestBuild_CountsTowardLimits(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM alpine\n")
	f := &prismcontainertest.Fake{ImagePresent: true}
	started := make(chan struct{})
	release := make(chan struct{})
	f.OnBuild = func(context.Context, io.Writer, []string) (int, error) {
		close(started)
		<-release
		return 0, nil
	}
	done := make(chan prismcontainer.BuildResult, 1)
	go func() {
		done <- prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{})
	}()
	<-started

	run := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine"})
	if run.ExitCode != prismcontainer.ExitRefused || !strings.Contains(run.Message, "per-session limit is 1") {
		t.Errorf("run during a build = %+v, want the per-session refusal", run)
	}
	f.OnBuild = nil
	second := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{})
	if second.ExitCode != prismcontainer.ExitRefused || !strings.Contains(second.Message, "per-session limit is 1") {
		t.Errorf("second build during a build = %+v, want the per-session refusal", second)
	}

	other := c
	other.SessionName = "prism-test@other"
	other.InstanceID = "22222222-3333-4444-8555-666666666666"
	d := deps(f)
	d.HostLimit = 1
	hostRun := prismcontainer.Run(context.Background(), d, other, prismcontainer.RunRequest{Image: "alpine"})
	if hostRun.ExitCode != prismcontainer.ExitRefused || !strings.Contains(hostRun.Message, "host-wide limit is 1") {
		t.Errorf("run of another session at host limit 1 = %+v, want the host-wide refusal", hostRun)
	}

	close(release)
	if res := <-done; res.ExitCode != 0 {
		t.Fatalf("first build: %+v", res)
	}
	after := prismcontainer.Run(context.Background(), deps(f), c, prismcontainer.RunRequest{Image: "alpine"})
	if after.ExitCode != 0 {
		t.Errorf("run after the build = %+v, want it to pass the limit check", after)
	}
}

func TestBuild_AuditLine(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "app/Containerfile", "FROM alpine\n")
	f := &prismcontainertest.Fake{}
	res := prismcontainer.Build(context.Background(), buildDeps(f), c, prismcontainer.BuildRequest{
		Context: "app", BuildArgs: []string{"TOKEN=s3cr3t-value"}, Tag: "x",
	})
	lines := readAudit(t, testInstanceID)
	if len(lines) != 1 {
		t.Fatalf("audit lines = %d, want 1", len(lines))
	}
	l := lines[0]
	if l["command"] != "build" || l["image"] != res.Image || l["context"] != "app" || l["file"] != "app/Containerfile" ||
		l["decision"] != prismcontainer.DecisionAllowed || l["exit_code"] != float64(0) {
		t.Errorf("audit line = %v", l)
	}
	if keys, _ := l["build_arg_keys"].([]any); len(keys) != 1 || keys[0] != "TOKEN" {
		t.Errorf("build_arg_keys = %v, want [TOKEN]", l["build_arg_keys"])
	}
	path, _ := container.PrismContainerAuditLogPath(testInstanceID)
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("s3cr3t-value")) {
		t.Errorf("audit log holds a --build-arg value: %s", raw)
	}
}

func TestSweepImages(t *testing.T) {
	const other = "11111111-2222-4333-8444-555555555555"
	f := &prismcontainertest.Fake{}
	f.AddImage(prismcontainertest.Image{ID: "mine-final", Names: []string{"localhost/prism-x"}, Labels: map[string]string{prismcontainer.LabelInstanceID: testInstanceID}})
	f.AddImage(prismcontainertest.Image{ID: "mine-layer", Labels: map[string]string{prismcontainer.LabelInstanceID: testInstanceID}})
	f.AddImage(prismcontainertest.Image{ID: "earlier", Labels: map[string]string{prismcontainer.LabelInstanceID: other}})
	f.AddImage(prismcontainertest.Image{ID: "theirs", Labels: map[string]string{prismcontainer.LabelInstanceID: "99999999-2222-4333-8444-555555555555"}})
	f.AddImage(prismcontainertest.Image{ID: "pulled", Names: []string{"docker.io/library/alpine"}})

	n, err := prismcontainer.SweepImages(context.Background(), f, []string{testInstanceID, other})
	if err != nil || n != 3 {
		t.Fatalf("SweepImages = %d, %v; want 3, nil", n, err)
	}
	var left []string
	for _, img := range f.Images() {
		left = append(left, img.ID)
	}
	if !slices.Equal(left, []string{"theirs", "pulled"}) {
		t.Errorf("left = %v, want [theirs pulled]", left)
	}
	for _, call := range f.Calls() {
		if call[0] == "rmi" && !slices.Contains(call, "--force") {
			t.Errorf("rmi without --force: %q", call)
		}
	}
}

func TestSweepSession(t *testing.T) {
	f := &prismcontainertest.Fake{}
	if err := prismcontainer.SweepSession(context.Background(), f, f, []string{testInstanceID, "not-a-uuid"}); err != nil {
		t.Fatalf("SweepSession: %v", err)
	}
	want := "prism-build-" + container.InstanceTokenForID(testInstanceID) + "-"
	if got := f.StopPrefixes(); !slices.Equal(got, []string{want}) {
		t.Errorf("StopBuilds prefixes = %q, want [%q]", got, want)
	}
	calls := f.Calls()
	if len(calls) < 2 || calls[0][0] != "ps" || calls[len(calls)-1][0] != "images" {
		t.Errorf("podman calls = %q, want the container sweep and then the image sweep", calls)
	}
}

// TestBuild_StoppedByCleanup: a cleanup sweep that stops the builds of the
// session ends the Build call, and the marker goes.
func TestBuild_StoppedByCleanup(t *testing.T) {
	c := newCaller(t)
	writeFile(t, c.Worktree, "Containerfile", "FROM alpine\n")
	f := &prismcontainertest.Fake{}
	started := make(chan struct{})
	f.OnBuild = func(ctx context.Context, _ io.Writer, _ []string) (int, error) {
		close(started)
		<-ctx.Done()
		return -1, ctx.Err()
	}
	d := deps(f)
	d.BuildExecutor = f
	done := make(chan prismcontainer.BuildResult, 1)
	go func() { done <- prismcontainer.Build(context.Background(), d, c, prismcontainer.BuildRequest{}) }()
	<-started

	if err := prismcontainer.SweepSession(context.Background(), f, f, []string{testInstanceID}); err != nil {
		t.Fatalf("SweepSession: %v", err)
	}
	select {
	case res := <-done:
		if res.ExitCode == 0 {
			t.Errorf("stopped build = %+v, want a non-zero exit", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the build did not end after the sweep stopped it")
	}
	lockDir, _ := container.PrismContainerBuildLockDirPath()
	if left, _ := os.ReadDir(lockDir); len(left) != 0 {
		t.Errorf("build marker left: %v", left)
	}
}
