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

// Command gendocs writes shell completions and manual pages for purlview.
//
// Release builds run it before packaging, so that archives and packages ship
// the completions and manual pages of the exact build:
//
//	go run ./internal/tools/gendocs -out <dir> -version <version> -date <RFC 3339 time>
//
// Output:
//
//	<out>/completions/purlview.bash, _purlview, purlview.fish, purlview.ps1
//	<out>/man/purlview.1, purlview-share.1, ...
//	<out>/man-gz/purlview.1.gz, ...            (for Linux packages)
package main

import (
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra/doc"

	"github.com/idyl-labs/purlview/internal/buildinfo"
	"github.com/idyl-labs/purlview/internal/command"
)

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "gendocs:", err)
		os.Exit(1)
	}
}

func run(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("gendocs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "output directory (required)")
	version := fs.String("version", buildinfo.DevelVersion, "version recorded in manual page headers")
	date := fs.String("date", "", "manual page date in RFC 3339 form; keeps output reproducible")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return fmt.Errorf("-out is required")
	}
	when := time.Now().UTC()
	if *date != "" {
		parsed, err := time.Parse(time.RFC3339, *date)
		if err != nil {
			return fmt.Errorf("-date: %w", err)
		}
		when = parsed.UTC()
	}
	return generate(*out, *version, when)
}

func generate(out, version string, when time.Time) error {
	root := command.New(buildinfo.Info{Version: version}, command.Streams{In: strings.NewReader(""), Out: io.Discard, Err: io.Discard})
	root.DisableAutoGenTag = true

	completions := filepath.Join(out, "completions")
	man := filepath.Join(out, "man")
	manGz := filepath.Join(out, "man-gz")
	for _, dir := range []string{completions, man, manGz} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	if err := root.GenBashCompletionFileV2(filepath.Join(completions, "purlview.bash"), true); err != nil {
		return fmt.Errorf("bash completion: %w", err)
	}
	if err := root.GenZshCompletionFile(filepath.Join(completions, "_purlview")); err != nil {
		return fmt.Errorf("zsh completion: %w", err)
	}
	if err := root.GenFishCompletionFile(filepath.Join(completions, "purlview.fish"), true); err != nil {
		return fmt.Errorf("fish completion: %w", err)
	}
	if err := root.GenPowerShellCompletionFileWithDesc(filepath.Join(completions, "purlview.ps1")); err != nil {
		return fmt.Errorf("powershell completion: %w", err)
	}

	header := &doc.GenManHeader{
		Title:   "PURLVIEW",
		Section: "1",
		Source:  "Purlview " + version,
		Manual:  "Purlview Manual",
		Date:    &when,
	}
	if err := doc.GenManTree(root, header, man); err != nil {
		return fmt.Errorf("manual pages: %w", err)
	}
	pages, err := filepath.Glob(filepath.Join(man, "*.1"))
	if err != nil {
		return err
	}
	for _, page := range pages {
		if err := gzipFile(page, filepath.Join(manGz, filepath.Base(page)+".gz"), when); err != nil {
			return fmt.Errorf("compress %s: %w", page, err)
		}
	}
	return nil
}

func gzipFile(src, dst string, when time.Time) (err error) {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	zw := gzip.NewWriter(f)
	zw.ModTime = when
	if _, err := zw.Write(data); err != nil {
		return err
	}
	return zw.Close()
}
