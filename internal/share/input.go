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

package share

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/idyl-labs/purlview/sdk/resource"
)

// Lifetime bounds: the default and the maximum are one hour.
const (
	DefaultTTL = time.Hour
	MaxTTL     = time.Hour
	// MinTTL: sub-second lifetimes are meaningless for a share and would
	// expire before the link is shown.
	MinTTL = time.Second
)

// InputError is an argument the CLI refuses, as the line it prints:
// "✗ Problem — Remedy Try". Try is the command to run instead and is shown
// bright; both are optional.
type InputError struct {
	Problem string
	Remedy  string
	Try     string
}

func (e *InputError) Error() string {
	if e.Remedy+e.Try == "" {
		return e.Problem
	}
	return e.Problem + " — " + e.Remedy + e.Try
}

// Target is a validated, resolved share target.
type Target struct {
	// URL is the absolute http(s) URL, including any path and query.
	URL string
}

func (t Target) String() string { return t.URL }

// ParseTarget accepts a port (3000, :3000), host:port or an http(s) URL. A
// port alone means localhost; without a scheme the target is http. A path or
// query is kept as the start page; a fragment, credentials in the URL, other
// schemes and malformed input are refused with what to type instead.
func ParseTarget(raw string) (Target, error) {
	raw = strings.TrimSpace(raw)
	notAddress := &InputError{Problem: raw + " isn't a port or an address", Remedy: "try ", Try: "purlview share 3000"}
	if raw == "" {
		return Target{}, &InputError{Problem: "share needs a port or an address", Remedy: "try ", Try: "purlview share 3000"}
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return Target{}, notAddress
	}
	candidate := raw
	if !strings.Contains(raw, "://") {
		if strings.HasPrefix(raw, "//") || strings.Contains(raw, ":/") {
			return Target{}, notAddress
		}
		// 3000 and :3000 mean localhost. What follows the port is the
		// start page.
		hostport, page := raw, ""
		if i := strings.IndexAny(raw, "/?"); i >= 0 {
			hostport, page = raw[:i], raw[i:]
		}
		if port, ok := strings.CutPrefix(hostport, ":"); ok || digits(hostport) {
			if !isPort(port) {
				return Target{}, invalidPort(port)
			}
			hostport = "localhost:" + port
		}
		candidate = "http://" + hostport + page
	}
	u, err := url.Parse(candidate)
	if err != nil {
		if p := portOf(candidate); p != "" && !isPort(p) {
			return Target{}, invalidPort(p)
		}
		return Target{}, notAddress
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		u.Scheme = strings.ToLower(u.Scheme)
	default:
		return Target{}, &InputError{Problem: u.Scheme + " addresses can't be shared", Remedy: "use http or https"}
	}
	if u.User != nil {
		return Target{}, &InputError{Problem: "An address with a username or password can't be shared", Remedy: "remove them; your app's own login still applies"}
	}
	if u.Fragment != "" || strings.HasSuffix(candidate, "#") {
		return Target{}, &InputError{Problem: "The part after # never reaches an app", Remedy: "remove it"}
	}
	host := u.Hostname()
	if host == "" {
		return Target{}, notAddress
	}
	if !strings.Contains(raw, "://") && u.Port() == "" {
		return Target{}, &InputError{Problem: host + " needs a port", Remedy: "try ", Try: "purlview share " + host + ":3000"}
	}
	if p := u.Port(); p != "" && !isPort(p) {
		return Target{}, invalidPort(p)
	}
	if strings.Contains(host, "..") || strings.HasPrefix(host, ".") {
		return Target{}, notAddress
	}
	if len(u.Scheme)+3+len(u.Host) > MaxTargetLength {
		// Bounds the addresses the daemon's rewrite table holds: the origin,
		// not the start page, which the table never holds. A host name is at
		// most 253 characters in any case.
		return Target{}, &InputError{Problem: "That address is too long", Remedy: fmt.Sprintf("addresses are up to %d characters", MaxTargetLength)}
	}
	if ip := net.ParseIP(strings.Trim(host, "[]")); ip == nil {
		for _, r := range host {
			if r != '-' && r != '.' && r != '_' && (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
				return Target{}, notAddress
			}
		}
	}
	u.Host = strings.ToLower(u.Host)
	return Target{URL: u.String()}, nil
}

