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
