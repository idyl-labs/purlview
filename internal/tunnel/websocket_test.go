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
	"bytes"
	"strings"
	"testing"
)

// The examples of RFC 6455, sections 1.3 and 5.7.
func TestWebSocketFramesFollowTheRFCExamples(t *testing.T) {
	if got := acceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("accept key %q", got)
	}
	hello := []byte{0x81, 0x05, 0x48, 0x65, 0x6c, 0x6c, 0x6f}
	if got := appendFrame(nil, true, opText, []byte("Hello"), false); !bytes.Equal(got, hello) {
		t.Fatalf("unmasked Hello % x", got)
	}
	masked := []byte{0x81, 0x85, 0x37, 0xfa, 0x21, 0x3d, 0x7f, 0x9f, 0x4d, 0x51, 0x58}
	h, err := readFrameHeader(bytes.NewReader(masked))
	payload := append([]byte{}, masked[6:]...)
	maskBytes(h.mask, 0, payload)
	if err != nil || !h.fin || h.opcode != opText || !h.masked || h.length != 5 || string(payload) != "Hello" {
		t.Fatalf("masked Hello: %+v %q %v", h, payload, err)
	}
	fragments := append(appendFrame(nil, false, opText, []byte("Hel"), false), appendFrame(nil, true, opContinuation, []byte("lo"), false)...)
	if !bytes.Equal(fragments, []byte{0x01, 0x03, 0x48, 0x65, 0x6c, 0x80, 0x02, 0x6c, 0x6f}) {
		t.Fatalf("fragments % x", fragments)
	}
	for n, prefix := range map[int][]byte{256: {0x82, 0x7E, 0x01, 0x00}, 65536: {0x82, 0x7F, 0, 0, 0, 0, 0, 1, 0, 0}} {
		frame := appendFrame(nil, true, opBinary, make([]byte, n), false)
		h, err := readFrameHeader(bytes.NewReader(frame))
		if !bytes.HasPrefix(frame, prefix) || err != nil || h.length != int64(n) {
			t.Fatalf("%d bytes: % x %+v %v", n, frame[:10], h, err)
		}
	}
	// A client frame is masked with a key, and unmasks to the payload.
	frame := appendFrame(nil, true, opPing, []byte("ping"), true)
	h, err = readFrameHeader(bytes.NewReader(frame))
	payload = append([]byte{}, frame[6:]...)
	maskBytes(h.mask, 0, payload)
	if err != nil || !h.masked || h.opcode != opPing || string(payload) != "ping" {
		t.Fatalf("masked ping: %+v %q", h, payload)
	}
	for name, bad := range map[string][]byte{
		"reserved bit":        {0xC1, 0x00},
		"unknown opcode":      {0x83, 0x00},
		"fragmented control":  {0x09, 0x00},
		"long control":        {0x89, 0x7E, 0x00, 0x7E},
		"length with top bit": {0x82, 0x7F, 0x80, 0, 0, 0, 0, 0, 0, 0},
	} {
		if _, err := readFrameHeader(bytes.NewReader(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// messageEnd finds the end of the first data message whatever the frame
// sizes and however the stream is cut, past control frames between its
// fragments.
func TestTheFirstMessageEndIsFoundInAnyPieces(t *testing.T) {
	var stream []byte
	stream = appendFrame(stream, true, opPing, nil, false)
	stream = appendFrame(stream, false, opText, []byte(strings.Repeat("a", 200)), false)
	stream = appendFrame(stream, true, opPong, []byte("x"), true)
	stream = appendFrame(stream, false, opContinuation, nil, false)
	stream = appendFrame(stream, true, opContinuation, []byte(strings.Repeat("b", 70000)), false)
	end := len(stream)
	stream = appendFrame(stream, true, opText, []byte("next"), false)
	for _, size := range []int{1, 2, 3, 7, 100, 4096, len(stream)} {
		var s messageEnd
		found := -1
		for at := 0; at < len(stream) && found < 0; at += size {
			piece := stream[at:min(at+size, len(stream))]
			if k := s.scan(piece); k >= 0 {
				found = at + k
			}
		}
		if found != end {
			t.Fatalf("pieces of %d: end at %d, want %d", size, found, end)
		}
	}
}
