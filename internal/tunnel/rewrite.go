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
	"io"
	"log"
	"mime"
	"net/http"
	"strings"
)

// Body rewriting. In supported text responses, exact references to a listed
// target's address become its public form: the share's origin for the first
// target, the share's origin plus the mount for a further one. The rewrite is
// one streaming pass over a fixed table of needles with a bounded hold-back;
// nothing buffers a whole body, so a body of any size and a server-sent event
// stream pass through with the same memory.

// needleKind says which delimiter rule a needle carries.
type needleKind uint8

const (
	// kindPort: the address ends in an explicit port; the next byte must not
	// be a digit, so :5173 does not match inside :51730.
	kindPort needleKind = iota
	// kindPortless: a default-port target's address without its port; the
	// next byte must not continue a host name or start a port.
	kindPortless
)

type needle struct {
	text []byte
	repl []byte
	kind needleKind
	// relative is the protocol-relative spelling, which is not matched after
	// a colon: there it is the tail of some other scheme's address.
	relative bool
}

// accepts applies the needle's delimiter rules. prev is the byte before the
// match or -1 at the start of the body; next is the byte after it when
// hasNext, otherwise the body ended there.
func (n *needle) accepts(prev int, hasNext bool, next byte) bool {
	if n.relative && prev == ':' {
		return false
	}
	if !hasNext {
		return true
	}
	if n.kind == kindPort {
		return !isDigit(next)
	}
	return !isHostByte(next)
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// isHostByte is a byte that could continue a host name or begin a port:
// ParseTarget accepts letters, digits, '-', '.' and '_' in a host name.
func isHostByte(b byte) bool {
	return isDigit(b) || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == ':' || b == '-' || b == '.' || b == '_'
}

type trieNode struct {
	next map[byte]int
	term int // 1 + the needle that ends here, 0 for none
}

// needleSet is the compiled rewrite table of one share: a trie of every
// needle, matched longest-first at each candidate position.
type needleSet struct {
	nodes   []trieNode
	needles []needle
	first   [256]bool // bytes that begin a needle
	maxLen  int
}

// newNeedleSet compiles the table for the listed targets. public is the
// share's origin; a target's mount is "" for the first target and its
// /_purlview/t/<n> path for a further one.
func newNeedleSet(public origin, targets []*target) *needleSet {
	s := &needleSet{nodes: []trieNode{{}}}
	if public == (origin{}) {
		return s
	}
	for _, t := range targets {
		s.addTarget(public, t)
	}
	return s
}

func (s *needleSet) addTarget(public origin, t *target) {
	o := t.origin
	if o == (origin{}) {
		return
	}
	ws := "ws"
	if o.scheme == "https" {
		ws = "wss"
	}
	publicWS := "ws"
	if public.scheme == "https" {
		publicWS = "wss"
	}
	pub := public.String() + t.mount
	pubWS := publicWS + strings.TrimPrefix(pub, public.scheme)
	pubRel := strings.TrimPrefix(pub, public.scheme+":")
	hosts := []string{o.host}
	if loopbackNames[o.host] {
		hosts = []string{"localhost", "127.0.0.1", "::1"}
	}
	for _, h := range hosts {
		if strings.Contains(h, ":") {
			h = "[" + h + "]"
		}
		ports := []string{":" + o.port}
		if o.port == defaultPorts[o.scheme] {
			ports = append(ports, "")
		}
		for _, p := range ports {
			kind := kindPort
			if p == "" {
				kind = kindPortless
			}
			s.add(o.scheme+"://"+h+p, pub, kind, false)
			s.add(ws+"://"+h+p, pubWS, kind, false)
			s.add(o.scheme+`:\/\/`+h+p, escapeSlashes(pub), kind, false)
			s.add(ws+`:\/\/`+h+p, escapeSlashes(pubWS), kind, false)
			s.add("//"+h+p, pubRel, kind, true)
		}
	}
}

func escapeSlashes(s string) string { return strings.ReplaceAll(s, "/", `\/`) }

func (s *needleSet) add(text, repl string, kind needleKind, relative bool) {
	node := 0
	for i := 0; i < len(text); i++ {
		b := text[i]
		child, ok := s.nodes[node].next[b]
		if !ok {
			child = len(s.nodes)
			s.nodes = append(s.nodes, trieNode{})
			if s.nodes[node].next == nil {
				s.nodes[node].next = map[byte]int{}
			}
			s.nodes[node].next[b] = child
		}
		node = child
	}
	if s.nodes[node].term != 0 {
		return // the same spelling twice: the first target keeps it
	}
	s.needles = append(s.needles, needle{text: []byte(text), repl: []byte(repl), kind: kind, relative: relative})
	s.nodes[node].term = len(s.needles)
	s.first[text[0]] = true
	s.maxLen = max(s.maxLen, len(text))
}

// empty reports a table with nothing to replace.
func (s *needleSet) empty() bool { return len(s.needles) == 0 }

// matchAt tries the longest needle at buf[i:]. It returns the matched length
// and replacement, or more when the buffer ends before the match, or its
// delimiter look-ahead, can be decided and the body has not ended.
func (s *needleSet) matchAt(buf []byte, i int, eof bool, prev int) (n int, repl []byte, more bool) {
	node := 0
	best := -1
	for k := i; ; k++ {
		if t := s.nodes[node].term; t != 0 {
			if k == len(buf) && !eof {
				return 0, nil, true
			}
			var next byte
			hasNext := k < len(buf)
			if hasNext {
				next = buf[k]
			}
			if s.needles[t-1].accepts(prev, hasNext, next) {
				best, n = t-1, k-i
			}
		}
		if k == len(buf) {
			if !eof {
				return 0, nil, true
			}
			break
		}
		child, ok := s.nodes[node].next[buf[k]]
		if !ok {
			break
		}
		node = child
	}
	if best < 0 {
		return 0, nil, false
	}
	return n, s.needles[best].repl, false
}

// process rewrites one chunk. It returns what can be written through and the
// tail that must wait for more input: the longest suffix that is a prefix of
// some needle, or a complete needle whose delimiter is the next byte. prev is
// the byte before the chunk, -1 for none; the returned prev is the byte before
// the pending tail, for the protocol-relative look-behind across writes.
func (s *needleSet) process(chunk []byte, eof bool, prev int) (out, pending []byte, prevOut int) {
	out = make([]byte, 0, len(chunk)+64)
	i := 0
	for i < len(chunk) {
		j := i
		for j < len(chunk) && !s.first[chunk[j]] {
			j++
		}
		out = append(out, chunk[i:j]...)
		if j == len(chunk) {
			i = j
			break
		}
		i = j
		before := prev
		if i > 0 {
			before = int(chunk[i-1])
		}
		n, repl, more := s.matchAt(chunk, i, eof, before)
		if more {
			return out, chunk[i:], before
		}
		if n == 0 {
			out = append(out, chunk[i])
			i++
			continue
		}
		out = append(out, repl...)
		i += n
	}
	if i > 0 {
		prev = int(chunk[i-1])
	}
	return out, nil, prev
}

// rewriter streams a body through a needle set. Read returns rewritten bytes
// as soon as they cannot be part of a match, so a flushed event reaches the
// browser without waiting for the next one.
type rewriter struct {
	src     io.ReadCloser
	set     *needleSet
	buf     []byte
	pending []byte
	out     []byte
	prev    int
	eof     bool
	err     error
}

func newRewriter(src io.ReadCloser, set *needleSet) *rewriter {
	return &rewriter{src: src, set: set, buf: make([]byte, 32<<10), prev: -1}
}

func (r *rewriter) Read(p []byte) (int, error) {
	for len(r.out) == 0 {
		if r.eof {
			return 0, r.err
		}
		n, err := r.src.Read(r.buf)
		if err != nil {
			r.eof = true
			r.err = err
		}
		chunk := make([]byte, 0, len(r.pending)+n)
		chunk = append(append(chunk, r.pending...), r.buf[:n]...)
		r.out, r.pending, r.prev = r.set.process(chunk, r.eof, r.prev)
		if r.eof && len(r.out) == 0 {
			return 0, r.err
		}
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

func (r *rewriter) Close() error { return r.src.Close() }

// rewritableType reports whether a response's declared type is text this
// rewrite handles. The declared type is believed; nothing is sniffed, and a
// missing or unparsable type is not rewritten.
func rewritableType(h http.Header) bool {
	ct := h.Get("Content-Type")
	if ct == "" {
		return false
	}
	mt, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	if cs := strings.ToLower(params["charset"]); strings.HasPrefix(cs, "utf-16") || strings.HasPrefix(cs, "utf-32") {
		return false
	}
	switch mt {
	case "application/javascript", "application/ecmascript", "application/x-javascript", "application/json", "application/xml", "application/xhtml+xml":
		return true
	}
	if strings.HasPrefix(mt, "text/") {
		return true
	}
	return strings.HasPrefix(mt, "application/") && (strings.HasSuffix(mt, "+json") || strings.HasSuffix(mt, "+xml"))
}

// compressed reports a Content-Encoding other than identity.
func compressed(h http.Header) bool {
	ce := strings.ToLower(strings.TrimSpace(h.Get("Content-Encoding")))
	return ce != "" && ce != "identity"
}

// rewritableBody reports whether the response's body may be rewritten: a
// final response with a body of a rewritable type, not an upgrade, not a
// partial body and not compressed.
func rewritableBody(res *http.Response) bool {
	switch res.StatusCode {
	case http.StatusSwitchingProtocols, http.StatusNoContent, http.StatusResetContent, http.StatusPartialContent, http.StatusNotModified:
		return false
	}
	if res.Header.Get("Upgrade") != "" || compressed(res.Header) {
		return false
	}
	return rewritableType(res.Header)
}

// weakenETag turns a strong validator weak: the bytes the browser receives
// are no longer the bytes the validator names.
func weakenETag(h http.Header) {
	if v := h.Get("ETag"); v != "" && !strings.HasPrefix(v, "W/") {
		h.Set("ETag", "W/"+v)
	}
}

// rewriteResponse applies the rewrite to a response on its way to the
// browser. A rewritten body loses its Content-Length and Accept-Ranges and
// has its ETag weakened; a 304 of a rewritable type has its ETag weakened
// too, so the validators of the two agree. A HEAD has no body and only loses
// the length its GET would not have.
func rewriteResponse(res *http.Response, set *needleSet, logger *log.Logger) {
	if set.empty() || !rewritableType(res.Header) {
		return
	}
	if res.StatusCode == http.StatusNotModified {
		weakenETag(res.Header)
		return
	}
	if !rewritableBody(res) {
		if compressed(res.Header) && logger != nil {
			logger.Printf("rewrite: %s body left alone, Content-Encoding %s", res.Header.Get("Content-Type"), res.Header.Get("Content-Encoding"))
		}
		return
	}
	if res.Request == nil || res.Request.Method != http.MethodHead {
		res.Body = newRewriter(res.Body, set)
	}
	res.Header.Del("Content-Length")
	res.ContentLength = -1
	res.Header.Del("Accept-Ranges")
	weakenETag(res.Header)
}

// Script insertion. A stamped page gets the catch-up script (catchup.go) in
// its head, at the first place where it runs before any of the page's own
// scripts that could run before parsing ends, so before the framework's
// client opens its socket. Deferred scripts (modules, and classic scripts
// with src and defer) run only after parsing, so an inline script anywhere
// in the head runs first.
//
// The place is chosen so that the page reads as it did:
//
//   - right after the page's <meta charset> (or http-equiv Content-Type), if
//     no script that runs at once comes before it, so the declaration stays
//     where the browser looks for it, in the first 1024 bytes;
//   - otherwise before the first script that runs at once;
//   - otherwise before the first script;
//   - otherwise right after <head>.
//
// The inserter holds back the start of the body until it can tell: up to
// </head> or <body>, at most insertLookahead bytes, or the end of the body.
// What it held back is then sent at once and the rest streams through. A
// <meta> Content-Security-Policy there means nothing is inserted, whatever it
// says; so does a page with no head and no script in that window. The scan
// reads each tag to its end, honouring quoted attribute values, and skips
// comments and the contents of raw-text elements such as <title> and
// <style>, so text that looks like a tag is never taken for one, and of
// <template>, whose scripts never run.

// insertLookahead bounds what the inserter holds back.
const insertLookahead = 64 << 10

type inserter struct {
	src     io.ReadCloser
	tag     []byte
	scan    insertScan
	buf     []byte
	out     []byte
	primed  bool
	decided bool
	err     error
}

func newInserter(src io.ReadCloser, tag []byte) *inserter {
	return &inserter{src: src, tag: tag, scan: newInsertScan()}
}

// prime reads the start of the body until the place is known. It may be
// called before Read, to look at the head first; Read primes otherwise.
func (r *inserter) prime() {
	for !r.primed {
		atEnd := len(r.buf) >= insertLookahead
		if !atEnd {
			chunk := make([]byte, min(32<<10, insertLookahead-len(r.buf)))
			n, err := r.src.Read(chunk)
			r.buf = append(r.buf, chunk[:n]...)
			if err != nil {
				r.err = err
				atEnd = true
			}
		}
		r.primed = r.scan.position(r.buf, atEnd || len(r.buf) >= insertLookahead)
	}
}

// head is what prime held back.
func (r *inserter) head() []byte { return r.buf }

// at is where the script goes, or -1 for nowhere.
func (r *inserter) at() int { return r.scan.at }

func (r *inserter) Read(p []byte) (int, error) {
	if !r.decided {
		r.prime()
		r.decided = true
		r.out = r.buf
		if at := r.scan.at; at >= 0 && r.tag != nil {
			r.out = make([]byte, 0, len(r.buf)+len(r.tag))
			r.out = append(append(append(r.out, r.buf[:at]...), r.tag...), r.buf[at:]...)
		}
		r.buf = nil
	}
	if len(r.out) > 0 {
		n := copy(p, r.out)
		r.out = r.out[n:]
		return n, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	return r.src.Read(p)
}

func (r *inserter) Close() error { return r.src.Close() }

// insertScan finds the insertion point in a growing buffer. It resumes where
// it stopped, so a body arriving in small pieces is scanned once.
type insertScan struct {
	i       int    // the next byte to look at
	raw     string // inside this skipped element, until its end tag
	script  int    // the first script tag
	early   int    // the first script tag that runs at once
	charset int    // just after the first tag that declares the charset
	head    int    // just after the <head> tag
	at      int    // the decision: where the script goes, -1 for nowhere
	// declared is the charset the page declares in a <meta>, if any.
	declared string
}

func newInsertScan() insertScan {
	return insertScan{script: -1, early: -1, charset: -1, head: -1, at: -1}
}

// skipped are the elements whose contents the scan passes over to their end
// tag: those whose contents are text, and <template>, whose contents are
// never run, so a script placed before one inside it would not run either.
// A <template> nested in another ends the skip early, which can only place
// the script later, in the outer template's contents, or nowhere.
var skipped = map[string]bool{"script": true, "style": true, "title": true, "textarea": true, "xmp": true, "iframe": true, "noembed": true, "noframes": true, "noscript": true, "template": true}

// position scans b and reports whether the place is decided; s.at holds it
// then. atEnd says b will not grow.
func (s *insertScan) position(b []byte, atEnd bool) bool {
	for {
		if s.raw != "" {
			k := indexEndTag(b[s.i:], s.raw)
			if k < 0 {
				// Keep what could be the start of the end tag.
				s.i = max(s.i, len(b)-len(s.raw)-2)
				break
			}
			s.i += k
			s.raw = ""
		}
		j := bytes.IndexByte(b[s.i:], '<')
		if j < 0 {
			s.i = len(b)
			break
		}
		s.i += j
		rest := b[s.i:]
		if len(rest) < 4 && bytes.HasPrefix([]byte("<!--"), rest) {
			break // perhaps a comment: wait for more
		}
		if bytes.HasPrefix(rest, []byte("<!--")) {
			k := bytes.Index(rest[4:], []byte("-->"))
			if k < 0 {
				break
			}
			s.i += 4 + k + 3
			continue
		}
		name, closing, complete := tagName(rest)
		if !complete {
			break
		}
		if name == "" {
			s.i++
			continue
		}
		attrs, end, ok := tagAttributes(rest)
		if !ok {
			break
		}
		switch {
		case closing && name == "head", !closing && (name == "body" || name == "plaintext"):
			s.decide()
			return true
		case closing:
		case name == "head":
			if s.head < 0 {
				s.head = s.i + end
			}
		case name == "meta":
			if equivIs(attrs, "content-security-policy") {
				return true // s.at stays -1
			}
			if cs := metaCharset(attrs); cs != "" && s.charset < 0 {
				s.charset, s.declared = s.i+end, cs
			}
		case name == "script":
			if s.script < 0 {
				s.script = s.i
			}
			if s.early < 0 && runsAtOnce(attrs) {
				s.early = s.i
			}
		}
		if !closing && skipped[name] {
			s.raw = name
		}
		s.i += end
	}
	if !atEnd {
		return false
	}
	s.decide()
	return true
}

func (s *insertScan) decide() {
	switch {
	case s.charset >= 0 && (s.early < 0 || s.early > s.charset):
		s.at = s.charset
	case s.early >= 0:
		s.at = s.early
	case s.script >= 0:
		s.at = s.script
	default:
		s.at = s.head
	}
}

// indexEndTag finds the end tag </name, followed by a delimiter, in b.
func indexEndTag(b []byte, name string) int {
	for i := 0; ; {
		k := bytes.Index(b[i:], []byte("</"))
		if k < 0 {
			return -1
		}
		i += k
		after := i + 2 + len(name)
		if after >= len(b) {
			return -1
		}
		if bytes.EqualFold(b[i+2:after], []byte(name)) && isTagDelimiter(b[after]) {
			return i
		}
		i += 2
	}
}

func isTagDelimiter(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r' || c == '/' || c == '>'
}

// tagName reads the name of the tag at the start of b, which begins with
// '<': lower-cased, after a '/' for a closing tag. complete is false when b
// ends before the name does. A '<' that begins no tag has the empty name.
func tagName(b []byte) (name string, closing bool, complete bool) {
	k := 1
	if k < len(b) && b[k] == '/' {
		closing = true
		k++
	}
	start := k
	for k < len(b) && (b[k] >= 'a' && b[k] <= 'z' || b[k] >= 'A' && b[k] <= 'Z' || k > start && isDigit(b[k])) {
		k++
	}
	if k == len(b) {
		return "", closing, false
	}
	if k > start && !isTagDelimiter(b[k]) {
		return "", closing, true // not a tag the scan needs
	}
	return strings.ToLower(string(b[start:k])), closing, true
}

// tagAttributes reads a tag's attributes, as the HTML tokenizer does, up to
// the '>' that ends it: names lower-cased, values unquoted, a value-less
// attribute present with the empty value. end is the length of the tag; ok
// is false when b ends first.
func tagAttributes(b []byte) (attrs map[string]string, end int, ok bool) {
	attrs = map[string]string{}
	k := 1
	for k < len(b) && b[k] != '>' && !isTagDelimiter(b[k]) || k == 1 && k < len(b) && b[k] == '/' {
		k++
	}
	for {
		for k < len(b) && (isTagDelimiter(b[k]) && b[k] != '>') {
			k++
		}
		if k >= len(b) {
			return nil, 0, false
		}
		if b[k] == '>' {
			return attrs, k + 1, true
		}
		start := k
		for k < len(b) && b[k] != '=' && b[k] != '>' && !isTagDelimiter(b[k]) || k == start && k < len(b) && b[k] == '=' {
			k++
		}
		name := strings.ToLower(string(b[start:k]))
		for k < len(b) && isTagDelimiter(b[k]) && b[k] != '/' && b[k] != '>' {
			k++
		}
		if k >= len(b) {
			return nil, 0, false
		}
		value := ""
		if b[k] == '=' {
			k++
			for k < len(b) && isTagDelimiter(b[k]) && b[k] != '/' && b[k] != '>' {
				k++
			}
			if k >= len(b) {
				return nil, 0, false
			}
			if q := b[k]; q == '"' || q == '\'' {
				e := bytes.IndexByte(b[k+1:], q)
				if e < 0 {
					return nil, 0, false
				}
				value = string(b[k+1 : k+1+e])
				k += e + 2
			} else {
				v := k
				for k < len(b) && b[k] != '>' && (!isTagDelimiter(b[k]) || b[k] == '/') {
					k++
				}
				value = string(b[v:k])
			}
		}
		if _, seen := attrs[name]; !seen && name != "" {
			attrs[name] = value
		}
	}
}

func equivIs(attrs map[string]string, value string) bool {
	v, ok := attrs["http-equiv"]
	return ok && strings.EqualFold(strings.TrimSpace(v), value)
}

// metaCharset is the charset a <meta> declares, by its charset attribute or
// as an http-equiv Content-Type, or "".
func metaCharset(attrs map[string]string) string {
	if cs, ok := attrs["charset"]; ok {
		return strings.TrimSpace(cs)
	}
	if equivIs(attrs, "content-type") {
		if _, params, err := mime.ParseMediaType(attrs["content"]); err == nil {
			return params["charset"]
		}
	}
	return ""
}

// runsAtOnce reports a script element that may run before parsing reaches
// the next element: anything but a module without async, a classic script
// with src and defer without async, or a data block of another type.
func runsAtOnce(attrs map[string]string) bool {
	_, async := attrs["async"]
	_, src := attrs["src"]
	_, deferred := attrs["defer"]
	switch t := strings.ToLower(strings.TrimSpace(attrs["type"])); {
	case t == "module":
		return async
	case t == "" || t == "text/javascript" || t == "application/javascript" || t == "text/ecmascript" || t == "application/ecmascript" || t == "text/jscript" || t == "text/livescript" || strings.HasPrefix(t, "text/javascript"):
		return async || !src || !deferred
	default:
		return false // importmap, JSON and other data blocks run no code
	}
}
