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

package updatecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idyl-labs/purlview/internal/ipc"
)

// fakeAPI mimics the two GitHub releases endpoints the checker uses.
type fakeAPI struct {
	*httptest.Server
	mu       sync.Mutex
	latest   *githubRelease // nil: 404
	releases []githubRelease
	etag     string
	status   int // when non-zero, every response uses this status
	body     string
	delay    time.Duration
	headers  map[string]string
	requests atomic.Int64
	seen     []string
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{etag: `"v1"`}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		f.mu.Lock()
		f.seen = append(f.seen, r.URL.String())
		latest, releases, etag, status, body, delay, headers := f.latest, f.releases, f.etag, f.status, f.body, f.delay, f.headers
		f.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		if r.Header.Get("If-None-Match") == etag && etag != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/latest"):
			if latest == nil {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(latest)
		case strings.HasSuffix(r.URL.Path, "/releases"):
			_ = json.NewEncoder(w).Encode(releases)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPI) set(fn func(*fakeAPI)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func rel(tag string, pre bool) githubRelease {
	return githubRelease{TagName: tag, Prerelease: pre, HTMLURL: "https://example.test/releases/tag/" + tag}
}

func newChecker(t *testing.T, api *fakeAPI) *Checker {
	t.Helper()
	env := map[string]string{EnvAPIBase: api.URL, EnvRepository: "acme/tool"}
	c := New(func(k string) string { return env[k] }, filepath.Join(t.TempDir(), "cache", "update-check.json"), "purlview/test", nil)
	c.Timeout = 2 * time.Second
	return c
}

func TestStableChannelFindsLatestAndCachesWithETag(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	api.set(func(f *fakeAPI) { r := rel("v0.9.1", false); f.latest = &r })
	c := newChecker(t, api)

	res := c.Check(context.Background(), ChannelStable, 2*time.Second)
	if res.Status != "fresh" || res.Outcome != OutcomeOK || res.Latest == nil || res.Latest.Version != "0.9.1" || res.Latest.URL == "" {
		t.Fatalf("first check: %+v", res)
	}
	// Within a day the answer is reused without asking again.
	if res = c.Check(context.Background(), ChannelStable, 2*time.Second); res.Status != "cached" || api.requests.Load() != 1 {
		t.Fatalf("within a day: %+v, %d requests", res, api.requests.Load())
	}
	// A day later the check sends the ETag and gets 304: the release is kept.
	later := time.Now().Add(checkInterval + time.Minute)
	c.Now = func() time.Time { return later }
	res = c.Check(context.Background(), ChannelStable, 2*time.Second)
	if res.Outcome != OutcomeNotModified || res.Latest == nil || res.Latest.Version != "0.9.1" {
		t.Fatalf("conditional check: %+v", res)
	}
	if n := api.requests.Load(); n != 2 {
		t.Fatalf("requests=%d, want 2", n)
	}
	data, err := os.ReadFile(c.CachePath)
	if err != nil || !strings.Contains(string(data), `"etag": "\"v1\""`) {
		t.Fatalf("cache file: %v\n%s", err, data)
	}
	if st, _ := os.Stat(c.CachePath); runtime.GOOS != "windows" && st.Mode().Perm()&0o077 != 0 {
		// Windows privacy comes from the inherited per-user ACL, which Go's
		// mode bits do not show.
		t.Fatalf("cache must be private, mode %o", st.Mode().Perm())
	}
	// A new checker (a new daemon) reuses the cache and its ETag.
	c2 := newChecker(t, api)
	c2.CachePath = c.CachePath
	evenLater := later.Add(checkInterval + time.Minute)
	c2.Now = func() time.Time { return evenLater }
	res = c2.Check(context.Background(), ChannelStable, 2*time.Second)
	if res.Outcome != OutcomeNotModified || res.Latest.Version != "0.9.1" {
		t.Fatalf("reloaded cache: %+v", res)
	}
}

func TestPrereleaseChannelPicksHighestSemVerIgnoringDrafts(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	api.set(func(f *fakeAPI) {
		f.releases = []githubRelease{rel("v0.9.0", false), rel("v0.10.0-rc.2", true), rel("v0.10.0-rc.10", true), {TagName: "v9.9.9", Draft: true}, rel("not-a-version", true)}
	})
	c := newChecker(t, api)
	res := c.Check(context.Background(), ChannelPrerelease, 2*time.Second)
	if res.Latest == nil || res.Latest.Version != "0.10.0-rc.10" || !res.Latest.Prerelease {
		t.Fatalf("prerelease channel: %+v", res)
	}
	// The stable channel never surfaces a prerelease even if the metadata
	// service hands one out.
	api.set(func(f *fakeAPI) { r := rel("v1.0.0-beta.1", true); f.latest = &r; f.etag = `"v2"` })
	res = c.Check(context.Background(), ChannelStable, 2*time.Second) // its first stable check
	if res.Latest != nil || res.Outcome != OutcomeNoRelease {
		t.Fatalf("stable must ignore a prerelease: %+v", res)
	}
}

func TestFailuresAreSilentAndKeepTheLastGoodAnswer(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	api.set(func(f *fakeAPI) { r := rel("v0.9.1", false); f.latest = &r })
	c := newChecker(t, api)
	if res := c.Check(context.Background(), ChannelStable, 2*time.Second); res.Latest == nil {
		t.Fatalf("seed: %+v", res)
	}
	cases := []struct {
		name    string
		setup   func(*fakeAPI)
		outcome string
		backoff bool
	}{
		{"rate limited 429", func(f *fakeAPI) { f.status = 429; f.headers = map[string]string{"Retry-After": "120"} }, OutcomeRateLimited, true},
		{"rate limited 403", func(f *fakeAPI) {
			f.status = 403
			f.headers = map[string]string{"X-RateLimit-Reset": fmt.Sprint(time.Now().Add(90 * time.Second).Unix())}
		}, OutcomeRateLimited, true},
		{"server error", func(f *fakeAPI) { f.status = 500 }, OutcomeNetwork, true},
		{"malformed json", func(f *fakeAPI) { f.status = 200; f.body = "{not json" }, OutcomeMalformed, false},
		{"oversized body", func(f *fakeAPI) {
			f.status = 200
			f.body = `{"tag_name":"v9.0.0","x":"` + strings.Repeat("a", MaxBody) + `"}`
		}, OutcomeMalformed, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api.set(func(f *fakeAPI) { f.status, f.body, f.headers = 0, "", nil; tc.setup(f) })
			c.mu.Lock()
			c.cache.Channels[ChannelStable].NotBefore = time.Time{}
			c.cache.Channels[ChannelStable].CheckedAt = time.Time{} // a day or more ago
			c.mu.Unlock()
			res := c.Check(context.Background(), ChannelStable, 2*time.Second)
			if res.Status != "fresh" || res.Outcome != tc.outcome {
				t.Fatalf("status=%s outcome=%s, want fresh/%s", res.Status, res.Outcome, tc.outcome)
			}
			if res.Latest == nil || res.Latest.Version != "0.9.1" {
				t.Fatalf("last good answer lost: %+v", res)
			}
			c.mu.Lock()
			nb := c.cache.Channels[ChannelStable].NotBefore
			c.mu.Unlock()
			if tc.backoff && !nb.After(time.Now()) {
				t.Fatal("expected a back-off after this failure")
			}
			if !tc.backoff && nb.After(time.Now()) {
				t.Fatal("unexpected back-off")
			}
		})
	}
	// While backing off no request is made and the cached answer is served.
	before := api.requests.Load()
	api.set(func(f *fakeAPI) { f.status = 429 })
	c.mu.Lock()
	c.cache.Channels[ChannelStable].NotBefore = time.Now().Add(time.Minute)
	c.mu.Unlock()
	res := c.Check(context.Background(), ChannelStable, time.Second)
	if res.Status != "cached" || res.Latest == nil || api.requests.Load() != before {
		t.Fatalf("back-off must not request: %+v requests=%d", res, api.requests.Load()-before)
	}
}

func TestNoReleaseAndOfflineAreQuiet(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	c := newChecker(t, api)
	res := c.Check(context.Background(), ChannelStable, 2*time.Second)
	if res.Outcome != OutcomeNoRelease || res.Latest != nil {
		t.Fatalf("404 must mean no release: %+v", res)
	}
	// Offline: a closed port.
	dead := httptest.NewServer(http.NotFoundHandler())
	url := dead.URL
	dead.Close()
	env := map[string]string{EnvAPIBase: url}
	c2 := New(func(k string) string { return env[k] }, filepath.Join(t.TempDir(), "c.json"), "purlview/test", nil)
	c2.Timeout = time.Second
	res = c2.Check(context.Background(), ChannelStable, 2*time.Second)
	if res.Outcome != OutcomeNetwork || res.Latest != nil {
		t.Fatalf("offline: %+v", res)
	}
}

func TestSlowServerDoesNotBlockAndFinishesInBackground(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	api.set(func(f *fakeAPI) { r := rel("v2.0.0", false); f.latest = &r; f.delay = 300 * time.Millisecond })
	c := newChecker(t, api)
	start := time.Now()
	res := c.Check(context.Background(), ChannelStable, 50*time.Millisecond)
	if el := time.Since(start); el > 250*time.Millisecond {
		t.Fatalf("Check waited %s, want about 50ms", el)
	}
	if res.Status != "none" {
		t.Fatalf("nothing cached yet: %+v", res)
	}
	// The in-flight check completes on its own and later calls see it.
	deadline := time.Now().Add(3 * time.Second)
	for {
		res = c.Check(context.Background(), ChannelStable, 100*time.Millisecond)
		if res.Latest != nil && res.Latest.Version == "2.0.0" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background check never completed: %+v", res)
		}
	}
	// A request that times out is reported as a network failure, quietly.
	api.set(func(f *fakeAPI) { f.delay = 2 * time.Second; f.etag = `"v3"` })
	c.Timeout = 200 * time.Millisecond
	c.mu.Lock()
	c.cache.Channels[ChannelStable].NotBefore = time.Time{}
	c.cache.Channels[ChannelStable].CheckedAt = time.Time{} // a day or more ago
	c.mu.Unlock()
	res = c.Check(context.Background(), ChannelStable, 2*time.Second)
	if res.Outcome != OutcomeNetwork || res.Latest == nil || res.Latest.Version != "2.0.0" {
		t.Fatalf("timeout: %+v", res)
	}
}

