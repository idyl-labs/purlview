package daemon

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/url"
	"time"

	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/ipc"
	"github.com/idyl-labs/purlview/internal/share"
	"github.com/idyl-labs/purlview/internal/share/engine"
	"github.com/idyl-labs/purlview/internal/tlsconfig"
	"github.com/idyl-labs/purlview/internal/tunnel"
	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/purlview"
	"github.com/idyl-labs/purlview/sdk/resource"
)

type prober struct{}

func (prober) Probe(ctx context.Context, target string) error {
	u, e := url.Parse(target)
	if e != nil {
		return e
	}
	addr := u.Host
	if u.Port() == "" {
		port := "80"
		if u.Scheme == "https" {
			port = "443"
		}
		addr = net.JoinHostPort(u.Hostname(), port)
	}
	c, e := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if e == nil {
		e = c.Close()
	}
	return e
}
func liveEngine(getenv func(string) string, logger *log.Logger) (*engine.Engine, error) {
	if getenv("PURLVIEW_PLATFORM_ENDPOINT") == "" {
		return nil, nil
	}
	trust, e := tlsconfig.Transport(getenv("PURLVIEW_CA_FILE"), getenv("PURLVIEW_CONNECT_IP"))
	if e != nil {
		return nil, e
	}
	sdk, e := purlview.New(purlview.Config{Endpoint: getenv("PURLVIEW_PLATFORM_ENDPOINT"), Transport: trust, Host: getenv("PURLVIEW_PLATFORM_HOST"), AllowHTTP: getenv("PURLVIEW_PLATFORM_ALLOW_HTTP") == "1", Client: "purlview/" + buildinfo.Current().Version})
	if e != nil {
		return nil, e
	}
	tc, e := tunnel.New(getenv)
	if e != nil {
		return nil, e
	}
	tc.Logger = logger
	return engine.New(engine.Config{Platform: engine.ComposedPlatform{Management: engine.SDKManagement{Client: sdk}, TunnelClient: tc}, Prober: prober{}, Logger: logger}), nil
}
func (s *Server) dispatchShare(c *serverConn, req *ipc.Request) *ipc.Response {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if c.shares == nil {
		session, e := s.shares.Open(ctx, false)
		if e != nil {
			return fail(req.ID, ipc.CodeShuttingDown, "daemon is stopping")
		}
		c.shares = session
		go func() {
			for ev := range session.Events() {
				if c.subscribed() {
					c.enqueueEvent(&ipc.Event{Event: ipc.EventShare, Data: ipc.Marshal(ipc.ShareEventData{Kind: string(ev.Kind), Share: record(ev.Share), Remote: remote(ev.Remote), Detail: ev.Detail})})
				}
				s.armIdle()
			}
		}()
	}
	bad := func() *ipc.Response { return fail(req.ID, ipc.CodeBadRequest, "invalid share request") }
	failed := func(err error) *ipc.Response {
		if errors.Is(err, io.EOF) {
			return fail(req.ID, ipc.CodeShuttingDown, "daemon unavailable")
		}
		return fail(req.ID, ipc.CodeNotFound, "share operation unavailable")
	}
	switch req.Op {
	case ipc.OpShareStart:
		p, e := decode[ipc.ShareStartParams](req)
		if e != nil {
			return bad()
		}
		listed := p.Targets
		if len(listed) == 0 {
			listed = []string{p.Target}
		}
		targets, e := share.ParseTargets(listed)
		if e != nil || p.TTLMs < 1000 || p.TTLMs > 3600000 || !resource.Recipients(p.Recipients) || (p.Owner != "attached" && p.Owner != "detached") {
			return bad()
		}
		cr := api.InstallationCredential{Identity: resource.Identity{Account: p.Credential.Account, AccountID: p.Credential.AccountID, Device: p.Credential.Device, DeviceLabel: p.Credential.DeviceLabel}, Token: p.Credential.Token}
		v, e := c.shares.Start(ctx, share.StartRequest{Attempt: p.Attempt, Owner: share.Owner(p.Owner), Credential: cr, Spec: share.Spec{Targets: targets, TTL: time.Duration(p.TTLMs) * time.Millisecond, Recipients: p.Recipients, NoRewrite: p.NoRewrite}})
		if e != nil {
			return failed(e)
		}
		return ok(req.ID, record(v))
	case ipc.OpShareStop:
		p, e := decode[ipc.ShareStopParams](req)
		if e != nil {
			return bad()
		}
		v, e := c.shares.Stop(ctx, share.StopRequest{ID: p.ID, Attempt: p.Attempt, Reason: share.EndReason(p.Reason)})
		if e != nil {
			return failed(e)
		}
		return ok(req.ID, ipc.ShareStopResult{Share: record(v.Share), AlreadyEnded: v.AlreadyEnded, Remote: remote(v.Remote)})
	case ipc.OpShareStopAll:
		p, e := decode[ipc.ShareStopAllParams](req)
		if e != nil {
			return bad()
		}
		n, e := c.shares.StopAll(ctx, share.EndReason(p.Reason))
		if e != nil {
			return failed(e)
		}
		return ok(req.ID, ipc.ShareStopAllResult{Ended: n})
	case ipc.OpShareList:
		shares, e := c.shares.Shares(ctx)
		if e != nil {
			return failed(e)
		}
		out := ipc.ShareListResult{Shares: []ipc.ShareRecord{}}
		for _, v := range shares {
			out.Shares = append(out.Shares, record(v))
		}
		return ok(req.ID, out)
	}
	return bad()
}
func record(v share.Share) ipc.ShareRecord {
	var invites []ipc.ShareInvite
	for _, i := range v.Invites {
		invites = append(invites, ipc.ShareInvite{Email: i.Email, Status: i.Status})
	}
	return ipc.ShareRecord{ID: v.ID, Origin: v.Origin, URL: v.URL, Attempt: v.Attempt, Target: v.Target, Targets: v.Targets, Device: v.Device, DeviceLabel: v.DeviceLabel, Recipients: v.Recipients, Invites: invites, RewriteURLs: v.RewriteURLs, NoRewrite: v.NoRewrite, Owner: string(v.Owner), State: string(v.State), CreatedAt: v.CreatedAt.Format(time.RFC3339Nano), ExpiresAt: v.ExpiresAt.Format(time.RFC3339Nano), EndReason: string(v.EndReason), EndApp: v.EndApp, EndLimit: v.EndLimit, Detail: v.Detail}
}
func remote(v share.RemoteOutcome) ipc.ShareRemote {
	return ipc.ShareRemote{Status: string(v.Status), Detail: v.Detail}
}
