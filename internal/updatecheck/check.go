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

// Package updatecheck asks the public release surface whether a newer
// Purlview exists and formats the notice the CLI prints on stderr.
//
// The check is metadata only: one bounded, conditional HTTP GET against the
// GitHub releases API of the configured release repository. It never
// downloads an executable, never sends account data, and never blocks
// sharing: every failure is silent and cached state is reused.
//
// The daemon runs the check on behalf of each `share` invocation (the CLI
// asks for it and waits briefly), so a share that exits quickly does not
// leak a half-finished request and the ETag cache survives across runs.
package updatecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/idyl-labs/purlview/internal/ipc"
	"github.com/idyl-labs/purlview/internal/semver"
)

// The release build may set it with -X to the repository its releases are
// published to, so the checker always looks where releases actually are.
// The default is Purlview's public release repository.
var releaseRepository = "idyl-labs/purlview-releases"

// DefaultReleaseRepository returns the owner/name the checker consults.
func DefaultReleaseRepository() string { return releaseRepository }

// Environment overrides. EnvAPIBase points the checker at a different API
// origin (tests use a loopback server); EnvRepository overrides the
// repository. Neither changes what the checker does with the metadata.
// EnvNoCheck turns the check off (see Disabled).
const (
	EnvAPIBase    = "PURLVIEW_UPDATE_API_URL"
	EnvRepository = "PURLVIEW_RELEASE_REPOSITORY"
	EnvNoCheck    = "PURLVIEW_NO_UPDATE_CHECK"
)

// Disabled reports whether this environment asked for no update checks:
// PURLVIEW_NO_UPDATE_CHECK or the DO_NOT_TRACK convention set to anything
// but empty, 0 or false, or a CI system (CI set likewise), where nobody reads
// the notice and every job would otherwise ask GitHub.
func Disabled(getenv func(string) string) bool {
	on := func(name string) bool {
		v := strings.ToLower(strings.TrimSpace(getenv(name)))
		return v != "" && v != "0" && v != "false"
	}
	return on(EnvNoCheck) || on("DO_NOT_TRACK") || on("CI")
}

// Channels.
const (
	ChannelStable     = "stable"
	ChannelPrerelease = "prerelease"
)

// Outcomes recorded per check.
const (
	OutcomeOK          = "ok"
	OutcomeNotModified = "not_modified"
	OutcomeNoRelease   = "no_release"
	OutcomeRateLimited = "rate_limited"
	OutcomeNetwork     = "network"
	OutcomeMalformed   = "malformed"
)

// Limits.
const (
	DefaultTimeout = 5 * time.Second
	MaxBody        = 1 << 20
	// rateLimitBackoff is applied after a 403/429 without a reset header.
	rateLimitBackoff = 5 * time.Minute
	// networkBackoff avoids retrying an unreachable service on every share
	// in quick succession; it is short so an offline laptop that comes back
	// is noticed soon.
	networkBackoff = 30 * time.Second
	prereleasePage = 20
	// checkInterval spaces answered checks: releases are rare, and the notice
	// itself shows at most once per version per day.
	checkInterval = 24 * time.Hour
)

// Checker performs and caches metadata checks.
type Checker struct {
	Repository string
	APIBase    string
	CachePath  string
	Timeout    time.Duration
	UserAgent  string
	Logger     *log.Logger
	Client     *http.Client
	Now        func() time.Time

	mu       sync.Mutex
	loaded   bool
	cache    cacheFile
	inflight map[string]chan struct{}
}

type cacheFile struct {
	Channels map[string]*channelState `json:"channels"`
}

type channelState struct {
	ETag      string           `json:"etag,omitempty"`
	CheckedAt time.Time        `json:"checked_at"`
	Outcome   string           `json:"outcome"`
	Latest    *ipc.ReleaseInfo `json:"latest,omitempty"`
	NotBefore time.Time        `json:"not_before,omitzero"`
}

// Cached is the newest release the cache file at path holds for a channel,
// without asking for a check; nil when there is none or the file cannot be
// read. daemon status reads it so that reporting never adds a request.
func Cached(path, channel string) *ipc.ReleaseInfo {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil
	}
	var f cacheFile
	if err := json.Unmarshal(data, &f); err != nil || f.Channels[channel] == nil {
		return nil
	}
	return f.Channels[channel].Latest
}

