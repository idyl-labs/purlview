package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// pair returns two framed ends of an in-memory connection.
func pair(t *testing.T) (*Conn, *Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return NewConn(a), NewConn(b)
}

func TestRequestResponseRoundTrip(t *testing.T) {
	t.Parallel()
	client, server := pair(t)
	go func() {
		req, err := server.ReadRequest(time.Now().Add(time.Second))
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		var p HelloParams
		_ = json.Unmarshal(req.Params, &p)
		_ = server.WriteResponse(&Response{ID: req.ID, OK: true, Result: Marshal(HelloResult{Protocol: ProtocolVersion, Instance: "i", Ready: true})}, time.Now().Add(time.Second))
		_ = server.WriteEvent(&Event{Event: EventOpEnded, Data: Marshal(map[string]string{"x": "y"})}, time.Now().Add(time.Second))
	}()
	if err := client.WriteRequest(&Request{ID: "7", Op: OpHello, Params: Marshal(HelloParams{Protocol: 1})}, time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	resp, ev, err := client.Read(time.Now().Add(time.Second))
	if err != nil || resp == nil || ev != nil || resp.ID != "7" || !resp.OK {
		t.Fatalf("response: %+v %+v %v", resp, ev, err)
	}
	resp, ev, err = client.Read(time.Now().Add(time.Second))
	if err != nil || resp != nil || ev == nil || ev.Event != EventOpEnded {
		t.Fatalf("event: %+v %+v %v", resp, ev, err)
	}
}

func TestOversizedAndMalformedMessagesAreRejected(t *testing.T) {
	t.Parallel()
	client, server := pair(t)
	go func() {
		big := `{"id":"1","op":"x","params":"` + strings.Repeat("a", MaxMessageSize) + `"}` + "\n"
		_, _ = client.Raw().Write([]byte(big))
	}()
	if _, err := server.ReadRequest(time.Now().Add(2 * time.Second)); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("oversized: err=%v", err)
	}

	client2, server2 := pair(t)
	go func() { _, _ = client2.Raw().Write([]byte("{not json}\n")) }()
	if _, err := server2.ReadRequest(time.Now().Add(time.Second)); err == nil || !strings.Contains(err.Error(), "malformed") {
		t.Fatalf("malformed: err=%v", err)
	}

	client3, server3 := pair(t)
	go func() { _, _ = client3.Raw().Write([]byte(`{"id":"1","op":"x","bogus":true}` + "\n")) }()
	if _, err := server3.ReadRequest(time.Now().Add(time.Second)); err == nil {
		t.Fatal("unknown top-level field must be rejected")
	}

	// A request without an id is rejected; a write over the limit fails
	// before anything hits the wire.
	client4, server4 := pair(t)
	go func() { _, _ = client4.Raw().Write([]byte(`{"op":"x"}` + "\n")) }()
	if _, err := server4.ReadRequest(time.Now().Add(time.Second)); err == nil {
		t.Fatal("request without id must be rejected")
	}
	if err := client4.WriteRequest(&Request{ID: "1", Op: "x", Params: Marshal(strings.Repeat("b", MaxMessageSize))}, time.Now().Add(time.Second)); !errors.Is(err, ErrMessageTooLarge) {
		t.Fatalf("oversized write: %v", err)
	}
}

func TestReadDeadlineIsHonoured(t *testing.T) {
	t.Parallel()
	_, server := pair(t)
	start := time.Now()
	_, err := server.ReadRequest(time.Now().Add(100 * time.Millisecond))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("expected timeout, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("deadline took far too long")
	}
}

func TestClientMultiplexesAndFailsPendingOnClose(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	client := NewClient(a)
	server := NewConn(b)
	go func() {
		// Answer "two" before "one" to prove id matching. The requests are
		// matched by operation, not arrival order: the two client
		// goroutines race to write, and a loaded machine may deliver them
		// in either order.
		ra, _ := server.ReadRequest(time.Time{})
		rb, _ := server.ReadRequest(time.Time{})
		one, two := ra, rb
		if ra.Op == "two" {
			one, two = rb, ra
		}
		_ = server.WriteResponse(&Response{ID: two.ID, OK: true, Result: Marshal(map[string]string{"which": "second"})}, time.Time{})
		_ = server.WriteResponse(&Response{ID: one.ID, OK: false, Error: &Error{Code: CodeNotFound, Message: "nope"}}, time.Time{})
	}()
	type res struct {
		out map[string]string
		err error
	}
	c1 := make(chan res, 1)
	c2 := make(chan res, 1)
	go func() {
		var out map[string]string
		err := client.Call(context.Background(), "one", nil, &out)
		c1 <- res{out, err}
	}()
	time.Sleep(20 * time.Millisecond)
	go func() {
		var out map[string]string
		err := client.Call(context.Background(), "two", nil, &out)
		c2 <- res{out, err}
	}()
	r2 := <-c2
	if r2.err != nil || r2.out["which"] != "second" {
		t.Fatalf("second call: %+v", r2)
	}
	r1 := <-c1
	if !IsCode(r1.err, CodeNotFound) {
		t.Fatalf("first call must carry the daemon error: %v", r1.err)
	}
	// Close the server side: a pending call fails promptly.
	done := make(chan error, 1)
	go func() { done <- client.Call(context.Background(), "three", nil, nil) }()
	time.Sleep(20 * time.Millisecond)
	_ = b.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("call must fail when the connection ends")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pending call did not fail after close")
	}
	<-client.Done()
}
