package scenario

// This is a development-only reference model of recipient access, not an
// HTTP gateway or a second production policy engine. It shares Platform's
// lock, share records and revocation authority with the CLI/daemon fixtures.
import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Paths and lifetimes of the model, with synthetic credentials for deterministic scenarios.
const (
	accessPath    = "/"
	handoffPath   = "/_purlview/redeem"
	emailPath     = "/open"
	cookiePrefix  = "__Host-purlview-"
	sessionCookie = cookiePrefix + "session"
	magicLifetime = 5 * time.Minute
)

type requestKind string

const (
	pageRequest      requestKind = "page"
	assetRequest     requestKind = "asset"
	apiRequest       requestKind = "api"
	websocketRequest requestKind = "websocket"
	sseRequest       requestKind = "sse"
)

type recipientRequest struct {
	URL     string
	Kind    requestKind
	Cookies []http.Cookie
}

type upstreamRequest struct {
	URL     string
	Kind    requestKind
	Cookies []http.Cookie
}

type accessResult struct {
	Handoff  *accessResult
	Status   int
	Reason   string
	Headers  http.Header
	Cookie   *http.Cookie
	Location string
	Body     string
	Upstream *upstreamRequest
	Stream   string
}

type verificationRequest struct {
	EntryURL string
	Email    string
}

type recipientEmail struct {
	To        string
	URL       string
	ExpiresAt time.Time
}

type recipientGrant struct {
	id, email string
	active    bool
}

type recipientSession struct {
	share, grant string
	expiresAt    time.Time
}

type emailRedemption struct {
	share, grant string
	expiresAt    time.Time
	used         bool
}

type recipientStream struct {
	share, grant string
	kind         requestKind
	closed       string
}

type recipientState struct {
	handoffs                           map[string]*emailRedemption
	sessions                           map[string]recipientSession
	emailTokens                        map[string]*emailRedemption
	streams                            map[string]*recipientStream
	outbox                             []recipientEmail
	nextSession, nextEmail, nextStream int
	upstreamCalls                      int
	diagnostics                        []string
}

func (a *recipientState) init() {
	a.handoffs = map[string]*emailRedemption{}
	a.sessions = map[string]recipientSession{}
	a.emailTokens = map[string]*emailRedemption{}
	a.streams = map[string]*recipientStream{}
}

// World advances call this synchronously: expiry closes idle streams even
// when no recipient request or daemon connection can trigger enforcement.
func (p *Platform) expireShares() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireSharesLocked()
}

func (p *Platform) expireSharesLocked() {
	for _, s := range p.shares {
		if !p.clock.Now().Before(s.expiresAt) {
			p.endShare(s, "expired")
		}
	}
}

func (p *Platform) closeStreams(id, grant, reason string) {
	for _, st := range p.access.streams {
		if st.share == id && (grant == "" || st.grant == grant) && st.closed == "" {
			st.closed = reason
		}
	}
}

// grant returns the share's standing grant with this id, or for this address
// when id is empty.
func (s *platformShare) grant(id, email string) *recipientGrant {
	for i := range s.grants {
		if g := &s.grants[i]; g.active && (id != "" && g.id == id || id == "" && g.email == email) {
			return g
		}
	}
	return nil
}

// revokeRecipientGrant ends one address's grant; the share's others stand.
func (p *Platform) revokeRecipientGrant(id, email string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.shares[id]; s != nil {
		if g := s.grant("", email); g != nil {
			g.active = false
			p.closeStreams(id, g.id, "grant_revoked")
		}
	}
}

func (p *Platform) recipientShare(raw string) (*platformShare, *url.URL) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Fragment != "" {
		return nil, nil
	}
	origin := "https://" + strings.ToLower(u.Host)
	for _, s := range p.shares {
		if s.origin == origin || s.entryOrigin == origin {
			return s, u
		}
	}
	return nil, u
}

