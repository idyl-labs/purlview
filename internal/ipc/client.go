package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Client is a CLI-side connection to the daemon. It multiplexes responses
// by request id and delivers events on Events.
type Client struct {
	conn *Conn

	nextID atomic.Uint64
	mu     sync.Mutex
	// pending maps a request id to the channel that receives its response.
	pending map[string]chan *Response
	// events receives unsolicited events; buffered so the reader never
	// blocks on a slow consumer for long. Unread events are dropped when
	// the buffer is full; a client that cares subscribes and drains it.
	events chan *Event
	done   chan struct{}
	err    error
	closed atomic.Bool
}

// DefaultCallTimeout bounds one request/response exchange.
const DefaultCallTimeout = 5 * time.Second

// NewClient starts the reader goroutine over an open transport.
func NewClient(raw net.Conn) *Client {
	c := &Client{
		conn:    NewConn(raw),
		pending: map[string]chan *Response{},
		events:  make(chan *Event, 256),
		done:    make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Events delivers daemon events after Subscribe.
func (c *Client) Events() <-chan *Event { return c.events }

// Done is closed when the connection ends; Err reports why.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns the reason the connection ended, nil while it is open.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Client) readLoop() {
	var err error
	for {
		var resp *Response
		var ev *Event
		resp, ev, err = c.conn.Read(time.Time{})
		if err != nil {
			break
		}
		if ev != nil {
			select {
			case c.events <- ev:
			default: // slow consumer: drop rather than stall the reader
			}
			continue
		}
		c.mu.Lock()
		ch, ok := c.pending[resp.ID]
		if ok {
			delete(c.pending, resp.ID)
		}
		c.mu.Unlock()
		if ok {
			ch <- resp
		}
	}
	c.mu.Lock()
	if c.closed.Load() {
		err = errors.New("ipc: connection closed")
	}
	c.err = err
	for id, ch := range c.pending {
		delete(c.pending, id)
		close(ch)
	}
	c.mu.Unlock()
	close(c.events)
	close(c.done)
}

// Close closes the connection; pending calls fail.
func (c *Client) Close() error {
	c.closed.Store(true)
	return c.conn.Close()
}

// Call sends op with params and decodes the result into out (which may be
// nil). It honours the context deadline and DefaultCallTimeout, whichever is
// sooner. A daemon-reported failure is returned as *Error.
func (c *Client) Call(ctx context.Context, op string, params any, out any) error {
	id := strconv.FormatUint(c.nextID.Add(1), 10)
	deadline := time.Now().Add(DefaultCallTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	req := &Request{ID: id, Op: op}
	if params != nil {
		req.Params = Marshal(params)
	}
	ch := make(chan *Response, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.conn.WriteRequest(req, deadline); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("ipc: send %s: %w", op, err)
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case resp, ok := <-ch:
		if !ok {
			if err := c.Err(); err != nil {
				return fmt.Errorf("ipc: %s: connection ended: %w", op, err)
			}
			return fmt.Errorf("ipc: %s: connection ended", op)
		}
		if !resp.OK {
			if resp.Error == nil {
				return &Error{Code: CodeInternal, Message: "daemon reported failure without details"}
			}
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("ipc: decode %s result: %w", op, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("ipc: %s: %w", op, ctx.Err())
	case <-timer.C:
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return fmt.Errorf("ipc: %s: no response within %s", op, DefaultCallTimeout)
	}
}

// Hello performs the handshake and returns the daemon's identity.
func (c *Client) Hello(ctx context.Context, client string, build BuildIdentity) (*HelloResult, error) {
	var res HelloResult
	if err := c.Call(ctx, OpHello, HelloParams{Protocol: ProtocolVersion, Client: client, Build: build}, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// IsCode reports whether err is a daemon error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}
