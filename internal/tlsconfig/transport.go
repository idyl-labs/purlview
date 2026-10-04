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
