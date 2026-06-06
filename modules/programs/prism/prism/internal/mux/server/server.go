package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/prismatic-koi/prism/internal/mux/pane"
)

// maxRequestBodyBytes caps inbound JSON payloads. The largest legitimate
// body is pane.send_input — a single keystroke or paste from the CLI.
// 1 MiB is comfortable headroom while still bounding memory exposure
// to malformed or hostile clients.
const maxRequestBodyBytes = 1 << 20 // 1 MiB

// shutdownGrace is the timeout passed to http.Server.Shutdown when the
// caller's context is cancelled. It bounds how long in-flight requests
// have to drain before we close their connections.
const shutdownGrace = 2 * time.Second

// Server is the prism mux Unix-socket API server. It owns the
// SessionTree (pane model) and the runtime ptyRegistry that tracks
// the live *pty.Session + *vt.Host pair for each pane that was
// created with a non-empty argv. The model is intentionally pure data
// — the runtime side lives here so the pane package never has to
// import os/exec or the pty package.
//
// Concurrency:
//
//   - The SessionTree has its own mutex; we never lock it externally.
//   - The ptyRegistry has its own mutex.
//   - The two are sequenced inside each handler: we always touch the
//     tree first (so a 4xx fails before any side effects on the
//     runtime) and then the registry.
//
// The zero value is NOT ready for use — construct with New.
type Server struct {
	tree   *pane.SessionTree
	ptys   *ptyRegistry
	logger *log.Logger
}

// Option configures a Server constructed by New.
type Option func(*Server)

// WithLogger overrides the default logger (log.Default()). Useful in
// tests that want to silence warnings.
func WithLogger(l *log.Logger) Option {
	return func(s *Server) {
		if l != nil {
			s.logger = l
		}
	}
}

// New returns a Server backed by tree. Panics if tree is nil — the
// server is meaningless without a model to mutate, and a panic at
// construction time is preferable to a nil-deref deep inside a handler.
func New(tree *pane.SessionTree, opts ...Option) *Server {
	if tree == nil {
		panic("mux/server: New called with nil tree")
	}
	s := &Server{
		tree:   tree,
		ptys:   newPTYRegistry(),
		logger: log.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Close releases every live PTY host. Tests use this to ensure their
// spawned children are reaped before t.Cleanup runs; production
// callers do not need it (process exit cleans up).
func (s *Server) Close() {
	if s == nil || s.ptys == nil {
		return
	}
	s.ptys.closeAll()
}

// Tree returns the SessionTree the server is bound to. Provided so
// tests and in-process callers can introspect server state without
// going through the socket.
func (s *Server) Tree() *pane.SessionTree { return s.tree }

// Handler returns the http.Handler implementing the mux API. Exposed
// independently of ListenAndServe so in-process tests can drive the
// server with httptest.NewServer or a custom listener.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/session/create", s.handleSessionCreate)
	mux.HandleFunc("/session/destroy", s.handleSessionDestroy)
	mux.HandleFunc("/session/list", s.handleSessionList)
	mux.HandleFunc("/session/switch", s.handleSessionSwitch)

	mux.HandleFunc("/pane/create", s.handlePaneCreate)
	mux.HandleFunc("/pane/destroy", s.handlePaneDestroy)
	mux.HandleFunc("/pane/list", s.handlePaneList)
	mux.HandleFunc("/pane/switch", s.handlePaneSwitch)
	mux.HandleFunc("/pane/resize", s.handlePaneResize)
	mux.HandleFunc("/pane/send_input", s.handlePaneSendInput)
	mux.HandleFunc("/pane/read_output", s.handlePaneReadOutput)

	// Fall-through: anything not matched above is a 404 with a structured
	// body so the CLI client gets a consistent error shape regardless of
	// which path it dialed.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, codeBadRequest,
			fmt.Sprintf("no such method: %s %s", r.Method, r.URL.Path), nil)
	})

	return mux
}

