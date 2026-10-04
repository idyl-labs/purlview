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

// Package resource owns public product state and intrinsic validation. It has
// no dependency on API, client, CLI, storage or server implementation code.
package resource

import (
	"errors"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Identity describes an account and its authorised installation, without secrets.
type Identity struct {
	Account     string `json:"account"`
	AccountID   string `json:"account_id"`
	Device      string `json:"device"`
	DeviceLabel string `json:"device_label"`
}

// Validate checks structural identity fields, not current authority.
func (v Identity) Validate() error {
	if !Text(v.Account, 320) || !Identifier(v.AccountID) || !Identifier(v.Device) || !Text(v.DeviceLabel, 256) {
		return errors.New("invalid identity")
	}
	return nil
}

// Share is public management state. Recipient access links and credentials are
// deliberately absent. State is the control plane's observation, not local IPC.
// Recipients are the verified addresses that may open a restricted share, in
// the creator's order; empty means anyone with the secret link.
//
// Target is the first target, what the link opens. Targets is the whole list
// when the share names several, the first included;
// it is absent for a share of one target. The list is display state: the
// daemon serves the further targets, the platform only records them.
type Share struct {
	EntryOrigin string    `json:"entry_origin,omitempty"`
	ID          string    `json:"id"`
	Origin      string    `json:"origin"`
	Target      string    `json:"target"`
	Targets     []string  `json:"targets,omitempty"`
	Device      string    `json:"device"`
	DeviceLabel string    `json:"device_label"`
	Recipients  []string  `json:"recipients,omitempty"`
	RewriteURLs bool      `json:"rewrite_urls"`
	State       string    `json:"state"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Validate checks the intrinsic shape and original lifetime of a share.
func (s Share) Validate() error {
	if !ShareID(s.ID) || !Origin(s.Origin) || (s.EntryOrigin != "" && !Origin(s.EntryOrigin)) || !URL(s.Target) || !Identifier(s.Device) || !Text(s.DeviceLabel, 256) || !Recipients(s.Recipients) || s.CreatedAt.IsZero() || !s.ExpiresAt.After(s.CreatedAt) || s.ExpiresAt.Sub(s.CreatedAt) > time.Hour {
		return errors.New("invalid share")
	}
	if len(s.Targets) != 0 && !TargetsFor(s.Target, s.Targets) {
		return errors.New("invalid share targets")
	}
	switch s.State {
	case "starting", "ready", "reconnecting", "ended":
		return nil
	}
	return errors.New("invalid share state")
}

// ShareRef is an identity-only management reference: exactly one field is set.
type ShareRef struct {
	ID     string `json:"id,omitempty"`
	Origin string `json:"origin,omitempty"`
}

// Validate refuses recipient URLs; callers strip their path/query at the UI boundary.
func (r ShareRef) Validate() error {
	if r.ID != "" && r.Origin == "" && ShareID(r.ID) {
		return nil
	}
	if r.ID == "" && Origin(r.Origin) {
		return nil
	}
	return errors.New("invalid share reference")
}

// Identifier accepts bounded opaque ASCII identifiers and idempotency keys.
func Identifier(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// Text accepts bounded display text without terminal control characters.
func Text(s string, limit int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= limit && strings.IndexFunc(s, unicode.IsControl) < 0
}

// URL accepts absolute HTTP(S) URLs without credentials, fragments or whitespace.
func URL(s string) bool {
	if len(s) > 8192 || strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 || strings.Contains(s, "#") {
		return false
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return false
	}
	if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return false
		}
	}
	host := u.Hostname()
	if net.ParseIP(host) == nil {
		if strings.Contains(host, "..") || strings.HasPrefix(host, ".") {
			return false
		}
		for _, c := range host {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '.', c == '_':
			default:
				return false
			}
		}
	}
	return true
}

// Origin accepts only canonical HTTPS route identity, without a path or secret.
func Origin(s string) bool {
	if !URL(s) {
		return false
	}
	u, _ := url.Parse(s)
	return u.Scheme == "https" && u.Path == "" && u.RawQuery == "" && !u.ForceQuery && u.Host == strings.ToLower(u.Host)
}

// Recipient accepts one plain email, or empty for secret-link access.
func Recipient(s string) bool {
	if s == "" {
		return true
	}
	if !Text(s, 320) || strings.ContainsAny(s, " ,;<>\"()\\\t\r\n") || strings.Count(s, "@") != 1 {
		return false
	}
	parts := strings.Split(s, "@")
	return parts[0] != "" && strings.Contains(parts[1], ".") && !strings.HasPrefix(parts[1], ".") && !strings.HasSuffix(parts[1], ".")
}

// MaxRecipients bounds the verified addresses one share can name.
const MaxRecipients = 10

// NormaliseRecipients lower-cases addresses and drops repeats, keeping the
// first occurrence's position. It does not validate; see Recipients.
func NormaliseRecipients(list []string) []string {
	var out []string
	for _, v := range list {
		if v = strings.ToLower(v); !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// Recipients accepts a normalised list: at most MaxRecipients plain, lower-case,
// distinct addresses. Empty means secret-link access.
func Recipients(list []string) bool {
	if len(list) > MaxRecipients {
		return false
	}
	for i, v := range list {
		if v == "" || !Recipient(v) || v != strings.ToLower(v) || slices.Contains(list[:i], v) {
			return false
		}
	}
	return true
}

// MaxTargets bounds the targets one share can name.
const MaxTargets = 10

// TargetKey identifies a target's origin for equality: scheme, host and port,
// the default port made explicit, host case ignored, and the loopback names
// localhost, 127.0.0.1 and ::1 read as one name. It is
// empty for anything that is not a target URL.
func TargetKey(s string) string {
	if !URL(s) {
		return ""
	}
	u, _ := url.Parse(s)
	host := strings.ToLower(u.Hostname())
	if host == "127.0.0.1" || host == "::1" {
		host = "localhost"
	}
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	return u.Scheme + "://" + host + ":" + port
}

// Targets accepts a share's target list: one to MaxTargets URLs, the first
// possibly naming a start page, the rest bare origins, and no two naming the
// same target.
func Targets(list []string) bool {
	if len(list) == 0 || len(list) > MaxTargets {
		return false
	}
	seen := map[string]bool{}
	for i, v := range list {
		key := TargetKey(v)
		if key == "" || seen[key] {
			return false
		}
		seen[key] = true
		if i > 0 {
			u, _ := url.Parse(v)
			if u.Path != "" || u.RawQuery != "" || u.ForceQuery {
				return false
			}
		}
	}
	return true
}

// TargetsFor accepts a target list whose first entry is the share's target.
func TargetsFor(target string, list []string) bool {
	return Targets(list) && TargetKey(list[0]) == TargetKey(target)
}

// ShareID validates the canonical shr_<label> identifier carried by shares and
// management references. People read and type the bare label, which is also the
// share's host label; clients add the prefix before crossing this boundary.
func ShareID(s string) bool {
	return strings.HasPrefix(s, "shr_") && len(s) > 4 && Identifier(s) && !strings.Contains(s, "-")
}
