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
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The installers and the executables they install derive per-user locations
// from the environment: install.sh and the CLI read HOME and the XDG base
// directories, install.sh also ZDOTDIR (where zsh's startup files are),
// install.ps1 and the CLI read LOCALAPPDATA, and install.ps1 reads
// USERPROFILE. Install writes shell startup files under them, uninstall
// deletes directories under them and every upgrade or uninstall stops "the
// daemon of this user" through them. No test
// may therefore run a script or an installed executable with the developer's
// own values: isolatedEnv is the only way this package builds the environment
// of a script or an installed executable, and it refuses to return one that
// still points at them or into their home directory; buildFixture's `go build`
// keeps the real one for the toolchain.

// userDirVars are the variables that locate per-user state, on every host.
var userDirVars = []string{
	"HOME", "XDG_RUNTIME_DIR", "XDG_STATE_HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "ZDOTDIR",
	"USERPROFILE", "LOCALAPPDATA", "APPDATA",
}

var testHomes sync.Map // testing.TB -> string

// testHome is the throwaway home directory of one test (or subtest). Every
// isolatedEnv call of that test uses the same one, so an install, the daemon
// it starts and the following uninstall agree about where state lives.
func testHome(t testing.TB) string {
	t.Helper()
	if home, ok := testHomes.Load(t); ok {
		return home.(string)
	}
	home := filepath.Join(t.TempDir(), "home")
	// They exist, as on a real account, and are private: the CLI only uses an
	// XDG_RUNTIME_DIR that belongs to the user alone.
	for _, dir := range userDirs(home) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	testHomes.Store(t, home)
	return home
}

// userDirs maps every variable in userDirVars to its place under home.
func userDirs(home string) map[string]string {
	return map[string]string{
		"HOME":            home,
		"XDG_RUNTIME_DIR": filepath.Join(home, ".xdg-runtime"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME":  filepath.Join(home, ".cache"),
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
		"ZDOTDIR":         home,
		"USERPROFILE":     home,
		"LOCALAPPDATA":    filepath.Join(home, "AppData", "Local"),
		"APPDATA":         filepath.Join(home, "AppData", "Roaming"),
	}
}

// isolatedEnv is the environment for an installer script or an installed
// executable: the test process environment (PATH, TMPDIR, proxy settings and
// so on) without its PURLVIEW_* settings and per-user locations, then every
// per-user location under testHome, then the test's own variables. It fails
// the test before anything runs if the result could still reach the real
// user's state, for example because a test passed its own HOME.
func isolatedEnv(t testing.TB, vars map[string]string) []string {
	t.Helper()
	var env []string
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		name = strings.ToUpper(name)
		if strings.HasPrefix(name, "PURLVIEW_") || slices.Contains(userDirVars, name) {
			continue
		}
		env = append(env, item)
	}
	for name, value := range userDirs(testHome(t)) {
		env = append(env, name+"="+value)
	}
	for name, value := range vars {
		env = append(env, name+"="+value)
	}
	if err := checkIsolated(env, realUserDirs()); err != nil {
		t.Fatalf("refusing to run with this environment: %v", err)
	}
	return env
}

// realUserDirs are the test process's own values of userDirVars, plus the
// home directory the operating system reports under the key "home" and the
// temporary directory under "tmp". Every throwaway home lives in the
// temporary directory, which on Windows (and wherever TMPDIR points into the
// profile) is itself inside the home directory.
func realUserDirs() map[string]string {
	own := map[string]string{}
	for _, name := range userDirVars {
		own[name] = os.Getenv(name)
	}
	if home, err := os.UserHomeDir(); err == nil {
		own["home"] = home
	}
	own["tmp"] = os.TempDir()
	return own
}

