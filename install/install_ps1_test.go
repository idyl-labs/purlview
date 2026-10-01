package install

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// powershellHosts returns the PowerShell executables to test with: Windows
// PowerShell 5.1 (what users have by default) and PowerShell 7 when present.
func powershellHosts(t *testing.T) []string {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("install.ps1 end-to-end tests run on Windows")
	}
	var hosts []string
	for _, name := range []string{"powershell", "pwsh"} {
		if p, err := exec.LookPath(name); err == nil {
			hosts = append(hosts, p)
		}
	}
	if len(hosts) == 0 {
		t.Skip("no PowerShell host available")
	}
	return hosts
}

// psEnv is env for install.ps1 with PURLVIEW_NO_MODIFY_PATH=1 unless env
// sets it: the user Path lives in the registry, which no environment
// variable moves, so only TestPsAddsDestToUserPath lets the script change it.
func psEnv(env map[string]string) map[string]string {
	vars := map[string]string{"PURLVIEW_NO_MODIFY_PATH": "1"}
	for name, value := range env {
		vars[name] = value
	}
	return vars
}

func runPs(t *testing.T, host string, srv *releaseServer, env map[string]string, args ...string) result {
	t.Helper()
	script, err := filepath.Abs("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	cmdArgs := append([]string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script}, args...)
	cmd := exec.Command(host, cmdArgs...)
	// As runSh: a throwaway USERPROFILE, LOCALAPPDATA and APPDATA.
	cmd.Env = append(isolatedEnv(t, psEnv(env)), "PURLVIEW_RELEASE_BASE_URL="+srv.URL)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run install.ps1: %v", err)
		}
		code = ee.ExitCode()
	}
	t.Logf("> %s install.ps1 %s\n%s", filepath.Base(host), strings.Join(args, " "), out)
	return result{code: code, output: string(out)}
}

func TestPsInstallUpgradeAndUninstall(t *testing.T) {
	for _, host := range powershellHosts(t) {
		t.Run(filepath.Base(host), func(t *testing.T) {
			old := buildFixture(t, "0.9.0")
			newer := buildFixture(t, "0.9.1")
			srv := newReleaseServer(t, old, newer)
			srv.setLatest(newer.tag)
			dest := filepath.Join(t.TempDir(), "Purlview ünïcode", "bin")
			env := map[string]string{"PURLVIEW_INSTALL_DIR": dest}

			r := runPs(t, host, srv, env, "-Version", "v0.9.0")
			if r.code != 0 || installedVersion(t, dest) != "purlview 0.9.0" {
				t.Fatalf("pinned install failed: code=%d version=%q", r.code, installedVersion(t, dest))
			}
			if !r.contains("checksum verified") || !r.contains("is not on your PATH") {
				t.Errorf("expected checksum and PATH messages")
			}

			r = runPs(t, host, srv, env)
			if r.code != 0 || installedVersion(t, dest) != "purlview 0.9.1" {
				t.Fatalf("upgrade to latest failed: code=%d version=%q", r.code, installedVersion(t, dest))
			}

			r = runPs(t, host, srv, env, "-Version", "0.9.0")
			if r.code != 0 || installedVersion(t, dest) != "purlview 0.9.0" {
				t.Fatalf("explicit pinned reinstall failed: code=%d", r.code)
			}
			if entries, _ := os.ReadDir(dest); len(entries) != 1 {
				t.Errorf("destination must contain only the executable, got %d entries", len(entries))
			}

			r = runPs(t, host, srv, env, "-Uninstall")
			if r.code != 0 || exists(filepath.Join(dest, "purlview.exe")) {
				t.Fatalf("uninstall failed: code=%d", r.code)
			}
		})
	}
}

func TestPsFailuresPreserveExistingInstall(t *testing.T) {
	for _, host := range powershellHosts(t) {
		t.Run(filepath.Base(host), func(t *testing.T) {
			old := buildFixture(t, "0.9.0")
			newer := buildFixture(t, "0.9.1")
			srv := newReleaseServer(t, old, newer)
			srv.setLatest(newer.tag)
			dest := t.TempDir()
			env := map[string]string{"PURLVIEW_INSTALL_DIR": dest}
			if r := runPs(t, host, srv, env, "-Version", "v0.9.0"); r.code != 0 {
				t.Fatalf("initial install failed: %d", r.code)
			}
			before, err := os.ReadFile(filepath.Join(dest, "purlview.exe"))
			if err != nil {
				t.Fatal(err)
			}

			cases := []struct {
				name    string
				setup   func()
				args    []string
				wantMsg string
			}{
				{"corrupt checksum", func() { srv.setFaults(true, false, false) }, nil, "checksum mismatch"},
				{"truncated download", func() { srv.setFaults(false, true, false) }, nil, "checksum mismatch"},
				{"unpublished version", func() {}, []string{"-Version", "v9.9.9"}, "could not download checksums.txt"},
				{"invalid version", func() {}, []string{"-Version", "nope"}, "invalid version"},
				{"no latest release", func() { srv.setFaults(false, false, true) }, nil, "no stable release is published"},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					srv.setFaults(false, false, false)
					tc.setup()
					r := runPs(t, host, srv, env, tc.args...)
					if r.code == 0 {
						t.Fatalf("expected failure")
					}
					if !r.contains(tc.wantMsg) {
						t.Errorf("output missing %q", tc.wantMsg)
					}
					after, err := os.ReadFile(filepath.Join(dest, "purlview.exe"))
					if err != nil || string(after) != string(before) {
						t.Fatalf("existing installation was modified by a failed run")
					}
					if entries, _ := os.ReadDir(dest); len(entries) != 1 {
						t.Errorf("failed run left files behind in %s", dest)
					}
				})
			}
			srv.setFaults(false, false, false)
		})
	}
}

