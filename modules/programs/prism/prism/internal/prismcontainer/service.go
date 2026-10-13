package prismcontainer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/prismatic-koi/prism/internal/container"
)

// PublishHost is the only host address that a published port binds to.
const PublishHost = "127.0.0.1"

// MaxPorts is the number of --port values that one start request can give.
const MaxPorts = 16

const (
	// startTimeout limits the image pull and the container create of a
	// start request. The service container itself has no timeout.
	startTimeout = 10 * time.Minute
	// logsTimeout limits one `podman logs` call.
	logsTimeout = time.Minute
	// stopGrace is the time podman gives the service to stop before it
	// kills it.
	stopGrace = "10"
)

// StartRequest holds every input the agent controls for `prism container
// start`. It is also the wire schema of the host-API endpoint POST
// /container/start. Prism sets every other podman option.
type StartRequest struct {
	Image   string   `json:"image"`
	Command []string `json:"command,omitempty"`
	Env     []string `json:"env,omitempty"`
	Mount   Mount    `json:"mount,omitempty"`
	// Ports are the container ports to publish, as the agent typed them.
	// Start validates them, so that a refusal writes an audit line.
	Ports []string `json:"ports,omitempty"`
}

type validStart struct {
	validRun
	ports []int
}

var portPattern = regexp.MustCompile(`^[1-9][0-9]{0,4}$`)

func (r StartRequest) validate() (validStart, error) {
	v, err := RunRequest{Image: r.Image, Command: r.Command, Env: r.Env, Mount: r.Mount}.validate()
	if err != nil {
		return validStart{}, err
	}
	if len(r.Ports) > MaxPorts {
		return validStart{}, fmt.Errorf("%d --port values are more than the maximum of %d", len(r.Ports), MaxPorts)
	}
	s := validStart{validRun: v}
	for _, p := range r.Ports {
		n, err := strconv.Atoi(p)
		if !portPattern.MatchString(p) || err != nil || n > 65535 {
			return validStart{}, fmt.Errorf("--port %q is not valid: give a container port, an integer from 1 to 65535", p)
		}
		if slices.Contains(s.ports, n) {
			return validStart{}, fmt.Errorf("--port %d is given more than once", n)
		}
		s.ports = append(s.ports, n)
	}
	return s, nil
}

// PortMapping is one published port of a service container.
type PortMapping struct {
	ContainerPort int    `json:"container_port"`
	HostIP        string `json:"host_ip"`
	HostPort      int    `json:"host_port"`
}

// StartResult is the outcome of one `prism container start` request. It is
// also the wire schema of the POST /container/start response.
type StartResult struct {
	ExitCode int `json:"exit_code"`
	// Handle names the service container for exec, logs, and stop. It is
	// set only when the container started.
	Handle string        `json:"handle,omitempty"`
	Ports  []PortMapping `json:"ports,omitempty"`
	// Stderr holds the error output of the pull and of podman run.
	Stderr  []byte `json:"stderr,omitempty"`
	Message string `json:"message,omitempty"`
}

