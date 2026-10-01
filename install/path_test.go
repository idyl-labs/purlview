package install

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// PATH: when the destination is not on PATH, install.sh puts it there for
// new terminals the way rustup and uv do, with one marked block at the end of
// the startup file of the user's shell. Every file it writes is under the
// test's throwaway home (isolatedEnv moves HOME, ZDOTDIR and XDG_CONFIG_HOME
// there).

const pathMark = "# Added by the Purlview installer (https://purlview.com); remove with install.sh --uninstall"

// shPathLine is the line of the block for dir, as the startup file spells it.
func shPathLine(dir string) string {
	return `case ":$PATH:" in *":` + dir + `:"*) ;; *) export PATH="` + dir + `:$PATH" ;; esac`
}

func fishPathLine(dir string) string { return `fish_add_path --path "` + dir + `"` }

// blocks counts the blocks with line in the file at path.
func blocks(t *testing.T, path, line string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(data), "\n")
	n := 0
	for i := 0; i+1 < len(lines); i++ {
		if lines[i] == pathMark && lines[i+1] == line {
			n++
		}
	}
	return n
}

// startupFiles are all the files install.sh may write, relative to home.
var startupFiles = []string{".zshrc", ".bashrc", ".bash_profile", ".bash_login", ".profile", ".config/fish/conf.d/purlview.fish"}

// shellPath is where the named shell is on this machine, or a path where it
// is not when it is not installed.
func shellPath(name string) (path string, installed bool) {
	if p, err := exec.LookPath(name); err == nil {
		return p, true
	}
	return "/nonexistent/bin/" + name, false
}

