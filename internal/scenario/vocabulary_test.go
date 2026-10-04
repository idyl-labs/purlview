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

package scenario

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// forbidden are the words the CLI's output never uses for what people see
// (it says "link" and "share", not "URL" and "tunnel"), plus the internal
// words that belong only behind PURLVIEW_DEBUG and in daemon commands.
var forbidden = regexp.MustCompile(`(?i)\b(tunnels?|sessions?|forward(s|ed|ing)?|urls?|capabilit(y|ies)|tokens?|unshare[ds]?|revok(e|es|ed|ing)|revocations?|end(s|ed|ing)?|timed out|installations?|endpoints?|credentials?|authori[sz](e|es|ed|ing|ation)|authenticat(e|es|ed|ing|ion)|verification|magic links?|recipients?|clients?|targets?|upstreams?|origins?|daemon|admission|control plane)\b`)

// data is what a line repeats from the person or the product rather than
// says: a link, a flag they typed, a <placeholder> in a usage line, the
// Opens value (the developer's own start page) and the one hint that names
// the daemon command. The words around them are still checked.
var data = regexp.MustCompile(`https://\S+|--\S+|<[^>]*>|^Opens\s+\S+|purlview daemon status`)

var escapes = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// checkVocabulary reports every forbidden word in the lines people read.
// Links are checked only for the internal route prefix.
func checkVocabulary(t *testing.T, where, text string, allow ...string) {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "_purlview") {
			t.Errorf("%s: _purlview must never be printed: %q", where, line)
		}
		for _, m := range forbidden.FindAllString(data.ReplaceAllString(escapes.ReplaceAllString(line, ""), ""), -1) {
			allowed := false
			for _, a := range allow {
				if strings.EqualFold(m, a) {
					allowed = true
				}
			}
			if !allowed {
				t.Errorf("%s: %q is not a word people read (in %q)", where, m, line)
			}
		}
	}
}

// TestVocabulary fails when any output a person reads, in the scenario
// transcripts, the help of every command or the command goldens, uses a
// word from the right-hand column of the vocabulary table, or _purlview.
func TestVocabulary(t *testing.T) {
	t.Parallel()
	for _, o := range ExecuteAll() {
		for _, e := range o.Transcript {
			if e.inv == nil || e.inv.args[0] == "daemon" || (e.kind != kindStdout && e.kind != kindStderr) {
				continue
			}
			var allow []string
			if e.inv.args[0] == "unshare" {
				allow = []string{"unshare"} // the hidden alias names itself in its own usage line
			}
			checkVocabulary(t, o.Scenario.Name+": purlview "+strings.Join(e.inv.args, " "), e.text, allow...)
		}
	}
	w := NewWorld()
	for _, args := range [][]string{nil, {"share"}, {"list"}, {"link"}, {"stop"}, {"unshare"}, {"devices"}, {"devices", "signout"}, {"login"}, {"logout"}, {"whoami"}} {
		r := w.Exec(append(append([]string{}, args...), "--help")...)
		if r.Code != 0 || r.Stderr != "" {
			t.Fatalf("help %v: %d %q", args, r.Code, r.Stderr)
		}
		var allow []string
		if len(args) > 0 && args[0] == "unshare" {
			allow = []string{"unshare"}
		}
		checkVocabulary(t, "help "+strings.Join(args, " "), r.Stdout, allow...)
	}
	goldens, _ := filepath.Glob(filepath.Join("..", "command", "testdata", "golden", "*.txt"))
	if len(goldens) == 0 {
		t.Fatal("no command goldens found")
	}
	for _, path := range goldens {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		checkVocabulary(t, filepath.Base(path), string(data))
	}
}

// TestStdoutCarriesOnlyData fails on any status line or escape on stdout
// in any scenario: the link, tables and the whoami block are all a pipe
// ever receives.
func TestStdoutCarriesOnlyData(t *testing.T) {
	t.Parallel()
	for _, o := range ExecuteAll() {
		for _, e := range o.Transcript {
			if e.kind != kindStdout || e.inv == nil || e.inv.args[0] == "daemon" {
				continue
			}
			for _, line := range strings.Split(e.text, "\n") {
				if strings.ContainsAny(line, "\x1b") || strings.HasPrefix(line, "✓") || strings.HasPrefix(line, "✗") || strings.HasPrefix(line, "!") || strings.HasPrefix(line, "→") || strings.HasPrefix(line, "Run '") {
					t.Errorf("%s: purlview %s wrote status to stdout: %q", o.Scenario.Name, strings.Join(e.inv.args, " "), line)
				}
			}
			if e.text != "" && !strings.HasSuffix(e.text, "\n") {
				t.Errorf("%s: purlview %s left stdout without a final newline: %q", o.Scenario.Name, strings.Join(e.inv.args, " "), e.text)
			}
		}
	}
}

// TestColourNeverReachesTheLink runs with colour on: status is coloured,
// tables may be, and the link on stdout is exactly the link.
func TestColourNeverReachesTheLink(t *testing.T) {
	t.Parallel()
	w := NewWorld()
	w.Color = true
	w.SignIn()
	r := w.Exec("share", "3000", "--background")
	if r.Code != 0 || r.Stdout != url1+"\n" {
		t.Fatalf("share: %d %q", r.Code, r.Stdout)
	}
	if !strings.HasPrefix(r.Stderr, "\x1b[32m✓\x1b[0m Sharing \x1b[36mlocalhost:3000\x1b[0m\n") || !strings.Contains(r.Stderr, "\x1b[90mAccess \x1b[0m  Anyone with this link\n") {
		t.Fatalf("share status must be coloured: %q", r.Stderr)
	}
	r = w.Exec("link", label1)
	if r.Code != 0 || r.Stdout != url1+"\n" || r.Stderr != "" {
		t.Fatalf("link: %d %q %q", r.Code, r.Stdout, r.Stderr)
	}
	r = w.Exec("list")
	if r.Code != 0 || !strings.Contains(r.Stdout, "\x1b[32m●\x1b[0m \x1b[36m"+label1+"\x1b[0m") || !strings.Contains(r.Stdout, "\x1b[32mSharing\x1b[0m") {
		t.Fatalf("list: %d %q", r.Code, r.Stdout)
	}
	r = w.Exec("stop", label1)
	if r.Code != 0 || r.Stderr != "\x1b[32m✓\x1b[0m Stopped \x1b[36m"+label1+"\x1b[0m \x1b[90m— the link no longer works\x1b[0m\n" {
		t.Fatalf("stop: %d %q", r.Code, r.Stderr)
	}
}
