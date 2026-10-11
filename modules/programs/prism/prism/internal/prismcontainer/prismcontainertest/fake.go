// Package prismcontainertest holds a fake podman for tests of
// `prism container`. It models the podman CLI calls that prismcontainer
// makes: ps, image exists, pull, run, rm, container exists, build, images,
// rmi, machine list, machine inspect, info, and unshare. It is also a prismcontainer.BuildExecutor that runs a build
// through the same fake podman.
package prismcontainertest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/prismatic-koi/prism/internal/prismcontainer"
)

// RealTempDir returns t.TempDir() with every symlink resolved. On macOS the
// temp dir is under /var, a symlink to /private/var, and prismcontainer
// refuses a worktree path that goes through a symlink.
func RealTempDir(t testing.TB) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(t.TempDir()): %v", err)
	}
	return dir
}

// Container is one container the fake holds.
type Container struct {
	ID     string
	Name   string
	State  string
	Labels map[string]string
}

// Image is one image the fake holds.
type Image struct {
	ID     string
	Names  []string
	Labels map[string]string
}

// RunFunc is the behaviour of the container command for a `podman run`
// call. args is the full podman argument vector.
type RunFunc func(ctx context.Context, stdout, stderr io.Writer, args []string) (int, error)

// BuildFunc is the behaviour of a `podman build` call. out receives the
// build output. args is the full podman argument vector.
type BuildFunc func(ctx context.Context, out io.Writer, args []string) (int, error)

// Fake is a prismcontainer.Runner that keeps container state in memory.
type Fake struct {
	mu           sync.Mutex
	calls        [][]string
	containers   []Container
	images       []Image
	nextID       int
	builds       map[string]context.CancelFunc
	stopPrefixes []string

	// ImagePresent makes `podman image exists` succeed.
	ImagePresent bool
	// PullCode and PullStderr are the result of `podman pull`.
	PullCode   int
	PullStderr string
	// OnPull runs during `podman pull`, for a test that changes the host
	// while the pull is in progress.
	OnPull func()
	// PsErr makes `podman ps` fail as an unreachable podman does.
	PsErr error
	// OnRun is the container command. Nil exits 0 with no output.
	OnRun RunFunc
	// OnBuild is the build. Nil exits 0 with no output. A build that
	// exits 0 adds an image with the --tag name and the --label labels,
	// and an intermediate image with the --layer-label labels.
	OnBuild BuildFunc
	// PreflightErr is the result of Preflight.
	PreflightErr error
	// MachineList, MachineInspect, and Info are the JSON output of
	// `podman machine list`, `podman machine inspect`, and `podman info`.
	// Empty selects a default machine with config dir /fake/machine, and a
	// store at /fake/graph and /fake/run.
	MachineList    string
	MachineInspect string
	Info           string
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Preflight implements prismcontainer.BuildExecutor.
func (f *Fake) Preflight(context.Context) error { return f.PreflightErr }

var (
	_ prismcontainer.Runner        = (*Fake)(nil)
	_ prismcontainer.BuildExecutor = (*Fake)(nil)
)

// AddImage puts an image into the fake, as if another build had made it.
func (f *Fake) AddImage(img Image) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if img.ID == "" {
		f.nextID++
		img.ID = fmt.Sprintf("image%04d", f.nextID)
	}
	f.images = append(f.images, img)
}

// Images returns the images the fake still holds.
func (f *Fake) Images() []Image {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Image(nil), f.images...)
}

// BuildCalls returns the argument vectors of the `podman build` calls.
func (f *Fake) BuildCalls() [][]string {
	var out [][]string
	for _, c := range f.Calls() {
		if len(c) > 0 && c[0] == "build" {
			out = append(out, c)
		}
	}
	return out
}

// StopPrefixes returns the prefixes of every StopBuilds call.
func (f *Fake) StopPrefixes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.stopPrefixes...)
}

// Build implements prismcontainer.BuildExecutor. It runs the build through
// the fake podman, with no executor option added. StopBuilds cancels it.
func (f *Fake) Build(ctx context.Context, unit string, out io.Writer, args []string) (int, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	f.mu.Lock()
	if f.builds == nil {
		f.builds = map[string]context.CancelFunc{}
	}
	f.builds[unit] = cancel
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.builds, unit)
		f.mu.Unlock()
	}()
	return f.Run(ctx, out, out, args...)
}

