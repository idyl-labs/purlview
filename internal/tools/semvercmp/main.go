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

// Command semvercmp compares two semantic versions for release scripts.
//
//	semvercmp A B   prints -1, 0 or 1 and exits 0; exits 2 on a malformed input
//
// A leading "v" is ignored. Prereleases order before the release they precede
// (0.1.0-alpha.2 < 0.1.0-rc.1 < 0.1.0), per SemVer 2.0.0 item 11. Build
// metadata after "+" is ignored. The comparison lives in internal/semver and
// is shared with the CLI's update checker.
package main

import (
	"fmt"
	"os"

	"github.com/idyl-labs/purlview/internal/semver"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: semvercmp A B")
		os.Exit(2)
	}
	c, err := semver.CompareStrings(os.Args[1], os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "semvercmp:", err)
		os.Exit(2)
	}
	fmt.Println(c)
}
