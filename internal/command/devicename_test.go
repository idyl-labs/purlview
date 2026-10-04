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

package command

import (
	"errors"
	"strings"
	"testing"
)

// A device is named as its system names it, never with the network's domain
// on the end.
func TestDeviceName(t *testing.T) {
	host := func(h string) func() (string, error) { return func() (string, error) { return h, nil } }
	for _, c := range []struct {
		pretty, host, want string
	}{
		{"Sam's MacBook Pro\n", "Sams-MacBook-Pro.local", "Sam's MacBook Pro"},
		{"", "Build-Box-2.home.arpa", "Build-Box-2"},
		{"", "MacBook-Pro.local.", "MacBook-Pro"},
		{"", "build-runner-07", "build-runner-07"},
		{"", "DESKTOP-4F2K9QX", "DESKTOP-4F2K9QX"},
		{"", "192.168.1.20", "192.168.1.20"},
		{"  Studio \t Mac\x00 ", "studio.home.arpa", "Studio Mac"},
		{"\n", "", "this device"},
	} {
		if got := nameFrom(c.pretty, host(c.host)); got != c.want {
			t.Errorf("nameFrom(%q, %q) = %q, want %q", c.pretty, c.host, got, c.want)
		}
	}
	if got := nameFrom("", func() (string, error) { return "", errors.New("no name") }); got != "this device" {
		t.Errorf("no hostname: %q", got)
	}
	long := strings.Repeat("é", 100)
	if got := nameFrom(long, host("x")); got != strings.Repeat("é", maxDeviceName) {
		t.Errorf("a long name is cut to %d characters: %d", maxDeviceName, len([]rune(got)))
	}
}

func TestMachineInfoPretty(t *testing.T) {
	for file, want := range map[string]string{
		"PRETTY_HOSTNAME=\"Sam's Workstation\"\nCHASSIS=desktop\n": "Sam's Workstation",
		"CHASSIS=laptop\nPRETTY_HOSTNAME='Build box'\n":            "Build box",
		"PRETTY_HOSTNAME=Plain\n":                                  "Plain",
		"PRETTY_HOSTNAME=\"Tab\\tName\"\n":                         "Tab\tName",
		"CHASSIS=vm\n":                                             "",
		"":                                                         "",
	} {
		if got := machineInfoPretty(file); got != want {
			t.Errorf("%q: %q, want %q", file, got, want)
		}
	}
}
