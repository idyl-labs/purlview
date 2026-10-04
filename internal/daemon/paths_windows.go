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

package daemon

import (
	"github.com/idyl-labs/purlview/internal/ipc"
)

// endpointAddress names the pipe. The named-pipe namespace is machine-wide
// and shared by every logon session, so the name carries the user's SID (one
// daemon per user, reachable from all of that user's sessions) and a hash of
// the runtime directory (so an overridden state directory gets its own
// daemon). Access is enforced by the pipe's security descriptor, not by the
// name.
func endpointAddress(runtimeDir string, _ func(string) string) (string, error) {
	sid, err := ipc.CurrentUserSID()
	if err != nil {
		return "", err
	}
	return `\\.\pipe\purlview-` + sid + "-" + shortHash(runtimeDir, 8), nil
}

func ownedByCurrentUser(string) bool { return true }

func ensurePrivateRuntime(string) error { return nil }

func ensureSocketDir(string) error { return nil }
