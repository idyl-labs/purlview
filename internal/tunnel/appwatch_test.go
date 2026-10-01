package tunnel

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/share/engine"
)

func kinds(events []engine.ConnEvent) []engine.ConnEventKind {
	var out []engine.ConnEventKind
	for _, ev := range events {
		if ev.Detail != "localhost:3000" {
			panic("event does not name the app")
		}
		out = append(out, ev.Kind)
	}
	return out
}

// settled is what the watch tells the engine once its state has held for the dwell.
func settled(w *appWatch) []engine.ConnEvent {
	w.mu.Lock()
	w.now = func() time.Time { return time.Now().Add(time.Hour) }
	w.mu.Unlock()
	due, _ := w.pending()
	w.mu.Lock()
	w.now = time.Now
	w.mu.Unlock()
	return due
}

func forward(t *testing.T, h http.Handler, method, path string, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "https://k7m2p4qx.purlview.invalid"+path, nil)
	for k, v := range header {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// The private signal is the daemon's alone: it marks the 502 of an app that
// gave no answer and is removed from whatever the app itself sends.
func TestOnlyTheDaemonAssertsThatTheAppIsUnresponsive(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(appStatusHeader, appUnresponsive)
		w.Header().Add(appStatusHeader, "again")
		if r.URL.Path == "/broken" {
			w.WriteHeader(http.StatusBadGateway)
		}
	}))
	target, _ := url.Parse(app.URL)
	watch := newAppWatch("localhost:3000", nil)
	proxy := testProxy(origin{}, target, watch)
	for _, path := range []string{"/", "/broken"} {
		res := forward(t, proxy, "GET", path, nil)
		if _, spoofed := res.Header()[appStatusHeader]; spoofed || (path == "/broken") != (res.Code == http.StatusBadGateway) {
			t.Fatalf("%s: %d %v", path, res.Code, res.Header())
		}
	}
	if got := kinds(settled(watch)); !slices.Equal(got, []engine.ConnEventKind{engine.ConnAppResponding}) {
		t.Fatalf("a 502 from the app is an answer: %v", got)
	}
	app.Close()
	res := forward(t, proxy, "GET", "/", nil)
	if res.Code != http.StatusBadGateway || res.Header().Get(appStatusHeader) != appUnresponsive || res.Body.Len() != 0 {
		t.Fatalf("refused connection: %d %v", res.Code, res.Header())
	}
	if got := kinds(settled(watch)); !slices.Equal(got, []engine.ConnEventKind{engine.ConnAppUnresponsive}) {
		t.Fatalf("events %v", got)
	}
	// A visitor who left says nothing about the app.
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	watch.unanswered(rec, httptest.NewRequest("GET", "/", nil).WithContext(gone), context.Canceled)
	if rec.Code != http.StatusBadGateway || rec.Header().Get(appStatusHeader) != "" {
		t.Fatalf("abandoned request: %d %v", rec.Code, rec.Header())
	}
}

