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

// Command applysigned is a GoReleaser post-build hook.
//
// The release workflow builds unsigned binaries once, signs the macOS and
// Windows ones on native runners, and then runs GoReleaser again to package
// everything. GoReleaser always rebuilds, so this hook replaces each freshly
// built binary with its signed counterpart, after proving that the rebuild is
// byte-for-byte identical to the binary that was signed. That keeps the
// provenance chain intact: what was signed is what was built from this commit.
//
// Usage (from .goreleaser.yaml):
//
//	applysigned <built binary> <goos> <goarch>
//
// Environment:
//
//	PURLVIEW_SIGNED_BINARIES   directory holding <goos>_<goarch>/<binary>
//	                            and <goos>_<goarch>/unsigned.sha256; when
//	                            unset the hook does nothing (snapshot builds)
//	PURLVIEW_ALLOW_UNSIGNED    "true" permits a darwin or windows target
//	                            without a signed binary (labelled rehearsals);
//	                            "windows" permits only windows targets (a
//	                            release published before Windows signing)
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func main() {
	if err := run(os.Args[1:], os.Getenv, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "applysigned:", err)
		os.Exit(1)
	}
}

func run(args []string, getenv func(string) string, out io.Writer) error {
	if len(args) != 3 {
		return errors.New("usage: applysigned <built binary> <goos> <goarch>")
	}
	built, goos, goarch := args[0], args[1], args[2]
	signedRoot := getenv("PURLVIEW_SIGNED_BINARIES")
	if signedRoot == "" {
		fmt.Fprintf(out, "applysigned: %s_%s: no signed binaries configured; keeping unsigned build\n", goos, goarch)
		return nil
	}
	target := goos + "_" + goarch
	dir := filepath.Join(signedRoot, target)
	signed := filepath.Join(dir, filepath.Base(built))
	if _, err := os.Stat(signed); errors.Is(err, os.ErrNotExist) {
		if (goos == "darwin" || goos == "windows") && !unsignedAllowed(getenv("PURLVIEW_ALLOW_UNSIGNED"), goos) {
			return fmt.Errorf("%s: no signed binary at %s and PURLVIEW_ALLOW_UNSIGNED permits no unsigned %s build", target, signed, goos)
		}
		fmt.Fprintf(out, "applysigned: %s: no signed binary; keeping unsigned build\n", target)
		return nil
	} else if err != nil {
		return err
	}

	want, err := os.ReadFile(filepath.Join(dir, "unsigned.sha256"))
	if err != nil {
		return fmt.Errorf("%s: missing hash of the binary that was signed: %w", target, err)
	}
	expected := strings.Fields(string(want))
	if len(expected) == 0 || len(expected[0]) != sha256.Size*2 {
		return fmt.Errorf("%s: unsigned.sha256 does not start with a SHA-256 digest", target)
	}
	actual, err := fileSHA256(built)
	if err != nil {
		return err
	}
	if !strings.EqualFold(actual, expected[0]) {
		return fmt.Errorf("%s: rebuilt binary %s does not match the binary that was signed (%s); refusing to package", target, actual, expected[0])
	}

	if err := replaceFile(signed, built); err != nil {
		return fmt.Errorf("%s: apply signed binary: %w", target, err)
	}
	fmt.Fprintf(out, "applysigned: %s: rebuild matched %s; applied signed binary\n", target, actual[:12])
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// replaceFile copies src over dst through a temporary file and a rename, so
// dst is never left half-written.
func replaceFile(src, dst string) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".applysigned-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	// Executables must stay executable; the mode is deliberate.
	if err = os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// unsignedAllowed reports whether PURLVIEW_ALLOW_UNSIGNED lets goos ship
// without a signed binary: "true" for every platform, "windows" for Windows
// only.
func unsignedAllowed(allow, goos string) bool {
	return allow == "true" || (allow == "windows" && goos == "windows")
}
