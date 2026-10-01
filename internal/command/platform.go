package command

import (
	"sync"

	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/tlsconfig"

	"github.com/idyl-labs/purlview/sdk/api"
	"github.com/idyl-labs/purlview/sdk/purlview"
)

// platformClient resolves application configuration on first product operation.
// There is deliberately no public endpoint default, discovery or TLS bypass.
func platformClient(getenv func(string) string) func() (*purlview.Client, error) {
	var once sync.Once
	var client *purlview.Client
	var err error
	return func() (*purlview.Client, error) {
		once.Do(func() {
			endpoint := getenv("PURLVIEW_PLATFORM_ENDPOINT")
			if endpoint == "" {
				err = api.ErrNotImplemented
				return
			}
			tlsConf, e := tlsconfig.Transport(getenv("PURLVIEW_CA_FILE"), getenv("PURLVIEW_CONNECT_IP"))
			if e != nil {
				err = e
				return
			}
			client, err = purlview.New(purlview.Config{Endpoint: endpoint, Transport: tlsConf, AllowHTTP: getenv("PURLVIEW_PLATFORM_ALLOW_HTTP") == "1", Host: getenv("PURLVIEW_PLATFORM_HOST"), Client: "purlview/" + buildinfo.Current().Version})
		})
		return client, err
	}
}
