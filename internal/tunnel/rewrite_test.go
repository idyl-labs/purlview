package tunnel

import (
	"bytes"
	"errors"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// testTargets is a typical share of several targets: a Vite front end
// first, an API second and a default-port internal host third.
func testTargets(t *testing.T, listed ...string) []*target {
	t.Helper()
	if len(listed) == 0 {
		listed = []string{"http://localhost:5173", "http://localhost:8000", "https://staging.internal"}
	}
	admitted, _ := url.Parse(listed[0])
	targets, mismatch := newTargets(admitted, listed, nil)
	if mismatch != "" {
		t.Fatal(mismatch)
	}
	return targets
}

func rewriteAll(set *needleSet, in string) string {
	out, pending, _ := set.process([]byte(in), true, -1)
	if len(pending) != 0 {
		panic("pending at eof")
	}
	return string(out)
}

const (
	pub  = "https://k7m2p4qx.purlview.invalid"
	pub2 = pub + "/_purlview/t/2"
	pub3 = pub + "/_purlview/t/3"
)

// Exact references to a listed target's address, in every spelling the
// rewrite knows, become the public form; everything else is left alone.
func TestNeedlesReplaceExactAddressesOnly(t *testing.T) {
	set := newNeedleSet(mustOrigin(t, livePublic), testTargets(t))
	cases := map[string]string{
		// The first target owns the origin, in http and WebSocket spellings.
		`<a href="http://localhost:5173/x?y=1">`:   `<a href="` + pub + `/x?y=1">`,
		`new WebSocket("ws://localhost:5173/hmr")`: `new WebSocket("wss://k7m2p4qx.purlview.invalid/hmr")`,
		`http://localhost:5173`:                    pub,
		// The loopback names of a loopback target are the target.
		`http://127.0.0.1:5173/ http://[::1]:5173/`: pub + `/ ` + pub + `/`,
		// A further target's address gains its mount.
		`fetch("http://localhost:8000/api")`: `fetch("` + pub2 + `/api")`,
		`ws://127.0.0.1:8000/s`:              `wss://k7m2p4qx.purlview.invalid/_purlview/t/2/s`,
		// The JSON escaped-slash spelling, replaced in the same spelling.
		`{"api":"http:\/\/localhost:8000\/j"}`: `{"api":"https:\/\/k7m2p4qx.purlview.invalid\/_purlview\/t\/2\/j"}`,
		`"ws:\/\/localhost:5173\/"`:            `"wss:\/\/k7m2p4qx.purlview.invalid\/"`,
		// Protocol-relative, unless it is the tail of another scheme.
		`src="//localhost:8000/p"`: `src="//k7m2p4qx.purlview.invalid/_purlview/t/2/p"`,
		`ftp://localhost:8000/p`:   `ftp://localhost:8000/p`,
		// An explicit port is followed by no digit.
		`http://localhost:51730/x http://localhost:80001`: `http://localhost:51730/x http://localhost:80001`,
		// A default-port target matches with and without its port, and only
		// where its name ends.
		`https://staging.internal/x https://staging.internal:443/y https://staging.internal`:                                 pub3 + `/x ` + pub3 + `/y ` + pub3,
		`wss://staging.internal/s "https://staging.internal"`:                                                                `wss://k7m2p4qx.purlview.invalid/_purlview/t/3/s "` + pub3 + `"`,
		`https://staging.internal.evil/ https://staging.internal_x https://staging.internal:8443/ https://staging.internals`: `https://staging.internal.evil/ https://staging.internal_x https://staging.internal:8443/ https://staging.internals`,
		// Deliberately not replaced.
		`localhost:5173 https://localhost:5173 HTTP://LOCALHOST:5173 http://localhost:9999 http://0.0.0.0:5173 http%3A%2F%2Flocalhost%3A5173`: `localhost:5173 https://localhost:5173 HTTP://LOCALHOST:5173 http://localhost:9999 http://0.0.0.0:5173 http%3A%2F%2Flocalhost%3A5173`,
		pub + `/already`:  pub + `/already`,
		``:                ``,
		`no address here`: `no address here`,
		// Adjacent and repeated addresses.
		`http://localhost:5173http://localhost:8000`: pub + pub2,
		`ws://localhost:8000ws://localhost:8000`:     `wss://k7m2p4qx.purlview.invalid/_purlview/t/2wss://k7m2p4qx.purlview.invalid/_purlview/t/2`,
	}
	for in, want := range cases {
		if got := rewriteAll(set, in); got != want {
			t.Errorf("%q\n got %q\nwant %q", in, got, want)
		}
	}
	// A public origin with an explicit port keeps it in every form.
	ported := newNeedleSet(mustOrigin(t, "https://k7m2p4qx.purlview-content.test:8443"), testTargets(t))
	if got := rewriteAll(ported, `http://localhost:8000/a ws://localhost:5173/b //localhost:5173/c`); got != `https://k7m2p4qx.purlview-content.test:8443/_purlview/t/2/a wss://k7m2p4qx.purlview-content.test:8443/b //k7m2p4qx.purlview-content.test:8443/c` {
		t.Errorf("origin with a port: %q", got)
	}
	// Without a public origin, or without targets, nothing is replaced.
	for _, empty := range []*needleSet{newNeedleSet(origin{}, testTargets(t)), newNeedleSet(mustOrigin(t, livePublic), nil)} {
		if !empty.empty() || rewriteAll(empty, "http://localhost:5173/") != "http://localhost:5173/" {
			t.Error("an empty table replaced something")
		}
	}
	// A target that is not loopback has no other names.
	lan := newNeedleSet(mustOrigin(t, livePublic), testTargets(t, "http://192.168.1.20:8080"))
	if got := rewriteAll(lan, `http://192.168.1.20:8080/x http://localhost:8080/x`); got != pub+`/x http://localhost:8080/x` {
		t.Errorf("LAN target: %q", got)
	}
}

func TestMountsAreRecognisedOnlyInTheirCanonicalSpelling(t *testing.T) {
	for in, want := range map[string]struct {
		n    int
		rest string
		ok   bool
	}{
		"/_purlview/t/2":                {2, "", true},
		"/_purlview/t/2/":               {2, "/", true},
		"/_purlview/t/2/x?q=1":          {2, "/x?q=1", true},
		"/_purlview/t/2?q=1":            {2, "?q=1", true},
		"/_purlview/t/2#f":              {2, "#f", true},
		"/_purlview/t/99/x":             {99, "/x", true},
		"/_purlview/t/10/x":             {10, "/x", true},
		"/_purlview/t/1":                {},
		"/_purlview/t/1/":               {},
		"/_purlview/t/0/x":              {},
		"/_purlview/t/02/x":             {},
		"/_purlview/t/100/x":            {},
		"/_purlview/t/2x/echo":          {},
		"/_purlview/t/":                 {},
		"/_purlview/t":                  {},
		"/_purlview/t/a":                {},
		"/%5Fpurlview/t/2/x":            {},
		"/_purlview/t/%32/x":            {},
		"/x/_purlview/t/2/x":            {},
		"/_purlview/redeem":             {},
		"/_purlview/t/2/_purlview/t/3/": {2, "/_purlview/t/3/", true},
		"":                              {},
		"/":                             {},
	} {
		n, rest, ok := cutMount(in)
		if n != want.n || rest != want.rest || ok != want.ok {
			t.Errorf("%q: %d %q %v", in, n, rest, ok)
		}
	}
	for n, want := range map[int]string{1: "", 2: "/_purlview/t/2", 10: "/_purlview/t/10"} {
		if got := mountFor(n); got != want {
			t.Errorf("mount %d: %q", n, got)
		}
	}
}

// The served list: the admitted target first, the further ones mounted; a
// disagreement between admission and the list serves the admitted target
// alone; a path on a further target is ignored.
func TestServedTargetsFollowTheAdmittedOne(t *testing.T) {
	admitted, _ := url.Parse("http://127.0.0.1:5173")
	targets, mismatch := newTargets(admitted, []string{"http://localhost:5173/review?round=1", "http://localhost:8000/ignored?too", "https://staging.internal"}, nil)
	if mismatch != "" || len(targets) != 3 {
		t.Fatalf("%d targets, %q", len(targets), mismatch)
	}
	for i, want := range []struct{ url, mount, name string }{{"http://127.0.0.1:5173", "", "127.0.0.1:5173"}, {"http://localhost:8000", "/_purlview/t/2", "localhost:8000"}, {"https://staging.internal", "/_purlview/t/3", "staging.internal"}} {
		if got := targets[i]; got.url.String() != want.url || got.mount != want.mount || got.name != want.name || got.watch == nil || got.watch.target != want.name {
			t.Errorf("target %d: %+v", i+1, got)
		}
	}
	if targets[0].watch.wake != targets[1].watch.wake {
		t.Error("the watchers must share one wake channel")
	}
	targets, mismatch = newTargets(admitted, []string{"http://localhost:3000", "http://localhost:8000"}, nil)
	if !strings.Contains(mismatch, "not the first listed target") || len(targets) != 1 || targets[0].mount != "" {
		t.Errorf("mismatch: %d targets, %q", len(targets), mismatch)
	}
	if targets, mismatch = newTargets(admitted, nil, nil); mismatch != "" || len(targets) != 1 {
		t.Errorf("no list: %d targets, %q", len(targets), mismatch)
	}
}

// The streaming rewriter produces the same bytes whatever the chunking, and
// holds back only what could still become a match.
func TestRewriterIsChunkingInvariant(t *testing.T) {
	set := newNeedleSet(mustOrigin(t, livePublic), testTargets(t))
	corpus := strings.Repeat(`<script>fetch("http://localhost:8000/api").then(r=>r.json());const ws=new WebSocket("ws://localhost:5173/hmr");</script>
data: {"url":"http:\/\/localhost:8000\/x","other":"http://localhost:80001","rel":"//localhost:8000/p","ftp":"ftp://localhost:8000/"}

https://staging.internal https://staging.internal:443/x https://staging.internals http://localhost:517 http://localhost:5173
hhhhttp://localhost:5173/ ws://ws://localhost:8000/ //localhost:51 //localhost:5173
`, 8)
	want := rewriteAll(set, corpus)
	if want == corpus || !strings.Contains(want, pub2+"/api") {
		t.Fatal("the corpus exercises nothing")
	}
	if got := string(oracle(set.needles, []byte(corpus))); got != want {
		t.Fatalf("the matcher disagrees with the oracle on the corpus:\n got %q\nwant %q", want, got)
	}
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 200; round++ {
		var chunks [][]byte
		rest := []byte(corpus)
		for len(rest) > 0 {
			n := 1 + rng.Intn(min(len(rest), 40))
			if round%3 == 0 {
				n = 1 + rng.Intn(len(rest))
			}
			chunks, rest = append(chunks, rest[:n]), rest[n:]
		}
		r := newRewriter(io.NopCloser(&chunkReader{chunks: chunks}), set)
		got, err := io.ReadAll(r)
		if err != nil || string(got) != want {
			t.Fatalf("round %d (%d chunks): %v\n got %q\nwant %q", round, len(chunks), err, got, want)
		}
	}
	// Every possible split of one address across two chunks.
	one := `x http://localhost:8000/api y`
	for cut := 0; cut <= len(one); cut++ {
		r := newRewriter(io.NopCloser(&chunkReader{chunks: [][]byte{[]byte(one[:cut]), []byte(one[cut:])}}), set)
		if got, _ := io.ReadAll(r); string(got) != `x `+pub2+`/api y` {
			t.Fatalf("cut at %d: %q", cut, got)
		}
	}
}

// oracle rewrites in with the rewrite rules written out directly, without
// the trie or the hold-back: at each position the longest needle that starts
// there and passes its delimiter rules is replaced. Each rule is read off the needle's spelling, not its flags, so
// the two implementations share nothing but the table of spellings.
func oracle(needles []needle, in []byte) []byte {
	out := make([]byte, 0, len(in))
	for i := 0; i < len(in); {
		best, bestLen := -1, 0
		for k, n := range needles {
			text := n.text
			if len(text) <= bestLen || !bytes.HasPrefix(in[i:], text) {
				continue
			}
			if bytes.HasPrefix(text, []byte("//")) && i > 0 && in[i-1] == ':' {
				continue // the tail of another scheme's address
			}
			if end := i + len(text); end < len(in) {
				next := in[end]
				if endsInPort(text) {
					if next >= '0' && next <= '9' {
						continue
					}
				} else if (next >= '0' && next <= '9') || (next >= 'a' && next <= 'z') || (next >= 'A' && next <= 'Z') || next == ':' || next == '-' || next == '.' || next == '_' {
					continue
				}
			}
			best, bestLen = k, len(text)
		}
		if best < 0 {
			out = append(out, in[i])
			i++
			continue
		}
		out = append(out, needles[best].repl...)
		i += bestLen
	}
	return out
}

// endsInPort reports a spelling that ends in ":<digits>".
func endsInPort(text []byte) bool {
	i := bytes.LastIndexByte(text, ':')
	if i < 0 || i == len(text)-1 {
		return false
	}
	for _, b := range text[i+1:] {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}

// Random inputs built from the needles' spellings and their near misses, cut
// into random chunks down to single bytes: the streaming matcher agrees with
// the oracle byte for byte, and what it holds back never exceeds the longest
// needle plus one byte of delimiter look-ahead.
func TestRewriterAgreesWithTheOracle(t *testing.T) {
	set := newNeedleSet(mustOrigin(t, livePublic), testTargets(t))
	fragments := []string{
		"http://localhost:8000", "http://127.0.0.1:8000", "http://[::1]:8000", "http://localhost:5173", "http://127.0.0.1:5173", "http://[::1]:5173",
		"ws://localhost:8000", "ws://127.0.0.1:5173", "wss://localhost:5173", `http:\/\/localhost:8000`, `ws:\/\/127.0.0.1:5173`, `http:\/\/[::1]:8000`,
		"//localhost:8000", "//127.0.0.1:5173", "//[::1]:8000", `\/\/localhost:8000`,
		"https://staging.internal", "https://staging.internal:443", "wss://staging.internal", `https:\/\/staging.internal`, "//staging.internal",
		"https://staging.internals", "https://staging.internal.evil", "https://staging.internal_x", "https://staging.internal:8443", "https://staging.internal-x", "https://staging.internal:443x",
		":51730", "0", "1", "9", ".evil", "_", "-", "x", "X", "A", "a", "z", "Z",
		"HTTP://LOCALHOST:8000", "http://LocalHost:8000", "Http://localhost:8000", "ws://LOCALHOST:5173",
		"http://localhost:9999", "https://localhost:5173", "localhost:5173", "http://localhost:800", "http://localhost:80001", "http://localhost", "ftp:", "ftp://localhost:8000", "ftp://[::1]:8000",
		"\n", "\r", ">", `"`, "/", "//", "h", "w", ":", " ", `\/`, "ht", "http:/", "http://l", "http://localhost:80", "[::1]", "127.0.0.1",
	}
	rng := rand.New(rand.NewSource(7))
	for round := 0; round < 3000; round++ {
		var in []byte
		for range 1 + rng.Intn(24) {
			in = append(in, fragments[rng.Intn(len(fragments))]...)
		}
		want := oracle(set.needles, in)
		// Through the reader, with random chunk sizes.
		var chunks [][]byte
		for rest := in; len(rest) > 0; {
			n := 1
			if round%3 != 0 {
				n = 1 + rng.Intn(min(len(rest), 17))
			}
			chunks, rest = append(chunks, rest[:n]), rest[n:]
		}
		got, err := io.ReadAll(newRewriter(io.NopCloser(&chunkReader{chunks: chunks}), set))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("round %d (%d chunks): %v\n  in %q\n got %q\nwant %q", round, len(chunks), err, in, got, want)
		}
		// Through process, watching the hold-back after every chunk.
		var out, pending []byte
		prev := -1
		for i, chunk := range chunks {
			var piece []byte
			piece, pending, prev = set.process(append(append([]byte{}, pending...), chunk...), i == len(chunks)-1, prev)
			out = append(out, piece...)
			if len(pending) > set.maxLen+1 {
				t.Fatalf("round %d chunk %d: held back %d bytes, more than %d: %q", round, i, len(pending), set.maxLen+1, pending)
			}
		}
		if len(pending) != 0 || !bytes.Equal(out, want) {
			t.Fatalf("round %d: process left %q pending\n got %q\nwant %q", round, pending, out, want)
		}
	}
}

type chunkReader struct{ chunks [][]byte }

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[0])
	if n < len(c.chunks[0]) {
		c.chunks[0] = c.chunks[0][n:]
	} else {
		c.chunks = c.chunks[1:]
	}
	return n, nil
}