func TestConcurrentChecksShareOneRequest(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	api.set(func(f *fakeAPI) { r := rel("v0.9.1", false); f.latest = &r; f.delay = 100 * time.Millisecond })
	c := newChecker(t, api)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res := c.Check(context.Background(), ChannelStable, 2*time.Second)
			if res.Latest == nil || res.Latest.Version != "0.9.1" {
				t.Errorf("concurrent check: %+v", res)
			}
		}()
	}
	wg.Wait()
	if n := api.requests.Load(); n != 1 {
		t.Fatalf("requests=%d, want 1", n)
	}
}

func TestRequestCarriesNoSecretsAndOnlyMetadataPaths(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	api.set(func(f *fakeAPI) { r := rel("v0.9.1", false); f.latest = &r })
	c := newChecker(t, api)
	c.Check(context.Background(), ChannelStable, 2*time.Second)
	c.Check(context.Background(), ChannelPrerelease, 2*time.Second)
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, u := range api.seen {
		if !strings.HasPrefix(u, "/repos/acme/tool/releases") {
			t.Fatalf("unexpected request %s", u)
		}
		if strings.Contains(u, "download") {
			t.Fatalf("checker must never fetch assets: %s", u)
		}
	}
	var r ipc.ReleaseInfo
	if safeURL("http://insecure.example") != "" || safeURL("https://ok.example/x") != "https://ok.example/x" || r.URL != "" {
		t.Fatal("url sanitising")
	}
}

