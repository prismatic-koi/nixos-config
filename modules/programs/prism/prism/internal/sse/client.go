// Package sse provides a client for consuming Server-Sent Events (SSE) streams.
//
// The client connects to an HTTP endpoint, parses event: and data: fields per
// the SSE specification, and delivers parsed events via a Go channel. It
// automatically reconnects on connection loss with exponential backoff.
package sse

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// Event represents a single SSE event parsed from the stream.
type Event struct {
	// Type is the event type from the SSE "event:" field.
	// Defaults to "message" when the server omits the event field.
	Type string

	// Data is the event payload from the SSE "data:" field.
	// Multiple consecutive data: lines are concatenated with newlines.
	Data string
}

// Client consumes a Server-Sent Events stream and delivers parsed events on a
// channel. It reconnects automatically when the connection drops and respects
// context cancellation for clean shutdown.
type Client struct {
	// HTTPClient is the HTTP client used for requests. If nil, http.DefaultClient
	// is used.
	HTTPClient *http.Client

	// BufferSize is the capacity of the event channel. If the consumer falls
	// behind by this many events, new events are dropped with a log warning.
	// Defaults to 64 if zero.
	BufferSize int

	// InitialRetryDelay is the base delay before the first reconnection attempt.
	// Subsequent attempts double the delay up to MaxRetryDelay.
	// Defaults to 1s if zero.
	InitialRetryDelay time.Duration

	// MaxRetryDelay caps the exponential backoff. Defaults to 30s if zero.
	MaxRetryDelay time.Duration
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *Client) bufferSize() int {
	if c.BufferSize > 0 {
		return c.BufferSize
	}
	return 64
}

func (c *Client) initialRetryDelay() time.Duration {
	if c.InitialRetryDelay > 0 {
		return c.InitialRetryDelay
	}
	return 1 * time.Second
}

func (c *Client) maxRetryDelay() time.Duration {
	if c.MaxRetryDelay > 0 {
		return c.MaxRetryDelay
	}
	return 30 * time.Second
}

// Connect starts consuming the SSE stream at url. It returns a channel of
// parsed events. The channel is closed when ctx is cancelled.
//
// If the server is not yet ready (e.g. opencode is still starting up),
// Connect retries the initial connection with the same exponential backoff
// used for reconnection (InitialRetryDelay…MaxRetryDelay). It only returns
// an error if ctx is cancelled before the first successful connection.
//
// On subsequent connection losses the client automatically reconnects with the
// same backoff policy.
func (c *Client) Connect(ctx context.Context, url string) (<-chan Event, error) {
	ch := make(chan Event, c.bufferSize())

	// Retry the initial connection with backoff — the server may not be ready
	// yet (e.g. opencode is still starting up from a tmux send-keys command).
	resp := c.connectWithRetry(ctx, url)
	if resp == nil {
		close(ch)
		return ch, fmt.Errorf("sse: connection cancelled: %w", ctx.Err())
	}

	go c.readLoop(ctx, url, resp, ch)
	return ch, nil
}

// connectWithRetry attempts the initial SSE connection. It tries once
// immediately — the common case where the server is already up — then falls
// into the same exponential backoff loop as reconnect() if that fails.
// Returns nil if ctx is cancelled before a connection is established.
func (c *Client) connectWithRetry(ctx context.Context, url string) *http.Response {
	// Try immediately first.
	resp, err := c.doRequest(ctx, url)
	if err == nil {
		return resp
	}
	if ctx.Err() != nil {
		return nil
	}
	log.Printf("sse: initial connection failed, retrying: %v", err)

	// Fall into the same backoff loop used for reconnection.
	return c.reconnect(ctx, url)
}

// doRequest performs a single SSE connection attempt.
func (c *Client) doRequest(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return resp, nil
}

// readLoop reads events from the response body, reconnecting on failure.
// It closes ch when ctx is done.
func (c *Client) readLoop(ctx context.Context, url string, resp *http.Response, ch chan<- Event) {
	defer close(ch)

	for {
		c.consumeStream(ctx, resp, ch)
		resp.Body.Close()

		// If the context is done, stop reconnecting.
		if ctx.Err() != nil {
			return
		}

		// Reconnect with exponential backoff.
		resp = c.reconnect(ctx, url)
		if resp == nil {
			// Context cancelled during reconnection.
			return
		}
	}
}

// reconnect attempts to re-establish the SSE connection with exponential
// backoff. Returns nil if ctx is cancelled before a connection is established.
func (c *Client) reconnect(ctx context.Context, url string) *http.Response {
	delay := c.initialRetryDelay()
	maxDelay := c.maxRetryDelay()

	for {
		log.Printf("sse: reconnecting in %v", delay)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}

		resp, err := c.doRequest(ctx, url)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Printf("sse: reconnection failed: %v", err)
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
			continue
		}

		return resp
	}
}

// consumeStream reads from a single response body until EOF or error.
func (c *Client) consumeStream(ctx context.Context, resp *http.Response, ch chan<- Event) {
	scanner := bufio.NewScanner(resp.Body)

	var eventType string
	var dataLines []string

	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}

		line := scanner.Text()

		// An empty line signals the end of an event.
		if line == "" {
			if len(dataLines) > 0 {
				evt := Event{
					Type: eventType,
					Data: strings.Join(dataLines, "\n"),
				}
				if evt.Type == "" {
					evt.Type = "message"
				}
				c.send(ch, evt)
			}
			eventType = ""
			dataLines = nil
			continue
		}

		// Comment lines start with ':' — silently ignore.
		if strings.HasPrefix(line, ":") {
			continue
		}

		// Parse "field: value" or "field:value" (space after colon is optional).
		field, value, _ := strings.Cut(line, ":")
		// Per SSE spec: if the line contains a colon, the value is everything
		// after the first colon. If a single space follows the colon, it is
		// stripped.
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}

		switch field {
		case "event":
			eventType = value
		case "data":
			dataLines = append(dataLines, value)
		case "id", "retry":
			// Recognised but not needed for our use case — ignore.
		default:
			// Unknown fields are ignored per spec.
		}
	}

	// If the stream ended with accumulated data but no trailing blank line,
	// we discard it. This matches the SSE spec: an event is only dispatched
	// when an empty line is encountered.
}

// send delivers an event to the channel, dropping it with a log warning if the
// buffer is full.
func (c *Client) send(ch chan<- Event, evt Event) {
	select {
	case ch <- evt:
	default:
		log.Printf("sse: buffer full, dropping event type=%q", evt.Type)
	}
}
