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

package apiserver_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/apiserver"
)

// Share creation sees the CLI version its Purlview-Client header names;
// anything that isn't a semantic version is none.
func TestCreateSeesTheClientVersion(t *testing.T) {
	var got string
	b := &serviceStub{}
	b.CreateHook = func(ctx context.Context, _ string, _ api.CreateShareRequest) (api.ShareAccess, error) {
		got = apiserver.ClientVersion(ctx)
		return api.ShareAccess{}, &api.Error{Code: api.Unavailable, Outcome: api.NotApplied}
	}
	s := httptest.NewServer(apiserver.New("account.example.invalid", b))
	defer s.Close()
	for client, want := range map[string]string{
		"purlview/0.2.0": "0.2.0", "purlview/v0.2.1-rc.1": "0.2.1-rc.1", "": "", "purlview/devel": "",
		"other/9.9.9": "", "purlview/0.2.0-" + strings.Repeat("x", 64): "",
	} {
		got = "unset"
		req, _ := http.NewRequest("POST", s.URL+api.SharesPath, strings.NewReader(`{"target":"http://localhost:3000","ttl_nanoseconds":60000000000,"rewrite_urls":false}`))
		req.Host = "account.example.invalid"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer synthetic-installation-secret")
		req.Header.Set(api.IdempotencyHeader, "attempt")
		if client != "" {
			req.Header.Set(api.ClientHeader, client)
		}
		res, err := s.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if got != want {
			t.Errorf("%q: the service saw %q", client, got)
		}
	}
}

func TestRequestIDIsAssignedNotTaken(t *testing.T) {
	var seen string
	h := apiserver.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = apiserver.RequestIDFrom(r.Context())
		// An inner RequestID keeps the outer identifier.
		apiserver.RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			if apiserver.RequestIDFrom(r.Context()) != seen {
				t.Error("inner RequestID replaced the identifier")
			}
		})).ServeHTTP(w, r)
	}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set(api.RequestIDHeader, "chosen-by-client")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if seen == "" || seen == "chosen-by-client" || rec.Header().Get(api.RequestIDHeader) != seen {
		t.Fatalf("identifier %q, header %q", seen, rec.Header().Get(api.RequestIDHeader))
	}
}
