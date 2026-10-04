package main

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestIsLicenceFile(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"LICENSE":            true,
		"LICENSE.txt":        true,
		"License.md":         true,
		"LICENCE":            true,
		"license-apache":     true,
		"LICENSE-MIT.txt":    true,
		"COPYING":            true,
		"NOTICE":             true,
		"NOTICE.md":          true,
		"PATENTS":            true,
		"license.go":         false,
		"licenses":           false,
		"LICENSE.html":       false,
		"README.md":          false,
		"notices.json":       false,
		"third_party_notice": false,
	} {
		if got := isLicenceFile(name); got != want {
			t.Errorf("isLicenceFile(%q) = %v, want %v", name, got, want)
		}
	}
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A linked package's licence files are those of its own directory and of
// every directory above it in the module; those of packages that are not
// linked are not.
func TestLicenceFilesCoverTheLinkedPackagesOnly(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "LICENSE"), "root licence")
	writeFile(t, filepath.Join(dir, "NOTICE"), "root notice")
	writeFile(t, filepath.Join(dir, "license.go"), "package x")
	writeFile(t, filepath.Join(dir, "internal", "LICENSE"), "internal licence")
	writeFile(t, filepath.Join(dir, "internal", "vendored", "COPYING"), "vendored copying")
	writeFile(t, filepath.Join(dir, "unlinked", "LICENSE"), "not linked")
	m := &module{Path: "example.invalid/mod", Version: "v1.2.3", Dir: dir, dirs: map[string]bool{".": true, "internal/vendored": true}}
	files, err := m.licenceFiles()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"LICENSE", "NOTICE", "internal/LICENSE", "internal/vendored/COPYING"}; !slices.Equal(files, want) {
		t.Fatalf("licence files %q, want %q", files, want)
	}
}

func TestRender(t *testing.T) {
	t.Parallel()
	goroot := t.TempDir()
	writeFile(t, filepath.Join(goroot, "LICENSE"), "Go licence\r\nsecond line\r\n")
	writeFile(t, filepath.Join(goroot, "PATENTS"), "Go patents\n")
	writeFile(t, filepath.Join(goroot, "README.md"), "not a licence")
	b, a := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(b, "LICENSE.txt"), "B licence\n\n\n")
	writeFile(t, filepath.Join(a, "LICENSE"), "A licence")
	mods := []*module{
		{Path: "example.invalid/a", Version: "v0.1.0", Dir: a, dirs: map[string]bool{".": true}},
		{Path: "example.invalid/b", Version: "v2.0.0", Dir: b, dirs: map[string]bool{"sub": true}},
	}
	if err := os.MkdirAll(filepath.Join(b, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := render(goroot, mods)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	_, body, ok := strings.Cut(text, "do not edit it by hand.\n\n")
	if !ok {
		t.Fatalf("no header:\n%s", text)
	}
	want := rule + "Go runtime and standard library\nhttps://go.dev\n\n-- LICENSE --\n\nGo licence\nsecond line\n\n-- PATENTS --\n\nGo patents\n\n" +
		rule + "example.invalid/a v0.1.0\nhttps://pkg.go.dev/example.invalid/a@v0.1.0\n\n-- LICENSE --\n\nA licence\n\n" +
		rule + "example.invalid/b v2.0.0\nhttps://pkg.go.dev/example.invalid/b@v2.0.0\n\n-- LICENSE.txt --\n\nB licence\n\n"
	if body != want {
		t.Fatalf("body:\n%s\nwant:\n%s", body, want)
	}
	if strings.Contains(text, "\r") {
		t.Fatal("line endings must be normalised")
	}
}

// A module without any licence file is an error, not a silent omission.
func TestRenderRefusesAModuleWithoutLicence(t *testing.T) {
	t.Parallel()
	goroot := t.TempDir()
	writeFile(t, filepath.Join(goroot, "LICENSE"), "Go licence")
	bare := t.TempDir()
	writeFile(t, filepath.Join(bare, "README.md"), "no licence here")
	_, err := render(goroot, []*module{{Path: "example.invalid/bare", Version: "v1.0.0", Dir: bare, dirs: map[string]bool{".": true}}})
	if err == nil || !strings.Contains(err.Error(), "example.invalid/bare v1.0.0: no licence file found") {
		t.Fatalf("got %v", err)
	}
}

func TestRunNeedsExactlyOneMode(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{nil, {"-write", "a", "-check", "b"}, {"-write", "a", "extra"}} {
		if err := run(args, io.Discard); err == nil {
			t.Errorf("%q: expected an error", args)
		}
	}
}