// checkIsolated reports why env must not be given to an installer or an
// installed executable: a per-user location is unset (the scripts and the CLI
// then fall back to the real one), is the real user's, or lies inside the
// real home directory anywhere but the temporary directory.
func checkIsolated(env []string, own map[string]string) error {
	effective := effectiveEnv(env)
	for _, name := range userDirVars {
		value := effective[name]
		if value == "" {
			return fmt.Errorf("%s is not set, so the real per-user directory would be used", name)
		}
		for _, other := range []string{own[name], own["home"]} {
			if other != "" && samePath(value, other) {
				return fmt.Errorf("%s=%s is the real per-user directory of whoever runs the tests", name, value)
			}
		}
		if home := own["home"]; home != "" && insideOf(value, home) {
			if tmp := own["tmp"]; tmp == "" || !insideOf(value, tmp) {
				return fmt.Errorf("%s=%s lies inside the home directory %s of whoever runs the tests", name, value, home)
			}
		}
	}
	return nil
}

// effectiveEnv is what a child started with env sees: like os/exec, the last
// assignment of a name wins, and Windows ignores the case of names.
func effectiveEnv(env []string) map[string]string {
	effective := map[string]string{}
	for _, item := range env {
		name, value, _ := strings.Cut(item, "=")
		if runtime.GOOS == "windows" {
			name = strings.ToUpper(name)
		}
		effective[name] = value
	}
	return effective
}

// comparablePath is p as paths are compared here: absolute, with symbolic
// links resolved, cleaned, and in lower case where file systems ignore case.
func comparablePath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	p = filepath.Clean(p)
	if runtime.GOOS != "linux" {
		p = strings.ToLower(p)
	}
	return p
}

func samePath(a, b string) bool { return comparablePath(a) == comparablePath(b) }

// insideOf reports whether path is dir or lies below it, once both are
// resolved.
func insideOf(path, dir string) bool { return within(comparablePath(path), comparablePath(dir)) }

// runtimeStateDirs are the directories uninstall removes for a user whose
// per-user locations are userDirs(home), and credential is a file the
// installers must leave alone.
func runtimeStateDirs(home string) (dirs []string, credential string) {
	d := userDirs(home)
	switch runtime.GOOS {
	case "darwin":
		support := filepath.Join(home, "Library", "Application Support", "purlview")
		return []string{
			filepath.Join(support, "runtime"),
			filepath.Join(home, "Library", "Logs", "purlview"),
			filepath.Join(home, "Library", "Caches", "purlview"),
		}, filepath.Join(support, "credentials.json")
	case "windows":
		base := filepath.Join(d["LOCALAPPDATA"], "purlview")
		return []string{
			filepath.Join(base, "runtime"),
			filepath.Join(base, "logs"),
			filepath.Join(base, "cache"),
		}, filepath.Join(base, "credentials.json")
	default:
		// The credential sits in $XDG_STATE_HOME/purlview, next to the
		// runtime fallback and the logs, which are removed around it.
		state := filepath.Join(d["XDG_STATE_HOME"], "purlview")
		return []string{
			filepath.Join(d["XDG_RUNTIME_DIR"], "purlview"),
			filepath.Join(state, "runtime"),
			filepath.Join(state, "logs"),
			filepath.Join(d["XDG_CACHE_HOME"], "purlview"),
		}, filepath.Join(state, "credentials.json")
	}
}

