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

// Package tlsconfig loads explicit deployment trust without disabling verification.
package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
)

// Client holds explicit endpoint and certificate trust for serving connections.
func Client(ca string) (*tls.Config, error) {
	roots, e := x509.SystemCertPool()
	if e != nil {
		roots = x509.NewCertPool()
	}
	if ca != "" {
		b, err := os.ReadFile(ca) //nolint:gosec // explicit application-owned CA path
		if err != nil || !roots.AppendCertsFromPEM(b) {
			return nil, errors.New("cannot load Purlview CA")
		}
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots}, nil
}