func accessResponse(status int, reason string) accessResult {
	return accessResult{Status: status, Reason: reason, Headers: http.Header{
		"Cache-Control": {"no-store"}, "Referrer-Policy": {"no-referrer"},
	}}
}

// requestVerification is deliberately non-enumerating at the browser
// boundary. Only an active permitted grant causes mail to be enqueued.
func (p *Platform) requestVerification(req verificationRequest) accessResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireSharesLocked()
	s, u := p.recipientShare(req.EntryURL)
	if s == nil || (u.Path != accessPath && u.Path != "") || s.ended || len(s.recipients) == 0 {
		return accessResponse(403, "verification_unavailable")
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if g := s.grant("", email); g != nil {
		p.sendEmailLink(s, g, magicLifetime)
	}
	return accessResponse(202, "verification_requested")
}

// sendEmailLink mints a single-use link for one grant and puts it in the outbox.
func (p *Platform) sendEmailLink(s *platformShare, g *recipientGrant, lifetime time.Duration) {
	p.access.nextEmail++
	token := fmt.Sprintf("synthetic-email-%02d", p.access.nextEmail)
	expires := p.clock.Now().Add(lifetime)
	if expires.After(s.expiresAt) {
		expires = s.expiresAt
	}
	p.access.emailTokens[token] = &emailRedemption{share: s.id, grant: g.id, expiresAt: expires}
	p.access.outbox = append(p.access.outbox, recipientEmail{To: g.email, URL: s.entryOrigin + emailPath + "?token=" + token, ExpiresAt: expires})
}

// recipientAccess computes every decision from credentials and current
// authority. The mutex also makes email redemption atomic across browsers.
func (p *Platform) recipientAccess(req recipientRequest) (res accessResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expireSharesLocked()
	s, u := p.recipientShare(req.URL)
	id := "unknown"
	if s != nil {
		id = s.id
	}
	defer func() {
		// Routine diagnostics contain no raw URLs, credentials or cookies.
		p.access.diagnostics = append(p.access.diagnostics, fmt.Sprintf("share=%s kind=%s status=%d reason=%s", id, req.Kind, res.Status, res.Reason))
	}()
	if s == nil {
		return accessResponse(403, "unknown_share")
	}
	if s.ended {
		return accessResponse(403, s.endReason)
	}
	if "https://"+u.Host == s.entryOrigin {
		if u.Path == "" { // a browser requests "/" for a bare origin
			u.Path = accessPath
		}
		if u.Path == accessPath || u.Path == emailPath {
			return p.redeem(s, u)
		}
		return accessResponse(404, "entry_only")
	}
	if u.Path == handoffPath {
		return p.consumeHandoff(s, u)
	}
	// Encoded/reserved paths cannot escape into the target application.
	if strings.HasPrefix(u.Path, "/_purlview/") {
		return accessResponse(404, "reserved_route")
	}
	var session recipientSession
	var valid bool
	count := 0
	for _, cookie := range req.Cookies {
		if cookie.Name == sessionCookie {
			count++
			if count > 1 { // fail closed on ambiguous session cookies
				return accessResponse(401, "ambiguous_session")
			}
			session, valid = p.access.sessions[cookie.Value]
		}
	}
	if !valid || session.share != s.id || !p.clock.Now().Before(session.expiresAt) {
		return accessResponse(401, "session_required")
	}
	if len(s.recipients) != 0 && s.grant(session.grant, "") == nil {
		return accessResponse(403, "grant_invalid")
	}
	if len(s.conns) == 0 || p.holdReady {
		return accessResponse(503, "target_unavailable")
	}
	target, _ := url.Parse(s.target) // validated by the real CLI
	up := *u
	up.Scheme, up.Host = target.Scheme, target.Host
	var cookies []http.Cookie
	for _, cookie := range req.Cookies {
		if !strings.HasPrefix(cookie.Name, cookiePrefix) {
			cookies = append(cookies, cookie)
		}
	}
	res = accessResult{Status: 200, Reason: "forwarded", Body: "application response", Upstream: &upstreamRequest{URL: up.String(), Kind: req.Kind, Cookies: cookies}}
	p.access.upstreamCalls++
	if req.Kind == websocketRequest || req.Kind == sseRequest {
		p.access.nextStream++
		res.Stream = fmt.Sprintf("stream_%02d", p.access.nextStream)
		p.access.streams[res.Stream] = &recipientStream{share: s.id, grant: session.grant, kind: req.Kind}
		if req.Kind == websocketRequest {
			res.Status = 101
		}
	}
	return res
}

