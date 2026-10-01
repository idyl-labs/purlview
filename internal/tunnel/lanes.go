package tunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/idyl-labs/hyperplane-go/dock"
	"github.com/idyl-labs/hyperplane-go/generation"
	"github.com/idyl-labs/hyperplane-go/wire"
	"github.com/spiffe/go-spiffe/v2/bundle/x509bundle"
	"github.com/spiffe/go-spiffe/v2/spiffeid"

	"github.com/idyl-labs/purlview/internal/share/engine"
	"github.com/idyl-labs/purlview/sdk/api"
)

// shareDock is the part of a dock that a share uses.
type shareDock interface {
	Gen() generation.DockGen
	Context() context.Context
	Drained() <-chan struct{}
	AcceptLane(ctx context.Context) (*dock.Lane, error)
	Close() error
}

// laneStreams is how many lane streams the edge may open toward a share's
// dock at once: the gateway's limit of concurrent streams per share. QUIC's
// default would stop at 100.
const laneStreams = 256

// openDock docks on the edge.
func openDock(ctx context.Context, cfg dock.Config) (shareDock, error) {
	d, err := dock.Open(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return d, nil
}

// ConnectServing docks a share on the edge with the tunnel material its
// create returned. The share docks with its own SVID
// and admission lease; each stream the gateway opens on a lane is
// end-to-end TLS in which the share is the server and the gateway the
// authenticated client. Docking is the admission: a dock the edge accepts
// is ready. A redock names the share's previous dock, so the edge treats it
// as a succession, and resumes its TLS session. s also carries the
// share's public content origin, its target list and whether bodies are
// rewritten; the proxy translates between the public origin and the
// targets in headers and bodies.
func (c *Client) ConnectServing(ctx context.Context, _ api.InstallationCredential, id string, s engine.Serving) (engine.Connection, error) {
	t := s.Tunnel
	if t == nil || s.Key == nil || len(t.SVID) == 0 {
		return nil, &engine.Rejected{Reason: engine.RejectRequest, Detail: "the platform issued no tunnel for the share"}
	}
	edge, err1 := spiffeid.FromString(t.Edge)
	gateway, err2 := spiffeid.FromString(t.Gateway)
	if err1 != nil || err2 != nil || len(s.Targets) == 0 {
		return nil, &engine.Rejected{Reason: engine.RejectRequest}
	}
	target, err := url.Parse(s.Targets[0])
	if err != nil || target.Host == "" {
		return nil, &engine.Rejected{Reason: engine.RejectRequest}
	}
	var bundle []*x509.Certificate
	for _, der := range t.Bundle {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, &engine.Rejected{Reason: engine.RejectRequest}
		}
		bundle = append(bundle, cert)
	}
	endpoint := t.Endpoint
	if c.ConnectIP != "" {
		_, port, _ := net.SplitHostPort(endpoint)
		endpoint = net.JoinHostPort(c.ConnectIP, port)
	}
	cert := tls.Certificate{Certificate: t.SVID, PrivateKey: s.Key}
	open := c.openDock
	if open == nil {
		open = openDock
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	d, err := open(dctx, dock.Config{
		Endpoint:     endpoint,
		ServerID:     edge,
		SVID:         cert,
		Bundles:      x509bundle.FromX509Authorities(edge.TrustDomain(), bundle),
		Admission:    &dock.DemandAdmission{LeaseEnvelope: t.Lease, Signer: s.Key},
		Predecessor:  s.Lineage.Predecessor(),
		SessionCache: s.Lineage.Sessions(),

		MaxIncomingStreams: laneStreams,
	})
	cancel()
	var busy dock.ErrOverloaded
	if errors.As(err, &busy) {
		return nil, &engine.Unavailable{Detail: "the edge is busy", RetryAfter: busy.RetryAfter}
	}
	if err != nil {
		return nil, &engine.Unavailable{}
	}
	s.Lineage.Docked(d.Gen())

	roots := x509.NewCertPool()
	for _, b := range bundle {
		roots.AddCert(b)
	}
	inner := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    roots,
		// VerifyConnection runs on every handshake, resumed ones included;
		// the chain itself was verified against ClientCAs.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || len(cs.PeerCertificates[0].URIs) != 1 || cs.PeerCertificates[0].URIs[0].String() != gateway.String() {
				return errors.New("lane peer is not the gateway")
			}
			return nil
		},
	}

	life, stop := context.WithCancel(context.Background())
	out := &connection{events: make(chan engine.ConnEvent, 2), cancel: stop, close: d.Close}
	out.events <- engine.ConnEvent{Kind: engine.ConnReady, Detail: s.Targets[0]}
	proxy := c.proxyFor(id, target, s)
	notAfter := leafNotAfter(t)
	ended := make(chan engine.ConnEventKind, 1)
	go func() {
		kind := engine.ConnLost
		select {
		case <-d.Context().Done():
		case <-d.Drained():
			kind = engine.ConnDrained
		case <-life.Done():
		}
		// The lease ends with the share, so a dock that ends at or after it
		// is the share expiring.
		if !notAfter.IsZero() && !time.Now().Before(notAfter) {
			kind = engine.ConnExpired
		}
		ended <- kind
	}()
	go func() {
		defer func() { _ = out.Close() }()
		defer proxy.closeIdle()
		forwardEvents(life, out.events, ended, proxy.watches()...)
	}()
	go func() {
		for {
			lane, err := d.AcceptLane(life)
			if err != nil {
				return
			}
			go serveLane(life, lane, inner, proxy)
		}
	}()
	return out, nil
}

