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
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// testObserver observes fv with timings short enough for tests: it never
// idles out unless a test says so, and reconnects quickly.
func testObserver(t *testing.T, fv *fakeVite) *observer {
	t.Helper()
	base, _ := url.Parse(fv.URL)
	tr := &http.Transport{Proxy: nil}
	o := newObserver(base, tr, "localhost:5173", nil)
	o.idle = time.Hour
	o.stampWait = 5 * time.Second
	o.barrierWait = 5 * time.Second
	o.backoffMin, o.backoffMax = 10*time.Millisecond, 40*time.Millisecond
	o.handshakeTimeout = 5 * time.Second
	t.Cleanup(func() {
		o.stop()
		tr.CloseIdleConnections()
	})
	return o
}

// connected starts the observer and waits for its connection; it returns
// the stamp a navigation would get.
func connected(t *testing.T, o *observer) stamp {
	t.Helper()
	o.navigated()
	s := o.stamp(context.Background())
	if !s.script || !s.known {
		t.Fatalf("the observer did not connect: %+v", s)
	}
	return s
}

// reconnected waits for a connection other than generation gone.
func reconnected(t *testing.T, o *observer, gone uint64) stamp {
	t.Helper()
	var s stamp
	eventually(t, "a new observer connection", func() bool {
		s = o.stamp(context.Background())
		return s.known && s.generation != gone
	})
	return s
}

func mustCheck(t *testing.T, o *observer) answer {
	t.Helper()
	a, ok := o.check(context.Background())
	if !ok {
		t.Fatal("the barrier did not pass")
	}
	return a
}

// The revision counts the messages that change a page, each once it is
// complete, and nothing else: not connected, custom, prune or error messages,
// nor binary ones; and a fragmented message counts once.
func TestObserverCountsCompleteChangeMessagesOnly(t *testing.T) {
	fv := newFakeVite(t)
	o := testObserver(t, fv)
	s := connected(t, o)
	if a := mustCheck(t, o); a.revision != s.revision || a.generation != s.generation {
		t.Fatalf("a quiet app moved from %+v to %+v", s, a)
	}
	for _, msg := range []string{`{"type":"custom","event":"astro:update"}`, `{"type":"prune","paths":[]}`, `{"type":"error","err":{"message":"x"}}`, `not json`, `{"type":"connected"}`} {
		fv.send(msg)
	}
	if a := mustCheck(t, o); a.revision != s.revision {
		t.Fatalf("messages that change nothing were counted: %d", a.revision-s.revision)
	}
	fv.send(`{"type":"update","updates":[{"type":"css-update","path":"/src/a.css"}]}`)
	fv.send(`{"type":"full-reload","path":"*"}`)
	fv.sendFragmented(`{"type":"full-reload","path":"/index.html"}`, 9, func() {})
	fv.mu.Lock()
	for c := range fv.clients {
		c.write(appendFrame(nil, true, opBinary, []byte(`{"type":"update"}`), false))
	}
	fv.mu.Unlock()
	if a := mustCheck(t, o); a.revision != s.revision+3 {
		t.Fatalf("revision moved by %d, want 3", a.revision-s.revision)
	}
}

