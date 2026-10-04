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

// runSh runs install.sh with the test's variables on top of isolatedEnv, so
// the script and the executables it starts see a throwaway home directory.
func runSh(t *testing.T, srv *releaseServer, env map[string]string, args ...string) result {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"install.sh"}, args...)...)
	cmd.Env = append(isolatedEnv(t, env), "PURLVIEW_RELEASE_BASE_URL="+srv.URL)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run install.sh: %v", err)
		}
		code = ee.ExitCode()
	}
	t.Logf("$ sh install.sh %s\n%s", strings.Join(args, " "), out)
	return result{code: code, output: string(out)}
}

func requireSh(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("install.sh targets macOS and Linux")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
}

func TestShInstallUpgradeAndUninstall(t *testing.T) {
	requireSh(t)
	old := buildFixture(t, "0.9.0")
	newer := buildFixture(t, "0.9.1")
	srv := newReleaseServer(t, old, newer)
	srv.setLatest(newer.tag)
	// Directory with spaces and non-ASCII characters, not on PATH.
	dest := filepath.Join(t.TempDir(), "Purlview ünïcode", "bin")
	env := map[string]string{"PURLVIEW_INSTALL_DIR": dest, "PATH": minimalPath(), "SHELL": "/bin/sh"}

	r := runSh(t, srv, env, "--version", "v0.9.0")
	if r.code != 0 || installedVersion(t, dest) != "purlview 0.9.0" {
		t.Fatalf("pinned install failed: code=%d version=%q", r.code, installedVersion(t, dest))
	}
	if !r.contains("checksum verified") || !r.contains("added "+dest+" to PATH in "+filepath.Join(testHome(t), ".profile")) {
		t.Errorf("expected checksum and PATH messages in output")
	}

	r = runSh(t, srv, env)
	if r.code != 0 || installedVersion(t, dest) != "purlview 0.9.1" {
		t.Fatalf("upgrade to latest failed: code=%d version=%q", r.code, installedVersion(t, dest))
	}
	if !r.contains("latest stable release: v0.9.1") {
		t.Errorf("latest resolution message missing")
	}

	r = runSh(t, srv, env, "--version", "0.9.0")
	if r.code != 0 || installedVersion(t, dest) != "purlview 0.9.0" {
		t.Fatalf("explicit pinned reinstall (downgrade) failed: code=%d version=%q", r.code, installedVersion(t, dest))
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 1 {
		t.Errorf("destination must contain only the executable, got %d entries", len(entries))
	}

	r = runSh(t, srv, env, "--uninstall")
	if r.code != 0 || exists(filepath.Join(dest, "purlview")) {
		t.Fatalf("uninstall failed: code=%d", r.code)
	}
	r = runSh(t, srv, env, "--uninstall")
	if r.code == 0 || !r.contains("nothing to remove") {
		t.Fatalf("second uninstall must fail clearly: code=%d", r.code)
	}
}

func TestShFailuresPreserveExistingInstall(t *testing.T) {
	requireSh(t)
	old := buildFixture(t, "0.9.0")
	newer := buildFixture(t, "0.9.1")
	srv := newReleaseServer(t, old, newer)
	srv.setLatest(newer.tag)
	dest := t.TempDir()
	env := map[string]string{"PURLVIEW_INSTALL_DIR": dest, "PATH": minimalPath()}

	if r := runSh(t, srv, env, "--version", "v0.9.0"); r.code != 0 {
		t.Fatalf("initial install failed: %d", r.code)
	}
	before, err := os.ReadFile(filepath.Join(dest, "purlview"))
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
		{"unpublished version", func() {}, []string{"--version", "v9.9.9"}, "could not download checksums.txt"},
		{"invalid version", func() {}, []string{"--version", "nope"}, "invalid version"},
		{"unsupported architecture", func() {}, []string{"--arch", "s390x"}, "unsupported --arch"},
		{"no latest release", func() { srv.setFaults(false, false, true) }, nil, "no stable release is published"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv.setFaults(false, false, false)
			tc.setup()
			r := runSh(t, srv, env, tc.args...)
			if r.code == 0 {
				t.Fatalf("expected failure")
			}
			if !r.contains(tc.wantMsg) {
				t.Errorf("output missing %q", tc.wantMsg)
			}
			after, err := os.ReadFile(filepath.Join(dest, "purlview"))
			if err != nil || string(after) != string(before) {
				t.Fatalf("existing installation was modified by a failed run")
			}
			if entries, _ := os.ReadDir(dest); len(entries) != 1 {
				t.Errorf("failed run left files behind in %s", dest)
			}
		})
	}
	srv.setFaults(false, false, false)
}

