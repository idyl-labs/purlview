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

package semver

import "testing"

func TestCompareOrdersPerSpec(t *testing.T) {
	t.Parallel()
	ordered := []string{"0.0.1", "0.1.0-alpha.1", "0.1.0-alpha.2", "0.1.0-alpha.10", "0.1.0-alpha.beta", "0.1.0-beta.1", "0.1.0-rc.1", "0.1.0", "0.1.1", "0.2.0", "1.0.0", "v1.0.1+build.5"}
	for i := range ordered {
		for j := range ordered {
			got, err := CompareStrings(ordered[i], ordered[j])
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
}

func TestParseRejectsMalformedVersions(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "1", "1.2", "1.2.3.4", "01.2.3", "1.2.-3", "1.2.3-", "1.2.3-a..b", "latest", "1.2.x"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("Parse(%q) accepted a malformed version", s)
		}
	}
	v, err := Parse("v1.2.3-rc.1+meta")
	if err != nil || v.String() != "1.2.3-rc.1" || !v.IsPrerelease() {
		t.Fatalf("Parse: %+v %v", v, err)
	}
}
