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
	"os"
	"os/signal"

	"golang.org/x/sys/windows"
)

var (
	kernel32         = windows.NewLazySystemDLL("kernel32.dll")
	getConsoleWindow = kernel32.NewProc("GetConsoleWindow")
)

// consoleState reports whether the process is attached to a console.
func consoleState() string {
	if err := getConsoleWindow.Find(); err != nil {
		return "unknown"
	}
	h, _, _ := getConsoleWindow.Call()
	if h == 0 {
		return "none"
	}
	return "attached"
}

// notifyTermination registers Ctrl-C/Ctrl-Break (only delivered when a
// console exists).
func notifyTermination(ch chan<- os.Signal) {
	signal.Notify(ch, os.Interrupt)
}
