package apiserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/idyl-labs/purlview/sdk/api"
)

type requestIDKey struct{}

// RequestID assigns every request a fresh identifier, sets it as the
// api.RequestIDHeader response header and makes it available to RequestIDFrom.
// A client's own X-Request-Id is ignored, so that identifiers in responses and
// logs cannot be chosen by callers. A request that already has an identifier
// from an outer RequestID keeps it.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if RequestIDFrom(r.Context()) != "" {
			next.ServeHTTP(w, r)
			return
		}
		id := newRequestID()
		w.Header().Set(api.RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id)))
	})
}

// RequestIDFrom returns the identifier RequestID assigned, or "".
func RequestIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A time-based identifier keeps requests distinguishable if the
		// system's random source ever fails.
		return "t" + hex.EncodeToString([]byte(time.Now().Format(time.RFC3339Nano)))[:23]
	}
	return hex.EncodeToString(b[:])
}

type clientKey struct{}

// ClientVersion is the version of the CLI a share creation came from, as its
// Purlview-Client header names it: "purlview/v0.2.0" gives "0.2.0". It is ""
// when the header names no semantic version.
func ClientVersion(ctx context.Context) string {
	v, _ := ctx.Value(clientKey{}).(string)
	return v
}

// WithClient returns ctx carrying the version a Purlview-Client header
// value names, for ClientVersion. A value that names no semantic version, or
// one longer than 64 bytes, leaves ctx unchanged.
func WithClient(ctx context.Context, header string) context.Context {
	v := strings.TrimPrefix(strings.TrimPrefix(header, "purlview/"), "v")
	if len(v) > 64 || !validVersion(v) {
		return ctx
	}
	return context.WithValue(ctx, clientKey{}, v)
}

// validVersion reports whether s is MAJOR.MINOR.PATCH[-pre][+build] as
// Semantic Versioning 2.0.0 defines the core and prerelease identifiers.
func validVersion(s string) bool {
	s, _, _ = strings.Cut(s, "+")
	core, pre, hasPre := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || (len(p) > 1 && p[0] == '0') || strings.ContainsAny(p, "+-") {
			return false
		}
	}
	if hasPre {
		for id := range strings.SplitSeq(pre, ".") {
			if id == "" {
				return false
			}
		}
	}
	return true
}