func TestShRefusesUnwritableDestination(t *testing.T) {
	requireSh(t)
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	rel := buildFixture(t, "0.9.0")
	srv := newReleaseServer(t, rel)
	dest := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dest, 0o555); err != nil {
		t.Fatal(err)
	}
	r := runSh(t, srv, map[string]string{"PURLVIEW_INSTALL_DIR": dest, "PATH": minimalPath()}, "--version", "v0.9.0")
	if r.code == 0 || !r.contains("cannot write to") {
		t.Fatalf("expected a clear permission error, code=%d", r.code)
	}
}

func TestShDetectsManagedInstall(t *testing.T) {
	requireSh(t)
	rel := buildFixture(t, "0.9.0")
	srv := newReleaseServer(t, rel)
	srv.setLatest(rel.tag)

	// A fake Homebrew prefix with a managed purlview on PATH.
	prefix := t.TempDir()
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(prefix, "bin", "purlview")
	if err := os.WriteFile(managed, []byte("#!/bin/sh\necho managed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	brew := filepath.Join(prefix, "bin", "brew")
	if err := os.WriteFile(brew, []byte("#!/bin/sh\necho \""+prefix+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(prefix, "bin") + string(os.PathListSeparator) + minimalPath()

	dest := t.TempDir()
	r := runSh(t, srv, map[string]string{"PURLVIEW_INSTALL_DIR": dest, "PATH": path})
	if r.code == 0 || !r.contains("already installed by Homebrew") {
		t.Fatalf("expected refusal for a Homebrew-managed install, code=%d", r.code)
	}
	if exists(filepath.Join(dest, "purlview")) {
		t.Fatalf("nothing must be installed after a refusal")
	}

	r = runSh(t, srv, map[string]string{"PATH": path}, "--dest", dest)
	if r.code != 0 || !r.contains("installing a second copy") || installedVersion(t, dest) != "purlview 0.9.0" {
		t.Fatalf("explicit --dest must proceed with a warning, code=%d", r.code)
	}

	r = runSh(t, srv, map[string]string{"PATH": path}, "--uninstall", "--dest", filepath.Join(prefix, "bin"))
	if r.code == 0 || !r.contains("managed by Homebrew") || !exists(managed) {
		t.Fatalf("uninstall must refuse to remove a managed executable, code=%d", r.code)
	}
}

// A release's executable runs for the first time only after its publisher's
// signature is verified. On macOS that is the executable's own Developer ID
// signature: one nobody signed is refused, never run, and the installation
// already there stays. A release served from the loopback interface is
// checked only when PURLVIEW_VERIFY_PUBLISHER=1 asks.
func TestShRunsNothingThePublisherDidNotSign(t *testing.T) {
	requireSh(t)
	if runtime.GOOS != "darwin" {
		t.Skip("only macOS executables carry a signature of their own")
	}
	old := buildFixture(t, "0.9.0")
	unsigned := markerFixture(t, "0.9.2")
	srv := newReleaseServer(t, old, unsigned)
	dest := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	// The system's own tools only: a cosign on the developer's machine would
	// refuse the fixture first (the next test).
	env := map[string]string{"PURLVIEW_INSTALL_DIR": dest, "PATH": "/usr/bin:/bin:/usr/sbin:/sbin", "INSTALL_TEST_MARKER": marker}
	if r := runSh(t, srv, env, "--version", "v0.9.0"); r.code != 0 {
		t.Fatalf("initial install failed: %d", r.code)
	}

	env["PURLVIEW_VERIFY_PUBLISHER"] = "1"
	r := runSh(t, srv, env, "--version", "v0.9.2")
	if r.code == 0 || !r.contains("is not signed by Purlview's publisher") || !r.contains("it was not run and nothing was installed") {
		t.Fatalf("an unsigned executable must be refused: code=%d", r.code)
	}
	if exists(marker) {
		t.Fatal("the unsigned executable ran")
	}
	if got := installedVersion(t, dest); got != "purlview 0.9.0" {
		t.Fatalf("the installation changed: %q", got)
	}

	// The fixture does run once nothing asks for its signature: the marker
	// above was absent because the executable never started.
	delete(env, "PURLVIEW_VERIFY_PUBLISHER")
	if r := runSh(t, srv, env, "--version", "v0.9.2"); r.code != 0 || !exists(marker) {
		t.Fatalf("the fixture must install from the loopback interface: code=%d ran=%v", r.code, exists(marker))
	}
}

// With cosign installed, the Sigstore signature of checksums.txt must be the
// release workflow's for the version: a release without the signature, or
// with one cosign rejects, is refused before its executable runs.
func TestShVerifiesTheSigstoreSignatureWhenCosignIsInstalled(t *testing.T) {
	requireSh(t)
	rel := markerFixture(t, "0.9.2")
	signed := markerFixture(t, "0.9.3")
	signed.bundle = "{}"
	srv := newReleaseServer(t, rel, signed)

	// A cosign that records its arguments and exits as COSIGN_EXIT says.
	tools := t.TempDir()
	calls := filepath.Join(t.TempDir(), "cosign.args")
	if err := os.WriteFile(filepath.Join(tools, "cosign"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$COSIGN_CALLS\"\nexit \"${COSIGN_EXIT:-0}\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	run := func(tag, exit string, verify bool) (result, string) {
		t.Helper()
		_ = os.Remove(calls)
		dest := t.TempDir()
		env := map[string]string{"PURLVIEW_INSTALL_DIR": dest, "PATH": tools + string(os.PathListSeparator) + minimalPath(),
			"INSTALL_TEST_MARKER": marker, "COSIGN_CALLS": calls, "COSIGN_EXIT": exit}
		if verify {
			env["PURLVIEW_VERIFY_PUBLISHER"] = "1"
		}
		return runSh(t, srv, env, "--version", tag), dest
	}

	r, dest := run("v0.9.2", "0", true)
	if r.code == 0 || !r.contains("could not download the Sigstore signature of checksums.txt") || exists(calls) {
		t.Fatalf("a release without a Sigstore signature must be refused: code=%d", r.code)
	}
	if exists(marker) || exists(filepath.Join(dest, "purlview")) {
		t.Fatal("the executable of an unsigned release ran or was installed")
	}

	r, dest = run("v0.9.3", "1", true)
	if r.code == 0 || !r.contains("Sigstore verification of checksums.txt failed for v0.9.3") {
		t.Fatalf("a signature cosign rejects must be refused: code=%d", r.code)
	}
	if exists(marker) || exists(filepath.Join(dest, "purlview")) {
		t.Fatal("the executable of a release with a bad signature ran or was installed")
	}

	// A signature cosign accepts: asked for the release workflow's identity
	// at this tag. macOS then asks for the executable's own signature too.
	r, _ = run("v0.9.3", "0", true)
	asked, _ := os.ReadFile(calls)
	if !r.contains("checksums.txt signature verified with cosign") ||
		!strings.Contains(string(asked), "--certificate-identity https://github.com/idyl-labs/purlview/.github/workflows/release.yml@refs/tags/v0.9.3 --certificate-oidc-issuer https://token.actions.githubusercontent.com") {
		t.Fatalf("cosign was not asked for the release workflow's identity: %q", asked)
	}
	if runtime.GOOS == "darwin" {
		if r.code == 0 || !r.contains("is not signed by Purlview's publisher") || exists(marker) {
			t.Fatalf("macOS must still refuse the unsigned executable: code=%d", r.code)
		}
	} else if r.code != 0 || !exists(marker) {
		t.Fatalf("a release cosign verified must install: code=%d", r.code)
	}

	// A fixture on the loopback interface is not checked unless asked.
	_ = os.Remove(marker)
	if r, _ = run("v0.9.2", "1", false); r.code != 0 || exists(calls) || !exists(marker) {
		t.Fatalf("an unchecked fixture must install without cosign: code=%d", r.code)
	}
}

// The daemon's runtime state is the user's, shared by every copy: removing
// one copy while another stays on PATH leaves it, and removing the last one
// clears it.
func TestShUninstallLeavesAnotherCopysRuntimeState(t *testing.T) {
	requireSh(t)
	rel := buildFixture(t, "0.9.0")
	srv := newReleaseServer(t, rel)
	kept, extra := t.TempDir(), t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	env := map[string]string{"PATH": kept + string(os.PathListSeparator) + minimalPath(), "PURLVIEW_STATE_DIR": state, "PURLVIEW_NO_MODIFY_PATH": "1"}
	for _, dest := range []string{kept, extra} {
		if r := runSh(t, srv, env, "--version", "v0.9.0", "--dest", dest); r.code != 0 {
			t.Fatalf("install to %s failed: %d", dest, r.code)
		}
	}
	daemonLog := filepath.Join(state, "logs", "daemon.log")
	credential := filepath.Join(state, "state", "credentials.json")
	writeSentinel(t, daemonLog)
	writeSentinel(t, credential)

	r := runSh(t, srv, env, "--uninstall", "--dest", extra)
	if r.code != 0 || exists(filepath.Join(extra, "purlview")) || !r.contains("another purlview remains at "+filepath.Join(kept, "purlview")) {
		t.Fatalf("uninstalling the second copy: code=%d", r.code)
	}
	if !exists(daemonLog) || r.contains("removed runtime state") {
		t.Fatal("the other copy's runtime state was removed")
	}

	r = runSh(t, srv, env, "--uninstall", "--dest", kept)
	if r.code != 0 || !r.contains("removed runtime state "+filepath.Join(state, "logs")) || exists(daemonLog) {
		t.Fatalf("uninstalling the last copy must clear the runtime state: code=%d", r.code)
	}
	if !exists(credential) {
		t.Fatal("uninstall removed the account credential")
	}
}

// minimalPath keeps the tools the installer needs without any purlview.
func minimalPath() string {
	return "/usr/bin:/bin:/usr/sbin:/sbin:/usr/local/bin:/opt/homebrew/bin"
}
