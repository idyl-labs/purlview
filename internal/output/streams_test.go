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
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// direct is a write to a process stream that bypasses this package.
var direct = regexp.MustCompile(`\bos\.Std(out|err)\b|\bfmt\.Print(f|ln)?\(|\bprint(ln)?\(`)

// allowed may touch the streams: this package, the executable that wires
// them in once, and the build-time tools, which are not CLI commands.
var allowed = []string{"internal/output/", "cmd/purlview/main.go", "internal/tools/"}

// TestOnlyThisPackageWritesTheStreams is the lint rule the forbidigo
// configuration cannot fully express: forbidigo sees fmt.Println but not
// os.Stderr handed to fmt.Fprintln. Everything else in the CLI (cmd and
// internal) renders through a Printer.
func TestOnlyThisPackageWritesTheStreams(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	found := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// Only the CLI is held to the rule: the SDK and the installers'
			// fixtures are not CLI code, and dot directories are not source.
			top, _, _ := strings.Cut(rel, "/")
			if rel != "." && top != "cmd" && top != "internal" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		found++
		for _, prefix := range allowed {
			if strings.HasPrefix(rel, prefix) {
				return nil
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(data), "\n") {
			if direct.MatchString(line) {
				t.Errorf("%s:%d writes a process stream outside package output: %s", rel, i+1, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found < 20 {
		t.Fatalf("scanned only %d files; the walk root is wrong", found)
	}
}
