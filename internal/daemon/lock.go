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
	"context"
	"errors"
	"os"
	"time"
)

// ErrLocked reports that another process holds the lock.
var ErrLocked = errors.New("lock is held by another process")

// FileLock is an exclusive advisory lock tied to an open file handle. The
// operating system releases it when the process exits, however it exits, so
// it proves that its holder is alive (unlike a PID written to a file).
type FileLock struct {
	f    *os.File
	path string
}

// TryLock acquires the lock or returns ErrLocked immediately.
func TryLock(path string) (*FileLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // lock files live in the user's private runtime directory
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &FileLock{f: f, path: path}, nil
}

// Lock waits for the lock until ctx is done, polling briefly.
func Lock(ctx context.Context, path string) (*FileLock, error) {
	for {
		l, err := TryLock(path)
		if !errors.Is(err, ErrLocked) {
			return l, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

// Unlock releases the lock.
func (l *FileLock) Unlock() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlockFile(l.f)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

// IsHeld reports whether some process currently holds the lock at path.
func IsHeld(path string) bool {
	l, err := TryLock(path)
	if err != nil {
		return errors.Is(err, ErrLocked)
	}
	_ = l.Unlock()
	return false
}
