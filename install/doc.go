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

// Package install holds the fallback installers for Purlview and their tests.
//
// install.sh (macOS, Linux) and install.ps1 (Windows) are the scripts; the Go
// tests in this directory build fixture releases, serve them from a loopback
// HTTP server and exercise the installers end to end, including failure cases.
package install
