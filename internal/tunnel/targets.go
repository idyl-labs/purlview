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
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"

	"github.com/idyl-labs/purlview/internal/share"
)

// Several targets. A share names 1 to 10 targets. The first owns the share's
// origin; each further one is mounted at /_purlview/t/<n>, n its 1-based
// position in the list, inside the reserved namespace the gateway lets
// through for exactly that shape. The mount index is the only thing that selects a target: no
// request field names a host or a port.

// mountPrefix begins every further target's mount.
const mountPrefix = "/_purlview/t/"

// target is one listed target as the proxy serves it.
type target struct {
	// url is the scheme and host requests are forwarded to.
	url    *url.URL
	origin origin
	// mount is "" for the first target and /_purlview/t/<n> for the nth.
	mount string
	// name is the target as people read it: localhost:8000.
	name  string
	watch *appWatch
	rp    *httputil.ReverseProxy
	// catchup is the target's catch-up observer, nil without rewriting.
	catchup *observer
}

// mountFor is the mount of the target at 1-based position n.
func mountFor(n int) string {
	if n < 2 {
		return ""
	}
	return mountPrefix + strconv.Itoa(n)
}

// cutMount recognises /_purlview/t/<n>, n from 2 to 99 without a leading
// zero, at the start of a raw request path or of a Referer's path and query.
// rest is what follows: empty, or beginning with '/', '?' or '#'. Any other
// spelling, an encoded one included, is no mount.
func cutMount(path string) (n int, rest string, ok bool) {
	after, found := strings.CutPrefix(path, mountPrefix)
	if !found {
		return 0, "", false
	}
	d := 0
	for d < len(after) && isDigit(after[d]) {
		d++
	}
	if d == 0 || d > 2 || after[0] == '0' {
		return 0, "", false
	}
	if d < len(after) && after[d] != '/' && after[d] != '?' && after[d] != '#' {
		return 0, "", false
	}
	n, _ = strconv.Atoi(after[:d])
	if n < 2 {
		return 0, "", false
	}
	return n, after[d:], true
}

// newTargets builds the served list: the admitted target first, then the
// further targets of the share's own list, each with its mount. The admitted
// target must be the first listed one; when the two disagree only the
// admitted target is served, with no mounts and no rewriting, and the reason
// is returned for the log.
func newTargets(admitted *url.URL, listed []string, wake chan struct{}) (targets []*target, mismatch string) {
	if wake == nil {
		wake = make(chan struct{}, 1)
	}
	first := &target{url: &url.URL{Scheme: admitted.Scheme, Host: admitted.Host}, name: share.DisplayTarget(admitted.String())}
	first.origin, _ = parseOrigin(first.url.String())
	first.watch = newAppWatch(first.name, wake)
	targets = []*target{first}
	if len(listed) == 0 {
		return targets, ""
	}
	o, ok := parseOrigin(originOf(listed[0]))
	if !ok || !namesTarget(o, first.origin) {
		return targets, "admitted " + first.url.String() + " is not the first listed target " + listed[0]
	}
	for i, raw := range listed[1:] {
		u, err := url.Parse(originOf(raw))
		if err != nil || u.Host == "" {
			return targets[:1], "unusable target " + raw
		}
		t := &target{url: &url.URL{Scheme: u.Scheme, Host: u.Host}, mount: mountFor(i + 2), name: share.DisplayTarget(raw)}
		if t.origin, ok = parseOrigin(t.url.String()); !ok {
			return targets[:1], "unusable target " + raw
		}
		t.watch = newAppWatch(t.name, wake)
		targets = append(targets, t)
	}
	return targets, ""
}

// originOf reduces a target URL to its scheme and host: a path or query on a
// further target is accepted and ignored (only the first target opens a page).
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return u.Scheme + "://" + u.Host
}
