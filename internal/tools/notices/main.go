// Command notices writes THIRD_PARTY_NOTICES: the licence and notice files
// of everything other than Purlview's own code that is linked into the
// purlview executable, for the binary distributions to ship.
//
//	go run ./internal/tools/notices -write THIRD_PARTY_NOTICES
//	go run ./internal/tools/notices -check THIRD_PARTY_NOTICES
//
// It asks `go list -deps` for the packages linked into ./cmd/purlview on
// every release target, with cgo disabled as release builds have it, and
// reads the licence and notice files of their modules from the module cache:
// those in the module's root and in every directory between it and a linked
// package. The Go runtime and standard library are listed first, with the
// licence files of the toolchain's GOROOT. The output depends only on the
// module graph and the toolchain, so -check fails exactly when the file
// needs to be regenerated.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// mainPackage is the command the notices describe.
const mainPackage = "./cmd/purlview"

// targets are the release targets of .goreleaser.yaml.
var targets = []struct{ goos, goarch string }{
	{"darwin", "amd64"}, {"darwin", "arm64"},
	{"linux", "amd64"}, {"linux", "arm64"},
	{"windows", "amd64"}, {"windows", "arm64"},
}

func main() {
	if err := run(os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
}

func run(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("notices", flag.ContinueOnError)
	fs.SetOutput(stderr)
	write := fs.String("write", "", "write the notices to this file")
	check := fs.String("check", "", "fail if this file differs from the notices")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*write == "") == (*check == "") || fs.NArg() != 0 {
		return errors.New("use exactly one of -write FILE or -check FILE")
	}
	goroot, err := goRoot()
	if err != nil {
		return err
	}
	mods, err := linkedModules()
	if err != nil {
		return err
	}
	text, err := render(goroot, mods)
	if err != nil {
		return err
	}
	if *write != "" {
		return os.WriteFile(*write, text, 0o644)
	}
	have, err := os.ReadFile(*check)
	if err != nil {
		return err
	}
	if !bytes.Equal(have, text) {
		return fmt.Errorf("%s is out of date; run: go run ./internal/tools/notices -write %s", *check, *check)
	}
	return nil
}

// module is a dependency module and the directories, relative to its root,
// in which linked packages may have licence files.
type module struct {
	Path    string
	Version string
	Dir     string
	dirs    map[string]bool
}

// listedPackage is the part of `go list -json` output this tool reads.
type listedPackage struct {
	ImportPath string
	Dir        string
	Standard   bool
	Module     *struct {
		Path    string
		Version string
		Dir     string
		Main    bool
		Replace *struct {
			Path    string
			Version string
			Dir     string
		}
	}
}

// linkedModules returns the dependency modules with packages linked into
// mainPackage on any target, sorted by path.
func linkedModules() ([]*module, error) {
	byPath := map[string]*module{}
	for _, t := range targets {
		cmd := exec.Command("go", "list", "-deps", "-json", mainPackage)
		cmd.Env = append(os.Environ(), "GOOS="+t.goos, "GOARCH="+t.goarch, "CGO_ENABLED=0")
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("go list for %s/%s: %w", t.goos, t.goarch, err)
		}
		dec := json.NewDecoder(bytes.NewReader(out))
		for {
			var p listedPackage
			if err := dec.Decode(&p); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				return nil, fmt.Errorf("go list for %s/%s: %w", t.goos, t.goarch, err)
			}
			if p.Standard || p.Module == nil || p.Module.Main {
				continue
			}
			path, version, dir := p.Module.Path, p.Module.Version, p.Module.Dir
			if r := p.Module.Replace; r != nil {
				version, dir = r.Version, r.Dir
				if r.Version == "" {
					version = r.Path
				}
			}
			m := byPath[path]
			if m == nil {
				m = &module{Path: path, Version: version, Dir: dir, dirs: map[string]bool{}}
				byPath[path] = m
			}
			rel, err := filepath.Rel(dir, p.Dir)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("package %s is outside its module directory %s", p.ImportPath, dir)
			}
			m.dirs[filepath.ToSlash(rel)] = true
		}
	}
	mods := make([]*module, 0, len(byPath))
	for _, m := range byPath {
		mods = append(mods, m)
	}
	slices.SortFunc(mods, func(a, b *module) int { return strings.Compare(a.Path, b.Path) })
	return mods, nil
}