// ListenAndServe binds a Unix socket at sockPath and serves the API
// until ctx is cancelled or the listener fails. When sockPath is empty,
// DefaultSocketPath() is used.
//
// Lifecycle details:
//
//   - The socket's parent directory is created with mode 0700 if it
//     does not exist (matches the sidecar's host-API setup —
//     internal/sidecar/sidecar.go).
//   - A stale socket file at sockPath is unlinked before bind so a
//     prior unclean shutdown does not block startup. This mirrors the
//     sidecar's "_ = os.Remove(...)" before net.Listen.
//   - On ctx cancellation, http.Server.Shutdown is called with a
//     bounded grace period so in-flight handlers drain.
//   - On return, the socket file is unlinked (best-effort) so the next
//     start does not have to discover stale state.
func (s *Server) ListenAndServe(ctx context.Context, sockPath string) error {
	if sockPath == "" {
		var err error
		sockPath, err = DefaultSocketPath()
		if err != nil {
			return fmt.Errorf("resolve default socket path: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		return fmt.Errorf("create socket dir %q: %w", filepath.Dir(sockPath), err)
	}
	// Clear stale socket; the error is ignored intentionally — if the
	// path does not exist we fall through to bind, and if it cannot be
	// removed the bind below will surface the real failure.
	_ = os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("listen unix %q: %w", sockPath, err)
	}

	return s.Serve(ctx, ln)
}

// Serve drives the API server against an already-bound listener. The
// listener is closed when the function returns. Useful in tests where
// a t.TempDir()-scoped socket is created externally so that on a
// failing assertion the path is preserved for inspection.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler: s.Handler(),
		// Per-request read/write timeouts are intentionally NOT set:
		// our handlers do not stream, and a Unix-socket peer can stall
		// for legitimate reasons (a CLI process briefly suspended).
		// Body size is bounded inside each handler via
		// http.MaxBytesReader instead.
	}

	// Track listener close so the goroutine below does not race with
	// the deferred ln.Close.
	var closed sync.Once
	closeLn := func() { closed.Do(func() { _ = ln.Close() }) }
	defer closeLn()

	// Shutdown when ctx is done. Use a fresh context bounded by
	// shutdownGrace so even a stuck handler cannot keep us alive.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			s.logger.Printf("mux/server: shutdown: %v", err)
		}
		closeLn()
	}()

	serveErr := srv.Serve(ln)
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) && !errors.Is(serveErr, net.ErrClosed) {
		return fmt.Errorf("serve: %w", serveErr)
	}

	// Best-effort: unlink the Unix socket so the next start does not
	// have to clean up. We only do this for Unix listeners; other
	// listener kinds (e.g. an httptest one) have no path to remove.
	if addr := ln.Addr(); addr != nil && addr.Network() == "unix" {
		_ = os.Remove(addr.String())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Request decoding helpers
// ---------------------------------------------------------------------------

// requireMethod writes 405 and returns false if r.Method does not match.
// Logging is deliberately omitted — wrong-method requests are routine
// from buggy clients and do not warrant log noise.
func requireMethod(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Method != want {
		writeError(w, http.StatusMethodNotAllowed, codeMethodNotAllowed,
			fmt.Sprintf("method not allowed: want %s, got %s", want, r.Method), nil)
		return false
	}
	return true
}

// decodeJSON reads a length-capped JSON body into dst. On error it
// writes a 400 with codeBadRequest and returns false. An empty body
// (Content-Length 0 or EOF on first read) decodes as the zero value of
// dst, which is the natural shape for endpoints like /session/list.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		// An empty body returns io.EOF — treat that as "no input",
		// which is fine for endpoints whose request struct is all
		// optional. Endpoints with required fields validate them
		// below regardless of this path.
		if errors.Is(err, io.EOF) {
			return true
		}
		writeError(w, http.StatusBadRequest, codeBadRequest,
			fmt.Sprintf("decode json: %v", err), nil)
		return false
	}
	return true
}

