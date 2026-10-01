package scenario

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/idyl-labs/purlview/internal/share"
	"github.com/idyl-labs/purlview/internal/share/engine"

	"github.com/idyl-labs/purlview/sdk/api"
)

const permittedEmail = "reviewer@example.invalid"

func accessScenario(name, title string, run func(*Check, *World)) Scenario {
	return Scenario{Name: "access/" + name, Title: title, Setup: "signed-in creator; real CLI and share engine; synthetic recipient tokens, browsers, email outbox and streams; separate entry/content hosts, single-use handoffs and SameSite=Lax.", Run: func(c *Check, w *World) {
		w.SignIn()
		run(c, w)
	}}
}

func createRecipientShare(c *Check, w *World, restricted bool, target string) share.Share {
	args := []string{"share", target, "--background"}
	if restricted {
		args = append(args, "--to", permittedEmail)
	}
	r := w.Exec(args...)
	c.Exit(r, 0)
	cred, _ := w.Store.Load()
	shares, err := w.Platform.listShares(context.Background(), *cred)
	c.True(err == nil && len(shares) > 0, "created share must be rediscoverable: %v", err)
	if len(shares) == 0 {
		panic("share creation failed")
	}
	sh := shares[len(shares)-1]
	c.StdoutExactly(r, sh.URL+"\n")
	c.True(sh.Origin != sh.URL && sh.ID != "", "share identity must be distinct from distributable link")
	if restricted {
		c.True(!strings.Contains(sh.URL, "token="), "restricted share must have no generic bearer link")
	} else {
		c.Stderr(r, "Access   Anyone with this link")
	}
	return sh
}

// entryOrigin is the entry host of a share's distributable link.
func entryOrigin(sh share.Share) string {
	u, _ := url.Parse(sh.URL)
	return "https://" + u.Host
}

func requestEmail(c *Check, w *World, sh share.Share, email string) recipientEmail {
	before := len(w.Platform.mail())
	w.Note("recipient requests verification for %s on %s", email, sh.Origin)
	res := w.Platform.requestVerification(verificationRequest{EntryURL: sh.URL, Email: email})
	w.recordAccess(res)
	c.True(res.Status == 202 && res.Upstream == nil && res.Cookie == nil, "verification request is not access: %+v", res)
	mail := w.Platform.mail()
	c.True(len(mail) == before+1, "permitted request must enqueue exactly one email")
	if len(mail) <= before {
		panic("missing verification email")
	}
	msg := mail[len(mail)-1]
	w.Note("email outbox to %s (synthetic): %s; expires %s", msg.To, msg.URL, msg.ExpiresAt.Format(time.RFC3339))
	return msg
}

func checkSession(c *Check, sh share.Share, res accessResult) {
	c.True(res.Status == 303 && res.Reason == "session_established" && res.Upstream == nil && res.Body == "", "redemption must establish a session before forwarding: %+v", res)
	c.True(res.Headers.Get("Cache-Control") == "no-store" && res.Headers.Get("Referrer-Policy") == "no-referrer", "access responses must suppress caching and referrers")
	if res.Cookie == nil {
		c.Failf("redemption returned no cookie")
		return
	}
	c.True(res.Cookie.Secure && res.Cookie.HttpOnly && res.Cookie.Domain == "" && res.Cookie.Path == "/", "session cookie must be Secure, HttpOnly and host-only: %+v", res.Cookie)
	c.True(res.Cookie.Expires.Equal(sh.ExpiresAt), "session cannot extend the original share lifetime")
	c.True(strings.HasPrefix(res.Location, sh.Origin+"/") && !strings.Contains(res.Location, "synthetic-"), "redirect must stay on the share without Purlview credentials: %s", res.Location)
}