// StopBuilds implements prismcontainer.BuildExecutor.
func (f *Fake) StopBuilds(_ context.Context, prefix string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopPrefixes = append(f.stopPrefixes, prefix)
	var n int
	for unit, cancel := range f.builds {
		if strings.HasPrefix(unit, prefix) {
			cancel()
			n++
		}
	}
	return n, nil
}

// Add puts a container into the fake, as if another run had created it.
func (f *Fake) Add(c Container) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.ID == "" {
		f.nextID++
		c.ID = fmt.Sprintf("preexisting%04d", f.nextID)
	}
	f.containers = append(f.containers, c)
}

// Containers returns the containers the fake still holds.
func (f *Fake) Containers() []Container {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Container(nil), f.containers...)
}

// Calls returns every podman argument vector the fake received.
func (f *Fake) Calls() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = append([]string(nil), c...)
	}
	return out
}

// RunCalls returns the argument vectors of the `podman run` calls.
func (f *Fake) RunCalls() [][]string {
	var out [][]string
	for _, c := range f.Calls() {
		if len(c) > 0 && c[0] == "run" {
			out = append(out, c)
		}
	}
	return out
}

// Run implements prismcontainer.Runner.
func (f *Fake) Run(ctx context.Context, stdout, stderr io.Writer, args ...string) (int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	f.mu.Unlock()
	if len(args) == 0 {
		return 125, nil
	}
	switch args[0] {
	case "ps":
		return f.ps(stdout)
	case "image":
		if f.ImagePresent || (len(args) == 3 && f.hasImage(args[2])) {
			return 0, nil
		}
		return 1, nil
	case "build":
		return f.build(ctx, stdout, args)
	case "images":
		return f.listImages(stdout)
	case "rmi":
		f.removeImages(args[1:])
		return 0, nil
	case "machine":
		switch {
		case len(args) > 1 && args[1] == "list":
			_, _ = io.WriteString(stdout, orDefault(f.MachineList, `[{"Name":"fake-machine","Default":true}]`))
			return 0, nil
		case len(args) > 1 && args[1] == "inspect":
			_, _ = io.WriteString(stdout, orDefault(f.MachineInspect, `[{"Name":"fake-machine","ConfigDir":{"Path":"/fake/machine"}}]`))
			return 0, nil
		}
	case "info":
		_, _ = io.WriteString(stdout, orDefault(f.Info, `{"store":{"graphRoot":"/fake/graph","runRoot":"/fake/run"}}`))
		return 0, nil
	case "unshare":
		return 0, nil
	case "pull":
		if f.OnPull != nil {
			f.OnPull()
		}
		_, _ = io.WriteString(stderr, f.PullStderr)
		if f.PullCode == 0 {
			f.mu.Lock()
			f.ImagePresent = true
			f.mu.Unlock()
		}
		return f.PullCode, nil
	case "run":
		return f.run(ctx, stdout, stderr, args)
	case "rm":
		f.remove(args[1:])
		return 0, nil
	case "container":
		if len(args) == 3 && args[1] == "exists" && f.has(args[2]) {
			return 0, nil
		}
		return 1, nil
	}
	return 125, fmt.Errorf("fake podman: unexpected call %v", args)
}

func (f *Fake) ps(stdout io.Writer) (int, error) {
	if f.PsErr != nil {
		return -1, f.PsErr
	}
	f.mu.Lock()
	type entry struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		State  string            `json:"State"`
		Labels map[string]string `json:"Labels"`
	}
	entries := []entry{}
	for _, c := range f.containers {
		if _, ok := c.Labels[prismcontainer.LabelInstanceID]; ok {
			entries = append(entries, entry{ID: c.ID, Names: []string{c.Name}, State: c.State, Labels: c.Labels})
		}
	}
	f.mu.Unlock()
	data, err := json.Marshal(entries)
	if err != nil {
		return 125, err
	}
	_, _ = stdout.Write(data)
	return 0, nil
}