func TestResetBeforeHeadersIsUnresponsiveAndTheNextAnswerResponding(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	answer := make(chan bool, 4)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = http.ReadRequest(bufio.NewReader(c))
			if <-answer {
				_, _ = c.Write([]byte("HTTP/1.1 500 Internal Server Error\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			}
			_ = c.Close()
		}
	}()
	target, _ := url.Parse("http://" + l.Addr().String())
	watch := newAppWatch("localhost:3000", nil)
	proxy := testProxy(origin{}, target, watch)
	var got []engine.ConnEventKind
	for _, answers := range []bool{true, false, false, true} {
		answer <- answers
		res := forward(t, proxy, "POST", "/api", nil)
		if answers != (res.Code == 500) || answers == (res.Header().Get(appStatusHeader) == appUnresponsive) {
			t.Fatalf("answers=%v: %d %v", answers, res.Code, res.Header())
		}
		got = append(got, kinds(settled(watch))...)
	}
	want := []engine.ConnEventKind{engine.ConnAppResponding, engine.ConnAppUnresponsive, engine.ConnAppResponding}
	if !slices.Equal(got, want) {
		t.Fatalf("events %v, want %v", got, want)
	}
}

func TestFirstVisitorIsThePageNavigation(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer app.Close()
	target, _ := url.Parse(app.URL)
	for _, tc := range []struct {
		name, method string
		header       http.Header
		visitor      bool
	}{
		{"navigation", "GET", http.Header{"Sec-Fetch-Mode": {"navigate"}, "Accept": {"text/html"}}, true},
		{"script fetch that accepts html", "GET", http.Header{"Sec-Fetch-Mode": {"cors"}, "Accept": {"text/html"}}, false},
		{"asset", "GET", http.Header{"Sec-Fetch-Mode": {"no-cors"}, "Accept": {"image/png"}}, false},
		{"form post", "POST", http.Header{"Sec-Fetch-Mode": {"navigate"}, "Accept": {"text/html"}}, false},
		{"no fetch metadata, html", "GET", http.Header{"Accept": {"application/xml;q=0.9, Text/HTML"}}, true},
		{"no fetch metadata, api", "GET", http.Header{"Accept": {"application/json"}}, false},
		{"no fetch metadata, nothing accepted", "GET", nil, false},
	} {
		watch := newAppWatch("localhost:3000", nil)
		proxy := testProxy(origin{}, target, watch)
		forward(t, proxy, tc.method, "/", tc.header)
		forward(t, proxy, tc.method, "/", tc.header)
		got := kinds(settled(watch))
		if slices.Contains(got, engine.ConnFirstVisitor) != tc.visitor || len(got) != map[bool]int{true: 2, false: 1}[tc.visitor] {
			t.Errorf("%s: events %v", tc.name, got)
		}
		if tc.visitor && got[0] != engine.ConnFirstVisitor {
			t.Errorf("%s: the visitor comes before the app's state: %v", tc.name, got)
		}
		if len(settled(watch)) != 0 {
			t.Errorf("%s: reported twice", tc.name)
		}
	}
}

func TestEventsReachTheEngineInOrderAndTheChannelCloses(t *testing.T) {
	life, stop := context.WithCancel(context.Background())
	defer stop()
	watch := newAppWatch("localhost:3000", nil)
	watch.dwell = 20 * time.Millisecond
	events := make(chan engine.ConnEvent, 2)
	ended := make(chan engine.ConnEventKind, 1)
	done := make(chan struct{})
	go func() { forwardEvents(life, events, ended, watch); close(done) }()
	next := func() engine.ConnEventKind {
		t.Helper()
		select {
		case ev := <-events:
			return ev.Kind
		case <-time.After(10 * time.Second):
			t.Fatal("no event")
			return ""
		}
	}
	page := httptest.NewRequest("GET", "/", nil)
	page.Header.Set("Sec-Fetch-Mode", "navigate")
	watch.request(page)
	watch.observe(true)
	got := []engine.ConnEventKind{next(), next()}
	// Nobody reads for a while: observing must not wait for the engine.
	for range 100 {
		watch.observe(false)
		watch.observe(true)
	}
	watch.observe(false)
	got = append(got, next())
	ended <- engine.ConnRevoked
	for ev := range events {
		got = append(got, ev.Kind)
	}
	<-done
	n := len(got)
	if n < 4 || got[0] != engine.ConnFirstVisitor || got[1] != engine.ConnAppUnresponsive || got[n-2] != engine.ConnAppResponding || got[n-1] != engine.ConnRevoked {
		t.Fatalf("events %v", got)
	}
	for i := 1; i < n; i++ {
		if got[i] == got[i-1] {
			t.Fatalf("repeated %s: %v", got[i], got)
		}
	}
	// A closed connection releases the forwarder even when nobody reads.
	life2, stop2 := context.WithCancel(context.Background())
	blocked := make(chan engine.ConnEvent)
	done2 := make(chan struct{})
	watch2 := newAppWatch("localhost:3000", nil)
	watch2.dwell = 0
	go func() { forwardEvents(life2, blocked, make(chan engine.ConnEventKind), watch2); close(done2) }()
	watch2.observe(true)
	stop2()
	<-done2
	if _, open := <-blocked; open {
		t.Fatal("channel left open")
	}
}

// An app that flaps while it restarts is not reported: nothing until a state
// has held for the dwell, and the first visitor at once.
func TestFlappingWithinTheDwellReportsNothing(t *testing.T) {
	life, stop := context.WithCancel(context.Background())
	defer stop()
	watch := newAppWatch("localhost:3000", nil)
	watch.dwell = time.Hour
	events := make(chan engine.ConnEvent, 8)
	ended := make(chan engine.ConnEventKind, 1)
	go forwardEvents(life, events, ended, watch)
	page := httptest.NewRequest("GET", "/", nil)
	page.Header.Set("Sec-Fetch-Mode", "navigate")
	watch.request(page)
	for range 1000 {
		watch.observe(true)
		watch.observe(false)
	}
	if ev := <-events; ev.Kind != engine.ConnFirstVisitor {
		t.Fatalf("first event %+v", ev)
	}
	ended <- engine.ConnLost
	var got []engine.ConnEventKind
	for ev := range events {
		got = append(got, ev.Kind)
	}
	if !slices.Equal(got, []engine.ConnEventKind{engine.ConnLost}) {
		t.Fatalf("events %v", got)
	}
}

// The dwell on a controlled clock: a change is reported once the new state has
// held for a second, exactly once, and reports strictly alternate.
func TestAppStateIsReportedOnceItHasHeldForTheDwell(t *testing.T) {
	if appStateDwell != time.Second {
		t.Fatalf("dwell %s: the contract says one second", appStateDwell)
	}
	at := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC)
	watch := newAppWatch("localhost:3000", nil)
	watch.now = func() time.Time { return at }
	var reports []engine.ConnEventKind
	after := func(d time.Duration) (int, time.Duration) {
		at = at.Add(d)
		due, wait := watch.pending()
		reports = append(reports, kinds(due)...)
		return len(due), wait
	}
	watch.observe(true)
	if n, wait := after(999 * time.Millisecond); n != 0 || wait != time.Millisecond {
		t.Fatalf("before the dwell: %d events, wait %s", n, wait)
	}
	if n, wait := after(time.Millisecond); n != 1 || wait != 0 {
		t.Fatalf("at the dwell: %d events, wait %s", n, wait)
	}
	if n, _ := after(time.Hour); n != 0 {
		t.Fatal("a held state was reported twice")
	}
	// A false alarm and its correction within the dwell cancel out.
	watch.observe(false)
	after(400 * time.Millisecond)
	watch.observe(true)
	if n, wait := after(5 * time.Second); n != 0 || wait != 0 {
		t.Fatalf("a flap back to the reported state: %d events, wait %s", n, wait)
	}
	// Each change restarts the dwell; repeats of one state do not.
	watch.observe(false)
	after(900 * time.Millisecond)
	watch.observe(false)
	if n, wait := after(50 * time.Millisecond); n != 0 || wait != 50*time.Millisecond {
		t.Fatalf("a repeated observation restarted the dwell: %d events, wait %s", n, wait)
	}
	watch.observe(true)
	watch.observe(false)
	if n, wait := after(999 * time.Millisecond); n != 0 || wait != time.Millisecond {
		t.Fatalf("a change must restart the dwell: %d events, wait %s", n, wait)
	}
	after(time.Millisecond)
	want := []engine.ConnEventKind{engine.ConnAppUnresponsive, engine.ConnAppResponding}
	if !slices.Equal(reports, want) || len(watch.wake) != 1 {
		t.Fatalf("reports %v, want %v", reports, want)
	}
}
