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
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Catch-up after a missed hot reload. A page that reloads shows the HTML of
// that moment but hears about later changes only once its new HMR socket is
// open; through the share that gap lasts seconds, and the framework never
// resends a message a socket missed. The daemon therefore keeps its own HMR
// socket to the app (the observer) and counts the page-changing messages it
// receives: the target's revision. Each page is stamped with the revision and
// the observer connection's generation as they were when its request was
// forwarded; once the page's own socket is open, it asks the daemon, which
// answers after a ping/pong barrier on the observer, and the page reloads
// once if it missed something.
//
// Why a page that missed a message always reloads: the app sent that message
// to every socket registered at the time, so the page's socket registered
// after it, and the page's check reaches the daemon after that. If the
// observer was connected throughout, the app wrote the message to it before
// the pong to the barrier's later ping, and the reader counts every complete
// message before it releases the barrier, so the answer's revision is newer
// than the stamp. If the observer was not connected when the message was
// sent, its generation has changed since the stamp. Reading the stamp when
// the request is forwarded, before the app renders, can only make the stamp
// older, which costs at most one extra reload, never a miss.

// Timings of the catch-up; tests shorten the ones they exercise.
const (
	// observerIdle is how long the observer outlives the last visitor HMR
	// socket, or the last visitor navigation if that came later.
	observerIdle = 60 * time.Second
	// stampWait is how long a navigation waits for the observer to connect
	// before it is stamped unknown.
	stampWait = 200 * time.Millisecond
	// barrierWait bounds the ping/pong barrier; after it the answer is
	// unavailable and the page retries.
	barrierWait = 500 * time.Millisecond
	// The observer reconnects after 100 ms, doubling to 5 s; a connection that
	// lasted longer than the maximum starts again at the minimum.
	reconnectMin = 100 * time.Millisecond
	reconnectMax = 5 * time.Second
	// handshakeTimeout bounds discovery and each handshake with the app. Vite
	// leaves an upgrade at a path it does not serve unanswered, so a wrong
	// path ends only here.
	handshakeTimeout = 5 * time.Second
	// maxClientModule bounds the framework client module read for discovery
	// (Vite's is about 330 KB).
	maxClientModule = 4 << 20
	// unreachedHandshakes is how many handshakes in a row may fail at a
	// socket the page dials on another port before the target is treated as
	// unsupported for the rest of the observer's life.
	unreachedHandshakes = 3
	// maxObservedMessage bounds a message the observer keeps. A longer one is
	// counted as a change, which can cost an extra reload but never a miss,
	// and is not replayed.
	maxObservedMessage = 4 << 20
)

// generations numbers observer connections across every share of this
// daemon, so a page stamped by one connection never matches another, even of
// a proxy built again for a redocked share. It starts from the clock so that
// a restarted daemon is unlikely to repeat a number either.
var generations atomic.Uint64

func init() { generations.Store(uint64(time.Now().UnixMicro())) }

// stamp is what a navigation response carries. script is false when the
// target's framework is not known to be supported: the page gets nothing,
// unless pending is set. known is false for an unknown stamp: the observer
// was not connected.
//
// pending marks a navigation stamped while it was not yet known whether the
// target is supported. An unknown stamp does not depend on when it is read,
// so the response decides: it gets an unknown stamp if the target has been
// found supported by then, or if the page loads the framework's client.
type stamp struct {
	script     bool
	known      bool
	pending    bool
	fw         *framework
	generation uint64
	revision   uint64
}

// answer is the state of the target after a successful barrier.
type answer struct {
	generation uint64
	revision   uint64
}

// support is what the observer knows about the target's framework.
type support uint8

const (
	supportUnknown support = iota
	supported
	unsupported
	// unconfirmed: the framework is supported, but its page dials the
	// socket on another port, which may not be the app's. Navigations wait
	// for nothing and are stamped pending: the page gets an unknown stamp
	// only if, by the time it is answered, a handshake or a visitor's socket
	// has shown the socket is reachable.
	unconfirmed
)

// endpoint is where a framework's HMR socket is: a path and query on the
// target, and the subprotocol to offer. elsewhere marks an endpoint read from
// a client module whose page dials another port.
type endpoint struct {
	fw        *framework
	uri       string
	elsewhere bool
}

type barrierWaiter struct {
	seq  uint64
	conn *wsClient
	ch   chan answer // receives once; closed without a value when unavailable
}