// A release's executable runs for the first time only after its Authenticode
// signature is verified as Purlview's publisher's: one nobody signed is
// refused, never run and never installed. A release served from the loopback
// interface is checked only when PURLVIEW_VERIFY_PUBLISHER=1 asks.
func TestPsRunsNothingThePublisherDidNotSign(t *testing.T) {
	for _, host := range powershellHosts(t) {
		t.Run(filepath.Base(host), func(t *testing.T) {
			unsigned := markerFixture(t, "0.9.2")
			srv := newReleaseServer(t, unsigned)
			dest := t.TempDir()
			marker := filepath.Join(t.TempDir(), "ran")
			env := map[string]string{"PURLVIEW_INSTALL_DIR": dest, "INSTALL_TEST_MARKER": marker, "PURLVIEW_VERIFY_PUBLISHER": "1"}

			r := runPs(t, host, srv, env, "-Version", "v0.9.2")
			if r.code == 0 || !r.contains("is not signed by Purlview's publisher") || !r.contains("it was not run and nothing was installed") {
				t.Fatalf("an unsigned executable must be refused: code=%d", r.code)
			}
			if exists(marker) {
				t.Fatal("the unsigned executable ran")
			}
			if exists(filepath.Join(dest, "purlview.exe")) {
				t.Fatal("the unsigned executable was installed")
			}

			// The fixture does run once nothing asks for its signature: the
			// marker above was absent because the executable never started.
			delete(env, "PURLVIEW_VERIFY_PUBLISHER")
			if r := runPs(t, host, srv, env, "-Version", "v0.9.2"); r.code != 0 || !exists(marker) {
				t.Fatalf("the fixture must install from the loopback interface: code=%d ran=%v", r.code, exists(marker))
			}
		})
	}
}

func TestPsReplacesRunningExecutable(t *testing.T) {
	for _, host := range powershellHosts(t) {
		t.Run(filepath.Base(host), func(t *testing.T) {
			old := buildFixture(t, "0.9.0")
			newer := buildFixture(t, "0.9.1")
			srv := newReleaseServer(t, old, newer)
			srv.setLatest(newer.tag)
			dest := t.TempDir()
			env := map[string]string{"PURLVIEW_INSTALL_DIR": dest}
			if r := runPs(t, host, srv, env, "-Version", "v0.9.0"); r.code != 0 {
				t.Fatalf("initial install failed: %d", r.code)
			}
			// Keep the installed executable running while upgrading. The
			// placeholder commands exit immediately, so hold it open through
			// stdin-driven completion output instead: `completion bash` is
			// quick too, so run it repeatedly in a loop process.
			exe := filepath.Join(dest, "purlview.exe")
			loop := exec.Command("cmd", "/c", "for /l %i in (1,0,2) do @\""+exe+"\" --help >nul")
			loop.Env = isolatedEnv(t, nil)
			if err := loop.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = loop.Process.Kill(); _, _ = loop.Process.Wait() }()

			r := runPs(t, host, srv, env)
			if r.code != 0 {
				t.Fatalf("upgrade while in use must succeed by renaming the old executable: %s", r.output)
			}
			if installedVersion(t, dest) != "purlview 0.9.1" {
				t.Fatalf("upgrade did not take effect: %q", installedVersion(t, dest))
			}
		})
	}
}

func TestPsDetectsManagedInstall(t *testing.T) {
	for _, host := range powershellHosts(t) {
		t.Run(filepath.Base(host), func(t *testing.T) {
			rel := buildFixture(t, "0.9.0")
			srv := newReleaseServer(t, rel)
			srv.setLatest(rel.tag)
			scoop := t.TempDir()
			shims := filepath.Join(scoop, "shims")
			if err := os.MkdirAll(shims, 0o755); err != nil {
				t.Fatal(err)
			}
			managed := filepath.Join(shims, "purlview.exe")
			if err := os.WriteFile(managed, rel.data[:0], 0o755); err != nil {
				t.Fatal(err)
			}
			dest := t.TempDir()
			env := map[string]string{
				"SCOOP":                scoop,
				"PATH":                 shims + string(os.PathListSeparator) + os.Getenv("PATH"),
				"PURLVIEW_INSTALL_DIR": "",
			}
			// Default destination, managed copy on PATH: refuse.
			r := runPs(t, host, srv, env)
			if r.code == 0 || !r.contains("already installed by Scoop") {
				t.Fatalf("expected refusal for a Scoop-managed install, code=%d", r.code)
			}
			// Explicit destination: proceed with a warning.
			r = runPs(t, host, srv, env, "-Destination", dest)
			if r.code != 0 || !r.contains("installing a second copy") || installedVersion(t, dest) != "purlview 0.9.0" {
				t.Fatalf("explicit -Destination must proceed with a warning, code=%d", r.code)
			}
			// Uninstalling a managed copy is refused.
			r = runPs(t, host, srv, env, "-Uninstall", "-Destination", shims)
			if r.code == 0 || !r.contains("managed by Scoop") || !exists(managed) {
				t.Fatalf("uninstall must refuse a managed executable, code=%d", r.code)
			}
		})
	}
}