func denyVisit(c *Check, w *World, b *recipientBrowser, raw string, kind requestKind) accessResult {
	before := w.Platform.forwardedCount()
	res := w.visit(b, raw, kind)
	c.True(res.Status >= 400 && res.Upstream == nil && res.Body == "" && res.Cookie == nil, "unauthorised request must return no application content or session: %+v", res)
	c.True(w.Platform.forwardedCount() == before, "unauthorised request called the target")
	return res
}

func allowVisit(c *Check, w *World, b *recipientBrowser, raw string, kind requestKind) accessResult {
	before := w.Platform.forwardedCount()
	res := w.visit(b, raw, kind)
	c.True(res.Upstream != nil && res.Body == "application response" && (res.Status == 200 || res.Status == 101), "authorised request must reach the application: %+v", res)
	c.True(w.Platform.forwardedCount() == before+1, "authorised request must forward exactly once")
	return res
}

func authorisedBrowser(c *Check, w *World, sh share.Share, name string) (*recipientBrowser, string) {
	link := sh.URL
	if len(sh.Recipients) != 0 {
		link = requestEmail(c, w, sh, permittedEmail).URL
	}
	b := newRecipientBrowser(name)
	res := w.visit(b, link, pageRequest)
	checkSession(c, sh, res)
	allowVisit(c, w, b, res.Location, pageRequest)
	return b, link
}

