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

package daemon

import (
	"context"
	"net"
	"slices"
	"testing"
	"time"
)

// TestProbeReachesLocalhostOnEitherLoopback: "localhost" is tried on every
// loopback address the system maps it to, so a dev server bound to only
// IPv4 or only IPv6 is found either way.
func TestProbeReachesLocalhostOnEitherLoopback(t *testing.T) {
	t.Parallel()
	mapped, err := net.LookupHost("localhost")
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"127.0.0.1", "::1"} {
		t.Run(host, func(t *testing.T) {
			t.Parallel()
			if !slices.Contains(mapped, host) {
				t.Skipf("this system does not map localhost to %s (%v)", host, mapped)
			}
			l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
			if err != nil {
				t.Skipf("cannot listen on %s: %v", host, err)
			}
			defer func() { _ = l.Close() }()
			_, port, _ := net.SplitHostPort(l.Addr().String())
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := (prober{}).Probe(ctx, "http://localhost:"+port); err != nil {
				t.Fatalf("localhost:%s bound on %s only: %v", port, host, err)
			}
		})
	}
	// Nothing listening is reported, not masked.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := (prober{}).Probe(ctx, "http://"+addr); err == nil {
		t.Fatal("a closed port must not probe as answering")
	}
}