// writeJSON serialises v with a 200 OK. On marshal failure it falls
// back to writeError(500, internal_error, ...). The success path sets
// Content-Type before WriteHeader so net/http does not auto-detect.
func writeJSON(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, codeInternal,
			fmt.Sprintf("marshal response: %v", err), nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// ---------------------------------------------------------------------------
// session.* handlers
// ---------------------------------------------------------------------------

// sessionCreateRequest is the wire shape for POST /session/create. The
// fields mirror pane.Session 1:1; we keep this struct distinct so the
// model can grow internal fields without forcing a wire change.
type sessionCreateRequest struct {
	ID          string      `json:"id"`
	Repo        string      `json:"repo,omitempty"`
	Branch      string      `json:"branch,omitempty"`
	Worktree    string      `json:"worktree,omitempty"`
	AgentRole   string      `json:"agent_role,omitempty"`
	SidecarAddr string      `json:"sidecar_addr,omitempty"`
	ParentID    string      `json:"parent_id,omitempty"`
	Panes       []paneInput `json:"panes,omitempty"`
	ActivePane  string      `json:"active_pane,omitempty"`
}

// paneInput is the JSON shape for pane entries inside a
// sessionCreateRequest. Distinct from pane.Pane so the wire format is
// pinned independently of any internal fields the model may grow.
type paneInput struct {
	Name string `json:"name"`
}

// sessionResponse wraps a pane.Session for transport. The model's JSON
// tags already include all the fields we want, so this is a thin
// envelope rather than a remapping.
type sessionResponse struct {
	Session pane.Session `json:"session"`
}

func (s *Server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req sessionCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"id is required", nil)
		return
	}

	panes := make([]pane.Pane, 0, len(req.Panes))
	for _, p := range req.Panes {
		panes = append(panes, pane.Pane{Name: p.Name})
	}

	sess := pane.Session{
		ID:          req.ID,
		Repo:        req.Repo,
		Branch:      req.Branch,
		Worktree:    req.Worktree,
		AgentRole:   req.AgentRole,
		SidecarAddr: req.SidecarAddr,
		ParentID:    req.ParentID,
		Panes:       panes,
		ActivePane:  req.ActivePane,
	}
	if err := s.tree.AddSession(sess); err != nil {
		writePaneErr(w, err, map[string]any{"id": req.ID})
		return
	}

	// Return the canonical post-insert view so callers can observe any
	// auto-applied defaults (e.g. ActivePane defaulting to the first
	// pane) without a follow-up list call.
	created, ok := s.tree.Session(req.ID)
	if !ok {
		// Should be impossible immediately after a successful
		// AddSession — log and return a structured 500 rather than
		// panic so the CLI sees a clean error.
		s.logger.Printf("mux/server: session %q vanished between AddSession and Session()", req.ID)
		writeError(w, http.StatusInternalServerError, codeInternal,
			"session vanished after create", map[string]any{"id": req.ID})
		return
	}
	writeJSON(w, sessionResponse{Session: created})
}

// sessionDestroyRequest is the wire shape for POST /session/destroy.
type sessionDestroyRequest struct {
	ID string `json:"id"`
}

func (s *Server) handleSessionDestroy(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req sessionDestroyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"id is required", nil)
		return
	}
	if err := s.tree.RemoveSession(req.ID); err != nil {
		writePaneErr(w, err, map[string]any{"id": req.ID})
		return
	}
	// Cascade PTY teardown for every pane that belonged to the destroyed
	// session. Done after the model accepts the remove so a 4xx returns
	// without killing children.
	for _, host := range s.ptys.removeSession(req.ID) {
		if err := host.Close(); err != nil && s.logger != nil {
			s.logger.Printf("mux/server: session.destroy cascade close (%s): %v",
				req.ID, err)
		}
	}
	writeJSON(w, struct{}{})
}

// sessionListResponse is the wire shape for GET /session/list. The
// sessions are in the same order the underlying tree iterates them:
// repo-cluster-major, then top-level by insertion, then children.
type sessionListResponse struct {
	Sessions      []pane.Session `json:"sessions"`
	ActiveSession string         `json:"active_session,omitempty"`
}

