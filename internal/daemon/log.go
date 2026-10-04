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
	"fmt"
	"io"
	"log"
	"os"
	"sync"
)

// maxLogSize bounds the daemon log; when exceeded the file is rotated once
// (daemon.log.1 replaces any previous rotation), so at most two files of
// this size exist.
const maxLogSize = 1 << 20

// boundedLog appends to a file and rotates it when it grows past the limit.
type boundedLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func openBoundedLog(path string) (*boundedLog, error) {
	b := &boundedLog{path: path}
	if err := b.open(); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *boundedLog) open() error {
	f, err := os.OpenFile(b.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return err
	}
	b.f, b.size = f, st.Size()
	return nil
}

func (b *boundedLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.f == nil {
		return 0, os.ErrClosed
	}
	if b.size+int64(len(p)) > maxLogSize {
		_ = b.f.Close()
		_ = os.Rename(b.path, b.path+".1")
		if err := b.open(); err != nil {
			return 0, err
		}
	}
	n, err := b.f.Write(p)
	b.size += int64(n)
	return n, err
}

func (b *boundedLog) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.f == nil {
		return nil
	}
	err := b.f.Close()
	b.f = nil
	return err
}

// newLogger returns a logger that writes to w with a UTC timestamp.
func newLogger(w io.Writer, prefix string) *log.Logger {
	return log.New(w, prefix, log.LstdFlags|log.Lmicroseconds|log.LUTC)
}

// tailFile returns the last n lines of a file, for error reports.
func tailFile(path string, n int) string {
	data, err := os.ReadFile(path) //nolint:gosec // the daemon's own log in the private log directory
	if err != nil {
		return ""
	}
	lines := splitLines(data)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := ""
	for _, l := range lines {
		out += fmt.Sprintf("    %s\n", l)
	}
	return out
}

func splitLines(data []byte) []string {
	var lines []string
	start := 0
	for i, c := range data {
		if c == '\n' {
			if i > start {
				lines = append(lines, string(data[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, string(data[start:]))
	}
	return lines
}