// newShellFinds starts shell as a new terminal window does, with the test's
// home and the system's default PATH, and returns where it finds purlview.
func newShellFinds(t *testing.T, shell string, env map[string]string) string {
	t.Helper()
	const probe = "command -v purlview"
	var args []string
	switch filepath.Base(shell) {
	case "zsh":
		args = []string{"-i", "-c", probe}
	case "bash":
		// Terminal windows on macOS start login shells; on Linux they do not.
		if hostOS == "darwin" {
			args = []string{"-l", "-i", "-c", probe}
		} else {
			args = []string{"-i", "-c", probe}
		}
	case "fish":
		args = []string{"-c", probe}
	default:
		args = []string{"-l", "-c", probe}
	}
	vars := map[string]string{}
	for name, value := range env {
		vars[name] = value
	}
	vars["PATH"] = "/usr/bin:/bin:/usr/sbin:/sbin"
	vars["SHELL"] = shell
	vars["TERM"] = "dumb"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, args...)
	cmd.Env = isolatedEnv(t, vars)
	out, err := cmd.Output()
	t.Logf("$ %s %s (%v)\n%s", shell, strings.Join(args, " "), err, out)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func TestShAddsDestToPathForTheUsersShell(t *testing.T) {
	requireSh(t)
	rel := buildFixture(t, "0.9.0")
	srv := newReleaseServer(t, rel)
	srv.setLatest(rel.tag)

	zsh, hasZsh := shellPath("zsh")
	bash, hasBash := shellPath("bash")
	fish, hasFish := shellPath("fish")
	bashFiles := []string{".bashrc"}
	if hostOS == "darwin" {
		bashFiles = []string{".profile"}
	}
	bashProfiles := []string{".bashrc"}
	if hostOS == "darwin" {
		bashProfiles = []string{".bash_profile", ".bashrc"}
	}
	const userContent = "# the user's own settings\nexport EDITOR=vi\n"

	for _, tc := range []struct {
		name      string
		shell     string
		installed bool     // the shell exists, so a new one can be started
		existing  []string // startup files the user has already
		zdotdir   string   // ZDOTDIR relative to home, if set
		want      []string // files that get the block
	}{
		{name: "zsh", shell: zsh, installed: hasZsh, want: []string{".zshrc"}},
		{name: "zsh with ZDOTDIR", shell: zsh, installed: hasZsh, zdotdir: ".config/zsh", want: []string{".config/zsh/.zshrc"}},
		{name: "zsh with a zshrc", shell: zsh, installed: hasZsh, existing: []string{".zshrc", ".profile"}, want: []string{".zshrc"}},
		{name: "bash", shell: bash, installed: hasBash, want: bashFiles},
		{name: "bash with profiles", shell: bash, installed: hasBash, existing: []string{".bash_profile", ".bashrc"}, want: bashProfiles},
		{name: "fish", shell: fish, installed: hasFish, want: []string{".config/fish/conf.d/purlview.fish"}},
		{name: "another shell", shell: "/bin/sh", installed: true, want: []string{".profile"}},
		{name: "no SHELL", shell: "", want: []string{".profile"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := testHome(t)
			// The default destination, below the home directory.
			dest := filepath.Join(home, ".local", "bin")
			env := map[string]string{"PATH": minimalPath(), "SHELL": tc.shell}
			if tc.zdotdir != "" {
				env["ZDOTDIR"] = filepath.Join(home, tc.zdotdir)
				if err := os.MkdirAll(env["ZDOTDIR"], 0o700); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range tc.existing {
				if err := os.WriteFile(filepath.Join(home, name), []byte(userContent), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			lineFor := func(name string) string {
				if strings.HasSuffix(name, ".fish") {
					return fishPathLine("$HOME/.local/bin")
				}
				return shPathLine("$HOME/.local/bin")
			}
			candidates := append([]string{}, startupFiles...)
			if tc.zdotdir != "" {
				candidates = append(candidates, tc.zdotdir+"/.zshrc")
			}
			untouched := func() {
				t.Helper()
				for _, name := range candidates {
					if slices.Contains(tc.want, name) {
						continue
					}
					data, err := os.ReadFile(filepath.Join(home, name))
					switch {
					case slices.Contains(tc.existing, name) && string(data) != userContent:
						t.Errorf("%s was changed:\n%s", name, data)
					case !slices.Contains(tc.existing, name) && err == nil:
						t.Errorf("%s was created:\n%s", name, data)
					}
				}
			}

			r := runSh(t, srv, env)
			if r.code != 0 || installedVersion(t, dest) != "purlview 0.9.0" {
				t.Fatalf("install failed: code=%d", r.code)
			}
			for _, name := range tc.want {
				file := filepath.Join(home, name)
				if n := blocks(t, file, lineFor(name)); n != 1 {
					data, _ := os.ReadFile(file)
					t.Fatalf("%s has %d blocks, want 1:\n%s", name, n, data)
				}
				if !r.contains("added " + dest + " to PATH in " + file) {
					t.Errorf("output does not name %s", file)
				}
			}
			untouched()
			next := "next: open a new terminal (or run 'exec " + tc.shell + " -l'), then run 'purlview login'"
			if tc.shell == "" {
				next = "next: open a new terminal, then run 'purlview login'"
			}
			if !r.contains(next) || r.contains("is not on your PATH") {
				t.Errorf("output must end with %q", next)
			}
			if tc.installed {
				if got := newShellFinds(t, tc.shell, env); got != filepath.Join(dest, "purlview") {
					t.Errorf("a new %s finds purlview at %q, want %s", filepath.Base(tc.shell), got, filepath.Join(dest, "purlview"))
				}
			}

			// Again: nothing is added twice.
			r = runSh(t, srv, env)
			if r.code != 0 {
				t.Fatalf("reinstall failed: code=%d", r.code)
			}
			for _, name := range tc.want {
				file := filepath.Join(home, name)
				if n := blocks(t, file, lineFor(name)); n != 1 {
					t.Errorf("%s has %d blocks after a reinstall, want 1", name, n)
				}
				if !r.contains(file + " already adds " + dest + " to PATH") {
					t.Errorf("reinstall output does not say %s already adds the destination", file)
				}
			}
			untouched()

			r = runSh(t, srv, env, "--uninstall")
			if r.code != 0 {
				t.Fatalf("uninstall failed: code=%d", r.code)
			}
			for _, name := range tc.want {
				file := filepath.Join(home, name)
				data, err := os.ReadFile(file)
				switch {
				case slices.Contains(tc.existing, name):
					if string(data) != userContent {
						t.Errorf("uninstall must leave %s as it was, got:\n%s", name, data)
					}
				case strings.HasSuffix(name, ".fish"):
					if err == nil {
						t.Errorf("uninstall must delete %s, which only held the block", name)
					}
				default:
					if err != nil || len(data) != 0 {
						t.Errorf("uninstall must leave the %s it created empty, got %q (%v)", name, data, err)
					}
				}
				if !r.contains("removed the PATH line for " + dest + " from " + file) {
					t.Errorf("uninstall output does not name %s", file)
				}
			}
			untouched()
		})
	}
}

func TestShLeavesPathAloneWhenDestIsOnPath(t *testing.T) {
	requireSh(t)
	rel := buildFixture(t, "0.9.0")
	srv := newReleaseServer(t, rel)
	srv.setLatest(rel.tag)
	dest := t.TempDir()
	env := map[string]string{
		"PURLVIEW_INSTALL_DIR": dest,
		"PATH":                 dest + string(os.PathListSeparator) + minimalPath(),
		"SHELL":                "/bin/zsh",
	}
	r := runSh(t, srv, env)
	if r.code != 0 {
		t.Fatalf("install failed: code=%d", r.code)
	}
	for _, name := range startupFiles {
		if exists(filepath.Join(testHome(t), name)) {
			t.Errorf("%s was written although the destination is on PATH", name)
		}
	}
	if r.contains("PATH in") || r.contains("is not on your PATH") || !r.contains("next: run 'purlview login'") {
		t.Errorf("with the destination on PATH, the output says nothing about PATH and names the first command")
	}
}

func TestShLeavesStartupFilesAlone(t *testing.T) {
	requireSh(t)
	rel := buildFixture(t, "0.9.0")
	srv := newReleaseServer(t, rel)
	srv.setLatest(rel.tag)
	for _, tc := range []struct {
		name string
		env  map[string]string
		args []string
		dest string // relative to a temporary directory
	}{
		{name: "--no-modify-path", args: []string{"--no-modify-path"}},
		{name: "PURLVIEW_NO_MODIFY_PATH=1", env: map[string]string{"PURLVIEW_NO_MODIFY_PATH": "1"}},
		// A destination that would need escaping in a startup file.
		{name: "a destination with a double quote", dest: `say "hi"`},
		{name: "a destination with a dollar sign", dest: `$HOME`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), tc.dest, "bin")
			env := map[string]string{"PURLVIEW_INSTALL_DIR": dest, "PATH": minimalPath(), "SHELL": "/bin/zsh"}
			for name, value := range tc.env {
				env[name] = value
			}
			r := runSh(t, srv, env, tc.args...)
			if r.code != 0 || installedVersion(t, dest) != "purlview 0.9.0" {
				t.Fatalf("install failed: code=%d", r.code)
			}
			for _, name := range startupFiles {
				if exists(filepath.Join(testHome(t), name)) {
					t.Errorf("%s was written", name)
				}
			}
			if !r.contains(dest+" is not on your PATH") || r.contains("PATH in") {
				t.Errorf("the output must say how to add the destination to PATH, and change nothing")
			}
			if r := runSh(t, srv, env, "--uninstall"); r.code != 0 {
				t.Fatalf("uninstall failed: code=%d", r.code)
			}
		})
	}
}