// Uninstall removes runtime state, and in a test that is the state under the
// test's throwaway home and nothing else.
func TestUninstallRemovesOnlyRuntimeStateUnderTestHome(t *testing.T) {
	rel := buildFixture(t, "0.9.0")
	srv := newReleaseServer(t, rel)
	srv.setLatest(rel.tag)
	dest := filepath.Join(t.TempDir(), "bin")
	env := map[string]string{"PURLVIEW_INSTALL_DIR": dest}
	if hostOS != "windows" {
		env["PATH"] = minimalPath()
	}
	if r := runInstaller(t, srv, env, "v0.9.0"); r.code != 0 {
		t.Fatalf("install: %d", r.code)
	}

	home := testHome(t)
	dirs, credential := runtimeStateDirs(home)
	keep := []string{credential, filepath.Join(home, "keep.txt")}
	for _, dir := range dirs {
		keep = append(keep, filepath.Join(filepath.Dir(dir), "not-purlview", "keep.txt"))
	}
	for _, file := range keep {
		writeSentinel(t, file)
	}
	for _, dir := range dirs {
		writeSentinel(t, filepath.Join(dir, "nested", "sentinel"))
	}
	// The same directories of whoever runs the tests, observed but never
	// written: whatever exists there now must still exist afterwards.
	var realBefore []string
	if realHome := realUserDirs()["home"]; realHome != "" {
		realDirs, _ := runtimeStateDirs(realHome)
		for _, dir := range realDirs {
			if exists(dir) {
				realBefore = append(realBefore, dir)
			}
		}
	}

	r := runUninstall(t, srv, env)
	if r.code != 0 {
		t.Fatalf("uninstall: code=%d\n%s", r.code, r.output)
	}
	for _, dir := range dirs {
		if exists(dir) {
			t.Errorf("runtime state %s must be removed", dir)
		}
	}
	for _, file := range keep {
		if !exists(file) {
			t.Errorf("uninstall removed %s, which is not runtime state", file)
		}
	}
	const marker = "removed runtime state "
	removed := 0
	for _, line := range strings.Split(r.output, "\n") {
		_, path, ok := strings.Cut(strings.TrimSpace(line), marker)
		if !ok {
			continue
		}
		removed++
		if !within(path, home) {
			t.Errorf("uninstall removed %s, which is outside the test home %s", path, home)
		}
	}
	if removed != len(dirs) {
		t.Errorf("uninstall reported %d removed directories, want %d:\n%s", removed, len(dirs), r.output)
	}
	for _, dir := range realBefore {
		if !exists(dir) {
			t.Errorf("%s of the user running the tests disappeared during the test", dir)
		}
	}
}

// The helper refuses the environment the tests used to run with, and any
// other that leaves a per-user location pointing at the real user.
func TestIsolatedEnvRefusesRealUserDirectories(t *testing.T) {
	own := realUserDirs()
	if own["home"] == "" {
		t.Skip("no home directory to protect")
	}
	good := isolatedEnv(t, map[string]string{"PURLVIEW_INSTALL_DIR": t.TempDir()})
	if err := checkIsolated(good, own); err != nil {
		t.Fatalf("the helper's own environment was refused: %v", err)
	}
	effective := effectiveEnv(good)
	for _, name := range userDirVars {
		if !within(effective[name], testHome(t)) {
			t.Errorf("%s=%s is outside the test home", name, effective[name])
		}
	}
	for name := range effective {
		if strings.HasPrefix(strings.ToUpper(name), "PURLVIEW_") && name != "PURLVIEW_INSTALL_DIR" {
			t.Errorf("%s was inherited from the process environment", name)
		}
	}

	if err := checkIsolated(os.Environ(), own); err == nil {
		t.Error("the unchanged process environment must be refused")
	}
	homeVar := "HOME"
	if hostOS == "windows" {
		homeVar = "USERPROFILE"
	}
	for _, tc := range []struct{ name, item string }{
		{"real home set last", homeVar + "=" + own["home"]},
		{"real home with a trailing separator", homeVar + "=" + own["home"] + string(filepath.Separator)},
		{"unset state location", "XDG_STATE_HOME="},
		{"unset local application data", "LOCALAPPDATA="},
		{"state location inside the real home", "XDG_STATE_HOME=" + filepath.Join(own["home"], ".local", "state")},
		{"local application data inside the real home", "LOCALAPPDATA=" + filepath.Join(own["home"], "AppData", "Local")},
		{"runtime location inside the real home", "XDG_RUNTIME_DIR=" + filepath.Join(own["home"], "purlview-test-runtime")},
	} {
		if err := checkIsolated(append(append([]string{}, good...), tc.item), own); err == nil {
			t.Errorf("%s: %q must be refused", tc.name, tc.item)
		}
	}
}

func writeSentinel(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// within reports whether path is dir or lies below it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
