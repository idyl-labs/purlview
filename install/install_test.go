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

package install

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// fixtureRelease is one version served by the fake release repository.
type fixtureRelease struct {
	tag       string
	archive   string // archive file name for the host OS/arch
	data      []byte // archive bytes
	checksums string // checksums.txt content
	bundle    string // checksums.txt.sigstore.json content; empty: not published
}

var (
	hostOS   = runtime.GOOS
	hostArch = runtime.GOARCH
)

// buildFixture compiles the real CLI with the given version and packages it
// the way the release pipeline does.
func buildFixture(t *testing.T, version string) fixtureRelease {
	t.Helper()
	dir := t.TempDir()
	exe := "purlview"
	if hostOS == "windows" {
		exe += ".exe"
	}
	bin := filepath.Join(dir, exe)
	ldflags := fmt.Sprintf("-s -w -X github.com/idyl-labs/purlview/internal/buildinfo.version=%s -X github.com/idyl-labs/purlview/internal/buildinfo.builtBy=test", version)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", bin, "./cmd/purlview")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return packageFixture(t, version, bin)
}

// markerFixture is a release whose executable is not the CLI and that nothing
// signed: run, it writes the file INSTALL_TEST_MARKER names (testdata/marker).
func markerFixture(t *testing.T, version string) fixtureRelease {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "marker")
	if hostOS == "windows" {
		bin += ".exe"
	}
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w -X main.version="+version, "-o", bin, "./testdata/marker")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return packageFixture(t, version, bin)
}

// packageFixture packages the executable at bin the way the release pipeline
// does.
func packageFixture(t *testing.T, version, bin string) fixtureRelease {
	t.Helper()
	exe := "purlview"
	if hostOS == "windows" {
		exe += ".exe"
	}
	content, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}

	var archive bytes.Buffer
	var name string
	if hostOS == "windows" {
		name = fmt.Sprintf("purlview_%s_windows_%s.zip", version, hostArch)
		zw := zip.NewWriter(&archive)
		w, err := zw.Create(exe)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(content); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		name = fmt.Sprintf("purlview_%s_%s_%s.tar.gz", version, hostOS, hostArch)
		gz := gzip.NewWriter(&archive)
		tw := tar.NewWriter(gz)
		if err := tw.WriteHeader(&tar.Header{Name: exe, Mode: 0o755, Size: int64(len(content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	sum := sha256.Sum256(archive.Bytes())
	return fixtureRelease{
		tag:       "v" + version,
		archive:   name,
		data:      archive.Bytes(),
		checksums: hex.EncodeToString(sum[:]) + "  " + name + "\n",
	}
}

// releaseServer mimics the GitHub release URL layout used by the installers.
type releaseServer struct {
	*httptest.Server
	releases map[string]fixtureRelease
	latest   string

	mu sync.Mutex
	// hooks let a test corrupt a specific response
	corruptChecksum bool
	truncateArchive bool
	noLatest        bool
}

// setLatest sets the tag that /releases/latest redirects to.
func (s *releaseServer) setLatest(tag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latest = tag
}

// setFaults configures the failure modes for subsequent requests.
func (s *releaseServer) setFaults(corruptChecksum, truncateArchive, noLatest bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.corruptChecksum, s.truncateArchive, s.noLatest = corruptChecksum, truncateArchive, noLatest
}

func (s *releaseServer) state() (latest string, corruptChecksum, truncateArchive, noLatest bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest, s.corruptChecksum, s.truncateArchive, s.noLatest
}

func newReleaseServer(t *testing.T, releases ...fixtureRelease) *releaseServer {
	t.Helper()
	s := &releaseServer{releases: map[string]fixtureRelease{}}
	for _, r := range releases {
		s.releases[r.tag] = r
	}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		latest, corruptChecksum, truncateArchive, noLatest := s.state()
		switch {
		case strings.HasPrefix(r.URL.Path, "/releases/tag/"):
			if _, ok := s.releases[strings.TrimPrefix(r.URL.Path, "/releases/tag/")]; !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = io.WriteString(w, "<html>release page</html>")
		case r.URL.Path == "/releases":
			_, _ = io.WriteString(w, "<html>releases</html>")
		case r.URL.Path == "/releases/latest":
			// GitHub redirects to the release list, not a 404, while no
			// stable release is published.
			if noLatest || latest == "" {
				http.Redirect(w, r, "/releases", http.StatusFound)
				return
			}
			http.Redirect(w, r, "/releases/tag/"+latest, http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/releases/download/"):
			rest := strings.TrimPrefix(r.URL.Path, "/releases/download/")
			tag, file, ok := strings.Cut(rest, "/")
			rel, found := s.releases[tag]
			if !ok || !found {
				http.NotFound(w, r)
				return
			}
			switch file {
			case "checksums.txt":
				body := rel.checksums
				if corruptChecksum {
					body = strings.Repeat("0", 64) + "  " + rel.archive + "\n"
				}
				_, _ = io.WriteString(w, body)
			case rel.archive:
				data := rel.data
				if truncateArchive {
					data = data[:len(data)/2]
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(data)))
				_, _ = w.Write(data)
			case "checksums.txt.sigstore.json":
				if rel.bundle == "" {
					http.NotFound(w, r)
					return
				}
				_, _ = io.WriteString(w, rel.bundle)
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

type result struct {
	code   int
	output string
}

func (r result) contains(s string) bool { return strings.Contains(r.output, s) }

// installedVersion runs DEST/purlview --version and returns the first line.
func installedVersion(t *testing.T, dest string) string {
	t.Helper()
	exe := filepath.Join(dest, "purlview")
	if hostOS == "windows" {
		exe += ".exe"
	}
	cmd := exec.Command(exe, "--version")
	cmd.Env = isolatedEnv(t, nil)
	out, err := cmd.Output()
	if err != nil {
		return "(not runnable: " + err.Error() + ")"
	}
	return strings.SplitN(string(out), "\n", 2)[0]
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