func TestShUnwritableStartupFile(t *testing.T) {
	requireSh(t)
	if os.Geteuid() == 0 {
		t.Skip("root can write anywhere")
	}
	rel := buildFixture(t, "0.9.0")
	srv := newReleaseServer(t, rel)
	srv.setLatest(rel.tag)
	dest := t.TempDir()
	// As home-manager leaves it: read-only.
	zshrc := filepath.Join(testHome(t), ".zshrc")
	if err := os.WriteFile(zshrc, []byte("export EDITOR=vi\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	r := runSh(t, srv, map[string]string{"PURLVIEW_INSTALL_DIR": dest, "PATH": minimalPath(), "SHELL": "/bin/zsh"})
	if r.code != 0 || installedVersion(t, dest) != "purlview 0.9.0" {
		t.Fatalf("an unwritable startup file must not fail the install: code=%d", r.code)
	}
	if data, _ := os.ReadFile(zshrc); string(data) != "export EDITOR=vi\n" {
		t.Errorf(".zshrc changed:\n%s", data)
	}
	if !r.contains("warning: could not write to "+zshrc) || !r.contains(dest+" is not on your PATH") {
		t.Errorf("the output must say the file could not be written, and how to add the destination to PATH")
	}
}

// Uninstall removes the block it added, and only that, from files the user
// has kept editing since.
func TestShUninstallRemovesOnlyTheBlock(t *testing.T) {
	requireSh(t)
	rel := buildFixture(t, "0.9.0")
	srv := newReleaseServer(t, rel)
	srv.setLatest(rel.tag)
	home := testHome(t)
	dest := t.TempDir()
	env := map[string]string{"PURLVIEW_INSTALL_DIR": dest, "PATH": minimalPath(), "SHELL": "/bin/zsh"}

	// ~/.zshrc is a link into a dotfiles checkout, and its last line has no
	// newline.
	dotfiles := filepath.Join(home, "dotfiles")
	if err := os.MkdirAll(dotfiles, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dotfiles, "zshrc")
	before := "export EDITOR=vi\n\n# the user's own PATH line, which stays\nexport PATH=\"" + dest + ":$PATH\""
	if err := os.WriteFile(target, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	zshrc := filepath.Join(home, ".zshrc")
	if err := os.Symlink(target, zshrc); err != nil {
		t.Fatal(err)
	}

	if r := runSh(t, srv, env); r.code != 0 {
		t.Fatalf("install failed: code=%d", r.code)
	}
	if n := blocks(t, target, shPathLine(dest)); n != 1 {
		data, _ := os.ReadFile(target)
		t.Fatalf("the linked file has %d blocks, want 1:\n%s", n, data)
	}
	// The user goes on editing the file.
	const later = "alias ll='ls -l'\n"
	f, err := os.OpenFile(target, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(later); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if r := runSh(t, srv, env, "--uninstall"); r.code != 0 {
		t.Fatalf("uninstall failed: code=%d", r.code)
	}
	if fi, err := os.Lstat(zshrc); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("~/.zshrc must still be a link")
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if want := before + "\n" + later; string(data) != want {
		t.Errorf("uninstall must remove the block and nothing else:\ngot:\n%s\nwant:\n%s", data, want)
	}

	// With the executable deleted by hand, uninstall still removes the
	// block, and then there is nothing left to remove.
	if r := runSh(t, srv, env); r.code != 0 {
		t.Fatalf("reinstall failed: code=%d", r.code)
	}
	if err := os.Remove(filepath.Join(dest, "purlview")); err != nil {
		t.Fatal(err)
	}
	r := runSh(t, srv, env, "--uninstall")
	if r.code != 0 || blocks(t, target, shPathLine(dest)) != 0 {
		t.Fatalf("uninstall without the executable must still remove the block: code=%d", r.code)
	}
	r = runSh(t, srv, env, "--uninstall")
	if r.code == 0 || !r.contains("nothing to remove") {
		t.Fatalf("a second uninstall must fail clearly: code=%d", r.code)
	}
}

// On Windows, install.ps1 appends the destination to the user Path in the
// registry (HKCU\Environment) and to the PATH of the session that ran it.

// userPathValue is the user Path as the registry stores it.
type userPathValue struct {
	present bool
	kind    string // a RegistryValueKind: ExpandString or String
	value   string
}

// psCommand runs PowerShell commands with the test's environment.
func psCommand(t *testing.T, host, script string) (string, error) {
	t.Helper()
	cmd := exec.Command(host, "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = isolatedEnv(t, nil)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func readUserPath(t *testing.T, host string) userPathValue {
	t.Helper()
	out, err := psCommand(t, host, `$k = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment')
$v = $k.GetValue('Path', $null, 'DoNotExpandEnvironmentNames')
if ($null -eq $v) { 'absent' } else { $k.GetValueKind('Path').ToString() + ' ' + [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($v)) }`)
	if err != nil {
		t.Fatalf("read the user Path: %v\n%s", err, out)
	}
	if out == "absent" {
		return userPathValue{}
	}
	kind, encoded, _ := strings.Cut(out, " ")
	value, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("read the user Path: %v\n%s", err, out)
	}
	return userPathValue{present: true, kind: kind, value: string(value)}
}

func writeUserPath(t *testing.T, host string, v userPathValue) error {
	t.Helper()
	script := "$k = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')\n"
	if v.present {
		script += "$k.SetValue('Path', [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String('" +
			base64.StdEncoding.EncodeToString([]byte(v.value)) + "')), '" + v.kind + "')"
	} else {
		script += "$k.DeleteValue('Path', $false)"
	}
	if out, err := psCommand(t, host, script); err != nil {
		return fmt.Errorf("%w: %s", err, out)
	}
	return nil
}

func TestPsAddsDestToUserPath(t *testing.T) {
	hosts := powershellHosts(t)
	if os.Getenv("CI") == "" {
		t.Skip("changes the user Path in the registry, which no environment variable moves; runs on throwaway CI runners")
	}
	for _, host := range hosts {
		t.Run(filepath.Base(host), func(t *testing.T) {
			rel := buildFixture(t, "0.9.0")
			srv := newReleaseServer(t, rel)
			srv.setLatest(rel.tag)
			saved := readUserPath(t, host)
			t.Cleanup(func() {
				if err := writeUserPath(t, host, saved); err != nil {
					t.Errorf("restore the user Path: %v", err)
				}
			})
			// A user Path as Windows keeps it: REG_EXPAND_SZ, with a variable
			// that must stay unexpanded.
			start := userPathValue{present: true, kind: "ExpandString", value: `%USERPROFILE%\AppData\Local\Microsoft\WindowsApps;C:\purlview-test\extra`}
			if err := writeUserPath(t, host, start); err != nil {
				t.Fatal(err)
			}
			// ASCII: Windows PowerShell writes redirected output in the OEM
			// code page, and the output is checked for this path.
			dest := filepath.Join(t.TempDir(), "Purlview test", "bin")
			env := map[string]string{"PURLVIEW_INSTALL_DIR": dest, "PURLVIEW_NO_MODIFY_PATH": "0"}

			// irm ... | iex: the user Path and this session's PATH.
			r := runPsText(t, host, srv, env, "INSTALLER | Invoke-Expression")
			if installedVersion(t, dest) != "purlview 0.9.0" {
				t.Fatalf("install failed: code=%d", r.code)
			}
			if got := readUserPath(t, host); got != (userPathValue{true, "ExpandString", start.value + ";" + dest}) {
				t.Fatalf("user Path after install: %+v", got)
			}
			if !r.contains("added "+dest+" to your user Path") || !r.contains("next: run 'purlview login'") {
				t.Errorf("the output must name the change and the first command")
			}
			if !r.contains("purlview-at=" + filepath.Join(dest, "purlview.exe")) {
				t.Errorf("purlview must work in the same session right away")
			}

			// Again, as a script file in a process of its own: nothing is added
			// twice, and only a new terminal has it.
			r = runPs(t, host, srv, env)
			if r.code != 0 {
				t.Fatalf("reinstall failed: code=%d", r.code)
			}
			if got := readUserPath(t, host); got.value != start.value+";"+dest {
				t.Errorf("a reinstall changed the user Path: %q", got.value)
			}
			if r.contains("added") || !r.contains("next: open a new terminal, then run 'purlview login'") {
				t.Errorf("a reinstall adds nothing, and a new terminal has purlview")
			}

			// Already on this session's PATH and in the user Path: nothing to do.
			onPath := map[string]string{"PURLVIEW_INSTALL_DIR": dest, "PURLVIEW_NO_MODIFY_PATH": "0", "PATH": dest + ";" + os.Getenv("PATH")}
			r = runPs(t, host, srv, onPath)
			if r.code != 0 || r.contains("added") || !r.contains("next: run 'purlview login'") {
				t.Errorf("with the destination on PATH, nothing is added: code=%d", r.code)
			}

			// Opting out leaves the user Path alone.
			for _, opt := range []struct {
				name string
				env  map[string]string
				args []string
			}{
				{"-NoModifyPath", map[string]string{"PURLVIEW_NO_MODIFY_PATH": "0"}, []string{"-NoModifyPath"}},
				{"PURLVIEW_NO_MODIFY_PATH=1", map[string]string{"PURLVIEW_NO_MODIFY_PATH": "1"}, nil},
			} {
				other := filepath.Join(t.TempDir(), "bin")
				opt.env["PURLVIEW_INSTALL_DIR"] = other
				r := runPs(t, host, srv, opt.env, opt.args...)
				if r.code != 0 || installedVersion(t, other) != "purlview 0.9.0" {
					t.Fatalf("%s: install failed: code=%d", opt.name, r.code)
				}
				if got := readUserPath(t, host); got.value != start.value+";"+dest {
					t.Errorf("%s changed the user Path: %q", opt.name, got.value)
				}
				if !r.contains(other + " is not on your PATH") {
					t.Errorf("%s: the output must say how to add the destination", opt.name)
				}
			}

			// Uninstall removes the entry and leaves the rest as it was.
			r = runPs(t, host, srv, env, "-Uninstall")
			if r.code != 0 || exists(filepath.Join(dest, "purlview.exe")) {
				t.Fatalf("uninstall failed: code=%d", r.code)
			}
			if got := readUserPath(t, host); got != start {
				t.Errorf("user Path after uninstall: %+v, want %+v", got, start)
			}
			if !r.contains("removed " + dest + " from your user Path") {
				t.Errorf("the uninstall output must name the change")
			}
		})
	}
}
