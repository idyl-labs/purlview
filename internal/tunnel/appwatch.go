package tunnel

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/idyl-labs/purlview/internal/share/engine"
)

// The daemon's private signal to the gateway: this
// 502 means the app gave no answer, as opposed to a 502 the app produced. The
// daemon removes the header from the app's own responses, so only the daemon
// can assert it; the gateway never passes it to a browser.
const (
	appStatusHeader = "Purlview-App-Status"
	appUnresponsive = "unresponsive"
)

// appStateDwell is how long the app's state must hold before it is reported.
// While an app restarts under parallel load, refused and answered requests
// interleave, and one request on a keep-alive connection the app has just
// closed fails although the app is up; neither is a change worth a line.
const appStateDwell = time.Second

// appWatch observes forwarded requests for the share's visitor and app events.
// It must never slow proxying: an observation only updates state under a mutex
// and wakes the connection's event forwarder, which alone writes to the engine.
// The first visitor is reported at once. The app's state is reported once it
// has held for the dwell, so flapping within it reports nothing, and changes
// coalesce into the latest state when the engine reads slowly. That keeps
// memory bounded and unresponsive and responding strictly alternating. The 502
// with the private header is not delayed: it answers each request on its own.
type appWatch struct {
	target string // as people read it: localhost:3000
	wake   chan struct{}
	dwell  time.Duration
	now    func() time.Time

	mu           sync.Mutex
	visitor      bool // a page navigation was forwarded
	visitorSent  bool
	observed     bool      // the app answered or failed to at least once
	down         bool      // latest observation
	since        time.Time // when the latest observation's state began
	reported     bool
	reportedDown bool
}

// newAppWatch watches one target. The watchers of one connection share a
// wake channel, so its forwarder can wait on all of them at once.
func newAppWatch(target string, wake chan struct{}) *appWatch {
	if wake == nil {
		wake = make(chan struct{}, 1)
	}
	return &appWatch{target: target, wake: wake, dwell: appStateDwell, now: time.Now}
}

func (w *appWatch) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// navigation reports a page navigation: a GET that the browser marks as one,
// or, from clients that send no fetch metadata, a GET that accepts HTML.
func navigation(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	for _, accept := range r.Header.Values("Accept") {
		if strings.Contains(strings.ToLower(accept), "text/html") {
			return true
		}
	}
	return false
}

// request observes a request on its way to the app.
func (w *appWatch) request(r *http.Request) {
	if !navigation(r) {
		return
	}
	w.mu.Lock()
	first := !w.visitor
	w.visitor = true
	w.mu.Unlock()
	if first {
		w.signal()
	}
}

func (w *appWatch) observe(down bool) {
	w.mu.Lock()
	changed := !w.observed || w.down != down
	if changed {
		w.observed, w.down, w.since = true, down, w.now()
	}
	w.mu.Unlock()
	if changed {
		w.signal()
	}
}

// answered observes any response from the app, whatever its status, and
// removes the daemon's own signal from it.
func (w *appWatch) answered(r *http.Response) {
	r.Header.Del(appStatusHeader)
	w.observe(false)
}

// unanswered is the proxy's error handler: the app refused the connection,
// reset it before response headers or exceeded the response-header timeout. A
// request the visitor abandoned says nothing about the app.
func (w *appWatch) unanswered(rw http.ResponseWriter, r *http.Request, _ error) {
	if r.Context().Err() == nil {
		w.observe(true)
		rw.Header().Set(appStatusHeader, appUnresponsive)
	}
	rw.WriteHeader(http.StatusBadGateway)
}

// forwardEvents owns a connection's event channel after ConnReady: it passes on
// what the proxy observed for every target, then how the connection ended,
// and closes the channel. It stops early once the connection is closed. The
// watchers share one wake channel.
func forwardEvents(life context.Context, events chan<- engine.ConnEvent, ended <-chan engine.ConnEventKind, watches ...*appWatch) {
	defer close(events)
	wake := watches[0].wake
	send := func(ev engine.ConnEvent) bool {
		select {
		case events <- ev:
			return true
		case <-life.Done():
			return false
		}
	}
	settle := time.NewTimer(time.Hour)
	defer settle.Stop()
	for {
		select {
		case kind := <-ended:
			send(engine.ConnEvent{Kind: kind})
			return
		case <-life.Done():
			return
		case <-wake:
		case <-settle.C:
		}
		var soonest time.Duration
		for _, w := range watches {
			due, wait := w.pending()
			for _, ev := range due {
				if !send(ev) {
					return
				}
			}
			if wait > 0 && (soonest == 0 || wait < soonest) {
				soonest = wait
			}
		}
		if soonest > 0 {
			settle.Reset(soonest)
		}
	}
}

// pending returns what the engine has not been told yet, in order: the first
// visitor, then the app's state if it differs from the last report and has
// held for the dwell. Otherwise wait says when such a state will have.
func (w *appWatch) pending() (due []engine.ConnEvent, wait time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.visitor && !w.visitorSent {
		w.visitorSent = true
		due = append(due, engine.ConnEvent{Kind: engine.ConnFirstVisitor, Detail: w.target})
	}
	if !w.observed || (w.reported && w.reportedDown == w.down) {
		return due, 0
	}
	if held := w.now().Sub(w.since); held < w.dwell {
		return due, w.dwell - held
	}
	w.reported, w.reportedDown = true, w.down
	kind := engine.ConnAppResponding
	if w.down {
		kind = engine.ConnAppUnresponsive
	}
	return append(due, engine.ConnEvent{Kind: kind, Detail: w.target}), 0
}
