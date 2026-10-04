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

package output

import (
	"os"

	"golang.org/x/sys/windows"
)

// EnableColor switches a Windows console to ANSI escape processing, which
// Windows Terminal and conhost support but leave off until a process asks.
// It reports false when a stream is a console that refuses, so the caller
// falls back to plain text instead of printing raw escapes. Streams that are
// not consoles (pipes, files) need nothing.
func EnableColor(streams ...any) bool {
	for _, s := range streams {
		f, ok := s.(*os.File)
		if !ok || f == nil {
			continue
		}
		h := windows.Handle(f.Fd())
		var mode uint32
		if windows.GetConsoleMode(h, &mode) != nil {
			continue
		}
		if windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) != nil {
			return false
		}
	}
	return true
}
