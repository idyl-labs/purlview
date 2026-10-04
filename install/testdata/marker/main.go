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

// Command marker stands in for a release's executable in the installer tests
// that must prove an executable never ran: run, it writes the file
// INSTALL_TEST_MARKER names, then answers --version as the release would.
package main

import (
	"fmt"
	"os"
)

var version = "0.0.0"

func main() {
	if path := os.Getenv("INSTALL_TEST_MARKER"); path != "" {
		_ = os.WriteFile(path, []byte("ran\n"), 0o600)
	}
	fmt.Println("purlview " + version)
}
