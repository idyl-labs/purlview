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
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The commands the website and docs/installation.md print pipe the script
// into the shell: `curl -fsSL https://purlview.com/install.sh | sh` and
// `irm https://purlview.com/install.ps1 | iex`. These tests run the scripts
// the same way, from standard input or as text, never from a file.

// runShPiped runs `sh -s -- args` with script on standard input, as
// `curl ... | sh -s -- args` does.
func runShPiped(t *testing.T, srv *releaseServer, env map[string]string, script []byte, args ...string) result {
	t.Helper()
	cmd := exec.Command("sh", append([]string{"-s", "--"}, args...)...)
	cmd.Stdin = bytes.NewReader(script)
	cmd.Env = append(isolatedEnv(t, env), "PURLVIEW_RELEASE_BASE_URL="+srv.URL)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run piped install.sh: %v", err)
		}
		code = ee.ExitCode()
	}
	t.Logf("$ ... | sh -s -- %s\n%s", strings.Join(args, " "), out)
	return result{code: code, output: string(out)}
}

func TestShPiped(t *testing.T) {
	requireSh(t)
	script, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	old := buildFixture(t, "0.9.0")
	newer := buildFixture(t, "0.9.1")
	srv := newReleaseServer(t, old, newer)
	srv.setLatest(newer.tag)
	dest := filepath.Join(t.TempDir(), "bin")
	env := map[string]string{"PURLVIEW_INSTALL_DIR": dest, "PATH": minimalPath()}

	// A download cut short is a syntax error: nothing runs, nothing is written.
	r := runShPiped(t, srv, env, script[:len(script)/2])
	if r.code == 0 || r.contains("purlview-install:") || exists(dest) {
		t.Fatalf("a truncated script must run nothing: code=%d", r.code)
	}

	r = runShPiped(t, srv, env, script, "--help")
	if r.code != 0 || !r.contains("curl -fsSL https://purlview.com/install.sh | sh") {
		t.Fatalf("--help must work piped: code=%d", r.code)
	}

	r = runShPiped(t, srv, env, script, "--version", "v0.9.0")
	if r.code != 0 || installedVersion(t, dest) != "purlview 0.9.0" {
		t.Fatalf("piped pinned install failed: code=%d version=%q", r.code, installedVersion(t, dest))
	}
	r = runShPiped(t, srv, env, script)
	if r.code != 0 || installedVersion(t, dest) != "purlview 0.9.1" {
		t.Fatalf("piped upgrade to latest failed: code=%d version=%q", r.code, installedVersion(t, dest))
	}
	r = runShPiped(t, srv, env, script, "--uninstall")
	if r.code != 0 || exists(filepath.Join(dest, "purlview")) {
		t.Fatalf("piped uninstall failed: code=%d", r.code)
	}
}

// runPsText runs PowerShell commands that read install.ps1 as text, as
// `irm ... | iex` does, then print what the caller's session was left with.
func runPsText(t *testing.T, host string, srv *releaseServer, env map[string]string, invoke string) result {
	t.Helper()
	script, err := filepath.Abs("install.ps1")
	if err != nil {
		t.Fatal(err)
	}
	text := "(Get-Content -Raw -LiteralPath '" + strings.ReplaceAll(script, "'", "''") + "')"
	command := strings.Join([]string{
		"$before = $env:PSModulePath",
		"$installer = " + text,
		strings.ReplaceAll(invoke, "INSTALLER", "$installer"),
		"'function-left=' + [bool](Get-Command Write-InstallLog -ErrorAction SilentlyContinue)",
		"'preference=' + $ErrorActionPreference",
		"'module-path-kept=' + ($env:PSModulePath -eq $before)",
		"'purlview-at=' + (Get-Command purlview -ErrorAction SilentlyContinue).Source",
	}, "\n")
	cmd := exec.Command(host, "-NoProfile", "-NonInteractive", "-Command", command)
	cmd.Env = append(isolatedEnv(t, psEnv(env)), "PURLVIEW_RELEASE_BASE_URL="+srv.URL)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("run install.ps1 as text: %v", err)
		}
		code = ee.ExitCode()
	}
	t.Logf("> %s %s\n%s", filepath.Base(host), invoke, out)
	return result{code: code, output: string(out)}
}

func TestPsPiped(t *testing.T) {
	for _, host := range powershellHosts(t) {
		t.Run(filepath.Base(host), func(t *testing.T) {
			old := buildFixture(t, "0.9.0")
			newer := buildFixture(t, "0.9.1")
			srv := newReleaseServer(t, old, newer)
			srv.setLatest(newer.tag)
			dest := filepath.Join(t.TempDir(), "bin")
			env := map[string]string{"PURLVIEW_INSTALL_DIR": dest}
			clean := func(r result) {
				t.Helper()
				for _, want := range []string{"function-left=False", "preference=Continue", "module-path-kept=True"} {
					if !r.contains(want) {
						t.Errorf("the caller's session was changed: want %s", want)
					}
				}
			}

			// irm ... | iex: the latest release, with no options.
			r := runPsText(t, host, srv, env, "INSTALLER | Invoke-Expression")
			if installedVersion(t, dest) != "purlview 0.9.1" {
				t.Fatalf("iex install failed: code=%d version=%q", r.code, installedVersion(t, dest))
			}
			clean(r)

			// With options, as a script block.
			r = runPsText(t, host, srv, env, "& ([scriptblock]::Create(INSTALLER)) -Version v0.9.0")
			if installedVersion(t, dest) != "purlview 0.9.0" {
				t.Fatalf("pinned install as a script block failed: code=%d version=%q", r.code, installedVersion(t, dest))
			}
			clean(r)

			r = runPsText(t, host, srv, env, "& ([scriptblock]::Create(INSTALLER)) -Uninstall")
			if exists(filepath.Join(dest, "purlview.exe")) {
				t.Fatalf("uninstall as a script block failed: code=%d", r.code)
			}
			clean(r)
		})
	}
}