// A pong that arrives between the fragments of a message is held until the
// message is complete and counted, so the barrier never passes a message the
// app sent before the pong.
func TestAPongDoesNotOvertakeAFragmentedMessage(t *testing.T) {
	fv := newFakeVite(t)
	o := testObserver(t, fv)
	s := connected(t, o)
	got := make(chan answer, 1)
	fv.sendFragmented(`{"type":"full-reload","path":"*"}`, 5, func() {
		go func() {
			a, _ := o.check(context.Background())
			got <- a
		}()
		select {
		case <-fv.pinged:
		case <-time.After(5 * time.Second):
			t.Error("the barrier sent no ping")
		}
		// The pong is on the wire; the message is not complete.
		select {
		case a := <-got:
			t.Errorf("the barrier passed mid-message with %+v", a)
		case <-time.After(100 * time.Millisecond):
		}
	})
	select {
	case a := <-got:
		if a.revision != s.revision+1 || a.generation != s.generation {
			t.Fatalf("after the message: %+v, stamped %+v", a, s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the barrier never passed")
	}
}

// Every connection has a new generation, larger than the last; a barrier
// open when its connection drops answers unavailable at once.
func TestTheGenerationIncreasesOnReconnect(t *testing.T) {
	fv := newFakeVite(t)
	o := testObserver(t, fv)
	first := connected(t, o)
	fv.set(func(fv *fakeVite) { fv.silent = true })
	waiting := make(chan bool, 1)
	go func() {
		_, ok := o.check(context.Background())
		waiting <- ok
	}()
	eventually(t, "the barrier is waiting", func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return len(o.waiters) == 1
	})
	fv.kick()
	select {
	case ok := <-waiting:
		if ok {
			t.Fatal("a barrier on a dropped connection passed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a barrier on a dropped connection kept waiting")
	}
	fv.set(func(fv *fakeVite) { fv.silent = false })
	second := reconnected(t, o, first.generation)
	if second.generation <= first.generation {
		t.Fatalf("generation %d after %d", second.generation, first.generation)
	}
	if a := mustCheck(t, o); a.generation != second.generation {
		t.Fatalf("answered generation %d, connected %d", a.generation, second.generation)
	}
	// A second observer, as for another share, never repeats a generation.
	other := connected(t, testObserver(t, fv))
	if other.generation == first.generation || other.generation == second.generation {
		t.Fatalf("generations repeat across observers: %d", other.generation)
	}
}

// Without a connection the barrier answers unavailable at once, and with
// one whose app does not answer the ping, after the barrier's bound.
func TestTheBarrierTimesOutToUnavailable(t *testing.T) {
	fv := newFakeVite(t)
	o := testObserver(t, fv)
	if _, ok := o.check(context.Background()); ok {
		t.Fatal("a barrier passed with no observer")
	}
	connected(t, o)
	fv.set(func(fv *fakeVite) { fv.silent = true })
	o.barrierWait = 150 * time.Millisecond
	start := time.Now()
	if _, ok := o.check(context.Background()); ok {
		t.Fatal("a barrier passed without a pong")
	}
	if took := time.Since(start); took < 150*time.Millisecond || took > 2*time.Second {
		t.Fatalf("unavailable after %v", took)
	}
	o.mu.Lock()
	left := len(o.waiters)
	o.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d waiters left behind", left)
	}
	// A pong for a ping this observer did not send releases nothing.
	fv.set(func(fv *fakeVite) { fv.silent = false })
	if _, ok := o.pongSeq([]byte("0123456789abcdef")); ok {
		t.Fatal("a foreign pong was accepted")
	}
	if production := newObserver(o.base, o.rt, "x", nil); production.barrierWait != 500*time.Millisecond || production.stampWait != 200*time.Millisecond ||
		production.idle != time.Minute || production.backoffMin != 100*time.Millisecond || production.backoffMax != 5*time.Second {
		t.Fatalf("timings %v %v %v %v %v", production.barrierWait, production.stampWait, production.idle, production.backoffMin, production.backoffMax)
	}
}

// The observer is a native client of the app: no Origin, the app's token,
// the framework's subprotocol and no extension. It connects at the socket's
// path as the page computes it, which includes server.hmr.path, not at the
// direct address the client module also names, which does not.
func TestTheObserverConnectsAsANativeClient(t *testing.T) {
	fv := newFakeVite(t)
	fv.set(func(fv *fakeVite) { fv.base, fv.path = "/app/", "/app/hmr" })
	o := testObserver(t, fv)
	o.handshakeTimeout = 200 * time.Millisecond
	connected(t, o)
	hs := fv.handshakesSoFar()
	if len(hs) != 1 {
		t.Fatalf("%d handshakes", len(hs))
	}
	h := hs[0]
	if h.uri != "/app/hmr?token=SyntheticTok3n" || h.header.Get("Origin") != "" || h.header.Get("Sec-Websocket-Protocol") != "vite-hmr" || h.header.Get("Sec-Websocket-Extensions") != "" {
		t.Fatalf("handshake %s %v", h.uri, h.header)
	}
}

// When the app does not answer at the socket its client module names (the
// module and the socket handled by different layers in front of the dev
// server, say), the observer connects where a visitor's page did.
func TestAnUnansweredHandshakeFallsBackToAVisitorsSocket(t *testing.T) {
	fv := newFakeVite(t)
	fv.set(func(fv *fakeVite) { fv.path, fv.advertised = "/elsewhere/", "/" })
	o := testObserver(t, fv)
	o.handshakeTimeout = 100 * time.Millisecond
	o.stampWait = 300 * time.Millisecond
	o.navigated()
	if s := o.stamp(context.Background()); !s.script || s.known {
		t.Fatalf("a Vite app that cannot be observed yet: %+v", s)
	}
	o.learn(vite, "/elsewhere/?token=SyntheticTok3n")
	o.stampWait = 5 * time.Second
	if s := o.stamp(context.Background()); !s.known {
		t.Fatalf("after a visitor socket: %+v", s)
	}
	hs := fv.handshakesSoFar()
	if hs[0].uri != "/?token=SyntheticTok3n" || hs[len(hs)-1].uri != "/elsewhere/?token=SyntheticTok3n" {
		t.Fatalf("handshakes at %s, then %s", hs[0].uri, hs[len(hs)-1].uri)
	}
}

// A page that dials a separate HMR server, which nothing reaches through the
// share, is never stamped and never waits: while the socket is unconfirmed
// its pages are stamped pending, which a response turns into nothing, and
// the target is treated as unsupported once a few handshakes at the app's
// own port go unanswered.
func TestASeparateHMRServerIsNeverStamped(t *testing.T) {
	fv := newFakeVite(t)
	fv.set(func(fv *fakeVite) { fv.hmrPort, fv.advertised, fv.path = "24678", "/", "/on-another-port/" })
	front, p, _, _ := catchupShare(t, fv, true)
	o := p.targets[0].catchup
	o.handshakeTimeout = 50 * time.Millisecond
	for range 3 {
		start := time.Now()
		if _, body := get(t, front, "/", navigate); strings.Contains(body, "data-purlview") {
			t.Fatalf("stamped %q", body)
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("a navigation waited %v", took)
		}
	}
	eventually(t, "the target is found unsupported", func() bool { return !o.applies() })
	if s := o.stamp(context.Background()); s.script || s.pending {
		t.Fatalf("stamped after giving up: %+v", s)
	}
	n := len(fv.handshakesSoFar())
	if n != unreachedHandshakes {
		t.Fatalf("%d handshakes before giving up", n)
	}
	time.Sleep(300 * time.Millisecond)
	if len(fv.handshakesSoFar()) != n {
		t.Fatal("the observer kept trying after giving up")
	}
}

// With clientPort set to the port the page is served on, the page dials
// through the share to the app's own server: the observer's handshake there
// succeeds and the target is supported.
func TestAClientPortOnTheAppsOwnServerIsObserved(t *testing.T) {
	fv := newFakeVite(t)
	fv.set(func(fv *fakeVite) { fv.hmrPort = "443" })
	o := testObserver(t, fv)
	o.navigated()
	eventually(t, "the observer connects", func() bool { return o.supportedFramework() == vite && fv.clientCount() == 1 })
	if s := o.stamp(context.Background()); !s.known {
		t.Fatalf("stamp %+v", s)
	}
}

// The first page of a fresh proxy for an app with clientPort set to the
// port the page is served on is stamped pending while the socket is
// unconfirmed, and gets an unknown stamp once the handshake at the app's own
// port has succeeded.
func TestAClientPortAppsFirstPageGetsAnUnknownStamp(t *testing.T) {
	fv := newFakeVite(t)
	fv.set(func(fv *fakeVite) {
		fv.hmrPort, fv.acceptWait = "443", 100*time.Millisecond
		fv.onPage = func() { time.Sleep(400 * time.Millisecond) }
	})
	front, _, _, _ := catchupShare(t, fv, true)
	_, body := get(t, front, "/", navigate)
	if s := stampOf(t, body); s == nil || s.Generation != nil {
		t.Fatalf("stamp %+v", s)
	}
}

// A socket on another port that the observer has reached once is never
// given up on: when its server restarts and handshakes go unanswered for a
// while, the target stays supported and the observer reconnects.
func TestAReachedSocketOnAnotherPortIsNotGivenUp(t *testing.T) {
	fv := newFakeVite(t)
	fv.set(func(fv *fakeVite) { fv.hmrPort = "443" })
	o := testObserver(t, fv)
	o.handshakeTimeout = 30 * time.Millisecond
	o.navigated()
	first := reconnected(t, o, 0)
	// The module still names "/", where nothing answers for a while.
	fv.set(func(fv *fakeVite) { fv.advertised, fv.path = "/", "/not-yet/" })
	before := len(fv.handshakesSoFar())
	fv.kick()
	eventually(t, "several unanswered handshakes", func() bool { return len(fv.handshakesSoFar()) >= before+2*unreachedHandshakes })
	if o.supportedFramework() != vite || !o.applies() {
		t.Fatal("a reached socket was given up on")
	}
	fv.set(func(fv *fakeVite) { fv.path = "/" })
	reconnected(t, o, first.generation)
}

// Two barriers whose pings reach the app in the other order are both
// released by the later pong, with the same answer, and a pong for a ping
// already answered releases nothing again.
func TestConcurrentBarriersPassOnTheLatestPong(t *testing.T) {
	fv := newFakeVite(t)
	o := testObserver(t, fv)
	connected(t, o)
	o.mu.Lock()
	c := o.conn
	first := &barrierWaiter{seq: o.pingSeq + 1, conn: c, ch: make(chan answer, 1)}
	second := &barrierWaiter{seq: o.pingSeq + 2, conn: c, ch: make(chan answer, 1)}
	o.pingSeq += 2
	o.waiters = append(o.waiters, first, second)
	o.mu.Unlock()
	fv.send(`{"type":"update","updates":[]}`)
	ping := func(seq uint64) {
		var payload [16]byte
		copy(payload[:8], o.nonce[:])
		payload[15] = byte(seq)
		payload[14] = byte(seq >> 8)
		if err := c.write(opPing, payload[:]); err != nil {
			t.Fatal(err)
		}
	}
	ping(second.seq)
	var answers []answer
	for _, w := range []*barrierWaiter{first, second} {
		select {
		case a := <-w.ch:
			answers = append(answers, a)
		case <-time.After(5 * time.Second):
			t.Fatal("a barrier was not released by a later pong")
		}
	}
	if answers[0] != answers[1] || answers[0].revision != 1 {
		t.Fatalf("answers %+v", answers)
	}
	ping(first.seq)
	if a := mustCheck(t, o); a.revision != 1 {
		t.Fatalf("after a late pong: %+v", a)
	}
}

// The decision that an app is no supported framework wakes a navigation
// waiting for its stamp at once.
func TestAnUnsupportedVerdictEndsTheStampWait(t *testing.T) {
	fv := newFakeVite(t)
	fv.set(func(fv *fakeVite) { fv.modulePath = "" })
	o := testObserver(t, fv)
	o.stampWait = 10 * time.Second
	o.navigated()
	start := time.Now()
	if s := o.stamp(context.Background()); s.script || s.pending {
		t.Fatalf("stamped %+v", s)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the stamp waited %v", took)
	}
}

// After a refused handshake or a restart, the observer finds the endpoint
// again before it reconnects, waiting 100 ms doubling to 5 s (scaled here
// to 10 ms doubling to 40 ms) between attempts.
func TestTheObserverFindsTheEndpointAgainAndBacksOff(t *testing.T) {
	fv := newFakeVite(t)
	o := testObserver(t, fv)
	first := connected(t, o)
	// The dev server restarts with another path and token.
	fv.set(func(fv *fakeVite) { fv.path, fv.token = "/moved/", "NewSyntheticTok3n" })
	fv.kick()
	reconnected(t, o, first.generation)
	hs := fv.handshakesSoFar()
	if last := hs[len(hs)-1]; last.uri != "/moved/?token=NewSyntheticTok3n" {
		t.Fatalf("reconnected at %s", last.uri)
	}
	// Refused handshakes back off, doubling to the maximum. A connection
	// that lasted longer than the maximum starts again at the minimum.
	time.Sleep(60 * time.Millisecond)
	fv.set(func(fv *fakeVite) { fv.refuse = true })
	before := len(fv.handshakesSoFar())
	fv.kick()
	eventually(t, "six refused handshakes", func() bool { return len(fv.handshakesSoFar()) >= before+6 })
	hs = fv.handshakesSoFar()[before:]
	for i, want := range []time.Duration{20, 40, 40, 40} {
		gap := hs[i+1].at.Sub(hs[i].at)
		if gap < want*time.Millisecond*8/10 {
			t.Errorf("attempt %d came %v after the last, want about %v", i+1, gap, want*time.Millisecond)
		}
	}
	for _, h := range hs {
		if h.uri != "/moved/?token=NewSyntheticTok3n" {
			t.Fatalf("attempt at %s", h.uri)
		}
	}
	if n := strings.Count(strings.Join(fv.requestsSoFar(), "\n"), "/@vite/client"); n < before+6 {
		t.Fatalf("the endpoint was looked up %d times for %d handshakes", n, before+6)
	}
}

// An app whose client module is not where discovery looks, as with a Vite
// base path, is observed at the address of the first visitor HMR socket.
// Until then its pages get no script.
func TestTheObserverLearnsTheEndpointFromAVisitorSocket(t *testing.T) {
	fv := newFakeVite(t)
	fv.set(func(fv *fakeVite) { fv.modulePath, fv.path = "/app/@vite/client", "/app/" })
	o := testObserver(t, fv)
	o.stampWait = 100 * time.Millisecond
	o.navigated()
	if s := o.stamp(context.Background()); s.script {
		t.Fatalf("an app not known to be supported was stamped: %+v", s)
	}
	o.learn(vite, "/app/?token=SyntheticTok3n")
	o.stampWait = 5 * time.Second
	s := o.stamp(context.Background())
	if !s.script || !s.known {
		t.Fatalf("after a visitor socket: %+v", s)
	}
	hs := fv.handshakesSoFar()
	if hs[len(hs)-1].uri != "/app/?token=SyntheticTok3n" || hs[len(hs)-1].header.Get("Origin") != "" {
		t.Fatalf("observed at %s", hs[len(hs)-1].uri)
	}
}

// An app that is no supported framework is asked once per visit, and its
// pages are never stamped.
func TestAnUnsupportedAppIsAskedOnceAndNeverStamped(t *testing.T) {
	fv := newFakeVite(t)
	fv.set(func(fv *fakeVite) { fv.modulePath = "" })
	o := testObserver(t, fv)
	for range 5 {
		o.navigated()
		if s := o.stamp(context.Background()); s.script || s.pending {
			t.Fatalf("stamped: %+v", s)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if got := fv.requestsSoFar(); len(got) != 1 || got[0] != "GET /@vite/client" {
		t.Fatalf("the app was asked %v", got)
	}
	if len(fv.handshakesSoFar()) != 0 || o.applies() {
		t.Fatal("an unsupported app was observed")
	}
}

// The observer runs from the first navigation until it has been idle for
// its bound: no visitor HMR socket open, and no navigation since the last
// one closed. The idle bound is scaled down; the checks that it has not
// ended wait several bounds, and the check that it has ended polls, so a
// slow machine makes the test slower, not wrong.
func TestTheObserverStopsAfterTheLastVisitorSocketCloses(t *testing.T) {
	fv := newFakeVite(t)
	o := testObserver(t, fv)
	const idle = 200 * time.Millisecond
	o.idle = idle
	running := func() bool {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.life != nil
	}
	if fv.clientCount() != 0 || running() {
		t.Fatal("observing before any visitor")
	}
	connected(t, o)
	o.socketOpened()
	time.Sleep(4 * idle)
	if !running() || fv.clientCount() != 1 {
		t.Fatal("the observer stopped while a visitor socket was open")
	}
	o.socketOpened()
	o.socketClosed()
	time.Sleep(4 * idle)
	if !running() {
		t.Fatal("the observer stopped while a second visitor socket was open")
	}
	// A navigation after the last close postpones the end: the observer
	// ends no sooner than the bound after it.
	o.socketClosed()
	o.navigated()
	navigated := time.Now()
	eventually(t, "the observer stops", func() bool { return !running() })
	if since := time.Since(navigated); since < idle {
		t.Fatalf("the observer stopped %v after a navigation", since)
	}
	eventually(t, "the app sees the observer go", func() bool { return fv.clientCount() == 0 })
	o.mu.Lock()
	s, _ := o.stampLocked(false)
	o.mu.Unlock()
	if s.known {
		t.Fatal("an observer that has stopped stamps known")
	}
	// The next navigation starts it again, with a new generation.
	connected(t, o)
}

// A visitor HMR socket starts the observer as a navigation does, and shows
// the app is supported.
func TestAVisitorSocketStartsTheObserver(t *testing.T) {
	fv := newFakeVite(t)
	o := testObserver(t, fv)
	o.learn(vite, "/?token=SyntheticTok3n")
	eventually(t, "the observer connects", func() bool { return fv.clientCount() == 1 })
	if o.supportedFramework() != vite {
		t.Fatal("not supported after a visitor socket")
	}
}

// Stopping the proxy ends its observers and every goroutine they run.
func TestStoppingTheProxyEndsItsObservers(t *testing.T) {
	fv := newFakeVite(t)
	target, _ := url.Parse(fv.URL)
	targets, _ := newTargets(target, nil, nil)
	p := newProxy(mustOrigin(t, livePublic), targets, true, nil)
	o := p.targets[0].catchup
	o.stampWait = 5 * time.Second
	connected(t, o)
	o.learn(vite, "/")
	o.socketOpened()
	p.stop() // waits for the observer's goroutines
	if fv.clientCount() != 0 {
		eventually(t, "the app sees the observer go", func() bool { return fv.clientCount() == 0 })
	}
	o.navigated()
	if s := o.stamp(context.Background()); s.script {
		t.Fatal("a stopped observer stamped a page")
	}
	if _, ok := o.check(context.Background()); ok || o.applies() {
		t.Fatal("a stopped observer answered")
	}
	p.closeIdle()
}
