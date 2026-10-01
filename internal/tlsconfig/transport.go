package tlsconfig

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Transport optionally pins the destination IP while still verifying the URL's
// original TLS hostname, so a deployment can be reached before its DNS
// records exist.
func Transport(ca, ip string) (*http.Transport, error) {
	conf, e := Client(ca)
	if e != nil {
		return nil, e
	}
	d := &net.Dialer{Timeout: 5 * time.Second}
	tr := &http.Transport{TLSClientConfig: conf, TLSHandshakeTimeout: 5 * time.Second, MaxIdleConnsPerHost: 16}
	if ip != "" {
		if net.ParseIP(ip) == nil {
			return nil, errors.New("PURLVIEW_CONNECT_IP must be a literal IP")
		}
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, e := net.SplitHostPort(addr)
			if e != nil {
				return nil, e
			}
			return d.DialContext(ctx, network, net.JoinHostPort(ip, port))
		}
	}
	return tr, nil
}
