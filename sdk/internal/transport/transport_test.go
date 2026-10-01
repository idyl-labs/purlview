package transport

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/resource"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) {
	return 0, errors.New("synthetic-secret: truncated reply")
}

func TestBodiesClosedAndDiagnosticsDoNotReflectTransport(t *testing.T) {
	for _, kind := range []string{"valid", "malformed", "oversize", "read error", "redirect", "failure"} {
		t.Run(kind, func(t *testing.T) {
			var reader io.Reader = strings.NewReader(`{"account":"creator@example.invalid","account_id":"acc_1","device":"dev_1","device_label":"test"}`)
			status := 200
			switch kind {
			case "malformed":
				reader = strings.NewReader(`{`)
			case "oversize":
				reader = strings.NewReader(strings.Repeat("x", api.MaxResponseBytes+1))
			case "read error":
				reader = brokenReader{}
			case "redirect":
				status = 307
			case "failure":
				status = 501
				reader = strings.NewReader(`{"error":"not_implemented","message":"synthetic-secret","outcome":"not_applied","retryable":false}`)
			}
			body := &trackedBody{Reader: reader}
			c, err := New(Config{Endpoint: "https://account.example.invalid", Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.GetBody != nil {
					t.Error("request can be replayed")
				}
				headers := http.Header{}
				headers.Set("Content-Type", "application/json")
				headers.Set(api.VersionHeader, api.Version)
				return &http.Response{StatusCode: status, Header: headers, Body: body}, nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			var result resource.Identity
			err = c.Call(context.Background(), "GET", api.IdentityPath, "Bearer synthetic-secret", "", nil, &result, false)
			if !body.closed {
				t.Fatal("response body leaked")
			}
			if kind == "valid" && err != nil {
				t.Fatal(err)
			}
			if kind != "valid" && (err == nil || strings.Contains(err.Error(), "synthetic-secret")) {
				t.Fatalf("unexpected diagnostic %v", err)
			}
		})
	}
}