func init() {
	register(
		accessScenario("default-link", "The CLI secret link establishes a session and redirects into the application in one opening.", func(c *Check, w *World) {
			sh := createRecipientShare(c, w, false, target)
			authorisedBrowser(c, w, sh, "alice")
		}),
		accessScenario("missing-authority", "A hostname, clean URL or missing/invalid token never calls the target.", func(c *Check, w *World) {
			sh := createRecipientShare(c, w, false, target)
			b := newRecipientBrowser("fresh")
			entry := entryOrigin(sh)
			// The former prefixed entry paths are not-found too: nothing redirects from them.
			for _, raw := range []string{sh.Origin, sh.Origin + "/", entry + "/", entry + "/?token=synthetic-invalid", sh.URL + "&token=synthetic-duplicate", sh.Origin + "/?token=synthetic-secret-01", entry + "/_purlview/access?token=synthetic-secret-01", entry + "/access?token=synthetic-secret-01", sh.Origin + "/_purlview/access?token=synthetic-secret-01"} {
				denyVisit(c, w, b, raw, pageRequest)
			}
		}),
		accessScenario("reuse-and-reconnect", "Two browsers reuse the same link; create retries and reconnect keep its secret and original expiry.", func(c *Check, w *World) {
			inv := w.Run("share", target)
			c.Seen(inv.WaitFor(readyLine), "ready")
			cred, _ := w.Store.Load()
			shares, err := w.Platform.listShares(context.Background(), *cred)
			c.True(err == nil && len(shares) == 1, "one authoritative share")
			sh := shares[0]
			authorisedBrowser(c, w, sh, "alice")
			authorisedBrowser(c, w, sh, "bob")
			w.Note("daemon retries create with the same attempt (synthetic input)")
			rec, err := w.Platform.Engine().CreateShare(context.Background(), *cred, api.CreateShareRequest{Key: sh.Attempt, Target: sh.Target, TTL: time.Hour})
			c.True(err == nil && rec.URL == sh.URL && rec.Share.ExpiresAt.Equal(sh.ExpiresAt), "idempotent create must preserve link and expiry")
			w.Note("owning daemon loses its serving connection")
			w.Platform.DropConnection(sh.ID)
			c.Seen(inv.WaitFor("Connection lost"), "connection loss")
			c.Seen(w.AdvanceUntil(inv, "Reconnected", time.Second, 10*time.Second), "reconnected")
			shares, err = w.Platform.listShares(context.Background(), *cred)
			c.True(err == nil && len(shares) == 1 && shares[0].URL == sh.URL && shares[0].ExpiresAt.Equal(sh.ExpiresAt), "reconnect must preserve link and expiry")
			authorisedBrowser(c, w, sh, "carol-after-reconnect")
			inv.Interrupt()
			c.Exit(inv.Wait(), 0)
		}),
		accessScenario("session-browsing-and-scope", "Session-backed pages, assets, APIs, WebSockets and SSE work only for the authorised share.", func(c *Check, w *World) {
			a := createRecipientShare(c, w, false, target)
			b := createRecipientShare(c, w, false, "localhost:4000")
			browser, _ := authorisedBrowser(c, w, a, "authorised")
			fresh := newRecipientBrowser("fresh")
			for _, item := range []struct {
				kind requestKind
				path string
			}{{pageRequest, "/dashboard"}, {assetRequest, "/assets/app.js"}, {apiRequest, "/api/items?token=app-token"}, {websocketRequest, "/socket"}, {sseRequest, "/events"}} {
				allowVisit(c, w, browser, a.Origin+item.path, item.kind)
				denyVisit(c, w, fresh, a.Origin+item.path, item.kind)
			}
			denyVisit(c, w, browser, b.Origin+"/", pageRequest)
			w.Note("explicitly replay share A's session cookie to B; server must reject even a malicious cookie jar")
			ua, _ := url.Parse(a.Origin)
			ub, _ := url.Parse(b.Origin)
			browser.cookies[ub.Host] = append([]http.Cookie(nil), browser.cookies[ua.Host]...)
			denyVisit(c, w, browser, b.Origin+"/", pageRequest)
			u, _ := url.Parse(a.URL)
			denyVisit(c, w, fresh, entryOrigin(b)+accessPath+"?token="+u.Query().Get("token"), pageRequest)
		}),
		accessScenario("restricted-entry-and-email", "--to sends no invitation; permitted verification sends a single-use link that directly opens the clicking browser.", func(c *Check, w *World) {
			sh := createRecipientShare(c, w, true, target)
			c.True(len(w.Platform.mail()) == 0, "creation must not send invitation mail")
			w.Note("email outbox after create: %d messages", len(w.Platform.mail()))
			requester := newRecipientBrowser("verification-requester")
			res := w.visit(requester, sh.URL, pageRequest)
			c.True(res.Reason == "verification_required" && res.Cookie == nil && res.Upstream == nil, "restricted URL must show verification entry only")
			w.Note("unpermitted stranger@example.invalid requests verification")
			res = w.Platform.requestVerification(verificationRequest{EntryURL: sh.URL, Email: "stranger@example.invalid"})
			w.recordAccess(res)
			c.True(res.Status == 202 && len(w.Platform.mail()) == 0 && res.Cookie == nil, "unpermitted request must not issue mail or access")
			msg := requestEmail(c, w, sh, permittedEmail)
			clicked := newRecipientBrowser("email-click-on-another-device")
			res = w.visit(clicked, msg.URL, pageRequest)
			checkSession(c, sh, res)
			allowVisit(c, w, clicked, res.Location, pageRequest)
			denyVisit(c, w, requester, res.Location, pageRequest)
			denyVisit(c, w, newRecipientBrowser("replay"), msg.URL, pageRequest)
		}),
		accessScenario("email-scope-and-expiry", "Email redemption checks share binding and the five-minute expiry without consuming another share's token.", func(c *Check, w *World) {
			a := createRecipientShare(c, w, true, target)
			b := createRecipientShare(c, w, true, "localhost:4000")
			msg := requestEmail(c, w, a, permittedEmail)
			u, _ := url.Parse(msg.URL)
			denyVisit(c, w, newRecipientBrowser("wrong-share"), entryOrigin(b)+emailPath+"?"+u.RawQuery, pageRequest)
			res := w.visit(newRecipientBrowser("correct-share"), msg.URL, pageRequest)
			checkSession(c, a, res)
			msg = requestEmail(c, w, a, permittedEmail)
			w.Advance(magicLifetime)
			denyVisit(c, w, newRecipientBrowser("expired-email"), msg.URL, pageRequest)
			// A fresh request is the recovery path; scanner detection is not modelled.
			msg = requestEmail(c, w, a, permittedEmail)
			checkSession(c, a, w.visit(newRecipientBrowser("fresh-email"), msg.URL, pageRequest))
		}),
		accessScenario("email-concurrent-redemption", "Concurrent clicks consume an email token once and establish exactly one browser session.", func(c *Check, w *World) {
			sh := createRecipientShare(c, w, true, target)
			msg := requestEmail(c, w, sh, permittedEmail)
			start := make(chan struct{})
			var wg sync.WaitGroup
			results := make([]accessResult, 2)
			for i := range results {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					results[i] = newRecipientBrowser("concurrent").request(w.Platform, msg.URL, pageRequest)
				}()
			}
			w.Note("two independent browsers click the same synthetic email URL concurrently; winner identity is unspecified")
			close(start)
			wg.Wait()
			sort.Slice(results, func(i, j int) bool { return results[i].Status < results[j].Status })
			for _, res := range results {
				w.recordAccess(res)
			}
			checkSession(c, sh, results[0])
			c.True(results[1].Status == 403 && results[1].Cookie == nil && results[1].Upstream == nil, "exactly one click must win")
			c.True(w.Platform.forwardedCount() == 0, "redemption never itself forwards")
			denyVisit(c, w, newRecipientBrowser("later-replay"), msg.URL, pageRequest)
		}),
		accessScenario("credential-purpose", "Recipient secrets, email links and sessions cannot manage shares or admit daemons; restricted shares reject general bearer access.", credentialPurpose),
		accessScenario("credential-handling-and-entry", "Purlview consumes its credentials while preserving application cookies and the stored entry path/query.", credentialHandling),
		accessScenario("unsafe-redirect", "Access endpoints reject destinations outside the share and reserved access routes before issuing a session.", unsafeRedirect),
		accessScenario("management-and-redaction", "Authenticated creators rediscover links and revoke by label, id, secret link, restricted link or clean URL; failures and logs omit secrets.", recipientManagement),
		accessScenario("grant-revocation", "Restricted sessions and outstanding email tokens retain the recipient grant binding.", func(c *Check, w *World) {
			sh := createRecipientShare(c, w, true, target)
			b, _ := authorisedBrowser(c, w, sh, "permitted")
			msg := requestEmail(c, w, sh, permittedEmail)
			stream := allowVisit(c, w, b, sh.Origin+"/events", sseRequest).Stream
			w.Note("control plane revokes this recipient grant")
			w.Platform.revokeRecipientGrant(sh.ID, permittedEmail)
			c.True(w.Platform.streamState(stream) == "grant_revoked", "grant revocation must close bound streams")
			w.Note("%s closed: %s", stream, w.Platform.streamState(stream))
			denyVisit(c, w, b, sh.Origin+"/", pageRequest)
			denyVisit(c, w, newRecipientBrowser("pending-email"), msg.URL, pageRequest)
		}),
	)
	register(accessScenario("several-recipients", "Every listed address verifies separately and holds its own grant; an unlisted address gets nothing.", func(c *Check, w *World) {
		r := w.Exec("share", target, "--background", "--to", permittedEmail, "--to", "second@example.invalid")
		c.Exit(r, 0)
		cred, _ := w.Store.Load()
		shares, _ := w.Platform.listShares(context.Background(), *cred)
		sh := shares[len(shares)-1]
		c.True(len(sh.Recipients) == 2 && !strings.Contains(sh.URL, "token="), "one restricted share names both: %+v", sh)
		first := newRecipientBrowser("first")
		res := w.visit(first, requestEmail(c, w, sh, permittedEmail).URL, pageRequest)
		checkSession(c, sh, res)
		second := newRecipientBrowser("second")
		res = w.visit(second, requestEmail(c, w, sh, "Second@example.invalid").URL, pageRequest)
		checkSession(c, sh, res)
		before := len(w.Platform.mail())
		w.Note("an unlisted address asks for a link")
		w.recordAccess(w.Platform.requestVerification(verificationRequest{EntryURL: sh.URL, Email: "third@example.invalid"}))
		c.True(len(w.Platform.mail()) == before, "an unlisted address must get no email")
		w.Note("control plane revokes the first recipient's grant")
		w.Platform.revokeRecipientGrant(sh.ID, permittedEmail)
		denyVisit(c, w, first, sh.Origin+"/", pageRequest)
		allowVisit(c, w, second, sh.Origin+"/", pageRequest)
	}))
	for _, mode := range []struct {
		name       string
		restricted bool
	}{{"secret", false}, {"restricted", true}} {
		for _, end := range []string{"expiry", "revocation", "revocation-disconnected"} {
			register(accessScenario(end+"-"+mode.name, "Ending a "+mode.name+" share invalidates links and sessions and closes streams independently of daemon connectivity.", func(c *Check, w *World) { recipientEnd(c, w, mode.restricted, end) }))
		}
	}
}