// life is one running period of the observer, from a navigation until it
// idles out or the proxy stops. Its context ends with it.
type life struct {
	ctx context.Context
	end context.CancelFunc
}

func (l *life) ended() bool { return l.ctx.Err() != nil }

// observer is a target's own HMR client. It runs only while visitors are
// present: while a visitor socket is open the app has another client anyway,
// so the observer changes nothing in how the app behaves. Its one effect on
// Vite, which buffers its last error only while it has no clients, is undone
// by replaying that error to new visitor sockets (catchup.go).
type observer struct {
	base   *url.URL
	rt     http.RoundTripper
	logger *log.Logger
	target string

	idle, stampWait, barrierWait, backoffMin, backoffMax time.Duration
	now                                                  func() time.Time

	// nonce marks this observer's pings, so a pong it did not ask for
	// releases nothing.
	nonce [8]byte
	wg    sync.WaitGroup

	mu         sync.Mutex
	closed     bool
	life       *life
	idleTimer  *time.Timer
	sockets    int       // open visitor HMR sockets
	lastActive time.Time // last navigation or visitor socket close
	support    support
	fw         *framework    // the supported framework, once known
	learned    *endpoint     // copied from the first visitor HMR upgrade
	wake       chan struct{} // signalled when learned changes
	conn       *wsClient     // the connected observer socket, nil when down
	generation uint64        // conn's generation
	revision   uint64
	failure    []byte        // the error message while the app is in error
	changed    chan struct{} // closed, and replaced, when conn connects or support is decided
	pingSeq    uint64
	waiters    []*barrierWaiter

	handshakeTimeout time.Duration
}

func newObserver(base *url.URL, rt http.RoundTripper, target string, logger *log.Logger) *observer {
	o := &observer{
		base: base, rt: rt, target: target, logger: logger,
		idle: observerIdle, stampWait: stampWait, barrierWait: barrierWait, backoffMin: reconnectMin, backoffMax: reconnectMax,
		now: time.Now, wake: make(chan struct{}, 1), changed: make(chan struct{}), handshakeTimeout: handshakeTimeout,
	}
	_, _ = rand.Read(o.nonce[:])
	return o
}

func (o *observer) logf(format string, args ...any) {
	if o.logger != nil {
		o.logger.Printf("catch-up %s: "+format, append([]any{o.target}, args...)...)
	}
}

// navigated records a visitor navigation: it starts the observer if it is
// not running and postpones its end.
func (o *observer) navigated() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.lastActive = o.now()
	o.startLocked()
	o.armIdleLocked()
}

// startLocked starts a life of the observer unless one is running.
func (o *observer) startLocked() {
	if o.life != nil || o.closed {
		return
	}
	l := &life{}
	l.ctx, l.end = context.WithCancel(context.Background())
	o.life = l
	o.wg.Add(1)
	go o.run(l)
}

// broadcastLocked wakes the navigations waiting for a stamp.
func (o *observer) broadcastLocked() {
	close(o.changed)
	o.changed = make(chan struct{})
}

// decideLocked records what the target's framework is.
func (o *observer) decideLocked(sup support, fw *framework) {
	if o.support == sup && o.fw == fw {
		return
	}
	o.support, o.fw = sup, fw
	o.broadcastLocked()
}

// armIdleLocked schedules the end of the observer's life, unless a visitor
// socket is open.
func (o *observer) armIdleLocked() {
	if o.sockets > 0 {
		if o.idleTimer != nil {
			o.idleTimer.Stop()
		}
		return
	}
	if o.idleTimer == nil {
		o.idleTimer = time.AfterFunc(o.idle, o.expire)
		return
	}
	o.idleTimer.Reset(o.idle)
}

// expire ends the observer's life if it has been idle long enough; a timer
// that fires late or early only re-arms.
func (o *observer) expire() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || o.life == nil || o.sockets > 0 {
		return
	}
	if left := o.idle - o.now().Sub(o.lastActive); left > 0 {
		o.idleTimer.Reset(left)
		return
	}
	o.endLifeLocked()
	if o.support == unsupported || o.support == unconfirmed {
		// Look again on the next visit: the app on this port may change.
		o.decideLocked(supportUnknown, nil)
	}
}

func (o *observer) endLifeLocked() {
	if o.life == nil {
		return
	}
	o.life.end()
	o.life = nil
	if o.conn != nil {
		o.dropLocked(o.conn)
	}
}

