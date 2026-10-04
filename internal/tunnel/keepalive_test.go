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
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The proxy lets go of an idle connection before the quickest development
// server would close it: gunicorn, after 2s.
func TestIdleAppConnectionsAreReleasedBeforeTheAppClosesThem(t *testing.T) {
	app, _ := url.Parse("http://localhost:3000")
	p := testProxy(origin{}, app, newAppWatch("localhost:3000", nil)).(*proxy)
	idle := p.targets[0].rp.Transport.(*http.Transport).IdleConnTimeout
	if idle <= 0 || idle >= 2*time.Second {
		t.Fatalf("idle connections are kept for %v", idle)
	}
}

// A visitor's POST reaches an app that closes idle keep-alive connections
// around the moment the proxy would reuse one. Go retries only requests it can
// replay, so with an idle timeout of the app's or longer the proxy answers
// some of these POSTs with the unresponsive 502 (measured: 3 to 6 of 93 at a
// 200ms app timeout). The timings here are the production ones scaled down.
func TestPostAtTheAppsIdleCloseIsAnswered(t *testing.T) {
	const appIdle = 50 * time.Millisecond
	app := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	app.Config.IdleTimeout = appIdle
	app.Start()
	defer app.Close()
	target, _ := url.Parse(app.URL)
	p := testProxy(origin{}, target, newAppWatch("localhost:3000", nil)).(*proxy)
	p.targets[0].rp.Transport.(*http.Transport).IdleConnTimeout = appIdle / 2
	post := func() int {
		req := httptest.NewRequest("POST", "https://k7m2p4qx.purlview.invalid/api", strings.NewReader("{}"))
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec.Code
	}
	var failed, sent int
	for d := appIdle - 10*time.Millisecond; d <= appIdle+10*time.Millisecond; d += time.Millisecond / 2 {
		post()
		time.Sleep(d)
		sent++
		if post() != http.StatusOK {
			failed++
		}
	}
	if failed != 0 {
		t.Fatalf("%d/%d POSTs at the app's idle close were not answered", failed, sent)
	}
}
