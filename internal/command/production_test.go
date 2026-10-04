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

import "testing"

func TestProductionIsTheDefaultPlatform(t *testing.T) {
	get := withProductionDefaults(func(string) string { return "" })
	for key, want := range map[string]string{
		"PURLVIEW_ENVIRONMENT":       "production",
		"PURLVIEW_PLATFORM_ENDPOINT": "https://purlview.dev",
		"PURLVIEW_PLATFORM_HOST":     "",
		"PURLVIEW_CA_FILE":           "",
		"PURLVIEW_STATE_DIR":         "",
	} {
		if got := get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestAConfiguredPlatformTurnsEveryDefaultOff(t *testing.T) {
	env := map[string]string{"PURLVIEW_PLATFORM_ENDPOINT": "https://staging.example.invalid"}
	get := withProductionDefaults(func(k string) string { return env[k] })
	if get("PURLVIEW_PLATFORM_ENDPOINT") != "https://staging.example.invalid" || get("PURLVIEW_ENVIRONMENT") != "" {
		t.Fatal("a configured platform was mixed with production defaults")
	}
}

func TestNoneRunsWithoutAnyPlatform(t *testing.T) {
	get := withProductionDefaults(func(k string) string {
		if k == "PURLVIEW_PLATFORM_ENDPOINT" {
			return "none"
		}
		return ""
	})
	for _, key := range []string{"PURLVIEW_PLATFORM_ENDPOINT", "PURLVIEW_ENVIRONMENT"} {
		if v := get(key); v != "" {
			t.Errorf("%s = %q with no platform", key, v)
		}
	}
}

func TestExplicitSettingsWinOverProductionDefaults(t *testing.T) {
	env := map[string]string{"PURLVIEW_CONNECT_IP": "127.0.0.1", "PURLVIEW_ENVIRONMENT": "staging"}
	get := withProductionDefaults(func(k string) string { return env[k] })
	if get("PURLVIEW_CONNECT_IP") != "127.0.0.1" || get("PURLVIEW_ENVIRONMENT") != "staging" || get("PURLVIEW_PLATFORM_ENDPOINT") != "https://purlview.dev" {
		t.Fatal("explicit settings did not win")
	}
}

func TestProductionDefaultsReadNothingUntilAsked(t *testing.T) {
	reads := 0
	withProductionDefaults(func(string) string { reads++; return "" })
	if reads != 0 {
		t.Fatalf("wrapping read the environment %d times", reads)
	}
}