// dropLocked forgets c as the observer's connection, if it still is, and
// answers its pending barriers unavailable. A life that starts while the
// previous one's reader is still winding down finds no connection.
func (o *observer) dropLocked(c *wsClient) {
	c.close()
	if o.conn == c {
		o.conn = nil
		o.failure = nil
	}
	kept := o.waiters[:0]
	for _, w := range o.waiters {
		if w.conn == c {
			close(w.ch)
		} else {
			kept = append(kept, w)
		}
	}
	o.waiters = kept
}

// learn records a visitor HMR socket the app accepted, at uri with fw's
// subprotocol: the app is that framework, and the observer runs, as for a
// navigation. The first such socket also tells the observer where the socket
// is, should discovery not find it.
func (o *observer) learn(fw *framework, uri string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.lastActive = o.now()
	o.decideLocked(supported, fw)
	if o.learned == nil {
		o.learned = &endpoint{fw: fw, uri: uri}
		select {
		case o.wake <- struct{}{}:
		default:
		}
	}
	o.startLocked()
	o.armIdleLocked()
}

// socketOpened counts a visitor HMR socket open; while one is, the observer
// keeps running.
func (o *observer) socketOpened() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sockets++
	if o.idleTimer != nil {
		o.idleTimer.Stop()
	}
}

// socketClosed counts a visitor HMR socket closed.
func (o *observer) socketClosed() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sockets--
	o.lastActive = o.now()
	if o.life != nil && !o.closed {
		o.armIdleLocked()
	}
}

// stop ends the observer for good and waits for its goroutines.
func (o *observer) stop() {
	o.mu.Lock()
	o.closed = true
	o.endLifeLocked()
	if o.idleTimer != nil {
		o.idleTimer.Stop()
	}
	o.mu.Unlock()
	o.wg.Wait()
}

// stamp returns the state to stamp a navigation with, read now. When the
// observer is not connected it waits up to stampWait for it, or for the
// target to be found unsupported; the request is forwarded only after that,
// so the stamp is never newer than the page.
func (o *observer) stamp(ctx context.Context) stamp {
	t := time.NewTimer(o.stampWait)
	defer t.Stop()
	for {
		o.mu.Lock()
		s, done := o.stampLocked(false)
		changed := o.changed
		o.mu.Unlock()
		if done {
			return s
		}
		select {
		case <-changed:
			continue
		case <-t.C:
		case <-ctx.Done():
		}
		break
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	s, _ := o.stampLocked(true)
	return s
}

func (o *observer) stampLocked(final bool) (stamp, bool) {
	switch {
	case o.closed || o.support == unsupported:
		return stamp{}, true
	case o.support == unconfirmed && o.conn == nil:
		return stamp{pending: true}, true
	case o.conn != nil:
		return stamp{script: true, known: true, fw: o.fw, generation: o.generation, revision: o.revision}, true
	case !final:
		return stamp{}, false
	case o.support == supported:
		return stamp{script: true, fw: o.fw}, true
	default:
		return stamp{pending: true}, true
	}
}

// supportedFramework is the target's framework once it is known to be a
// supported one, else nil.
func (o *observer) supportedFramework() *framework {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed || o.support != supported {
		return nil
	}
	return o.fw
}

// undecided reports that it is not yet known what the target's framework
// is.
func (o *observer) undecided() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return !o.closed && o.support == supportUnknown
}

// applies reports whether a page of the target may carry a stamp to check:
// anything but a target found unsupported, or an observer that has stopped
// for good.
func (o *observer) applies() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return !o.closed && o.support != unsupported
}

// check runs the barrier: a ping on the observer, and its pong, received
// after every message the app sent before it has been counted. ok is false
// when the observer is not connected or the pong does not come in time.
func (o *observer) check(ctx context.Context) (answer, bool) {
	o.mu.Lock()
	c := o.conn
	if c == nil || o.closed {
		o.mu.Unlock()
		return answer{}, false
	}
	o.pingSeq++
	w := &barrierWaiter{seq: o.pingSeq, conn: c, ch: make(chan answer, 1)}
	o.waiters = append(o.waiters, w)
	o.mu.Unlock()
	var payload [16]byte
	copy(payload[:8], o.nonce[:])
	binary.BigEndian.PutUint64(payload[8:], w.seq)
	t := time.NewTimer(o.barrierWait)
	defer t.Stop()
	if c.write(opPing, payload[:]) == nil {
		select {
		case a, ok := <-w.ch:
			return a, ok
		case <-t.C:
		case <-ctx.Done():
		}
	}
	o.mu.Lock()
	for i, x := range o.waiters {
		if x == w {
			o.waiters = append(o.waiters[:i], o.waiters[i+1:]...)
			break
		}
	}
	o.mu.Unlock()
	return answer{}, false
}