// licenceFiles returns the licence and notice files of m that apply to its
// linked packages, as slash-separated paths relative to its root, sorted.
func (m *module) licenceFiles() ([]string, error) {
	seen := map[string]bool{}
	for dir := range m.dirs {
		// The package's directory and each one above it, up to the root.
		for d := dir; ; d = parentDir(d) {
			names, err := licenceFilesIn(filepath.Join(m.Dir, filepath.FromSlash(d)))
			if err != nil {
				return nil, err
			}
			for _, name := range names {
				seen[joinSlash(d, name)] = true
			}
			if d == "." {
				break
			}
		}
	}
	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	slices.SortFunc(files, compareLicencePaths)
	return files, nil
}

// compareLicencePaths orders the module root's files first, then deeper ones.
func compareLicencePaths(a, b string) int {
	if da, db := strings.Count(a, "/"), strings.Count(b, "/"); da != db {
		return da - db
	}
	return strings.Compare(a, b)
}

func parentDir(d string) string {
	if i := strings.LastIndex(d, "/"); i >= 0 {
		return d[:i]
	}
	return "."
}

func joinSlash(dir, name string) string {
	if dir == "." {
		return name
	}
	return dir + "/" + name
}

// licenceFilesIn returns the names of the licence and notice files in dir.
func licenceFilesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && isLicenceFile(e.Name()) {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// isLicenceFile reports whether a file name is a licence, copying or notice
// file: LICENSE, LICENCE, COPYING, NOTICE or PATENTS, in any case, possibly
// with a suffix after a dash (LICENSE-APACHE) and a text extension.
func isLicenceFile(name string) bool {
	upper := strings.ToUpper(name)
	stem, ext, _ := strings.Cut(upper, ".")
	switch ext {
	case "", "MD", "TXT", "RST":
	default:
		return false
	}
	base, _, _ := strings.Cut(stem, "-")
	switch base {
	case "LICENSE", "LICENCE", "COPYING", "NOTICE", "PATENTS":
		return true
	}
	return false
}

const rule = "================================================================================\n"

// render produces the notices document.
func render(goroot string, mods []*module) ([]byte, error) {
	var b bytes.Buffer
	b.WriteString(`THIRD-PARTY NOTICES

The purlview executable includes the software listed below, in addition to
Purlview's own code (see LICENSE and NOTICE). Each entry gives the module and
its version, followed by the full text of its licence and notice files. The
list covers every supported operating system and CPU architecture; some
modules are linked only on some of them.

This file is generated by internal/tools/notices; do not edit it by hand.

`)
	goFiles, err := licenceFilesIn(goroot)
	if err != nil {
		return nil, fmt.Errorf("GOROOT: %w", err)
	}
	slices.Sort(goFiles)
	if err := entry(&b, "Go runtime and standard library", "https://go.dev", goroot, goFiles); err != nil {
		return nil, err
	}
	for _, m := range mods {
		files, err := m.licenceFiles()
		if err != nil {
			return nil, err
		}
		if err := entry(&b, m.Path+" "+m.Version, "https://pkg.go.dev/"+m.Path+"@"+m.Version, m.Dir, files); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}

// entry writes one component and the text of its files, which must exist.
func entry(b *bytes.Buffer, title, source, dir string, files []string) error {
	if len(files) == 0 {
		return fmt.Errorf("%s: no licence file found in %s", title, dir)
	}
	b.WriteString(rule)
	fmt.Fprintf(b, "%s\n%s\n", title, source)
	for _, f := range files {
		text, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(f)))
		if err != nil {
			return err
		}
		text = bytes.ReplaceAll(text, []byte("\r\n"), []byte("\n"))
		text = bytes.TrimRight(text, "\n \t")
		fmt.Fprintf(b, "\n-- %s --\n\n", f)
		b.Write(text)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return nil
}

// goRoot returns the GOROOT of the go command on PATH, the toolchain that
// builds the release.
func goRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOROOT: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}
