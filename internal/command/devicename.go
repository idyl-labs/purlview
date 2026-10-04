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
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

// deviceName is what this computer is called in `purlview devices`, on share
// cards and in the console: the name the system shows people when it
// has one (macOS's computer name, Linux's pretty hostname), otherwise the
// hostname without its domain, since `Studio.home.arpa` and `MacBook-Pro.local`
// carry the network's name rather than the computer's. It is read once, at
// sign-in.
func deviceName() string {
	return nameFrom(prettyName(), os.Hostname)
}

// maxDeviceName bounds a name in characters; the platform takes 256 bytes.
const maxDeviceName = 64

func nameFrom(pretty string, hostname func() (string, error)) string {
	if name := cleanName(pretty); name != "" {
		return name
	}
	if h, err := hostname(); err == nil {
		if name := cleanName(shortHost(h)); name != "" {
			return name
		}
	}
	return "this device"
}

// shortHost is a hostname's first label: `Studio.home.arpa` is `Studio`. An
// address is kept whole, since its first part names nothing.
func shortHost(h string) string {
	h = strings.TrimSuffix(strings.TrimSpace(h), ".")
	if strings.Trim(h, "0123456789.:abcdefABCDEF") == "" && strings.ContainsAny(h, ".:") {
		return h
	}
	first, _, _ := strings.Cut(h, ".")
	return first
}

// cleanName keeps a name to one printable line of at most maxDeviceName
// characters, with its spaces collapsed.
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > maxDeviceName {
		s = strings.TrimSpace(string([]rune(s)[:maxDeviceName]))
	}
	return s
}
