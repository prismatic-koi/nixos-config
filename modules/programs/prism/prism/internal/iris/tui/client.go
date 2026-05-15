package tui

// client.go — daemon socket client for the iris TUI.
//
// DaemonClient manages a persistent connection to the iris daemon at
// ~/.local/state/iris/iris.sock. It sends request frames and delivers
// incoming frames to a bubbletea Program via Send(). It is the only
// component in this package that touches the network — all state access
// goes through the daemon socket, never the DB directly.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/prismatic-koi/prism/internal/iris"
)

// DaemonFrame is a bubbletea message carrying a decoded frame from the daemon.
type DaemonFrame struct {
	// Raw is the decoded frame. The caller switches on RawType to decide
	// how to interpret it.
	RawType string
	// Snapshot is populated when RawType == DaemonFrameSessionsSnapshot.
	Snapshot *iris.DaemonSessionsSnapshotFrame
	// Event is populated when RawType == DaemonFrameSessionEvent.
	Event *iris.DaemonSessionEventFrame
	// State is populated when RawType == DaemonFrameSessionState.
	State *iris.DaemonSessionStateFrame
	// Spawned is populated when RawType == DaemonFrameSessionSpawned.
	Spawned *iris.DaemonSessionSpawnedFrame
	// Error is populated when RawType == DaemonFrameError.
	Error *iris.DaemonErrorFrame
}

// ConnectedMsg is sent to the bubbletea program when the client connects.
type ConnectedMsg struct{}

// DisconnectedMsg is sent when the connection is lost.
type DisconnectedMsg struct {
	Err error
}

// DaemonClient is the socket client. It is safe to send frames from any
// goroutine via the Send* methods.
type DaemonClient struct {
	sockPath string
	program  *tea.Program
	// sink is an optional message sink for testing without a real tea.Program.
	sink func(tea.Msg)

	mu      sync.Mutex
	conn    net.Conn
	writer  *bufio.Writer
	writeMu sync.Mutex
}

// NewDaemonClient creates a DaemonClient. Connect must be called to establish
// the connection.
func NewDaemonClient(sockPath string) *DaemonClient {
	return &DaemonClient{sockPath: sockPath}
}

// SetProgram wires the bubbletea program for message delivery. Must be called
// before Connect.
func (c *DaemonClient) SetProgram(p *tea.Program) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.program = p
}

// SetSink wires a plain message sink function for testing. When set, messages
// are delivered to the sink instead of a tea.Program. This allows integration
// tests to drive the client without starting a terminal program.
func (c *DaemonClient) SetSink(fn func(tea.Msg)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sink = fn
}

// Connect dials the daemon socket and starts the read loop. Call in a goroutine.
func (c *DaemonClient) Connect() {
	for {
		conn, err := net.Dial("unix", c.sockPath)
		if err != nil {
			c.send(DisconnectedMsg{Err: fmt.Errorf("dial %q: %w", c.sockPath, err)})
			time.Sleep(2 * time.Second)
			continue
		}
		c.mu.Lock()
		c.conn = conn
		c.writer = bufio.NewWriter(conn)
		c.mu.Unlock()

		// Request the initial sessions list immediately on connect so the
		// UI can populate before the user interacts. This is done before
		// sending ConnectedMsg so the model sees the list on its first update.
		_ = c.sendSessionsListLocked()
		c.send(ConnectedMsg{})
		c.readLoop(conn)

		c.mu.Lock()
		c.conn = nil
		c.writer = nil
		c.mu.Unlock()
		c.send(DisconnectedMsg{Err: fmt.Errorf("connection closed")})
		time.Sleep(2 * time.Second)
	}
}

// readLoop reads frames from the connection and delivers them to the program.
func (c *DaemonClient) readLoop(conn net.Conn) {
	r := bufio.NewReaderSize(conn, 4*1024*1024)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var generic struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &generic); err != nil {
			continue
		}
		msg := DaemonFrame{RawType: generic.Type}
		switch generic.Type {
		case iris.DaemonFrameSessionsSnapshot:
			var f iris.DaemonSessionsSnapshotFrame
			if err := json.Unmarshal(line, &f); err == nil {
				msg.Snapshot = &f
			}
		case iris.DaemonFrameSessionEvent:
			var f iris.DaemonSessionEventFrame
			if err := json.Unmarshal(line, &f); err == nil {
				msg.Event = &f
			}
		case iris.DaemonFrameSessionState:
			var f iris.DaemonSessionStateFrame
			if err := json.Unmarshal(line, &f); err == nil {
				msg.State = &f
			}
		case iris.DaemonFrameSessionSpawned:
			var f iris.DaemonSessionSpawnedFrame
			if err := json.Unmarshal(line, &f); err == nil {
				msg.Spawned = &f
			}
		case iris.DaemonFrameError:
			var f iris.DaemonErrorFrame
			if err := json.Unmarshal(line, &f); err == nil {
				msg.Error = &f
			}
			// pong is handled silently (keepalive only).
		}
		c.send(msg)
	}
}

// send delivers a message to the bubbletea program or sink (if wired).
func (c *DaemonClient) send(msg tea.Msg) {
	c.mu.Lock()
	p := c.program
	s := c.sink
	c.mu.Unlock()
	if p != nil {
		p.Send(msg)
	}
	if s != nil {
		s(msg)
	}
}

// writeFrame serialises v as a JSON line and sends it on the current
// connection. Returns an error if the connection is not open.
func (c *DaemonClient) writeFrame(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	c.mu.Lock()
	w := c.writer
	c.mu.Unlock()

	if w == nil {
		return fmt.Errorf("not connected")
	}
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = w.Write(data)
	if err != nil {
		return err
	}
	return w.Flush()
}

// SendSessionsList requests a sessions snapshot from the daemon.
func (c *DaemonClient) SendSessionsList() error {
	return c.writeFrame(iris.ClientSessionsListFrame{
		Type: iris.ClientFrameSessionsList,
	})
}

// sendSessionsListLocked is identical to SendSessionsList but is safe to call
// from within Connect() where c.writer has just been set. It uses the
// write-frame path directly without acquiring c.mu.
func (c *DaemonClient) sendSessionsListLocked() error {
	return c.writeFrame(iris.ClientSessionsListFrame{
		Type: iris.ClientFrameSessionsList,
	})
}

// SendSessionSubscribe subscribes to a session's event stream.
// sinceEventID = 0 means no replay.
func (c *DaemonClient) SendSessionSubscribe(name string, sinceEventID int64) error {
	return c.writeFrame(iris.ClientSessionSubscribeFrame{
		Type:         iris.ClientFrameSessionSubscribe,
		Name:         name,
		SinceEventID: sinceEventID,
	})
}

// SendSessionUnsubscribe cancels a subscription.
func (c *DaemonClient) SendSessionUnsubscribe(name string) error {
	return c.writeFrame(iris.ClientSessionUnsubscribeFrame{
		Type: iris.ClientFrameSessionUnsubscribe,
		Name: name,
	})
}

// SendPromptDeliver delivers a prompt to the named session.
func (c *DaemonClient) SendPromptDeliver(name, text string) error {
	return c.writeFrame(iris.ClientPromptDeliverFrame{
		Type: iris.ClientFramePromptDeliver,
		Name: name,
		Text: text,
	})
}
