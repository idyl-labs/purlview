package daemon

import (
	"path/filepath"
	"testing"
)

func TestProductionKeepsThePlainPathsAndOtherPlatformsTheirOwn(t *testing.T) {
	root := t.TempDir()
	resolve := func(env map[string]string) Paths {
		t.Helper()
		env[EnvStateDir] = root
		p, err := Resolve(func(k string) string { return env[k] })
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	plain := resolve(map[string]string{})
	prod := resolve(map[string]string{"PURLVIEW_PLATFORM_ENDPOINT": "https://purlview.dev", "PURLVIEW_ENVIRONMENT": "production"})
	if prod.Logs != plain.Logs || prod.Runtime != plain.Runtime || prod.Cache != plain.Cache || prod.Logs != filepath.Join(root, "logs") {
		t.Fatalf("production moved its files: %+v, want %+v", prod, plain)
	}
	staging := resolve(map[string]string{"PURLVIEW_PLATFORM_ENDPOINT": "https://staging.example.invalid", "PURLVIEW_ENVIRONMENT": "staging"})
	if filepath.Dir(staging.Logs) != plain.Logs || filepath.Dir(staging.Runtime) != plain.Runtime || staging.Endpoint == prod.Endpoint {
		t.Fatalf("another platform must have its own directories: %+v", staging)
	}
}