func (f *Fake) run(ctx context.Context, stdout, stderr io.Writer, args []string) (int, error) {
	c := Container{State: "running", Labels: map[string]string{}}
	var cidFile string
	for i := 1; i < len(args)-1; i++ {
		switch args[i] {
		case "--name":
			c.Name = args[i+1]
		case "--label":
			k, v, _ := strings.Cut(args[i+1], "=")
			c.Labels[k] = v
		case "--cidfile":
			cidFile = args[i+1]
		}
	}
	f.mu.Lock()
	f.nextID++
	c.ID = fmt.Sprintf("fake%04d", f.nextID)
	f.containers = append(f.containers, c)
	f.mu.Unlock()
	if cidFile != "" {
		_ = os.WriteFile(cidFile, []byte(c.ID), 0o600)
	}

	code, err := 0, error(nil)
	if f.OnRun != nil {
		code, err = f.OnRun(ctx, stdout, stderr, args)
	}
	if err == nil {
		// --rm: the container goes away when it exits on its own.
		f.remove([]string{c.ID})
	}
	return code, err
}

// remove drops every container named by an ID or a name in args. Flags
// are skipped.
func (f *Fake) remove(args []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	drop := map[string]bool{}
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			drop[a] = true
		}
	}
	kept := f.containers[:0]
	for _, c := range f.containers {
		if drop[c.ID] || drop[c.Name] {
			continue
		}
		kept = append(kept, c)
	}
	f.containers = kept
}

func (f *Fake) build(ctx context.Context, out io.Writer, args []string) (int, error) {
	code, err := 0, error(nil)
	if f.OnBuild != nil {
		code, err = f.OnBuild(ctx, out, args)
	}
	if err != nil || code != 0 {
		return code, err
	}
	final := Image{Labels: map[string]string{}}
	layer := Image{Labels: map[string]string{}}
	for i := 1; i < len(args)-1; i++ {
		switch args[i] {
		case "--tag":
			final.Names = append(final.Names, args[i+1])
		case "--label":
			k, v, _ := strings.Cut(args[i+1], "=")
			final.Labels[k] = v
		case "--layer-label":
			k, v, _ := strings.Cut(args[i+1], "=")
			layer.Labels[k] = v
		}
	}
	f.AddImage(layer)
	f.AddImage(final)
	return 0, nil
}

func (f *Fake) listImages(stdout io.Writer) (int, error) {
	f.mu.Lock()
	type entry struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		Labels map[string]string `json:"Labels"`
	}
	entries := []entry{}
	for _, img := range f.images {
		if _, ok := img.Labels[prismcontainer.LabelInstanceID]; ok {
			entries = append(entries, entry{ID: img.ID, Names: img.Names, Labels: img.Labels})
		}
	}
	f.mu.Unlock()
	data, err := json.Marshal(entries)
	if err != nil {
		return 125, err
	}
	_, _ = stdout.Write(data)
	return 0, nil
}

func (f *Fake) removeImages(args []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	drop := map[string]bool{}
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			drop[a] = true
		}
	}
	kept := f.images[:0]
	for _, img := range f.images {
		if !drop[img.ID] {
			kept = append(kept, img)
		}
	}
	f.images = kept
}

func (f *Fake) hasImage(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, img := range f.images {
		if img.ID == name || slices.Contains(img.Names, name) {
			return true
		}
	}
	return false
}

func (f *Fake) has(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if c.ID == name || c.Name == name {
			return true
		}
	}
	return false
}

// MaskBuildArgs returns a copy of a `podman build` argument vector with
// the paths of the build copy, which change on every call, replaced by
// fixed text.
func MaskBuildArgs(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i < len(out)-1; i++ {
		if out[i] == "--file" {
			out[i+1] = "<file>"
		}
	}
	if len(out) > 1 {
		out[len(out)-1] = "<context>"
	}
	return out
}

// MaskRunArgs returns a copy of a `podman run` argument vector with the
// values that change on every call, the container name and the cidfile
// path, replaced by fixed text. Two routes that build the same vector give
// equal masked copies.
func MaskRunArgs(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i < len(out)-1; i++ {
		switch out[i] {
		case "--name":
			out[i+1] = "<name>"
		case "--cidfile":
			out[i+1] = "<cidfile>"
		}
	}
	return out
}