// MaxTargetLength bounds one target's origin, scheme to port; a start page
// does not count.
const MaxTargetLength = 256

// ParseTargets accepts the share's targets in the order given: one to
// resource.MaxTargets, each in any form
// ParseTarget takes. The first is what the link opens; a path or query on a
// further one is accepted and ignored, since only the first opens a page. A
// target named twice, under any of its loopback names, is refused.
func ParseTargets(raw []string) ([]Target, error) {
	if len(raw) == 0 {
		return nil, &InputError{Problem: "share needs a port or an address", Remedy: "try ", Try: "purlview share 3000"}
	}
	if len(raw) > resource.MaxTargets {
		return nil, &InputError{Problem: fmt.Sprintf("share names %d apps", len(raw)), Remedy: fmt.Sprintf("a share takes %d at most", resource.MaxTargets)}
	}
	out := make([]Target, 0, len(raw))
	keys := map[string]int{}
	for i, v := range raw {
		t, err := ParseTarget(v)
		if err != nil {
			return nil, err
		}
		if i > 0 {
			u, _ := url.Parse(t.URL)
			t.URL = u.Scheme + "://" + u.Host
		}
		key := resource.TargetKey(t.URL)
		if first, seen := keys[key]; seen {
			return nil, &InputError{Problem: DisplayTarget(out[first].URL) + " is named twice", Remedy: "list each app once"}
		}
		keys[key] = i
		out = append(out, t)
	}
	return out, nil
}

// TargetURLs is the targets' addresses, as a share record and the daemon
// carry them.
func TargetURLs(targets []Target) []string {
	out := make([]string, len(targets))
	for i, t := range targets {
		out[i] = t.URL
	}
	return out
}

func invalidPort(p string) *InputError {
	return &InputError{Problem: p + " isn't a valid port", Remedy: "ports run from 1 to 65535"}
}

func digits(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

// portOf finds the port of an address url.Parse refused for it.
func portOf(candidate string) string {
	rest := candidate[strings.Index(candidate, "://")+3:]
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndex(rest, ":"); i >= 0 && !strings.Contains(rest[i:], "]") {
		return rest[i+1:]
	}
	return ""
}

// DisplayTarget names a target the way people read it: host and port without
// the scheme (localhost:3000).
func DisplayTarget(target string) string {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return target
	}
	return u.Host
}

// EntryAndOrigin splits a target into the entry page (path and query) and
// the origin recipients can reach, when the target names more than the
// origin's root. Recipients can reach the whole origin, not only the path.
func EntryAndOrigin(target string) (entry, origin string, ok bool) {
	u, err := url.Parse(target)
	if err != nil || u.Host == "" {
		return "", "", false
	}
	if (u.Path == "" || u.Path == "/") && u.RawQuery == "" {
		return "", "", false
	}
	entry = u.Path
	if entry == "" {
		entry = "/"
	}
	if u.RawQuery != "" {
		entry += "?" + u.RawQuery
	}
	return entry, u.Scheme + "://" + u.Host, true
}

func isPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && digits(s) && n > 0 && n <= 65535
}

// ParseTTL parses --ttl and enforces the bounds. An excessive value is
// rejected explicitly rather than clamped.
func ParseTTL(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultTTL, nil
	}
	d, err := time.ParseDuration(raw)
	switch {
	case err != nil:
		return 0, &InputError{Problem: "--ttl " + raw + " isn't a duration", Remedy: "use something like 15m"}
	case d < MinTTL:
		return 0, &InputError{Problem: "--ttl " + raw + " is too short", Remedy: "use " + FormatDuration(MinTTL) + " or more"}
	case d > MaxTTL:
		limit := FormatDuration(MaxTTL)
		return 0, &InputError{Problem: "--ttl " + raw + " is over the " + limit + " limit", Remedy: "use " + limit + " or less"}
	}
	return d, nil
}

