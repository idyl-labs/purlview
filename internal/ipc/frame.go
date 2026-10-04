// Copyright 2026 Idyl Labs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ipc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// ErrMessageTooLarge is returned when a line exceeds MaxMessageSize.
var ErrMessageTooLarge = errors.New("ipc: message exceeds size limit")

// Conn frames JSON messages over a byte stream with deadlines. It is safe
// for one reader and one writer goroutine at a time.
type Conn struct {
	raw net.Conn
	r   *bufio.Reader

	wmu sync.Mutex
}

// NewConn wraps a transport connection.
func NewConn(raw net.Conn) *Conn {
	// The reader buffer is one byte larger than the limit so a line that fills
	// it is by definition too large.
	return &Conn{raw: raw, r: bufio.NewReaderSize(raw, MaxMessageSize+1)}
}

// Close closes the transport.
func (c *Conn) Close() error { return c.raw.Close() }

// LocalAddr exposes the transport address for logging.
func (c *Conn) LocalAddr() net.Addr { return c.raw.LocalAddr() }

// RemoteAddr exposes the peer transport address for logging.
func (c *Conn) RemoteAddr() net.Addr { return c.raw.RemoteAddr() }

// Raw returns the transport connection (for peer checks).
func (c *Conn) Raw() net.Conn { return c.raw }

// readMessage reads one line, enforcing the size limit and the deadline.
// A zero deadline means no deadline.
func (c *Conn) readMessage(deadline time.Time) (*message, error) {
	if err := c.raw.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	line, err := c.r.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		// Drain the oversized line so the error is precise, then fail.
		for errors.Is(err, bufio.ErrBufferFull) {
			_, err = c.r.ReadSlice('\n')
		}
		return nil, ErrMessageTooLarge
	}
	if err != nil {
		if len(line) > 0 && errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	if len(line) > MaxMessageSize {
		return nil, ErrMessageTooLarge
	}
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, errors.New("ipc: empty message")
	}
	var m message
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields() // strict: unknown top-level keys are a protocol error
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("ipc: malformed message: %w", err)
	}
	return &m, nil
}

// writeMessage writes one line with the deadline.
func (c *Conn) writeMessage(m *message, deadline time.Time) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(data)+1 > MaxMessageSize {
		return ErrMessageTooLarge
	}
	data = append(data, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.raw.SetWriteDeadline(deadline); err != nil {
		return err
	}
	_, err = c.raw.Write(data)
	return err
}

// ReadRequest reads a request (server side).
func (c *Conn) ReadRequest(deadline time.Time) (*Request, error) {
	m, err := c.readMessage(deadline)
	if err != nil {
		return nil, err
	}
	if m.Op == "" {
		return nil, errors.New("ipc: expected a request")
	}
	if m.ID == "" {
		return nil, errors.New("ipc: request without id")
	}
	return &Request{ID: m.ID, Op: m.Op, Params: m.Params}, nil
}

// WriteResponse writes a response (server side).
func (c *Conn) WriteResponse(resp *Response, deadline time.Time) error {
	ok := resp.OK
	return c.writeMessage(&message{ID: resp.ID, OK: &ok, Result: resp.Result, Error: resp.Error}, deadline)
}

// WriteEvent writes an event (server side).
func (c *Conn) WriteEvent(ev *Event, deadline time.Time) error {
	return c.writeMessage(&message{Event: ev.Event, Data: ev.Data}, deadline)
}

// WriteRequest writes a request (client side).
func (c *Conn) WriteRequest(req *Request, deadline time.Time) error {
	return c.writeMessage(&message{ID: req.ID, Op: req.Op, Params: req.Params}, deadline)
}

// Read reads the next response or event (client side). Exactly one of the
// results is non-nil.
func (c *Conn) Read(deadline time.Time) (*Response, *Event, error) {
	m, err := c.readMessage(deadline)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case m.Event != "":
		return nil, &Event{Event: m.Event, Data: m.Data}, nil
	case m.OK != nil:
		return &Response{ID: m.ID, OK: *m.OK, Result: m.Result, Error: m.Error}, nil, nil
	default:
		return nil, nil, errors.New("ipc: message is neither a response nor an event")
	}
}

// Marshal encodes params or results, failing loudly on programmer error.
func Marshal(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic("ipc: marshal: " + err.Error())
	}
	return data
}
