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

package command

import "github.com/idyl-labs/purlview/internal/production"

// productionPlatform is where an installed CLI and its daemon connect when no
// platform is configured. People who install Purlview never set anything.
var productionPlatform = map[string]string{
	"PURLVIEW_ENVIRONMENT":       production.Environment,
	"PURLVIEW_PLATFORM_ENDPOINT": production.Endpoint,
}

// noPlatform, as PURLVIEW_PLATFORM_ENDPOINT, runs with no platform at all: the
// behaviour before this default existed. Tests of the built executable use it
// so that nothing they run can reach production.
const noPlatform = "none"

// withProductionDefaults fills in the production platform for the process
// environment. Setting PURLVIEW_PLATFORM_ENDPOINT (another deployment, a local stack,
// or none) turns every default off, so a configured environment is never
// mixed with production. Values are read only when a command asks for them,
// which keeps offline commands from touching the environment.
func withProductionDefaults(getenv func(string) string) func(string) string {
	return func(key string) string {
		if key == "PURLVIEW_PLATFORM_ENDPOINT" && getenv(key) == noPlatform {
			return ""
		}
		if v := getenv(key); v != "" {
			return v
		}
		if v, ok := productionPlatform[key]; ok && getenv("PURLVIEW_PLATFORM_ENDPOINT") == "" {
			return v
		}
		return ""
	}
}