// Start starts a service container for caller c and returns its handle and
// its published ports. The container keeps running after Start returns,
// until `prism container stop` or `prism cleanup` removes it. When ctx ends
// before the container runs, Start removes it.
func Start(ctx context.Context, d Deps, c Caller, req StartRequest) StartResult {
	d = d.withDefaults()
	stderr := newTailBuffer(OutputLimit)
	result := func(code int, msg string) StartResult {
		r := StartResult{ExitCode: code, Message: msg}
		r.Stderr, _ = stderr.snapshot()
		return r
	}

	if container.InstanceTokenForID(c.InstanceID) == "" {
		return result(ExitRefused, "refused: "+c.validate(MountNone).Error())
	}
	audit, err := openAuditLog(c.InstanceID)
	if err != nil {
		return result(ExitRefused, "refused: prism cannot write the audit log: "+err.Error())
	}
	defer audit.close()

	entry := AuditEntry{
		Session:    c.SessionName,
		InstanceID: c.InstanceID,
		Command:    "start",
		Image:      req.Image,
		Args:       req.Command,
		Mount:      string(req.Mount),
		Ports:      req.Ports,
	}
	finish := func(decision string, code int, msg string) StartResult {
		entry.Decision = decision
		entry.Reason = msg
		if decision == DecisionAllowed && code == 0 {
			entry.ExitCode = &code
		}
		if err := audit.write(entry, d.Now()); err != nil {
			msg = joinMessages(msg, "warning: prism did not write the audit log: "+err.Error())
		}
		return result(code, msg)
	}
	refuse := func(reason string) StartResult {
		return finish(DecisionRefused, ExitRefused, "refused: "+reason)
	}

	v, err := req.validate()
	if err != nil {
		return refuse(err.Error())
	}
	entry.Image = v.image
	entry.EnvKeys = v.envKeys
	entry.Mount = string(v.mount)
	if err := c.validate(v.mount); err != nil {
		return refuse(err.Error())
	}

	startCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	stopped := func(warning string) StartResult {
		msg := fmt.Sprintf("the start did not complete within %s, so prism removed the container", startTimeout)
		if ctx.Err() != nil {
			msg = "the request was cancelled, so prism removed the container"
		}
		return finish(DecisionAllowed, ExitRefused, joinMessages(msg, warning))
	}
	podmanFailed := func(err error) StartResult {
		if startCtx.Err() != nil {
			return stopped("")
		}
		return finish(DecisionError, ExitRefused, unreachableMessage(d.GOOS, err.Error()))
	}
	limitCheckFailed := func(err error) StartResult {
		var pe *podmanError
		if errors.As(err, &pe) {
			return podmanFailed(err)
		}
		return finish(DecisionError, ExitRefused, err.Error())
	}

	if msg, err := limitRefusal(startCtx, d.Runner, c.InstanceID, d.HostLimit); err != nil {
		return limitCheckFailed(err)
	} else if msg != "" {
		return refuse(msg)
	}
	code, err := ensureImage(startCtx, d.Runner, v.image, stderr)
	if err != nil {
		return podmanFailed(err)
	}
	if code != 0 {
		return finish(DecisionAllowed, ExitRefused, fmt.Sprintf("the podman pull of image %s failed", v.image))
	}

	lockPath, err := container.PrismContainerLockPath()
	if err != nil {
		return finish(DecisionError, ExitRefused, err.Error())
	}
	lock, err := acquireHostLock(startCtx, lockPath, d.LockWait)
	if err != nil {
		if startCtx.Err() != nil {
			return stopped("")
		}
		return finish(DecisionError, ExitRefused, err.Error())
	}
	defer lock.release()
	if msg, err := limitRefusal(startCtx, d.Runner, c.InstanceID, d.HostLimit); err != nil {
		return limitCheckFailed(err)
	} else if msg != "" {
		return refuse(msg)
	}
	if err := c.validate(v.mount); err != nil {
		return refuse(err.Error())
	}

	name, handle, err := serviceName(c)
	if err != nil {
		return finish(DecisionError, ExitRefused, err.Error())
	}
	cidDir, err := makeCIDDir()
	if err != nil {
		return finish(DecisionError, ExitRefused, "create cidfile dir: "+err.Error())
	}
	defer os.RemoveAll(cidDir)
	cidFile := filepath.Join(cidDir, "cid")
	entry.Container = name
	entry.Handle = handle

	done := make(chan runOutcome, 1)
	args := startArgs(c, v, name, handle, cidFile)
	go func() {
		code, err := d.Runner.Run(startCtx, io.Discard, stderr, args...)
		done <- runOutcome{code: code, err: err}
	}()
	outcome, finished := waitForCreate(startCtx, cidFile, done, d.CreateWait)
	lock.release()
	if !finished {
		outcome = <-done
	}
	if outcome.err != nil {
		warning := ensureRemoved(d.Runner, name)
		if startCtx.Err() != nil {
			return stopped(warning)
		}
		return finish(DecisionError, ExitRefused, joinMessages("podman run did not complete: "+outcome.err.Error(), warning))
	}
	if outcome.code != 0 {
		warning := ensureRemoved(d.Runner, name)
		return finish(DecisionError, ExitRefused, joinMessages(fmt.Sprintf("podman run exited %d, so the service container did not start", outcome.code), warning))
	}

	ports, err := publishedPorts(startCtx, d.Runner, name, v.ports)
	if err != nil {
		warning := ensureRemoved(d.Runner, name)
		return finish(DecisionError, ExitRefused, joinMessages(err.Error()+", so prism removed the container", warning))
	}
	res := finish(DecisionAllowed, 0, "")
	res.Handle = handle
	res.Ports = ports
	return res
}

