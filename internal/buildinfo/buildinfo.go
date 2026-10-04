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

// Package buildinfo reports the version and build identity of the executable.
//
// Release builds receive their identity from the linker (-X flags).
// Source builds and `go install` builds fall back to the module and VCS data
// that the Go toolchain embeds, and identify themselves as development builds.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

// Set by the release linker with -X. Empty in source builds.
var (
	version string
	commit  string
	date    string
	builtBy string
)

// DevelVersion is reported when no release version is known.
const DevelVersion = "devel"

// Info describes the executable that is running.
type Info struct {
	// Version is the release version without a leading "v", or DevelVersion.
	Version string
	// Commit is the full source revision, or "unknown".
	Commit string
	// CommitDate is the commit or build time in RFC 3339 form, when known.
	CommitDate string
	// BuiltBy names the producer: "goreleaser", "go install" or "source".
	BuiltBy string
	// Modified reports a build from a working tree with uncommitted changes.
	Modified bool
	// GoVersion, OS and Arch describe the toolchain and target.
	GoVersion string
	OS        string
	Arch      string
}

// Current returns the identity of the running executable.
func Current() Info {
	return build(version, commit, date, builtBy, debug.ReadBuildInfo)
}

// build derives Info from linker values, falling back to embedded build data.
func build(ldVersion, ldCommit, ldDate, ldBuiltBy string, read func() (*debug.BuildInfo, bool)) Info {
	info := Info{
		Version:   DevelVersion,
		Commit:    "unknown",
		BuiltBy:   "source",
		GoVersion: runtime.Version(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
	}

	if ldVersion != "" {
		info.Version = strings.TrimPrefix(ldVersion, "v")
		if ldCommit != "" {
			info.Commit = ldCommit
		}
		info.CommitDate = ldDate
		info.BuiltBy = ldBuiltBy
		if info.BuiltBy == "" {
			info.BuiltBy = "unknown"
		}
		return info
	}

	bi, ok := read()
	if !ok || bi == nil {
		return info
	}
	// Since Go 1.24 a build from a checkout carries the version derived from
	// git (a tag, or a pseudo-version with the commit, plus "+dirty" for a
	// modified tree) together with vcs.* settings. A `go install pkg@version`
	// build carries the module version but no vcs settings.
	fromVCS := false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if s.Value != "" {
				info.Commit = s.Value
				fromVCS = true
			}
		case "vcs.time":
			info.CommitDate = s.Value
		case "vcs.modified":
			info.Modified = s.Value == "true"
		}
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		info.Version = strings.TrimPrefix(v, "v")
		if !fromVCS {
			info.BuiltBy = "go install"
		}
	}
	return info
}

// String renders the identity for `purlview --version`.
//
// The first line is "purlview <version>" so scripts can read it with
// `purlview --version | head -n 1`. The remaining lines carry build identity.
func (i Info) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "purlview %s\n", i.Version)
	commitLine := i.Commit
	if i.Modified {
		commitLine += " (modified working tree)"
	}
	if i.CommitDate != "" {
		commitLine += " " + i.CommitDate
	}
	fmt.Fprintf(&b, "  commit:   %s\n", commitLine)
	fmt.Fprintf(&b, "  go:       %s %s/%s\n", i.GoVersion, i.OS, i.Arch)
	fmt.Fprintf(&b, "  built by: %s", i.BuiltBy)
	return b.String()
}