func (p *Platform) redeem(s *platformShare, u *url.URL) accessResult {
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(query["token"]) > 1 || len(query["return_to"]) > 1 {
		return accessResponse(400, "invalid_access_request")
	}
	location, ok := redirectDestination(s, query.Get("return_to"))
	if !ok {
		return accessResponse(400, "unsafe_redirect")
	}
	token := query.Get("token")
	grant := ""
	if u.Path == accessPath {
		if len(s.recipients) != 0 {
			if token != "" {
				return accessResponse(403, "verification_required")
			}
			return accessResponse(200, "verification_required")
		}
		if token == "" || token != s.secret {
			return accessResponse(401, "invalid_secret")
		}
	} else {
		redemption := p.access.emailTokens[token]
		if len(s.recipients) == 0 || redemption == nil || redemption.share != s.id || s.grant(redemption.grant, "") == nil || redemption.used || !p.clock.Now().Before(redemption.expiresAt) {
			return accessResponse(403, "invalid_email_link")
		}
		redemption.used = true
		grant = redemption.grant
	}
	p.access.nextSession++
	code := fmt.Sprintf("synthetic-handoff-%02d", p.access.nextSession)
	p.access.handoffs[code] = &emailRedemption{share: s.id, grant: grant, expiresAt: p.clock.Now().Add(30 * time.Second)}
	res := accessResponse(303, "handoff_issued")
	res.Location = s.origin + handoffPath + "?code=" + code
	_ = location // Destination is recomputed from immutable share state at consumption.
	return res
}

func (p *Platform) consumeHandoff(s *platformShare, u *url.URL) accessResult {
	code := u.Query().Get("code")
	handoff := p.access.handoffs[code]
	if handoff == nil || handoff.used || handoff.share != s.id || !p.clock.Now().Before(handoff.expiresAt) || (len(s.recipients) != 0 && s.grant(handoff.grant, "") == nil) {
		return accessResponse(403, "invalid_handoff")
	}
	handoff.used = true
	location, ok := redirectDestination(s, "")
	if !ok {
		return accessResponse(403, "unsafe_redirect")
	}
	grant := handoff.grant
	p.access.nextSession++
	value := fmt.Sprintf("synthetic-session-%02d", p.access.nextSession)
	p.access.sessions[value] = recipientSession{share: s.id, grant: grant, expiresAt: s.expiresAt}
	res := accessResponse(303, "session_established")
	res.Location = location
	res.Cookie = &http.Cookie{Name: sessionCookie, Value: value, Path: "/", Secure: true, HttpOnly: true, Expires: s.expiresAt, SameSite: http.SameSiteLaxMode}
	return res
}