// psPort is one entry of the Ports field of `podman ps --format json`.
type psPort struct {
	HostIP        string `json:"host_ip"`
	ContainerPort int    `json:"container_port"`
	HostPort      int    `json:"host_port"`
	Range         int    `json:"range"`
	Protocol      string `json:"protocol"`
}

// publishedPorts returns the host port of each container port in want. It
// fails when podman reports no host port for a port, or a binding to any
// address other than PublishHost.
func publishedPorts(ctx context.Context, r Runner, name string, want []int) ([]PortMapping, error) {
	if len(want) == 0 {
		return nil, nil
	}
	entries, err := listLabelled(ctx, r)
	if err != nil {
		return nil, fmt.Errorf("prism cannot read the published ports: %w", err)
	}
	i := slices.IndexFunc(entries, func(e psEntry) bool { return slices.Contains(e.Names, name) })
	if i < 0 {
		return nil, fmt.Errorf("podman does not list the service container %s", name)
	}
	var out []PortMapping
	for _, p := range entries[i].Ports {
		if p.HostIP != PublishHost {
			return nil, fmt.Errorf("podman published container port %d on host address %q, not on %s", p.ContainerPort, p.HostIP, PublishHost)
		}
	}
	for _, port := range want {
		j := slices.IndexFunc(entries[i].Ports, func(p psPort) bool {
			return p.ContainerPort == port && (p.Protocol == "" || p.Protocol == "tcp")
		})
		if j < 0 || entries[i].Ports[j].HostPort < 1 {
			return nil, fmt.Errorf("podman reports no host port for container port %d", port)
		}
		out = append(out, PortMapping{ContainerPort: port, HostIP: PublishHost, HostPort: entries[i].Ports[j].HostPort})
	}
	return out, nil
}

// WriteStartResult writes a StartResult to the streams of the `prism
// container start` process. Both routes use it. stdout holds one "handle"
// line and one "port" line for each published port.
func WriteStartResult(stdout, stderr io.Writer, r StartResult) {
	_, _ = stderr.Write(r.Stderr)
	if r.Message != "" {
		fmt.Fprintf(stderr, "prism container start: %s\n", r.Message)
	}
	if r.ExitCode != 0 || r.Handle == "" {
		return
	}
	fmt.Fprintf(stdout, "handle %s\n", r.Handle)
	for _, p := range r.Ports {
		fmt.Fprintf(stdout, "port %d %s:%d\n", p.ContainerPort, p.HostIP, p.HostPort)
	}
}

// ExecRequest holds every input the agent controls for `prism container
// exec`. It is also the wire schema of POST /container/exec. The response
// is a RunResult.
type ExecRequest struct {
	Handle         string   `json:"handle"`
	Command        []string `json:"command"`
	TimeoutSeconds int64    `json:"timeout_seconds,omitempty"`
}

// HandleRequest names a service container. It is the wire schema of POST
// /container/logs and POST /container/stop. The response is a RunResult.
type HandleRequest struct {
	Handle string `json:"handle"`
}

// handlePattern accepts a handle, a container name, and a container ID.
// The value never reaches podman: prism gives podman the ID that it
// resolved.
var handlePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// minIDPrefix is the shortest container ID prefix that resolveService
// accepts. It is the length of the short ID that podman prints.
const minIDPrefix = 12