// What a chunk leaves pending is exactly a possible start of a match: a
// server-sent event, a line, a tag or a string ends with nothing held.
func TestRewriterHoldsBackOnlyAPossibleMatch(t *testing.T) {
	set := newNeedleSet(mustOrigin(t, livePublic), testTargets(t))
	for in, want := range map[string]struct{ out, pending string }{
		"data: hello\n\n":                {"data: hello\n\n", ""},
		"<p>x</p>\r\n":                   {"<p>x</p>\r\n", ""},
		`"http://localhost:8000/x"`:      {`"` + pub2 + `/x"`, ""},
		"see http://loc":                 {"see ", "http://loc"},
		"see http://localhost:8000":      {"see ", "http://localhost:8000"}, // the delimiter is the next byte
		"see http://localhost:8000/":     {"see " + pub2, "/"},              // a slash can begin //host
		"see http://localhost:8000/x":    {"see " + pub2 + "/x", ""},
		"see https://staging.internal":   {"see ", "https://staging.internal"},
		"see https://staging.internal\n": {"see " + pub3 + "\n", ""},
		"a/":                             {"a", "/"},
		"a//":                            {"a", "//"},
		"a//x":                           {"a//x", ""},
		"ftp:":                           {"ftp:", ""},
		"h":                              {"", "h"},
		"http://localhost:9":             {"http://localhost:9", ""},
		"":                               {"", ""},
	} {
		out, pending, _ := set.process([]byte(in), false, -1)
		if string(out) != want.out || string(pending) != want.pending {
			t.Errorf("%q: out %q pending %q, want %q %q", in, out, pending, want.out, want.pending)
		}
	}
	// The look-behind for a protocol-relative address survives a chunk
	// boundary: "ftp:" then "//localhost:8000/" stays as it was.
	r := newRewriter(io.NopCloser(&chunkReader{chunks: [][]byte{[]byte("ftp:"), []byte("//localhost:8000/ "), []byte("//localhost:8000/")}}), set)
	if got, _ := io.ReadAll(r); string(got) != "ftp://localhost:8000/ //k7m2p4qx.purlview.invalid/_purlview/t/2/" {
		t.Errorf("look-behind: %q", got)
	}
	// A body ending in a complete address is replaced at the end.
	r = newRewriter(io.NopCloser(strings.NewReader("go to http://localhost:8000")), set)
	if got, _ := io.ReadAll(r); string(got) != "go to "+pub2 {
		t.Errorf("at end: %q", got)
	}
	// A read error after some bytes still delivers them, then the error.
	r = newRewriter(io.NopCloser(io.MultiReader(strings.NewReader("ok http://localhost:8000/x"), &failingReader{})), set)
	got, err := io.ReadAll(r)
	if string(got) != "ok "+pub2+"/x" || err == nil || errors.Is(err, io.EOF) {
		t.Errorf("error: %q %v", got, err)
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestRewritableTypes(t *testing.T) {
	for ct, want := range map[string]bool{
		"text/html":                         true,
		"text/html; charset=utf-8":          true,
		"TEXT/HTML; charset=UTF-8":          true,
		"text/css":                          true,
		"text/javascript":                   true,
		"text/plain; charset=iso-8859-1":    true,
		"text/event-stream":                 true,
		"application/javascript":            true,
		"application/x-javascript":          true,
		"application/ecmascript":            true,
		"application/json":                  true,
		"application/json; charset=utf-8":   true,
		"application/manifest+json":         true,
		"application/ld+json":               true,
		"application/problem+json":          true,
		"application/xml":                   true,
		"application/xhtml+xml":             true,
		"application/atom+xml":              true,
		"text/html; charset=utf-16":         false,
		"text/html; charset=UTF-16LE":       false,
		"text/plain; charset=utf-32":        false,
		"image/svg+xml":                     false,
		"image/png":                         false,
		"application/octet-stream":          false,
		"application/wasm":                  false,
		"application/pdf":                   false,
		"application/zip":                   false,
		"application/x-ndjson":              false,
		"multipart/form-data; boundary=x":   false,
		"video/mp4":                         false,
		"font/woff2":                        false,
		"":                                  false,
		"text":                              false,
		"text/html; charset":                false,
		"application/vnd.api+json":          true,
		"application/graphql-response+json": true,
	} {
		h := http.Header{}
		if ct != "" {
			h.Set("Content-Type", ct)
		}
		if got := rewritableType(h); got != want {
			t.Errorf("%q: %v", ct, got)
		}
	}
}

// The response rules: which responses are rewritten, and what a rewritten
// response loses and keeps.
func TestRewriteResponse(t *testing.T) {
	set := newNeedleSet(mustOrigin(t, livePublic), testTargets(t))
	const body = `<a href="http://localhost:8000/api">`
	response := func(status int, method string, header http.Header) *http.Response {
		res := &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: &http.Request{Method: method}}
		res.Header.Set("Content-Length", "36")
		return res
	}
	html := func(extra ...string) http.Header {
		h := http.Header{"Content-Type": {"text/html; charset=utf-8"}, "Etag": {`"abc"`}, "Accept-Ranges": {"bytes"}, "Last-Modified": {"Tue, 22 Sep 2026 14:47:19 GMT"}}
		for i := 0; i+1 < len(extra); i += 2 {
			h.Set(extra[i], extra[i+1])
		}
		return h
	}
	read := func(res *http.Response) string {
		b, _ := io.ReadAll(res.Body)
		return string(b)
	}
	var logged bytes.Buffer
	logger := log.New(&logged, "", 0)

	res := response(200, "GET", html())
	rewriteResponse(res, set, logger)
	if got := read(res); got != `<a href="`+pub2+`/api">` || res.ContentLength != -1 || res.Header.Get("Content-Length") != "" || res.Header.Get("Accept-Ranges") != "" || res.Header.Get("ETag") != `W/"abc"` || res.Header.Get("Last-Modified") == "" {
		t.Errorf("rewritten 200: %q %d %v", got, res.ContentLength, res.Header)
	}
	// A weak validator stays as it is.
	res = response(200, "GET", html("ETag", `W/"abc"`))
	rewriteResponse(res, set, logger)
	if res.Header.Get("ETag") != `W/"abc"` {
		t.Errorf("weak ETag: %q", res.Header.Get("ETag"))
	}
	// A HEAD has no body; its length is dropped so it does not contradict the GET.
	res = response(200, "HEAD", html())
	rewriteResponse(res, set, logger)
	if read(res) != body || res.ContentLength != -1 || res.Header.Get("Content-Length") != "" || res.Header.Get("ETag") != `W/"abc"` {
		t.Errorf("HEAD: %d %v", res.ContentLength, res.Header)
	}
	// A 304 of a rewritable type has its validator weakened and nothing else.
	res = response(304, "GET", html())
	rewriteResponse(res, set, logger)
	if read(res) != body || res.Header.Get("ETag") != `W/"abc"` || res.Header.Get("Content-Length") != "36" || res.Header.Get("Accept-Ranges") != "bytes" {
		t.Errorf("304: %v", res.Header)
	}
	// Left alone: no type, a binary type, an upgrade, a partial body, a compressed body, a UTF-16 body, other bodiless statuses.
	untouched := []struct {
		name   string
		status int
		header http.Header
	}{
		{"no type", 200, http.Header{"Etag": {`"abc"`}}},
		{"binary", 200, http.Header{"Content-Type": {"image/png"}, "Etag": {`"abc"`}}},
		{"upgrade", 101, html("Upgrade", "websocket")},
		{"upgrade header on a 200", 200, html("Upgrade", "h2c")},
		{"partial", 206, html("Content-Range", "bytes 0-35/100")},
		{"gzip", 200, html("Content-Encoding", "gzip")},
		{"br", 200, html("Content-Encoding", "BR")},
		{"utf-16", 200, html("Content-Type", "text/html; charset=utf-16")},
		{"204", 204, html()},
		{"205", 205, html()},
	}
	for _, tc := range untouched {
		logged.Reset()
		res = response(tc.status, "GET", tc.header)
		rewriteResponse(res, set, logger)
		if got := read(res); got != body || res.ContentLength != 36 || res.Header.Get("Content-Length") != "36" || res.Header.Get("ETag") != tc.header.Get("ETag") || res.Header.Get("Accept-Ranges") != tc.header.Get("Accept-Ranges") {
			t.Errorf("%s: rewritten: %q %d %v", tc.name, got, res.ContentLength, res.Header)
		}
		if isCompressed := compressed(tc.header); isCompressed != strings.Contains(logged.String(), "left alone") {
			t.Errorf("%s: logged %q", tc.name, logged.String())
		}
	}
	// Identity is not compression; an empty table rewrites nothing.
	res = response(200, "GET", html("Content-Encoding", "identity"))
	rewriteResponse(res, set, logger)
	if read(res) != `<a href="`+pub2+`/api">` {
		t.Error("identity encoding must be rewritten")
	}
	res = response(200, "GET", html())
	rewriteResponse(res, newNeedleSet(origin{}, nil), logger)
	if read(res) != body || res.Header.Get("Content-Length") != "36" || res.Header.Get("ETag") != `"abc"` {
		t.Errorf("empty table: %v", res.Header)
	}
	// A nil logger is fine.
	rewriteResponse(response(200, "GET", html("Content-Encoding", "gzip")), set, nil)
}
