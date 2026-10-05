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

package tunnel

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6455 defines Sec-WebSocket-Accept with SHA-1; it is not used for security here.
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// The smallest WebSocket (RFC 6455) the catch-up observer needs: a client
// handshake over the target's own transport, frame headers in both
// directions, and masked client frames. No extension is ever negotiated, so
// the reserved bits must be clear.

const (
	opContinuation byte = 0x0
	opText         byte = 0x1
	opBinary       byte = 0x2
	opClose        byte = 0x8
	opPing         byte = 0x9
	opPong         byte = 0xA
)

// maxControlPayload is the largest payload a control frame may carry.
const maxControlPayload = 125

var errFrame = errors.New("websocket: malformed frame")

type frameHeader struct {
	fin    bool
	opcode byte
	masked bool
	mask   [4]byte
	length int64
}

func (h frameHeader) control() bool { return h.opcode&0x8 != 0 }

// readFrameHeader reads one frame header. It refuses reserved bits, unknown
// opcodes, lengths with the top bit set and control frames that are
// fragmented or longer than 125 bytes.
func readFrameHeader(r io.Reader) (frameHeader, error) {
	var h frameHeader
	var b [8]byte
	if _, err := io.ReadFull(r, b[:2]); err != nil {
		return h, err
	}
	h.fin = b[0]&0x80 != 0
	h.opcode = b[0] & 0x0F
	h.masked = b[1]&0x80 != 0
	if b[0]&0x70 != 0 {
		return h, errFrame
	}
	switch h.opcode {
	case opContinuation, opText, opBinary, opClose, opPing, opPong:
	default:
		return h, errFrame
	}
	switch n := b[1] & 0x7F; n {
	case 126:
		if _, err := io.ReadFull(r, b[:2]); err != nil {
			return h, err
		}
		h.length = int64(binary.BigEndian.Uint16(b[:2]))
	case 127:
		if _, err := io.ReadFull(r, b[:8]); err != nil {
			return h, err
		}
		v := binary.BigEndian.Uint64(b[:8])
		if v>>63 != 0 {
			return h, errFrame
		}
		h.length = int64(v)
	default:
		h.length = int64(n)
	}
	if h.control() && (!h.fin || h.length > maxControlPayload) {
		return h, errFrame
	}
	if h.masked {
		if _, err := io.ReadFull(r, h.mask[:]); err != nil {
			return h, err
		}
	}
	return h, nil
}

// maskBytes applies a frame's mask to p, whose first byte is at offset pos
// of the payload. Masking and unmasking are the same operation.
func maskBytes(mask [4]byte, pos int64, p []byte) {
	for i := range p {
		p[i] ^= mask[(pos+int64(i))%4]
	}
}

// appendFrame appends one frame to dst. A client masks every frame with a
// fresh key (mask true); a server sends them unmasked.
func appendFrame(dst []byte, fin bool, opcode byte, payload []byte, mask bool) []byte {
	b0 := opcode
	if fin {
		b0 |= 0x80
	}
	var m byte
	if mask {
		m = 0x80
	}
	switch n := len(payload); {
	case n < 126:
		dst = append(dst, b0, m|byte(n))
	case n <= 0xFFFF:
		dst = append(dst, b0, m|126)
		dst = binary.BigEndian.AppendUint16(dst, uint16(n))
	default:
		dst = append(dst, b0, m|127)
		dst = binary.BigEndian.AppendUint64(dst, uint64(n))
	}
	if !mask {
		return append(dst, payload...)
	}
	var key [4]byte
	_, _ = rand.Read(key[:])
	dst = append(dst, key[:]...)
	start := len(dst)
	dst = append(dst, payload...)
	maskBytes(key, 0, dst[start:])
	return dst
}

// wsClient is the client end of an established WebSocket. One goroutine
// reads; writes are serialised so that control frames never split a frame.
type wsClient struct {
	rwc io.ReadWriteCloser
	br  *bufio.Reader

	wmu       sync.Mutex
	closeOnce sync.Once
}

// write sends one complete, masked frame.
func (c *wsClient) write(opcode byte, payload []byte) error {
	frame := appendFrame(nil, true, opcode, payload, true)
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.rwc.Write(frame)
	return err
}