func (e psEntry) matches(handle string) bool {
	return e.Labels[LabelService] == handle || slices.Contains(e.Names, handle) ||
		e.ID == handle || (len(handle) >= minIDPrefix && strings.HasPrefix(e.ID, handle))
}

func (e psEntry) name() string {
	if len(e.Names) > 0 {
		return e.Names[0]
	}
	return e.ID
}

// resolveService returns the service container of caller c that handle
// names. handle can be the handle that start printed, the container name,
// the container ID, or an ID prefix of at least 12 characters.
//
// Ownership is the instance label of the container, never the name. A
// container of another session, a container with no prism label, and a
// container of the session that start did not create give a refusal
// message. A *podmanError means that podman did not answer.
func resolveService(ctx context.Context, r Runner, c Caller, handle string) (psEntry, string, error) {
	if !handlePattern.MatchString(handle) {
		return psEntry{}, fmt.Sprintf("handle %q is not valid: give the handle that prism container start printed", handle), nil
	}
	entries, err := listLabelled(ctx, r)
	if err != nil {
		return psEntry{}, "", err
	}
	var own, other []psEntry
	for _, e := range entries {
		if !e.matches(handle) {
			continue
		}
		if e.Labels[LabelInstanceID] == c.InstanceID {
			own = append(own, e)
		} else {
			other = append(other, e)
		}
	}
	switch {
	case len(own) == 1 && own[0].Labels[LabelService] == "":
		return psEntry{}, fmt.Sprintf("container %q was not created by prism container start: exec, logs, and stop act only on a service container", handle), nil
	case len(own) == 1:
		return own[0], "", nil
	case len(own) > 1:
		return psEntry{}, fmt.Sprintf("handle %q names more than one container of this session: give the full container ID", handle), nil
	case len(other) > 0:
		return psEntry{}, fmt.Sprintf("container %q does not carry the label of this session", handle), nil
	}
	return psEntry{}, fmt.Sprintf("service container %q does not exist in this session", handle), nil
}

// serviceOp is the shared frame of exec, logs, and stop: the audit line,
// the handle check, and the result.
type serviceOp struct {
	d      Deps
	audit  *auditLog
	entry  AuditEntry
	stdout *tailBuffer
	stderr *tailBuffer
}

func (o *serviceOp) result(code int, msg string) RunResult {
	r := RunResult{ExitCode: code, Message: msg}
	r.Stdout, r.StdoutDropped = o.stdout.snapshot()
	r.Stderr, r.StderrDropped = o.stderr.snapshot()
	return r
}

func (o *serviceOp) finish(decision string, code int, ran bool, msg string) RunResult {
	o.entry.Decision = decision
	o.entry.Reason = msg
	if ran {
		o.entry.ExitCode = &code
	}
	if err := o.audit.write(o.entry, o.d.Now()); err != nil {
		msg = joinMessages(msg, "warning: prism did not write the audit log: "+err.Error())
	}
	return o.result(code, msg)
}

func (o *serviceOp) refuse(reason string) RunResult {
	return o.finish(DecisionRefused, ExitRefused, false, "refused: "+reason)
}

// openServiceOp opens the audit log for one exec, logs, or stop request.
// It returns a result when the request ends before it starts.
func openServiceOp(d Deps, c Caller, command, handle string) (*serviceOp, *RunResult) {
	o := &serviceOp{d: d, stdout: newTailBuffer(OutputLimit), stderr: newTailBuffer(OutputLimit)}
	if container.InstanceTokenForID(c.InstanceID) == "" {
		r := o.result(ExitRefused, "refused: "+c.validateIdentity().Error())
		return nil, &r
	}
	audit, err := openAuditLog(c.InstanceID)
	if err != nil {
		r := o.result(ExitRefused, "refused: prism cannot write the audit log: "+err.Error())
		return nil, &r
	}
	o.audit = audit
	o.entry = AuditEntry{Session: c.SessionName, InstanceID: c.InstanceID, Command: command, Handle: handle}
	return o, nil
}