// failureMessage is the error message to replay to a new visitor socket, or
// nil while the app is not in error.
func (o *observer) failureMessage() []byte {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.failure
}

// run is one life of the observer: find the endpoint, connect, read until
// the connection ends, and again with backoff, until the life ends.
func (o *observer) run(l *life) {
	defer o.wg.Done()
	backoff := o.backoffMin
	sleep := func() bool {
		t := time.NewTimer(backoff)
		defer t.Stop()
		backoff = min(2*backoff, o.backoffMax)
		select {
		case <-l.ctx.Done():
			return false
		case <-t.C:
			return true
		}
	}
	ctx := l.ctx
	unreached := 0
	for !l.ended() {
		ep, err := o.discover(ctx)
		if errors.Is(err, errNotSupported) {
			o.mu.Lock()
			if o.learned != nil {
				ep, err = *o.learned, nil
			} else {
				if o.support != unsupported {
					o.logf("no supported framework found; pages are not stamped")
				}
				o.decideLocked(unsupported, nil)
			}
			o.mu.Unlock()
			if err != nil {
				select {
				case <-l.ctx.Done():
					return
				case <-o.wake:
				}
				continue
			}
		}
		if err != nil {
			if !sleep() {
				return
			}
			continue
		}
		o.mu.Lock()
		learned := o.learned
		switch {
		case !ep.elsewhere || learned != nil:
			o.decideLocked(supported, ep.fw)
		case o.support != supported:
			o.decideLocked(unconfirmed, ep.fw)
		}
		o.mu.Unlock()
		c, err := o.dial(ctx, ep)
		if err != nil && learned != nil && learned.uri != ep.uri {
			// The client module named a socket the app does not answer at:
			// use the one a visitor's page reached.
			ep = *learned
			c, err = o.dial(ctx, ep)
		}
		if err != nil && ep.elsewhere && learned == nil {
			unreached++
			o.mu.Lock()
			demote := unreached >= unreachedHandshakes && o.support == unconfirmed
			if demote {
				// The page dials a separate HMR server, which neither the
				// observer nor a visitor reaches through the share. A target
				// whose socket has been reached keeps trying instead: its
				// server is only restarting.
				o.logf("the hot reload socket is on another port; pages are not stamped")
				o.decideLocked(unsupported, nil)
			}
			o.mu.Unlock()
			if demote {
				select {
				case <-l.ctx.Done():
					return
				case <-o.wake:
				}
				unreached = 0
				continue
			}
		}
		if err != nil {
			if !sleep() {
				return
			}
			continue
		}
		unreached = 0
		started := o.now()
		o.serve(l, c, ep.fw)
		if o.now().Sub(started) > o.backoffMax {
			backoff = o.backoffMin
		}
		if !sleep() {
			return
		}
	}
}

func (o *observer) dial(ctx context.Context, ep endpoint) (*wsClient, error) {
	hs, cancel := context.WithTimeout(ctx, o.handshakeTimeout)
	defer cancel()
	return dialWebSocket(hs, o.rt, o.base, ep.uri, ep.fw.subprotocol)
}

var errNotSupported = errors.New("no supported framework")

// discover finds the HMR endpoint from the framework's client module. It
// returns errNotSupported when the app answers but is no supported
// framework, and another error when the app cannot be asked. Once a visitor
// socket has shown where the endpoint is, that is used when the module does
// not say.
func (o *observer) discover(ctx context.Context) (endpoint, error) {
	answered := false
	for _, fw := range frameworks {
		module, err := o.fetch(ctx, fw.clientPath)
		if err != nil {
			continue
		}
		answered = true
		if uri, elsewhere, ok := fw.endpoint(module); ok {
			return endpoint{fw: fw, uri: uri, elsewhere: elsewhere}, nil
		}
	}
	if answered {
		return endpoint{}, errNotSupported
	}
	o.mu.Lock()
	learned := o.learned
	o.mu.Unlock()
	if learned != nil {
		return *learned, nil
	}
	return endpoint{}, errors.New("the app did not answer")
}

// fetch reads a framework's client module from the app. An answer other than
// 200 is an empty module, not an error: the app is reachable but is not that
// framework. An error means the app could not be asked.
func (o *observer) fetch(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, o.handshakeTimeout)
	defer cancel()
	u := *o.base
	u.Path, u.RawPath, u.RawQuery = path, "", ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "purlview")
	res, err := o.rt.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxClientModule))
	if err != nil {
		return nil, err
	}
	return body, nil
}