// close ends the connection without a closing handshake; it is safe to call
// more than once and from any goroutine.
func (c *wsClient) close() { c.closeOnce.Do(func() { _ = c.rwc.Close() }) }

// acceptKey is the Sec-WebSocket-Accept value for a handshake key.
func acceptKey(key string) string {
	h := sha1.New() //nolint:gosec // RFC 6455 section 4.2.2.
	_, _ = io.WriteString(h, key+"258EAFA5-E914-47DA-95CA-C5AB0DC85B11")
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// dialWebSocket opens a WebSocket to base's host at uri (a path and query)
// over rt, offering one subprotocol. It sends no Origin: the observer is a
// native client of the app, not a page. ctx bounds the handshake only.
func dialWebSocket(ctx context.Context, rt http.RoundTripper, base *url.URL, uri, subprotocol string) (*wsClient, error) {
	u, err := url.Parse(base.Scheme + "://" + base.Host + uri)
	if err != nil {
		return nil, err
	}
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	key := base64.StdEncoding.EncodeToString(raw[:])
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header = http.Header{
		"Connection":             {"Upgrade"},
		"Upgrade":                {"websocket"},
		"Sec-Websocket-Version":  {"13"},
		"Sec-Websocket-Key":      {key},
		"Sec-Websocket-Protocol": {subprotocol},
		"User-Agent":             {"purlview"},
	}
	res, err := rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	rwc, ok := res.Body.(io.ReadWriteCloser)
	if res.StatusCode != http.StatusSwitchingProtocols || !ok {
		_ = res.Body.Close()
		return nil, fmt.Errorf("websocket: handshake answered %d", res.StatusCode)
	}
	if !strings.EqualFold(res.Header.Get("Upgrade"), "websocket") || res.Header.Get("Sec-Websocket-Accept") != acceptKey(key) ||
		res.Header.Get("Sec-Websocket-Protocol") != subprotocol || res.Header.Get("Sec-Websocket-Extensions") != "" {
		_ = rwc.Close()
		return nil, errors.New("websocket: handshake refused")
	}
	return &wsClient{rwc: rwc, br: bufio.NewReader(rwc)}, nil
}

// messageEnd finds where the first complete data message ends in a stream of
// frames read in arbitrary pieces. Control frames, which may come between a
// message's fragments, do not end it.
type messageEnd struct {
	hdr       [14]byte
	have      int   // header bytes collected
	need      int   // header length, once the second byte is known
	remaining int64 // payload bytes of the current frame still to come
	inPayload bool
	ends      bool // the current frame is the final frame of a data message
}

// scan consumes p and returns the offset in p just past the end of the
// first complete data message, or -1 if it has not ended within p. After it
// returns an offset the scanner must not be used again.
func (s *messageEnd) scan(p []byte) int {
	for i := 0; i < len(p); {
		if !s.inPayload {
			s.hdr[s.have] = p[i]
			s.have++
			i++
			if s.have == 2 {
				s.need = 2
				switch s.hdr[1] & 0x7F {
				case 126:
					s.need += 2
				case 127:
					s.need += 8
				}
				if s.hdr[1]&0x80 != 0 {
					s.need += 4
				}
			}
			if s.have < 2 || s.have < s.need {
				continue
			}
			switch n := s.hdr[1] & 0x7F; n {
			case 126:
				s.remaining = int64(binary.BigEndian.Uint16(s.hdr[2:4]))
			case 127:
				v := binary.BigEndian.Uint64(s.hdr[2:10])
				if v>>63 != 0 {
					v = 1<<63 - 1 // malformed: the message never ends here
				}
				s.remaining = int64(v)
			default:
				s.remaining = int64(n)
			}
			s.ends = s.hdr[0]&0x80 != 0 && s.hdr[0]&0x08 == 0
			s.have, s.inPayload = 0, true
		}
		take := min(s.remaining, int64(len(p)-i))
		i += int(take)
		s.remaining -= take
		if s.remaining == 0 {
			s.inPayload = false
			if s.ends {
				return i
			}
		}
	}
	return -1
}