// resolve finds the service container, or returns the result of a refusal
// or a podman failure.
func (o *serviceOp) resolve(ctx context.Context, c Caller, handle string) (psEntry, *RunResult) {
	e, msg, err := resolveService(ctx, o.d.Runner, c, handle)
	if err != nil {
		r := o.finish(DecisionError, ExitRefused, false, unreachableMessage(o.d.GOOS, err.Error()))
		return psEntry{}, &r
	}
	if msg != "" {
		r := o.refuse(msg)
		return psEntry{}, &r
	}
	o.entry.Container = e.name()
	o.entry.Image = e.Image
	return e, nil
}

// Exec runs one command in a service container of caller c. It does not
// count toward a limit, because it starts no container.
func Exec(ctx context.Context, d Deps, c Caller, req ExecRequest) RunResult {
	d = d.withDefaults()
	o, early := openServiceOp(d, c, "exec", req.Handle)
	if early != nil {
		return *early
	}
	defer o.audit.close()
	o.entry.Args = req.Command
	o.entry.TimeoutSeconds = req.TimeoutSeconds

	if len(req.Command) == 0 {
		return o.refuse("exec needs a command: put it after --, for example prism container exec HANDLE -- echo hello")
	}
	timeout, err := validTimeout(req.TimeoutSeconds, DefaultTimeout)
	if err != nil {
		return o.refuse(err.Error())
	}
	o.entry.TimeoutSeconds = int64(timeout / time.Second)
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	e, early := o.resolve(execCtx, c, req.Handle)
	if early != nil {
		return *early
	}
	code, err := d.Runner.Run(execCtx, o.stdout, o.stderr, execArgs(e.ID, req.Command)...)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return o.finish(DecisionAllowed, ExitRefused, false, "the request was cancelled, so prism stopped podman exec. The command can still run in the service container")
		case execCtx.Err() != nil:
			return o.finish(DecisionAllowed, ExitTimeout, false,
				fmt.Sprintf("the --timeout of %s expired, so prism stopped podman exec. The command can still run in the service container", timeout))
		}
		return o.finish(DecisionError, ExitRefused, false, unreachableMessage(d.GOOS, err.Error()))
	}
	return o.finish(DecisionAllowed, code, true, "")
}

// Logs returns the logs of a service container of caller c.
func Logs(ctx context.Context, d Deps, c Caller, req HandleRequest) RunResult {
	d = d.withDefaults()
	o, early := openServiceOp(d, c, "logs", req.Handle)
	if early != nil {
		return *early
	}
	defer o.audit.close()
	logsCtx, cancel := context.WithTimeout(ctx, logsTimeout)
	defer cancel()

	e, early := o.resolve(logsCtx, c, req.Handle)
	if early != nil {
		return *early
	}
	code, err := d.Runner.Run(logsCtx, o.stdout, o.stderr, "logs", e.ID)
	if err != nil {
		if logsCtx.Err() != nil {
			return o.finish(DecisionAllowed, ExitRefused, false, fmt.Sprintf("podman logs did not complete within %s", logsTimeout))
		}
		return o.finish(DecisionError, ExitRefused, false, unreachableMessage(d.GOOS, err.Error()))
	}
	if code != 0 {
		return o.finish(DecisionError, ExitRefused, true, fmt.Sprintf("podman logs exited %d", code))
	}
	return o.finish(DecisionAllowed, 0, true, "")
}

// Stop stops and removes a service container of caller c.
func Stop(ctx context.Context, d Deps, c Caller, req HandleRequest) RunResult {
	d = d.withDefaults()
	o, early := openServiceOp(d, c, "stop", req.Handle)
	if early != nil {
		return *early
	}
	defer o.audit.close()
	stopCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	e, early := o.resolve(stopCtx, c, req.Handle)
	if early != nil {
		return *early
	}
	var podmanErr bytes.Buffer
	code, err := d.Runner.Run(stopCtx, io.Discard, &podmanErr, "rm", "--force", "--ignore", "--volumes", "--time", stopGrace, e.ID)
	if err != nil {
		return o.finish(DecisionError, ExitRefused, false, unreachableMessage(d.GOOS, err.Error()))
	}
	if code != 0 {
		return o.finish(DecisionError, ExitRefused, true,
			fmt.Sprintf("podman rm exited %d: %s", code, strings.TrimSpace(podmanErr.String())))
	}
	return o.finish(DecisionAllowed, 0, true, "")
}

