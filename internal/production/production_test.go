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

package production

import "testing"

func TestIs(t *testing.T) {
	env := func(pairs ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(pairs); i += 2 {
			m[pairs[i]] = pairs[i+1]
		}
		return func(k string) string { return m[k] }
	}
	if !Is(env("PURLVIEW_PLATFORM_ENDPOINT", Endpoint, "PURLVIEW_ENVIRONMENT", Environment)) {
		t.Fatal("the defaults are production")
	}
	for name, get := range map[string]func(string) string{
		"nothing":        env(),
		"staging":        env("PURLVIEW_PLATFORM_ENDPOINT", "https://staging.example.invalid", "PURLVIEW_ENVIRONMENT", "staging"),
		"no environment": env("PURLVIEW_PLATFORM_ENDPOINT", Endpoint),
		"host override":  env("PURLVIEW_PLATFORM_ENDPOINT", Endpoint, "PURLVIEW_ENVIRONMENT", Environment, "PURLVIEW_PLATFORM_HOST", "x.invalid"),
	} {
		if Is(get) {
			t.Errorf("%s counted as production", name)
		}
	}
}
