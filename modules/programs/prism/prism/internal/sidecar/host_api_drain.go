package sidecar

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultHostAPIDrainTimeout is the bound of CloseHostAPI when
// Config.HostAPIDrainTimeout is zero.
const DefaultHostAPIDrainTimeout = 5 * time.Second

// hostAPIStoppingMessage is the error of a host-API request that arrives
// after CloseHostAPI started.
const hostAPIStoppingMessage = "the sidecar of this session is stopping, so it does not start a new host-API request"

// hostAPIDrain records the host-API handlers that run, for CloseHostAPI.
type hostAPIDrain struct {
	mu      sync.Mutex
	stopped bool
	nextID  uint64
	running map[uint64]runningHostAPIHandler
	// idle is closed when no handler runs after stop.
	idle chan struct{}
	// deadline is the end of the bound. stop sets it.
	deadline time.Time

	closeOnce sync.Once
}

type runningHostAPIHandler struct {
	method string
	path   string
	start  time.Time
}

// enter records a handler that starts. After stop it returns false, and
// the handler must not run.
func (d *hostAPIDrain) enter(r *http.Request) (uint64, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return 0, false
	}
	if d.running == nil {
		d.running = map[uint64]runningHostAPIHandler{}
	}
	d.nextID++
	d.running[d.nextID] = runningHostAPIHandler{method: r.Method, path: r.URL.Path, start: time.Now()}
	return d.nextID, true
}

func (d *hostAPIDrain) exit(id uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.running, id)
	if d.stopped && len(d.running) == 0 {
		close(d.idle)
	}
}

// stop makes enter refuse each later handler, and sets the end of the
// bound.
func (d *hostAPIDrain) stop(deadline time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped = true
	d.deadline = deadline
	d.idle = make(chan struct{})
	if len(d.running) == 0 {
		close(d.idle)
	}
}

// wait returns when no handler runs or at the end of the bound. It returns
// each handler that still runs, the oldest first.
func (d *hostAPIDrain) wait() []string {
	d.mu.Lock()
	idle, deadline := d.idle, d.deadline
	d.mu.Unlock()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-idle:
	case <-timer.C:
	}

	d.mu.Lock()
	running := make([]runningHostAPIHandler, 0, len(d.running))
	for _, h := range d.running {
		running = append(running, h)
	}
	d.mu.Unlock()
	sort.Slice(running, func(i, j int) bool { return running[i].start.Before(running[j].start) })
	now := time.Now()
	out := make([]string, len(running))
	for i, h := range running {
		out[i] = fmt.Sprintf("%s %s (runs for %s)", h.method, h.path, now.Sub(h.start).Round(time.Millisecond))
	}
	return out
}

// HostAPIDrainError reports the host-API handlers that still run at the end
// of the bound of CloseHostAPI.
type HostAPIDrainError struct {
	Session  string
	Timeout  time.Duration
	Handlers []string
}

func (e *HostAPIDrainError) Error() string {
	return fmt.Sprintf("sidecar of session %q: %d host-API handler(s) still run after %s: %s",
		e.Session, len(e.Handlers), e.Timeout, strings.Join(e.Handlers, ", "))
}

// trackHostAPIHandler records each host-API handler for CloseHostAPI. After
// CloseHostAPI starts, it refuses each request before a route runs.
func (s *Sidecar) trackHostAPIHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := s.hostAPIDrain.enter(r)
		if !ok {
			b, _ := json.Marshal(map[string]string{"error": hostAPIStoppingMessage})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write(b)
			return
		}
		defer s.hostAPIDrain.exit(id)
		next.ServeHTTP(w, r)
	})
}

// CloseHostAPI closes the host-API listeners and waits until each host-API
// handler that runs returns, at most until the end of the bound
// (Config.HostAPIDrainTimeout). No handler starts after the call begins.
//
// The bound starts at the first call. A later call waits until the same
// end, and then reports the handlers that run at that time.
//
// It returns a *HostAPIDrainError when a handler still runs. The sidecar
// must then keep its PID file. A handler can start a podman client, and a
// later stop finds the process group of that client only through the PID
// file (#3391).
func (s *Sidecar) CloseHostAPI() error {
	timeout := s.cfg.HostAPIDrainTimeout
	if timeout <= 0 {
		timeout = DefaultHostAPIDrainTimeout
	}
	s.hostAPIDrain.closeOnce.Do(func() { s.closeHostAPIServers(time.Now().Add(timeout)) })
	if running := s.hostAPIDrain.wait(); len(running) > 0 {
		return &HostAPIDrainError{Session: s.cfg.SessionName, Timeout: timeout, Handlers: running}
	}
	return nil
}

// closeHostAPIServers stops new handlers and closes the host-API servers.
// Each server waits for its open connections until deadline, and then
// closes the connections that are left.
func (s *Sidecar) closeHostAPIServers(deadline time.Time) {
	s.hostAPIDrain.stop(deadline)

	s.mu.Lock()
	servers := []*http.Server{s.hostAPISrv, s.hostAPITCPSrv}
	listeners := []net.Listener{s.hostAPIListener, s.hostAPITCPListener}
	s.mu.Unlock()

	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	var wg sync.WaitGroup
	for i, srv := range servers {
		if srv == nil {
			if listeners[i] != nil {
				_ = listeners[i].Close()
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if srv.Shutdown(ctx) != nil {
				_ = srv.Close()
			}
		}()
	}
	wg.Wait()
}