func (s *Server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	sessions := s.tree.Sessions()
	// Sessions() may return nil for an empty tree; normalise to an
	// empty slice so the wire shape is "sessions":[] not
	// "sessions":null, which would force the client to special-case
	// the JSON null.
	if sessions == nil {
		sessions = []pane.Session{}
	}
	writeJSON(w, sessionListResponse{
		Sessions:      sessions,
		ActiveSession: s.tree.ActiveSessionID(),
	})
}

// sessionSwitchRequest is the wire shape for POST /session/switch. An
// empty ID clears the focus pointer (matches
// pane.SessionTree.ActivateSession("")).
type sessionSwitchRequest struct {
	ID string `json:"id"`
}

// sessionSwitchResponse echoes the new active-session pointer so the
// caller does not need a follow-up list call to confirm.
type sessionSwitchResponse struct {
	ActiveSession string `json:"active_session"`
}

func (s *Server) handleSessionSwitch(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req sessionSwitchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.tree.ActivateSession(req.ID); err != nil {
		writePaneErr(w, err, map[string]any{"id": req.ID})
		return
	}
	writeJSON(w, sessionSwitchResponse{ActiveSession: s.tree.ActiveSessionID()})
}

// ---------------------------------------------------------------------------
// pane.* handlers
// ---------------------------------------------------------------------------