// ParseRecipients validates every --to and returns the list a share records:
// lower case, without repeats, in the order given, at most
// resource.MaxRecipients. No --to means anyone with the link; an empty --to
// (an unset variable, say) is refused rather than read as none.
func ParseRecipients(raw []string) ([]string, error) {
	var out []string
	for _, v := range raw {
		email, err := ParseRecipient(v)
		if err != nil {
			return nil, err
		}
		out = append(out, email)
	}
	out = resource.NormaliseRecipients(out)
	if len(out) > resource.MaxRecipients {
		return nil, &InputError{Problem: fmt.Sprintf("--to names %d addresses", len(out)), Remedy: fmt.Sprintf("a share takes %d at most", resource.MaxRecipients)}
	}
	return out, nil
}

// ParseRecipient validates one email address, as --to takes it.
func ParseRecipient(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", &InputError{Problem: "--to needs an email address"}
	}
	if strings.ContainsAny(raw, ", ;") {
		return "", &InputError{Problem: raw + " isn't one email address", Remedy: "repeat --to for each"}
	}
	notEmail := &InputError{Problem: raw + " isn't an email address"}
	at := strings.Index(raw, "@")
	if at <= 0 || at != strings.LastIndex(raw, "@") || at == len(raw)-1 {
		return "", notEmail
	}
	domain := raw[at+1:]
	if !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.ContainsAny(raw, "<>\"()\\") {
		return "", notEmail
	}
	return strings.ToLower(raw), nil
}

// IDPrefix begins the API's canonical share identifier, shr_<label>. People
// read and type the bare label, which is also the share's host label.
const IDPrefix = "shr_"

// Label returns the id to display: the canonical id without its prefix.
func Label(id string) string { return strings.TrimPrefix(id, IDPrefix) }

// isLabel reports whether s has the share label shape: eight characters, the
// first a letter, without look-alike characters or vowels.
func isLabel(s string) bool {
	if len(s) != 8 || s[0] < 'a' {
		return false
	}
	return strings.Trim(s, "23456789bcdfghjkmnpqrstvwxyz") == ""
}

// ParseRef accepts a share label, its canonical id (shr_<label>) or any URL
// of the share: the secret or restricted link, or a page copied from the app.
func ParseRef(raw string) (Ref, error) {
	raw = strings.TrimSpace(raw)
	// Only something short enough to be a mistyped id is repeated: anything
	// longer may be a secret.
	notRef := &InputError{Problem: "That isn't a share id or link", Remedy: "see ", Try: "purlview list"}
	if raw != "" && len(raw) <= 12 && strings.Trim(raw, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_") == "" {
		notRef.Problem = raw + " isn't a share id or link"
	}
	if isLabel(raw) {
		return Ref{ID: IDPrefix + raw}, nil
	}
	if strings.HasPrefix(raw, IDPrefix) {
		if !resource.ShareID(raw) {
			return Ref{}, notRef
		}
		return Ref{ID: raw}, nil
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return Ref{}, notRef
		}
		return Ref{URL: "https://" + strings.ToLower(u.Host)}, nil
	}
	return Ref{}, notRef
}

// FormatDuration renders a lifetime compactly: 1h, 1h30m, 15m, 45s.
func FormatDuration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	d = d.Round(time.Second)
	h := d / time.Hour
	m := (d % time.Hour) / time.Minute
	s := (d % time.Minute) / time.Second
	var b strings.Builder
	if h > 0 {
		fmt.Fprintf(&b, "%dh", h)
	}
	if m > 0 {
		fmt.Fprintf(&b, "%dm", m)
	}
	if s > 0 && h == 0 {
		fmt.Fprintf(&b, "%ds", s)
	}
	if b.Len() == 0 {
		return "0s"
	}
	return b.String()
}

// FormatRemaining renders the time left until t as seen from now, rounded
// to the nearest minute once a minute or more remains, so the display does
// not flicker between "1h" and "59m" for a fresh share.
func FormatRemaining(now, t time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return "expired"
	}
	if d >= time.Minute {
		d = d.Round(time.Minute)
	}
	return FormatDuration(d)
}