func TestModuleReleasesNeverBecomeClientUpdates(t *testing.T) {
	for _, tag := range []string{"sdk/v99.0.0", "tools/v99.0.0", "cmd/purlview/v99.0.0", "nested/module/v99.0.0"} {
		body := []byte(`{"tag_name":"` + tag + `","html_url":"https://github.com/idyl-labs/purlview-releases/releases/tag/ignored"}`)
		if latest, ok := parseRelease(ChannelStable, body); !ok || latest != nil {
			t.Fatalf("module tag %s selected for stable update: %+v", tag, latest)
		}
		list := append([]byte("["), body...)
		list = append(list, []byte(`,{"tag_name":"v0.2.0-beta.1"}]`)...)
		if latest, ok := parseRelease(ChannelPrerelease, list); !ok || latest == nil || latest.Tag != "v0.2.0-beta.1" {
			t.Fatalf("module tag %s influenced prerelease update: %+v", tag, latest)
		}
	}
}

func TestAnsweredChecksAreReusedForADayButFailuresRetrySooner(t *testing.T) {
	t.Parallel()
	api := newFakeAPI(t)
	api.set(func(f *fakeAPI) { f.status = 500 })
	c := newChecker(t, api)
	now := time.Now()
	c.Now = func() time.Time { return now }
	if res := c.Check(context.Background(), ChannelStable, 2*time.Second); res.Outcome != OutcomeNetwork {
		t.Fatalf("failure: %+v", res)
	}
	// A failure is retried once its short back-off passes, not a day later.
	now = now.Add(networkBackoff + time.Second)
	api.set(func(f *fakeAPI) { f.status = 0; r := rel("v0.9.1", false); f.latest = &r })
	if res := c.Check(context.Background(), ChannelStable, 2*time.Second); res.Outcome != OutcomeOK || api.requests.Load() != 2 {
		t.Fatalf("retry after back-off: %+v, %d requests", res, api.requests.Load())
	}
	for range 5 {
		now = now.Add(time.Hour)
		if res := c.Check(context.Background(), ChannelStable, 2*time.Second); res.Status != "cached" || res.Latest == nil || res.Latest.Version != "0.9.1" {
			t.Fatalf("within the day: %+v", res)
		}
	}
	if n := api.requests.Load(); n != 2 {
		t.Fatalf("%d requests within a day of an answer, want 2", n)
	}
}

func TestDisabledByOptOutDoNotTrackOrCI(t *testing.T) {
	t.Parallel()
	for env, want := range map[string]bool{
		"":                             false,
		"PURLVIEW_NO_UPDATE_CHECK=1":   true,
		"PURLVIEW_NO_UPDATE_CHECK=0":   false,
		"DO_NOT_TRACK=1":               true,
		"DO_NOT_TRACK=false":           false,
		"CI=true":                      true,
		"CI=1":                         true,
		"CI=false":                     false,
		"CI= ":                         false,
		"PURLVIEW_NO_UPDATE_CHECK=yes": true,
	} {
		name, value, _ := strings.Cut(env, "=")
		got := Disabled(func(k string) string {
			if k == name {
				return value
			}
			return ""
		})
		if got != want {
			t.Errorf("%q: %v, want %v", env, got, want)
		}
	}
}