// serve reads the observer connection until it ends. It is the only reader,
// and handles frames in the order the app sent them.
func (o *observer) serve(l *life, c *wsClient, fw *framework) {
	o.mu.Lock()
	if o.closed || o.life != l {
		o.mu.Unlock()
		c.close()
		return
	}
	if o.conn != nil {
		o.dropLocked(o.conn)
	}
	o.conn = c
	o.generation = generations.Add(1)
	o.failure = nil
	o.support, o.fw = supported, fw
	o.broadcastLocked()
	gen := o.generation
	o.mu.Unlock()
	o.logf("observing %s hot reload (generation %d)", fw.name, gen)

	err := o.read(c, fw)

	o.mu.Lock()
	o.dropLocked(c)
	o.mu.Unlock()
	if !l.ended() {
		o.logf("hot reload socket ended (generation %d): %v", gen, err)
	}
}

// read handles c's frames until the connection fails or closes. A message
// is counted only once its final fragment has arrived. A pong that arrives
// between the fragments of a message is held until that message is counted,
// so a barrier never passes a message the app sent before the pong.
func (o *observer) read(c *wsClient, fw *framework) error {
	var (
		msg      []byte
		msgOp    byte
		inMsg    bool
		oversize bool
		pong     uint64 // highest pong sequence held back, 0 for none
	)
	for {
		h, err := readFrameHeader(c.br)
		if err != nil {
			return err
		}
		if h.control() {
			payload := make([]byte, h.length)
			if _, err := io.ReadFull(c.br, payload); err != nil {
				return err
			}
			if h.masked {
				maskBytes(h.mask, 0, payload)
			}
			switch h.opcode {
			case opPing:
				if err := c.write(opPong, payload); err != nil {
					return err
				}
			case opPong:
				seq, ok := o.pongSeq(payload)
				if !ok {
					continue
				}
				if inMsg {
					pong = max(pong, seq)
				} else {
					o.release(c, seq)
				}
			case opClose:
				_ = c.write(opClose, nil)
				return io.EOF
			}
			continue
		}
		switch {
		case h.opcode == opContinuation && !inMsg, h.opcode != opContinuation && inMsg:
			return errFrame
		case h.opcode != opContinuation:
			inMsg, msgOp, msg, oversize = true, h.opcode, msg[:0], false
		}
		if oversize || int64(len(msg))+h.length > maxObservedMessage {
			oversize = true
			if _, err := io.CopyN(io.Discard, c.br, h.length); err != nil {
				return err
			}
		} else {
			start := len(msg)
			msg = append(msg, make([]byte, h.length)...)
			if _, err := io.ReadFull(c.br, msg[start:]); err != nil {
				return err
			}
			if h.masked {
				maskBytes(h.mask, 0, msg[start:])
			}
		}
		if !h.fin {
			continue
		}
		inMsg = false
		if msgOp == opText {
			o.record(c, fw, msg, oversize)
		}
		if pong != 0 {
			o.release(c, pong)
			pong = 0
		}
	}
}

// pongSeq reads the sequence of one of this observer's pings from a pong.
func (o *observer) pongSeq(payload []byte) (uint64, bool) {
	if len(payload) != 16 || !bytes.Equal(payload[:8], o.nonce[:]) {
		return 0, false
	}
	seq := binary.BigEndian.Uint64(payload[8:])
	return seq, seq != 0
}

// record counts one complete text message from the app.
func (o *observer) record(c *wsClient, fw *framework, msg []byte, oversize bool) {
	typ := ""
	if !oversize {
		typ = messageType(msg)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.conn != c {
		return
	}
	switch {
	case oversize || fw.changes[typ]:
		o.revision++
		o.failure = nil
	case typ == fw.errorType:
		o.failure = bytes.Clone(msg)
	}
}

// release answers every barrier on c whose ping is at or before seq. Pings
// are numbered before they are sent, so a pong to a later ping also proves
// the earlier ones' messages counted.
func (o *observer) release(c *wsClient, seq uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.conn != c {
		return
	}
	a := answer{generation: o.generation, revision: o.revision}
	kept := o.waiters[:0]
	for _, w := range o.waiters {
		if w.conn == c && w.seq <= seq {
			w.ch <- a
		} else {
			kept = append(kept, w)
		}
	}
	o.waiters = kept
}
