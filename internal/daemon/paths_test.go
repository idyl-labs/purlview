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