// New builds a checker from the environment and cache path.
func New(getenv func(string) string, cachePath, userAgent string, logger *log.Logger) *Checker {
	c := &Checker{
		Repository: releaseRepository,
		APIBase:    "https://api.github.com",
		CachePath:  cachePath,
		Timeout:    DefaultTimeout,
		UserAgent:  userAgent,
		Logger:     logger,
		Now:        time.Now,
	}
	if v := getenv(EnvRepository); v != "" {
		c.Repository = v
	}
	if v := getenv(EnvAPIBase); v != "" {
		c.APIBase = strings.TrimRight(v, "/")
	}
	return c
}

func (c *Checker) logf(format string, args ...any) {
	if c.Logger != nil {
		c.Logger.Printf("update: "+format, args...)
	}
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Check runs or joins a check for channel and waits up to wait for it. It
// always returns promptly with the best information available.
func (c *Checker) Check(ctx context.Context, channel string, wait time.Duration) ipc.UpdateCheckResult {
	if channel != ChannelPrerelease {
		channel = ChannelStable
	}
	c.mu.Lock()
	c.loadLocked()
	if c.inflight == nil {
		c.inflight = map[string]chan struct{}{}
	}
	done, running := c.inflight[channel]
	if !running {
		st := c.cache.Channels[channel]
		if st != nil && c.now().Before(st.NotBefore) {
			c.logf("%s: backing off until %s after %s", channel, st.NotBefore.UTC().Format(time.RFC3339), st.Outcome)
			res := c.resultLocked(channel, "cached")
			c.mu.Unlock()
			return res
		}
		if st != nil && answered(st.Outcome) && c.now().Sub(st.CheckedAt) < checkInterval {
			res := c.resultLocked(channel, "cached")
			c.mu.Unlock()
			return res
		}
		done = make(chan struct{})
		c.inflight[channel] = done
		// The fetch deliberately outlives the request: a share that exits
		// early must not cancel it, and its result lands in the cache.
		go c.run(channel, done) //nolint:gosec // see above
	}
	c.mu.Unlock()

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-done:
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.resultLocked(channel, "fresh")
	case <-timer.C:
	case <-ctx.Done():
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.resultLocked(channel, "cached")
}

func (c *Checker) resultLocked(channel, status string) ipc.UpdateCheckResult {
	st := c.cache.Channels[channel]
	if st == nil {
		return ipc.UpdateCheckResult{Status: "none"}
	}
	res := ipc.UpdateCheckResult{Status: status, Outcome: st.Outcome, CheckedAt: st.CheckedAt.UTC().Format(time.RFC3339)}
	if st.Latest != nil {
		l := *st.Latest
		res.Latest = &l
	}
	return res
}

// run performs one fetch outside the lock and records the outcome.
func (c *Checker) run(channel string, done chan struct{}) {
	defer func() {
		c.mu.Lock()
		delete(c.inflight, channel)
		c.mu.Unlock()
		close(done)
	}()
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	c.mu.Lock()
	prev := c.cache.Channels[channel]
	etag := ""
	if prev != nil {
		etag = prev.ETag
	}
	c.mu.Unlock()

	next := c.fetch(ctx, channel, etag, prev)

	c.mu.Lock()
	if c.cache.Channels == nil {
		c.cache.Channels = map[string]*channelState{}
	}
	c.cache.Channels[channel] = next
	if err := c.saveLocked(); err != nil {
		c.logf("cache write failed: %v", err)
	}
	c.mu.Unlock()
	if next.Latest != nil {
		c.logf("%s: %s, latest %s", channel, next.Outcome, next.Latest.Version)
	} else {
		c.logf("%s: %s, no release known", channel, next.Outcome)
	}
}

// githubRelease is the subset of the releases API the checker reads.
type githubRelease struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
	HTMLURL    string `json:"html_url"`
}

