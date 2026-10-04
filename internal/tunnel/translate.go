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
	"net/http"
	"net/url"
	"strings"
)

// Header translation. The share stands in for the apps' own addresses: the
// browser knows every listed target by the share's public origin, the first
// at its root and each further one under its mount, while each app knows
// itself by its own address. Requests naming the
// public origin are rewritten to a target's and responses naming a target's
// to the public form. Everything else passes unchanged, so the apps'
// cross-site checks still see foreign sites as foreign. Vary is left alone.
// X-Forwarded-* headers are deliberately not added: a framework that trusts
// them would generate public URLs and defeat the translation.

// origin is an http or https origin with a lower-case host and an explicit
// port, so https://k7m2p4qx.purlview.invalid and https://K7M2P4QX.purlview.invalid:443 are equal. The zero value is
// no origin and equals nothing cutOrigin returns.
type origin struct{ scheme, host, port string }

var defaultPorts = map[string]string{"http": "80", "https": "443"}

// String is the origin as browsers serialise it: no default port.
func (o origin) String() string {
	host := o.host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if o.port != defaultPorts[o.scheme] {
		host += ":" + o.port
	}
	return o.scheme + "://" + host
}

// cutOrigin splits an absolute http(s) URL into its origin and the rest (path,
// query and fragment, possibly empty) exactly as written. Relative and
// protocol-relative references, other schemes, "null", "*", credentials and
// malformed values are not ok.
func cutOrigin(value string) (o origin, rest string, ok bool) {
	scheme, after, found := strings.Cut(value, "://")
	scheme = strings.ToLower(scheme)
	if !found || defaultPorts[scheme] == "" {
		return origin{}, "", false
	}
	authority := after
	if i := strings.IndexAny(after, "/?#"); i >= 0 {
		authority, rest = after[:i], after[i:]
	}
	u, err := url.Parse(scheme + "://" + authority)
	if err != nil || u.User != nil || u.Hostname() == "" || u.Host != authority {
		return origin{}, "", false
	}
	port := u.Port()
	if port == "" {
		port = defaultPorts[scheme]
	}
	return origin{scheme, strings.ToLower(u.Hostname()), port}, rest, true
}

// parseOrigin reads a bare origin, such as the share's public origin or the
// admitted target.
func parseOrigin(value string) (origin, bool) {
	o, rest, ok := cutOrigin(value)
	return o, ok && rest == ""
}

// The names of this machine's loopback interface: localhost, 127.0.0.1, and
// ::1, which localhost also resolves to. An app
// reached as localhost:3000 may call itself 127.0.0.1:3000 or [::1]:3000 in
// what it sends.
var loopbackNames = map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}

// namesTarget reports whether o is the target's origin. For a loopback target
// the other loopback names with the same scheme and port are the target too.
func namesTarget(o, target origin) bool {
	if o == target {
		return true
	}
	return o.scheme == target.scheme && o.port == target.port && loopbackNames[o.host] && loopbackNames[target.host]
}

// listed finds the listed target an origin names, if any.
func listed(o origin, targets []*target) *target {
	for _, t := range targets {
		if namesTarget(o, t.origin) {
			return t
		}
	}
	return nil
}

// translateRequest makes a request on its way to dest name a target where it
// named the share. A Referer at the public origin names the target whose page
// it is: the one under its mount, else the first; the mount is stripped from
// its path. An Origin at the public origin names the same target, the source,
// so the app sees what it would see on the developer's machine: a page on
// localhost:5173 calling localhost:8000 arrives with Origin
// http://localhost:5173. Without a Referer at the public origin, as on a
// WebSocket handshake, the source is dest itself. This includes WebSocket
// upgrades, whose Origin development servers check.
func translateRequest(h http.Header, public origin, dest *target, targets []*target) {
	if public == (origin{}) || dest == nil || dest.origin == (origin{}) || len(targets) == 0 {
		return
	}
	source := dest
	for i, v := range h["Referer"] {
		o, rest, ok := cutOrigin(v)
		if !ok || o != public {
			continue
		}
		src := targets[0]
		if n, after, ok := cutMount(rest); ok {
			// A mount the share does not have names no page: the source
			// falls back to dest and the path stays as it was.
			src = dest
			if n <= len(targets) {
				src, rest = targets[n-1], after
				if rest == "" {
					rest = "/" // the mount's root is that target's root
				}
			}
		}
		h["Referer"][i] = src.origin.String() + rest
		if i == 0 {
			source = src
		}
	}
	for i, v := range h["Origin"] {
		if o, rest, ok := cutOrigin(v); ok && rest == "" && o == public {
			h["Origin"][i] = source.origin.String()
		}
	}
}

// translateResponse makes a response from a target name the share where it
// named any listed target: the origin part of an absolute Location becomes
// that target's public form, the public origin plus its mount; an absolute
// Location at a mounted target's own root, one beginning with a single slash,
// gains that target's mount, since the browser would otherwise resolve it at
// the first target; and an Access-Control-Allow-Origin that is a listed
// target's origin becomes the public origin.
func translateResponse(h http.Header, public origin, from *target, targets []*target) {
	if public == (origin{}) || from == nil {
		return
	}
	for i, v := range h["Location"] {
		if o, rest, ok := cutOrigin(v); ok {
			if t := listed(o, targets); t != nil {
				h["Location"][i] = public.String() + t.mount + rest
			}
		} else if from.mount != "" && strings.HasPrefix(v, "/") && !strings.HasPrefix(v, "//") {
			h["Location"][i] = from.mount + v
		}
	}
	for i, v := range h["Access-Control-Allow-Origin"] {
		if o, rest, ok := cutOrigin(v); ok && rest == "" && listed(o, targets) != nil {
			h["Access-Control-Allow-Origin"][i] = public.String()
		}
	}
}