func credentialPurpose(c *Check, w *World) {
	creator, _ := w.Store.Load()
	a := createRecipientShare(c, w, false, target)
	b := createRecipientShare(c, w, true, "localhost:4000")
	browser := newRecipientBrowser("recipient")
	session := w.visit(browser, a.URL, pageRequest)
	checkSession(c, a, session)
	mail := requestEmail(c, w, b, permittedEmail)
	link, _ := url.Parse(a.URL)
	email, _ := url.Parse(mail.URL)
	denyVisit(c, w, newRecipientBrowser("restricted-bypass"), entryOrigin(b)+accessPath+"?token="+link.Query().Get("token"), pageRequest)
	for _, token := range []string{link.Query().Get("token"), email.Query().Get("token"), session.Cookie.Value} {
		w.Note("present %s as a creator/device credential (synthetic fixture input)", token)
		wrong := *creator
		wrong.Token = token
		c.True(w.Store.Save(&wrong) == nil, "save fixture credential")
		r := w.Exec("list")
		c.Exit(r, 1)
		c.NoStdout(r)
		c.NotStderr(r, token)
		r = w.Exec("stop", a.URL)
		c.Exit(r, 1)
		c.NoStdout(r)
		c.NotStderr(r, token)
		conn, err := w.Platform.Engine().ConnectServing(context.Background(), wrong, a.ID, engine.Serving{})
		c.True(conn == nil && engine.AsRejected(err) != nil && engine.AsRejected(err).Reason == engine.RejectAuthority, "recipient credential must not admit daemon: %v", err)
		w.Note("daemon admission rejected: authority_invalid")
	}
	c.True(w.Store.Save(creator) == nil, "restore creator")
	// Creator credentials cannot be used as recipient credentials either.
	denyVisit(c, w, newRecipientBrowser("creator-as-recipient"), entryOrigin(a)+accessPath+"?token="+creator.Token, pageRequest)
	_, ended, _, _ := w.Platform.ShareState(a.ID)
	c.True(!ended, "failed recipient management must not revoke share")
}