// The target path/query is the entry page, and later
// requests expose the target origin. A same-origin destination is required;
// this model never follows a network redirect supplied by an application.
func redirectDestination(s *platformShare, returnTo string) (string, bool) {
	u, err := url.Parse(s.target)
	if err != nil {
		return "", false
	}
	origin, _ := url.Parse(s.origin)
	u.Scheme, u.Host = origin.Scheme, origin.Host
	if u.Path == "" {
		u.Path = "/"
	}
	entry := u.String()
	if returnTo != "" {
		candidate, err := url.Parse(returnTo)
		if err != nil || candidate.User != nil || candidate.Fragment != "" || strings.ContainsAny(returnTo, "\\\r\n") || strings.HasPrefix(returnTo, "//") {
			return "", false
		}
		if !candidate.IsAbs() && !strings.HasPrefix(returnTo, "/") {
			return "", false
		}
		u = origin.ResolveReference(candidate)
	}
	if u.Scheme != origin.Scheme || u.Host != origin.Host || u.User != nil || strings.HasPrefix(u.Path, "/_purlview/") || strings.HasPrefix(u.Path, "//") || strings.ContainsAny(u.Path, "\\\r\n") {
		return "", false
	}
	// The recorded entry is the only redirect destination in this fixture.
	// A caller cannot smuggle its access token into an application query by
	// supplying an arbitrary same-origin return_to. Later session navigation
	// still reaches other paths on the exposed origin.
	if u.String() != entry {
		return "", false
	}
	return entry, true
}

func (p *Platform) mail() []recipientEmail {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recipientEmail(nil), p.access.outbox...)
}

func (p *Platform) forwardedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.access.upstreamCalls
}

func (p *Platform) streamState(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st := p.access.streams[id]; st != nil {
		return st.closed
	}
	return "unknown_stream"
}

func (p *Platform) recipientDiagnostics() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return strings.Join(p.access.diagnostics, "\n")
}

// A deliberately small cookie jar: separate per browser, host-only, secure,
// expiry checked. This does not simulate SameSite or sibling isolation.
type recipientBrowser struct {
	name    string
	cookies map[string][]http.Cookie
}

func newRecipientBrowser(name string) *recipientBrowser {
	return &recipientBrowser{name: name, cookies: map[string][]http.Cookie{}}
}

func (b *recipientBrowser) request(p *Platform, raw string, kind requestKind) accessResult {
	u, err := url.Parse(raw)
	var cookies []http.Cookie
	if err == nil && u.Scheme == "https" {
		for _, cookie := range b.cookies[strings.ToLower(u.Host)] {
			if cookie.Expires.IsZero() || p.clock.Now().Before(cookie.Expires) {
				cookies = append(cookies, cookie)
			}
		}
	}
	res := p.recipientAccess(recipientRequest{URL: raw, Kind: kind, Cookies: cookies})
	if res.Reason == "handoff_issued" {
		next := b.request(p, res.Location, kind)
		next.Handoff = &res
		return next
	}
	if res.Cookie != nil && err == nil {
		// Replacement models repeated default-link redemption in one browser.
		kept := b.cookies[strings.ToLower(u.Host)][:0]
		for _, c := range b.cookies[strings.ToLower(u.Host)] {
			if c.Name != res.Cookie.Name {
				kept = append(kept, c)
			}
		}
		b.cookies[strings.ToLower(u.Host)] = append(kept, *res.Cookie)
	}
	return res
}

func (w *World) visit(b *recipientBrowser, raw string, kind requestKind) accessResult {
	w.Note("recipient %s: %s %s (synthetic fixture input)", b.name, kind, raw)
	res := b.request(w.Platform, raw, kind)
	w.recordAccess(res)
	return res
}

func (w *World) recordAccess(res accessResult) {
	if res.Handoff != nil {
		w.recordAccess(*res.Handoff)
	}
	w.Note("response %d %s; application forwarded=%t", res.Status, res.Reason, res.Upstream != nil)
	if res.Headers != nil {
		w.Note("Cache-Control: %s; Referrer-Policy: %s", res.Headers.Get("Cache-Control"), res.Headers.Get("Referrer-Policy"))
	}
	if res.Cookie != nil {
		w.Note("Set-Cookie (synthetic): %s; host-only", res.Cookie.String())
	}
	if res.Location != "" {
		w.Note("Location: %s", res.Location)
	}
	if res.Upstream != nil {
		w.Note("upstream: %s %s; application cookies=%v; stream=%s", res.Upstream.Kind, res.Upstream.URL, res.Upstream.Cookies, res.Stream)
	}
}
