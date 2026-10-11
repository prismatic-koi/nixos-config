// Package prismcontainertest holds a fake podman for tests of
// `prism container`. It models the podman CLI calls that prismcontainer
// makes: ps, image exists, pull, run, exec, logs, rm, container exists,
// build, images, rmi, machine list, machine inspect, info, and unshare. It is also a prismcontainer.BuildExecutor that runs a build
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
	Image  string
	State  string
	Labels map[string]string
	Ports  []Port
	// Logs is the output of `podman logs`.
	Logs string
}

// Port is one published port of a container.
type Port struct {
	HostIP        string
	ContainerPort int
	HostPort      int
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
	// RmCode and RmiCode, when not zero, make `podman rm` and `podman rmi`
	// exit with that code and remove nothing.
	RmCode  int
	RmiCode int
	// OnRun is the container command. Nil exits 0 with no output. For
	// `podman run --detach` it is the result of the start: the container
	// stays.
	OnRun RunFunc
	// OnExec is the result of `podman exec`. Nil exits 0 with no output
	// for a container that exists, and 125 for one that does not.
	OnExec RunFunc
	// PublishIP, when set, replaces the host address that ps reports for
	// a published port.
	PublishIP string
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
		if code := f.code(&f.RmiCode); code != 0 {
			return code, nil
		}
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
	case "exec":
		if len(args) < 2 || !f.has(args[1]) {
			_, _ = io.WriteString(stderr, "Error: no such container\n")
			return 125, nil
		}
		if f.OnExec != nil {
			return f.OnExec(ctx, stdout, stderr, args)
		}
		return 0, nil
	case "logs":
		if len(args) != 2 {
			return 125, nil
		}
		for _, c := range f.Containers() {
			if c.ID == args[1] {
				_, _ = io.WriteString(stdout, c.Logs)
				return 0, nil
			}
		}
		_, _ = io.WriteString(stderr, "Error: no such container\n")
		return 125, nil
	case "rm":
		if code := f.code(&f.RmCode); code != 0 {
			return code, nil
		}
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

// SetRmCodes sets RmCode and RmiCode while other goroutines can use the
// fake.
func (f *Fake) SetRmCodes(rm, rmi int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.RmCode, f.RmiCode = rm, rmi
}

func (f *Fake) code(p *int) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return *p
}

func (f *Fake) ps(stdout io.Writer) (int, error) {
	if f.PsErr != nil {
		return -1, f.PsErr
	}
	f.mu.Lock()
	type port struct {
		HostIP        string `json:"host_ip"`
		ContainerPort int    `json:"container_port"`
		HostPort      int    `json:"host_port"`
		Range         int    `json:"range"`
		Protocol      string `json:"protocol"`
	}
	type entry struct {
		ID     string            `json:"Id"`
		Names  []string          `json:"Names"`
		Image  string            `json:"Image"`
		State  string            `json:"State"`
		Labels map[string]string `json:"Labels"`
		Ports  []port            `json:"Ports"`
	}
	entries := []entry{}
	for _, c := range f.containers {
		if _, ok := c.Labels[prismcontainer.LabelInstanceID]; ok {
			e := entry{ID: c.ID, Names: []string{c.Name}, Image: c.Image, State: c.State, Labels: c.Labels}
			for _, p := range c.Ports {
				ip := p.HostIP
				if f.PublishIP != "" {
					ip = f.PublishIP
				}
				e.Ports = append(e.Ports, port{HostIP: ip, ContainerPort: p.ContainerPort, HostPort: p.HostPort, Range: 1, Protocol: "tcp"})
			}
			entries = append(entries, e)
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
	detach := false
	f.mu.Lock()
	// Every option that prism gives takes a value, except --rm and
	// --detach. The first argument that is not an option is the image.
	for i := 1; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			c.Image = a
			break
		}
		if a == "--rm" || a == "--detach" {
			detach = detach || a == "--detach"
			continue
		}
		if i+1 >= len(args) {
			break
		}
		i++
		switch val := args[i]; a {
		case "--name":
			c.Name = val
		case "--label":
			k, v, _ := strings.Cut(val, "=")
			c.Labels[k] = v
		case "--cidfile":
			cidFile = val
		case "--publish":
			// HOST_IP::CONTAINER_PORT/tcp
			parts := strings.Split(strings.TrimSuffix(val, "/tcp"), ":")
			var cport int
			_, _ = fmt.Sscan(parts[len(parts)-1], &cport)
			f.nextID++
			c.Ports = append(c.Ports, Port{HostIP: parts[0], ContainerPort: cport, HostPort: 40000 + f.nextID})
		}
	}
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
	if detach {
		if err == nil && code == 0 {
			_, _ = io.WriteString(stdout, c.ID+"\n")
		}
		return code, err
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
// values that change on every call, the container name, the cidfile path,
// and the service handle, replaced by fixed text. Two routes that build
// the same vector give equal masked copies.
func MaskRunArgs(args []string) []string {
	out := append([]string(nil), args...)
	for i := 0; i < len(out)-1; i++ {
		switch {
		case out[i] == "--name":
			out[i+1] = "<name>"
		case out[i] == "--cidfile":
			out[i+1] = "<cidfile>"
		case out[i] == "--label" && strings.HasPrefix(out[i+1], prismcontainer.LabelService+"="):
			out[i+1] = prismcontainer.LabelService + "=<handle>"
		}
	}
	return out
}
