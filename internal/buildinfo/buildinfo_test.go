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

package buildinfo

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestBuildUsesLinkerValuesFirst(t *testing.T) {
	t.Parallel()
	read := func() (*debug.BuildInfo, bool) {
		t.Fatal("embedded build info must not be consulted for release builds")
		return nil, false
	}
	got := build("v1.2.3", "abc123", "2026-09-16T00:00:00Z", "goreleaser", read)
	if got.Version != "1.2.3" || got.Commit != "abc123" || got.CommitDate != "2026-09-16T00:00:00Z" || got.BuiltBy != "goreleaser" {
		t.Fatalf("unexpected info: %+v", got)
	}
	if got.Modified {
		t.Fatalf("release builds must not report a modified tree: %+v", got)
	}
}

func TestBuildFallsBackToEmbeddedInfo(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		bi      *debug.BuildInfo
		ok      bool
		version string
		commit  string
		builtBy string
		modded  bool
	}{
		{
			name:    "no build info",
			ok:      false,
			version: DevelVersion,
			commit:  "unknown",
			builtBy: "source",
		},
		{
			name: "source checkout stamped by Go 1.24+",
			bi: &debug.BuildInfo{
				Main: debug.Module{Version: "v0.0.0-20260916010203-deadbeefcafe+dirty"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "deadbeefcafe"},
					{Key: "vcs.time", Value: "2026-09-16T01:02:03Z"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			ok:      true,
			version: "0.0.0-20260916010203-deadbeefcafe+dirty",
			commit:  "deadbeefcafe",
			builtBy: "source",
			modded:  true,
		},
		{
			name: "source checkout at an exact tag",
			bi: &debug.BuildInfo{
				Main: debug.Module{Version: "v0.3.0"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "0123abcd"},
					{Key: "vcs.modified", Value: "false"},
				},
			},
			ok:      true,
			version: "0.3.0",
			commit:  "0123abcd",
			builtBy: "source",
		},
		{
			name: "source checkout without version stamping",
			bi: &debug.BuildInfo{
				Main: debug.Module{Version: "(devel)"},
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: "deadbeef"},
					{Key: "vcs.time", Value: "2026-09-16T01:02:03Z"},
					{Key: "vcs.modified", Value: "true"},
				},
			},
			ok:      true,
			version: DevelVersion,
			commit:  "deadbeef",
			builtBy: "source",
			modded:  true,
		},
		{
			name: "go install of a tagged module",
			bi: &debug.BuildInfo{
				Main: debug.Module{Version: "v0.4.0"},
			},
			ok:      true,
			version: "0.4.0",
			commit:  "unknown",
			builtBy: "go install",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := build("", "", "", "", func() (*debug.BuildInfo, bool) { return tc.bi, tc.ok })
			if got.Version != tc.version || got.Commit != tc.commit || got.BuiltBy != tc.builtBy || got.Modified != tc.modded {
				t.Fatalf("got %+v, want version=%q commit=%q builtBy=%q modified=%v", got, tc.version, tc.commit, tc.builtBy, tc.modded)
			}
			if got.GoVersion == "" || got.OS == "" || got.Arch == "" {
				t.Fatalf("toolchain identity missing: %+v", got)
			}
		})
	}
}

func TestStringLeadsWithNameAndVersion(t *testing.T) {
	t.Parallel()
	info := Info{Version: "0.1.0-alpha.1", Commit: "abc", CommitDate: "2026-09-16T00:00:00Z", BuiltBy: "goreleaser", GoVersion: "go1.27.1", OS: "linux", Arch: "arm64", Modified: true}
	out := info.String()
	lines := strings.Split(out, "\n")
	if lines[0] != "purlview 0.1.0-alpha.1" {
		t.Fatalf("first line %q must be the name and version", lines[0])
	}
	for _, want := range []string{"abc", "modified working tree", "2026-09-16T00:00:00Z", "go1.27.1 linux/arm64", "goreleaser"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.HasSuffix(out, "\n") {
		t.Fatalf("String must not end with a newline; callers add it")
	}
}