// ListedContainer is one container in a ListResult.
type ListedContainer struct {
	// Handle is the handle of a service container, or the name of another
	// container.
	Handle string        `json:"handle"`
	Kind   string        `json:"kind"`
	State  string        `json:"state"`
	Image  string        `json:"image"`
	Ports  []PortMapping `json:"ports,omitempty"`
}

// Container kinds in a ListResult. A build is the anchor container of a
// macOS build.
const (
	KindService = "service"
	KindRun     = "run"
	KindBuild   = "build"
)

// ListResult is the outcome of one `prism container list` request. It is
// also the wire schema of the POST /container/list response.
type ListResult struct {
	ExitCode   int               `json:"exit_code"`
	Containers []ListedContainer `json:"containers,omitempty"`
	// Images are the names of the images that the session built. Give a
	// name to prism container run or prism container start.
	Images  []string `json:"images,omitempty"`
	Message string   `json:"message,omitempty"`
}

// List returns the containers and the built images of caller c.
func List(ctx context.Context, d Deps, c Caller) ListResult {
	d = d.withDefaults()
	o, early := openServiceOp(d, c, "list", "")
	if early != nil {
		return ListResult{ExitCode: early.ExitCode, Message: early.Message}
	}
	defer o.audit.close()
	failed := func(err error) ListResult {
		r := o.finish(DecisionError, ExitRefused, false, unreachableMessage(d.GOOS, err.Error()))
		return ListResult{ExitCode: r.ExitCode, Message: r.Message}
	}
	listCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	entries, err := listLabelled(listCtx, d.Runner)
	if err != nil {
		return failed(err)
	}
	var res ListResult
	for _, e := range entries {
		if e.Labels[LabelInstanceID] != c.InstanceID {
			continue
		}
		lc := ListedContainer{Handle: e.Labels[LabelService], Kind: KindService, State: e.State, Image: e.Image}
		if lc.Handle == "" {
			lc.Handle, lc.Kind = e.name(), KindRun
			if anchorBuildName(e) != "" {
				lc.Kind = KindBuild
			}
		}
		for _, p := range e.Ports {
			lc.Ports = append(lc.Ports, PortMapping{ContainerPort: p.ContainerPort, HostIP: p.HostIP, HostPort: p.HostPort})
		}
		res.Containers = append(res.Containers, lc)
	}
	images, err := listImages(listCtx, d.Runner, false)
	if err != nil {
		return failed(err)
	}
	for _, img := range images {
		if img.Labels[LabelInstanceID] != c.InstanceID {
			continue
		}
		res.Images = append(res.Images, img.Names...)
	}
	if r := o.finish(DecisionAllowed, 0, false, ""); r.Message != "" {
		res.Message = r.Message
	}
	return res
}

// WriteListResult writes a ListResult to the streams of the `prism
// container list` process. Both routes use it.
func WriteListResult(stdout, stderr io.Writer, r ListResult) {
	if r.Message != "" {
		fmt.Fprintf(stderr, "prism container list: %s\n", r.Message)
	}
	if r.ExitCode != 0 {
		return
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "HANDLE\tKIND\tSTATE\tIMAGE\tPORTS")
	for _, c := range r.Containers {
		var ports []string
		for _, p := range c.Ports {
			ports = append(ports, fmt.Sprintf("%d->%s:%d", p.ContainerPort, p.HostIP, p.HostPort))
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", c.Handle, c.Kind, c.State, c.Image, strings.Join(ports, ","))
	}
	_ = w.Flush()
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "BUILT IMAGE")
	for _, img := range r.Images {
		fmt.Fprintln(stdout, img)
	}
}