func (c *Checker) fetch(ctx context.Context, channel, etag string, prev *channelState) *channelState {
	now := c.now()
	st := &channelState{CheckedAt: now}
	if prev != nil {
		st.Latest = prev.Latest // keep the last good answer through failures
		st.ETag = prev.ETag
	}
	var url string
	if channel == ChannelPrerelease {
		url = fmt.Sprintf("%s/repos/%s/releases?per_page=%d", c.APIBase, c.Repository, prereleasePage)
	} else {
		url = fmt.Sprintf("%s/repos/%s/releases/latest", c.APIBase, c.Repository)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		st.Outcome = OutcomeMalformed
		return st
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", c.UserAgent)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: c.Timeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		st.Outcome = OutcomeNetwork
		st.NotBefore = now.Add(networkBackoff)
		c.logf("%s: request failed: %v", channel, err)
		return st
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNotModified:
		st.Outcome = OutcomeNotModified
		return st
	case http.StatusNotFound:
		// No release published yet, or the repository is not public. Quiet.
		st.Outcome = OutcomeNoRelease
		st.Latest = nil
		st.ETag = ""
		return st
	case http.StatusForbidden, http.StatusTooManyRequests:
		st.Outcome = OutcomeRateLimited
		st.NotBefore = now.Add(rateLimitBackoff)
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset > 0 {
			if t := time.Unix(reset, 0); t.After(now) && t.Before(now.Add(time.Hour)) {
				st.NotBefore = t
			}
		}
		if ra, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && ra > 0 && ra < 3600 {
			st.NotBefore = now.Add(time.Duration(ra) * time.Second)
		}
		return st
	case http.StatusOK:
	default:
		st.Outcome = OutcomeNetwork
		st.NotBefore = now.Add(networkBackoff)
		return st
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		st.Outcome = OutcomeNetwork
		return st
	}
	if len(body) > MaxBody {
		st.Outcome = OutcomeMalformed
		return st
	}
	latest, ok := parseRelease(channel, body)
	if !ok {
		st.Outcome = OutcomeMalformed
		return st
	}
	st.Outcome = OutcomeOK
	st.Latest = latest
	st.ETag = resp.Header.Get("ETag")
	if latest == nil {
		st.Outcome = OutcomeNoRelease
	}
	return st
}

// parseRelease extracts the newest release for the channel. It returns
// ok=false for malformed metadata and latest=nil when the body is valid but
// lists no usable release.
func parseRelease(channel string, body []byte) (latest *ipc.ReleaseInfo, ok bool) {
	var releases []githubRelease
	if channel == ChannelPrerelease {
		if err := json.Unmarshal(body, &releases); err != nil {
			return nil, false
		}
	} else {
		var r githubRelease
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, false
		}
		releases = []githubRelease{r}
	}
	type cand struct {
		v    semver.Version
		info ipc.ReleaseInfo
	}
	var cands []cand
	for _, r := range releases {
		if r.Draft {
			continue
		}
		v, err := semver.Parse(r.TagName)
		if err != nil {
			continue
		}
		// The stable endpoint must not hand out a prerelease; a tag that
		// looks like one is inconsistent metadata and is ignored.
		if channel == ChannelStable && (r.Prerelease || v.IsPrerelease()) {
			continue
		}
		cands = append(cands, cand{v: v, info: ipc.ReleaseInfo{Version: v.String(), Tag: r.TagName, Prerelease: r.Prerelease || v.IsPrerelease(), URL: safeURL(r.HTMLURL)}})
	}
	if len(cands) == 0 {
		return nil, true
	}
	sort.Slice(cands, func(i, j int) bool { return semver.Compare(cands[i].v, cands[j].v) > 0 })
	best := cands[0].info
	return &best, true
}

// safeURL keeps only https links from metadata; anything else is dropped so
// a notice never points at an unexpected scheme.
func safeURL(u string) string {
	if strings.HasPrefix(u, "https://") && !strings.ContainsAny(u, " \t\r\n\"'<>") {
		return u
	}
	return ""
}

func (c *Checker) loadLocked() {
	if c.loaded {
		return
	}
	c.loaded = true
	c.cache.Channels = map[string]*channelState{}
	if c.CachePath == "" {
		return
	}
	data, err := os.ReadFile(c.CachePath)
	if err != nil {
		return
	}
	var f cacheFile
	if err := json.Unmarshal(data, &f); err != nil || f.Channels == nil {
		return
	}
	c.cache = f
}

func (c *Checker) saveLocked() error {
	if c.CachePath == "" {
		return nil
	}
	data, err := json.MarshalIndent(c.cache, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(c.CachePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".update-check.*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, c.CachePath); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// ErrDisabled reports that notices are disabled for this build.
var ErrDisabled = errors.New("update notices are disabled for development builds")

// answered reports an outcome that told us the latest release (or that there
// is none), as opposed to a failure that backs off on its own schedule.
func answered(outcome string) bool {
	return outcome == OutcomeOK || outcome == OutcomeNotModified || outcome == OutcomeNoRelease
}
