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

package main

import (
	"testing"

	"github.com/idyl-labs/purlview/internal/semver"
)

// The comparison itself is tested in internal/semver; this pins the cases
// the release scripts rely on so a change there cannot silently alter a
// channel-rollback decision.
func TestCompare(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.1.0", "0.1.0", 0},
		{"0.1.0", "0.1.1", -1},
		{"0.2.0", "0.1.9", 1},
		{"1.0.0", "0.9.9", 1},
		{"0.1.0-alpha.1", "0.1.0", -1},
		{"0.1.0", "0.1.0-rc.1", 1},
		{"0.1.0-alpha.1", "0.1.0-alpha.2", -1},
		{"0.1.0-alpha.2", "0.1.0-alpha.10", -1},
		{"0.1.0-alpha.1", "0.1.0-beta.1", -1},
		{"0.1.0-beta.1", "0.1.0-rc.1", -1},
		{"0.1.0-rc.1", "0.1.0-rc.1", 0},
		{"0.1.0-alpha", "0.1.0-alpha.1", -1},
		{"0.1.0+build.1", "0.1.0", 0},
	}
	for _, tc := range cases {
		got, err := semver.CompareStrings(tc.a, tc.b)
		if err != nil {
			t.Fatalf("compare %q %q: %v", tc.a, tc.b, err)
		}
		if got != tc.want {
			t.Errorf("compare(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "1", "1.2", "1.2.3.4", "01.2.3", "1.2.x", "1.2.3-", "1.2.3-a..b"} {
		if _, err := semver.Parse(s); err == nil {
			t.Errorf("parse(%q) should fail", s)
		}
	}
}