// paneCreateRequest is the wire shape for POST /pane/create.
//
// Argv (when non-empty) tells the server to spawn the process under a
// PTY and host its output in a vt.Host. The pane row in the model is
// runtime-agnostic; the PTY handle and the VT engine live in the
// server's internal ptyRegistry, keyed by (SessionID, Name).
//
// Cwd is the child's working directory. Env, when non-nil, replaces
// the daemon's environment. A nil Env means "inherit the daemon's
// env" — the typical CLI shape.
//
// Cols / Rows are the initial PTY geometry. Zero falls back to a
// conventional 80x24; the renderer resizes on first paint.
//
// When Argv is empty the pane is created as a pure data-model entry
// with no PTY — useful for tests and for the legacy "validate-only"
// behaviour from before #2158.
type paneCreateRequest struct {
	SessionID string            `json:"session_id"`
	Name      string            `json:"name"`
	Argv      []string          `json:"argv,omitempty"`
	Cwd       string            `json:"cwd,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	Cols      uint16            `json:"cols,omitempty"`
	Rows      uint16            `json:"rows,omitempty"`
}

func (s *Server) handlePaneCreate(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req paneCreateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SessionID == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"session_id and name are required", nil)
		return
	}
	// Model insertion first so a duplicate / unknown-session error is
	// reported before we go to the work of spawning a child process.
	if err := s.tree.AddPane(req.SessionID, pane.Pane{Name: req.Name}); err != nil {
		writePaneErr(w, err, map[string]any{
			"session_id": req.SessionID,
			"name":       req.Name,
		})
		return
	}

	// Runtime side: only spawn a PTY when the caller supplied an argv.
	// An empty argv is the legacy "validate-only" shape — the pane row
	// is created in the model and that is all.
	if len(req.Argv) > 0 {
		host, err := startPTYHost(req.Argv, req.Cwd, req.Env, req.Cols, req.Rows, s.logger)
		if err != nil {
			// Roll the model insertion back so a failed spawn leaves no
			// trace — callers retrying see a clean slate.
			if rmErr := s.tree.RemovePane(req.SessionID, req.Name); rmErr != nil && s.logger != nil {
				s.logger.Printf("mux/server: pane.create rollback (RemovePane): %v", rmErr)
			}
			writeError(w, http.StatusInternalServerError, codeInternal,
				fmt.Sprintf("spawn pty: %v", err),
				map[string]any{
					"session_id": req.SessionID,
					"name":       req.Name,
				})
			return
		}
		if err := s.ptys.add(req.SessionID, req.Name, host); err != nil {
			// Programming-error path: the model said the pane was new,
			// but the registry already had a host. Clean both up so the
			// next call sees a consistent state, then report 500.
			_ = host.Close()
			if rmErr := s.tree.RemovePane(req.SessionID, req.Name); rmErr != nil && s.logger != nil {
				s.logger.Printf("mux/server: pane.create rollback (RemovePane): %v", rmErr)
			}
			writeError(w, http.StatusInternalServerError, codeInternal,
				fmt.Sprintf("register pty host: %v", err),
				map[string]any{
					"session_id": req.SessionID,
					"name":       req.Name,
				})
			return
		}
	}
	writeJSON(w, struct{}{})
}

// paneDestroyRequest is the wire shape for POST /pane/destroy.
type paneDestroyRequest struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
}

func (s *Server) handlePaneDestroy(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req paneDestroyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SessionID == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"session_id and name are required", nil)
		return
	}
	if err := s.tree.RemovePane(req.SessionID, req.Name); err != nil {
		writePaneErr(w, err, map[string]any{
			"session_id": req.SessionID,
			"name":       req.Name,
		})
		return
	}
	// Runtime teardown after the model side has accepted the destroy.
	// SIGTERM → (destroyGrace) → SIGKILL, then close the master FD so
	// the pump goroutines unblock. Best-effort: a destroy can never
	// fail the API call once the model side has succeeded.
	if host := s.ptys.remove(req.SessionID, req.Name); host != nil {
		if err := host.Close(); err != nil && s.logger != nil {
			s.logger.Printf("mux/server: pane.destroy close pty (%s/%s): %v",
				req.SessionID, req.Name, err)
		}
	}
	writeJSON(w, struct{}{})
}

// paneListResponse is the wire shape for GET /pane/list. The slice is
// in pane insertion order — the same order NextPane / PrevPane cycle.
type paneListResponse struct {
	Panes      []pane.Pane `json:"panes"`
	ActivePane string      `json:"active_pane,omitempty"`
}

func (s *Server) handlePaneList(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	sessID := r.URL.Query().Get("session_id")
	if sessID == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"session_id query parameter is required", nil)
		return
	}
	sess, ok := s.tree.Session(sessID)
	if !ok {
		writeError(w, http.StatusNotFound, codeSessionNotFound,
			fmt.Sprintf("session %q not found", sessID),
			map[string]any{"session_id": sessID})
		return
	}
	// sess.Panes is already a deep copy (Session() clones), so we can
	// hand it straight to writeJSON. Normalise nil to []pane.Pane{} so
	// the wire shape never carries "panes":null.
	panes := sess.Panes
	if panes == nil {
		panes = []pane.Pane{}
	}
	writeJSON(w, paneListResponse{
		Panes:      panes,
		ActivePane: sess.ActivePane,
	})
}

// paneSwitchRequest is the wire shape for POST /pane/switch. Exactly
// one of {Name, Direction} must be set. Direction may be "next" or
// "prev"; everything else is rejected with a 400.
type paneSwitchRequest struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name,omitempty"`
	Direction string `json:"direction,omitempty"`
}

// paneSwitchResponse echoes the resulting active pane so the caller
// can confirm without a follow-up pane.list.
type paneSwitchResponse struct {
	ActivePane string `json:"active_pane"`
}

func (s *Server) handlePaneSwitch(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req paneSwitchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SessionID == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"session_id is required", nil)
		return
	}
	// XOR-ish validation: exactly one of name / direction.
	hasName := req.Name != ""
	hasDir := req.Direction != ""
	if hasName == hasDir {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"exactly one of name or direction must be set", nil)
		return
	}

	var (
		newActive string
		err       error
	)
	switch {
	case hasName:
		err = s.tree.ActivatePane(req.SessionID, req.Name)
		newActive = req.Name
	case req.Direction == "next":
		newActive, err = s.tree.NextPane(req.SessionID)
	case req.Direction == "prev":
		newActive, err = s.tree.PrevPane(req.SessionID)
	default:
		writeError(w, http.StatusBadRequest, codeBadRequest,
			fmt.Sprintf("invalid direction %q (want \"next\" or \"prev\")", req.Direction), nil)
		return
	}
	if err != nil {
		writePaneErr(w, err, map[string]any{
			"session_id": req.SessionID,
			"name":       req.Name,
			"direction":  req.Direction,
		})
		return
	}
	writeJSON(w, paneSwitchResponse{ActivePane: newActive})
}

