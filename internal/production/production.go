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

// Package production names the platform an installed CLI and its daemon use
// when no platform is configured.
package production

// The production platform: its environment name and account API. The edge
// a share docks on comes with the share from the platform.
const (
	Environment = "production"
	Endpoint    = "https://purlview.dev"
)

// Is reports whether the resolved settings name the production platform, as
// the defaults do. Its daemon files keep the plain paths people are pointed
// to; any other platform gets a directory of its own beside them.
func Is(getenv func(string) string) bool {
	return getenv("PURLVIEW_PLATFORM_ENDPOINT") == Endpoint &&
		getenv("PURLVIEW_ENVIRONMENT") == Environment &&
		getenv("PURLVIEW_PLATFORM_HOST") == ""
}
