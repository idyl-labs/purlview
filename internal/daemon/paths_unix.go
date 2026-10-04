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

//go:build !windows

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/idyl-labs/purlview/internal/ipc"
)

// maxSocketPath is the longest socket path the kernel accepts (sun_path
// minus the terminating NUL): 104 bytes on macOS and the BSDs, 108 on Linux.
func maxSocketPath() int {
	if runtime.GOOS == "linux" {
		return 107
	}
	return 103
}

// endpointAddress chooses the socket path. It lives in the runtime directory
// when that fits the kernel limit; otherwise (long home directories,
// non-ASCII user names, deep test directories) it moves to a private
// per-user directory under the temporary directory, keyed by a hash of the
// runtime directory so distinct runtime directories never share a socket.
func endpointAddress(runtimeDir string, getenv func(string) string) (string, error) {
	direct := filepath.Join(runtimeDir, socketFile)
	if len(direct) <= maxSocketPath() {
		return direct, nil
	}
	tmp := getenv("TMPDIR")
	if tmp == "" {
		tmp = os.TempDir()
	}
	alt := filepath.Join(tmp, "purlview-"+strconv.Itoa(os.Geteuid()), shortHash(runtimeDir, 12)+".sock")
	if len(alt) > maxSocketPath() {
		return "", fmt.Errorf("no usable socket path: %q and %q both exceed %d bytes; set TMPDIR to a shorter directory", direct, alt, maxSocketPath())
	}
	return alt, nil
}

// ownedByCurrentUser reports whether dir exists and belongs to this user.
func ownedByCurrentUser(dir string) bool {
	return ipc.CheckPrivateDir(dir) == nil
}

// ensurePrivateRuntime verifies the runtime directory is private and, when
// the socket lives elsewhere, creates that directory privately too.
func ensurePrivateRuntime(runtimeDir string) error {
	if err := ipc.CheckPrivateDir(runtimeDir); err != nil {
		return err
	}
	return nil
}

// ensureSocketDir creates the socket's parent directory (the fallback under
// TMPDIR) with private permissions and verifies its ownership.
func ensureSocketDir(socket string) error {
	dir := filepath.Dir(socket)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return ipc.CheckPrivateDir(dir)
}