func credentialHandling(c *Check, w *World) {
	const entry = "/demo/a%2Fb?token=application-token&x=a%20b&x=2"
	sh := createRecipientShare(c, w, false, "http://localhost:3000"+entry)
	w.Note("Whole-origin scope: recipients start at the entry page and may navigate the target origin")
	b := newRecipientBrowser("alice")
	u, _ := url.Parse(sh.Origin)
	//nolint:gosec // Presented request cookies have no Set-Cookie security attributes; these are fixture inputs.
	b.cookies[u.Host] = []http.Cookie{{Name: "app_session", Value: "application-cookie"}, {Name: "token", Value: "application-cookie-token"}, {Name: cookiePrefix + "unused", Value: "synthetic-private-cookie"}}
	res := w.visit(b, sh.URL, pageRequest)
	checkSession(c, sh, res)
	c.True(res.Location == sh.Origin+entry, "entry path and raw query must be preserved exactly: %s", res.Location)
	app := allowVisit(c, w, b, res.Location, pageRequest)
	if app.Upstream != nil {
		c.True(app.Upstream.URL == "http://localhost:3000"+entry, "upstream path/query changed: %s", app.Upstream.URL)
		c.True(len(app.Upstream.Cookies) == 2 && app.Upstream.Cookies[0].Name == "app_session" && app.Upstream.Cookies[0].Value == "application-cookie" && app.Upstream.Cookies[1].Name == "token", "only Purlview cookies may be consumed: %v", app.Upstream.Cookies)
		c.True(!strings.Contains(app.Upstream.URL, "synthetic-") && !strings.Contains(app.Body, "synthetic-"), "Purlview credential leaked upstream or to application response")
	}
	allowVisit(c, w, b, sh.Origin+"/outside-entry?token=another-app-value", apiRequest)
	denyVisit(c, w, newRecipientBrowser("fresh-clean-link"), res.Location, pageRequest)
	logs := w.Platform.recipientDiagnostics() + w.Daemon.logs.String()
	for _, secret := range []string{"synthetic-secret-01", res.Cookie.Value, "synthetic-private-cookie"} {
		c.True(!strings.Contains(logs, secret), "operational diagnostics leaked %s", secret)
	}
	w.Note("operational diagnostics checked: no Purlview tokens/cookies; synthetic values above are explicit transcript inputs")
}