// paneResizeRequest is the wire shape for POST /pane/resize. Cols and
// Rows are validated as non-negative; the actual resize effect lands in
// the PTY layer (separate package) but this handler still validates
// session/pane existence so the CLI gets a structured error today.
type paneResizeRequest struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Cols      int    `json:"cols"`
	Rows      int    `json:"rows"`
}

func (s *Server) handlePaneResize(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req paneResizeRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SessionID == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"session_id and name are required", nil)
		return
	}
	if req.Cols < 0 || req.Rows < 0 {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"cols and rows must be non-negative",
			map[string]any{"cols": req.Cols, "rows": req.Rows})
		return
	}
	if err := s.checkPaneExists(req.SessionID, req.Name); err != nil {
		writePaneErr(w, err, map[string]any{
			"session_id": req.SessionID,
			"name":       req.Name,
		})
		return
	}
	// Runtime side: forward to the live PTY when one exists. A pane
	// created without an argv has no PTY — in that case the model
	// accepts the call and we return 200 with no effect, matching the
	// pre-#2158 wire contract.
	if host, ok := s.ptys.get(req.SessionID, req.Name); ok {
		if err := host.Resize(uint16(req.Cols), uint16(req.Rows)); err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				fmt.Sprintf("resize pty: %v", err),
				map[string]any{
					"session_id": req.SessionID,
					"name":       req.Name,
					"cols":       req.Cols,
					"rows":       req.Rows,
				})
			return
		}
	}
	writeJSON(w, struct{}{})
}

// paneSendInputRequest is the wire shape for POST /pane/send_input.
// Data is sent as a string — callers that need to deliver binary
// keystrokes can encode them however the eventual PTY-layer client
// agrees on (base64 is the most likely scheme). At this layer the
// payload is opaque and not interpreted; only its length is bounded
// by the body cap.
type paneSendInputRequest struct {
	SessionID string `json:"session_id"`
	Name      string `json:"name"`
	Data      string `json:"data"`
}

func (s *Server) handlePaneSendInput(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodPost) {
		return
	}
	var req paneSendInputRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SessionID == "" || req.Name == "" {
		writeError(w, http.StatusBadRequest, codeBadRequest,
			"session_id and name are required", nil)
		return
	}
	if err := s.checkPaneExists(req.SessionID, req.Name); err != nil {
		writePaneErr(w, err, map[string]any{
			"session_id": req.SessionID,
			"name":       req.Name,
		})
		return
	}
	// Runtime side: write the bytes to the PTY when a host is
	// registered. As with resize, a pane without a PTY is the
	// legacy validate-only shape and the handler returns 200 with
	// no effect.
	if host, ok := s.ptys.get(req.SessionID, req.Name); ok {
		if err := host.SendInput([]byte(req.Data)); err != nil {
			writeError(w, http.StatusInternalServerError, codeInternal,
				fmt.Sprintf("send input: %v", err),
				map[string]any{
					"session_id": req.SessionID,
					"name":       req.Name,
				})
			return
		}
	}
	writeJSON(w, struct{}{})
}

// checkPaneExists returns a typed pane.* error when the named pane is
// missing from the session, or when the session itself is missing. Used
// by the handlers that take a (session, pane) tuple but have no model
// operation of their own — pane.resize and pane.send_input.
func (s *Server) checkPaneExists(sessionID, paneName string) error {
	sess, ok := s.tree.Session(sessionID)
	if !ok {
		return fmt.Errorf("%w: %q", pane.ErrSessionNotFound, sessionID)
	}
	for _, p := range sess.Panes {
		if p.Name == paneName {
			return nil
		}
	}
	return fmt.Errorf("%w: session %q has no pane %q",
		pane.ErrPaneNotFound, sessionID, paneName)
}
