// Package prismcontainertest holds a fake podman for tests of
// `prism container`. It models the podman CLI calls that prismcontainer
// makes: ps, image exists, pull, run, rm, and container exists.
package prismcontainertest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/prismatic-koi/prism/internal/prismcontainer"
)

// Container is one container the fake holds.
type Container struct {
	ID     string
	Name   string
	State  string
	Labels map[string]string
}

// RunFunc is the behaviour of the container command for a `podman run`
// call. args is the full podman argument vector.
type RunFunc func(ctx context.Context, stdout, stderr io.Writer, args []string) (int, error)

// Fake is a prismcontainer.Runner that keeps container state in memory.
type Fake struct {
	mu         sync.Mutex
	calls      [][]string
	containers []Container
	nextID     int

	// ImagePresent makes `podman image exists` succeed.
	ImagePresent bool
	// PullCode and PullStderr are the result of `podman pull`.
	PullCode   int
	PullStderr string
	// PsErr makes `podman ps` fail as an unreachable podman does.
	PsErr error
	// OnRun is the container command. Nil exits 0 with no output.
	OnRun RunFunc
}

var _ prismcontainer.Runner = (*Fake)(nil)

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
		if f.ImagePresent {
			return 0, nil
		}
		return 1, nil
	case "pull":
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