func unsafeRedirect(c *Check, w *World) {
	for _, restricted := range []bool{false, true} {
		sh := createRecipientShare(c, w, restricted, target)
		link := sh.URL
		if restricted {
			link = requestEmail(c, w, sh, permittedEmail).URL
		}
		parsedLink, _ := url.Parse(link)
		for _, dest := range []string{"/app?token=" + parsedLink.Query().Get("token"), "https://evil.invalid/", "//evil.invalid/", "https://" + AccountDomain + "/", sh.Origin + ".evil.invalid/", "/\\evil.invalid/", "/%2f%2fevil.invalid/", "/_purlview/access?token=synthetic-leak"} {
			res := denyVisit(c, w, newRecipientBrowser("unsafe-redirect"), link+"&return_to="+url.QueryEscape(dest), pageRequest)
			c.True(res.Reason == "unsafe_redirect" && res.Location == "", "unsafe redirect must fail before issuing a session")
		}
		// Rejected redirects must not consume a single-use email token.
		checkSession(c, sh, w.visit(newRecipientBrowser("safe-destination"), link+"&return_to="+url.QueryEscape(sh.Origin+"/"), pageRequest))
	}
}

func recipientManagement(c *Check, w *World) {
	for _, refKind := range []string{"label", "id", "secret-link", "clean-url", "restricted-link"} {
		sh := createRecipientShare(c, w, refKind == "restricted-link", target)
		listed := w.Exec("list")
		c.Exit(listed, 0)
		c.Stdout(listed, share.Label(sh.ID))
		c.True(!strings.Contains(listed.Stdout, "token=") && !strings.Contains(listed.Stdout, "https://"), "list never shows a link:\n%s", listed.Stdout)
		c.StdoutExactly(w.Exec("link", share.Label(sh.ID)), sh.URL+"\n")
		ref := share.Label(sh.ID)
		switch refKind {
		case "id":
			ref = sh.ID
		case "secret-link", "restricted-link":
			ref = sh.URL
		case "clean-url":
			ref = sh.Origin + "/app?token=application-token"
		}
		r := w.Exec("stop", ref)
		c.Exit(r, 0)
		c.NoStdout(r)
		c.Stderr(r, "✓ Stopped "+share.Label(sh.ID))
		r = w.Exec("stop", sh.Origin+accessPath+"?token=synthetic-wrong-token")
		c.Exit(r, 0)
		c.NotStderr(r, "synthetic-")
	}
	for _, raw := range []string{"https://unknown." + ShareDomain + accessPath + "?token=synthetic-secret-error", "https://bad%host/access?token=synthetic-secret-error", "https://user:synthetic-secret-error@demo." + ShareDomain, "shr_bad?token=synthetic-secret-error"} {
		r := w.Exec("stop", raw)
		c.True(r.Code != 0, "invalid or unknown reference must fail")
		c.NotStderr(r, "synthetic-secret-error")
		c.NoStdout(r)
	}
	sh := createRecipientShare(c, w, false, target)
	w.Platform.RevokeUncertain()
	r := w.Exec("stop", sh.URL)
	c.Exit(r, 1)
	c.NotStderr(r, "synthetic-")
	c.Stderr(r, "✗ Couldn't confirm "+share.Label(sh.ID)+" stopped")
	sh = createRecipientShare(c, w, false, target)
	w.Note("platform becomes unreachable; clean URL still resolves the local share")
	w.Platform.Unreachable()
	r = w.Exec("stop", sh.Origin+"/app?token=synthetic-error-token")
	c.Exit(r, 1)
	c.Stderr(r, "! Stopped here, but Purlview couldn't confirm")
	c.NotStderr(r, "synthetic-")
	c.True(!strings.Contains(w.Daemon.logs.String(), "synthetic-secret-"), "engine logged a secret link")
}