func leafNotAfter(t *api.Tunnel) time.Time {
	leaf, err := x509.ParseCertificate(t.SVID[0])
	if err != nil {
		return time.Time{}
	}
	return leaf.NotAfter
}

// serveLane serves every end-to-end stream on one lane; streams of any
// other class are refused.
func serveLane(ctx context.Context, lane *dock.Lane, inner *tls.Config, h http.Handler) {
	for {
		stream, err := lane.AcceptStream(ctx)
		if err != nil {
			return
		}
		if stream.Class() != wire.LaneStreamClassE2EMTLS {
			stream.Abort()
			continue
		}
		go func() {
			conn := tls.Server(laneConn{stream}, inner)
			hs, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := conn.HandshakeContext(hs)
			cancel()
			if err != nil {
				_ = conn.Close()
				return
			}
			serveStream(ctx, withDeadlines(conn), h)
		}()
	}
}

// withDeadlines bridges c through an in-memory pipe so its reader honours
// deadlines. Lane streams have none, and net/http needs a read deadline to
// interrupt its background read when a handler hijacks the connection, as
// the proxy does for a WebSocket upgrade; without it the upgrade hangs.
func withDeadlines(c net.Conn) net.Conn {
	served, bridge := net.Pipe()
	go func() {
		_, _ = io.Copy(bridge, c)
		_ = bridge.Close()
	}()
	go func() {
		_, _ = io.Copy(c, bridge)
		_ = c.Close()
	}()
	return served
}

// laneConn gives a lane stream the net.Conn shape the TLS server needs.
// Lane streams have no deadlines: the handshake is bounded by its context
// and serving by the HTTP server's own timeouts.
type laneConn struct{ *dock.LaneStream }

func (laneConn) LocalAddr() net.Addr              { return laneAddr{} }
func (laneConn) RemoteAddr() net.Addr             { return laneAddr{} }
func (laneConn) SetDeadline(time.Time) error      { return nil }
func (laneConn) SetReadDeadline(time.Time) error  { return nil }
func (laneConn) SetWriteDeadline(time.Time) error { return nil }

type laneAddr struct{}

func (laneAddr) Network() string { return "lane" }
func (laneAddr) String() string  { return "lane" }
