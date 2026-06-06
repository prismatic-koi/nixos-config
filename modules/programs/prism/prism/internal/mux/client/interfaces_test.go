package client_test

// This file is in client_test (external) rather than client
// (internal) on purpose — it exercises the package surface from a
// consumer's perspective. The compile-time interface checks below
// would catch a missing method even without an importable consumer,
// but the test also serves as a documented example for downstream
// callers who want to mock the client in their own tests.

import (
	"context"
	"testing"

	"github.com/prismatic-koi/prism/internal/mux/client"
	"github.com/prismatic-koi/prism/internal/mux/pane"
)

// fakeMuxClient is a hand-rolled mock. It exists solely to prove
// MuxClient is satisfiable from outside the package — if a future
// refactor accidentally tightens the interface (e.g. returns an
// unexported type), this file fails to compile and the break is
// caught at PR time.
type fakeMuxClient struct {
	sessions client.SessionAPI
	panes    client.PaneAPI
}

func (f *fakeMuxClient) Sessions() client.SessionAPI { return f.sessions }
func (f *fakeMuxClient) Panes() client.PaneAPI       { return f.panes }
func (f *fakeMuxClient) Close() error                { return nil }

type fakeSessionAPI struct {
	createFn  func(ctx context.Context, sess pane.Session) (pane.Session, error)
	destroyFn func(ctx context.Context, id string) error
	listFn    func(ctx context.Context) (client.SessionList, error)
	switchFn  func(ctx context.Context, id string) (string, error)
}

func (f *fakeSessionAPI) Create(ctx context.Context, sess pane.Session) (pane.Session, error) {
	if f.createFn == nil {
		return pane.Session{}, nil
	}
	return f.createFn(ctx, sess)
}
func (f *fakeSessionAPI) Destroy(ctx context.Context, id string) error {
	if f.destroyFn == nil {
		return nil
	}
	return f.destroyFn(ctx, id)
}
func (f *fakeSessionAPI) List(ctx context.Context) (client.SessionList, error) {
	if f.listFn == nil {
		return client.SessionList{}, nil
	}
	return f.listFn(ctx)
}
func (f *fakeSessionAPI) Switch(ctx context.Context, id string) (string, error) {
	if f.switchFn == nil {
		return id, nil
	}
	return f.switchFn(ctx, id)
}

type fakePaneAPI struct {
	createFn    func(ctx context.Context, sessionID, name string) error
	destroyFn   func(ctx context.Context, sessionID, name string) error
	listFn      func(ctx context.Context, sessionID string) (client.PaneList, error)
	switchFn    func(ctx context.Context, req client.PaneSwitchRequest) (string, error)
	resizeFn    func(ctx context.Context, sessionID, name string, cols, rows int) error
	sendInputFn func(ctx context.Context, sessionID, name, data string) error
}

func (f *fakePaneAPI) Create(ctx context.Context, sessionID, name string) error {
	if f.createFn == nil {
		return nil
	}
	return f.createFn(ctx, sessionID, name)
}
func (f *fakePaneAPI) Destroy(ctx context.Context, sessionID, name string) error {
	if f.destroyFn == nil {
		return nil
	}
	return f.destroyFn(ctx, sessionID, name)
}
func (f *fakePaneAPI) List(ctx context.Context, sessionID string) (client.PaneList, error) {
	if f.listFn == nil {
		return client.PaneList{}, nil
	}
	return f.listFn(ctx, sessionID)
}
func (f *fakePaneAPI) Switch(ctx context.Context, req client.PaneSwitchRequest) (string, error) {
	if f.switchFn == nil {
		return "", nil
	}
	return f.switchFn(ctx, req)
}
func (f *fakePaneAPI) Resize(ctx context.Context, sessionID, name string, cols, rows int) error {
	if f.resizeFn == nil {
		return nil
	}
	return f.resizeFn(ctx, sessionID, name, cols, rows)
}
func (f *fakePaneAPI) SendInput(ctx context.Context, sessionID, name, data string) error {
	if f.sendInputFn == nil {
		return nil
	}
	return f.sendInputFn(ctx, sessionID, name, data)
}

// Compile-time interface-satisfaction checks. These run at build
// time; a failure here means the public surface drifted.
var (
	_ client.MuxClient  = (*fakeMuxClient)(nil)
	_ client.SessionAPI = (*fakeSessionAPI)(nil)
	_ client.PaneAPI    = (*fakePaneAPI)(nil)
)

// TestInterface_Mockable demonstrates that a consumer can drive a
// mock through the MuxClient interface end-to-end. The unit under
// test is intentionally trivial — the value here is the worked
// example, not the assertion.
func TestInterface_Mockable(t *testing.T) {
	called := 0
	mock := &fakeMuxClient{
		sessions: &fakeSessionAPI{
			createFn: func(_ context.Context, sess pane.Session) (pane.Session, error) {
				called++
				sess.ActivePane = "agent" // simulate server-applied default
				return sess, nil
			},
		},
		panes: &fakePaneAPI{},
	}

	got, err := mock.Sessions().Create(context.Background(), pane.Session{ID: "x", Repo: "r"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if called != 1 {
		t.Errorf("createFn called %d times, want 1", called)
	}
	if got.ActivePane != "agent" {
		t.Errorf("ActivePane = %q, want %q", got.ActivePane, "agent")
	}
}