func recipientEnd(c *Check, w *World, restricted bool, end string) {
	sh := createRecipientShare(c, w, restricted, target)
	b, link := authorisedBrowser(c, w, sh, "authorised")
	if restricted {
		link = requestEmail(c, w, sh, permittedEmail).URL
	}
	streams := []string{allowVisit(c, w, b, sh.Origin+"/socket", websocketRequest).Stream, allowVisit(c, w, b, sh.Origin+"/events", sseRequest).Stream}
	// Remember the presented cookie so server-side expiry is tested even if a
	// conforming browser would stop sending it at the cookie expiry.
	u, _ := url.Parse(sh.Origin)
	cookies := append([]http.Cookie(nil), b.cookies[u.Host]...)
	if end == "revocation-disconnected" || end == "expiry" {
		w.Note("owning daemon disconnected; authority remains on the platform")
		w.Platform.DisconnectServing(sh.ID)
		_, _, _, conns := w.Platform.ShareState(sh.ID)
		c.True(conns == 0, "owner connection must stay offline while authority ends the share")
	}
	if end == "expiry" {
		w.Advance(time.Hour)
	} else {
		c.Exit(w.Exec("stop", sh.URL), 0)
	}
	for _, id := range streams {
		reason := w.Platform.streamState(id)
		c.True(reason == "expired" && end == "expiry" || reason == "revoked" && end != "expiry", "stream %s did not close at authority change: %s", id, reason)
		w.Note("%s closed: %s (before another recipient request)", id, reason)
	}
	denyVisit(c, w, newRecipientBrowser("link-after-end"), link, pageRequest)
	denyVisit(c, w, b, sh.Origin+"/", pageRequest)
	before := w.Platform.forwardedCount()
	res := w.Platform.recipientAccess(recipientRequest{URL: sh.Origin + "/api", Kind: apiRequest, Cookies: cookies})
	w.Note("replay the formerly valid session cookie after the share ended (synthetic fixture input)")
	w.recordAccess(res)
	c.True(res.Status == 403 && res.Upstream == nil && w.Platform.forwardedCount() == before, "server must reject ended session regardless of browser expiry")
	if restricted {
		res := w.Platform.requestVerification(verificationRequest{EntryURL: sh.URL, Email: permittedEmail})
		c.True(res.Status == 403, "ended share must not issue new email links")
	}
}
