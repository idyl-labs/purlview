# Installers

`install.sh` (POSIX sh, macOS and Linux) and `install.ps1` (Windows
PowerShell 5.1 and PowerShell 7). Usage and guarantees are documented in
[docs/installation.md](../docs/installation.md). `go test ./install`
runs them end to end against a loopback fixture server, including corrupt and
truncated downloads, executables the publisher did not sign (refused before
they run; `PURLVIEW_VERIFY_PUBLISHER=1` makes the scripts check a fixture,
which nothing signs), unsupported architectures, unwritable destinations,
package-manager-owned installs, paths with spaces and non-ASCII characters,
pinned reinstalls, uninstall, PATH (the block install.sh writes for zsh,
bash, fish and other shells, and that a new shell then finds purlview; the
user Path install.ps1 writes, on CI runners only; reruns, opt-outs and
uninstall), and the daemon integration: an upgrade stops a
running daemon (ending its work) before replacing the executable, a failed
replacement keeps the old executable without restoring that work, and
uninstall stops the daemon and removes its runtime state.

The scripts delete per-user directories and stop "this user's" daemon, so the
tests never run them, or an executable they installed, with your own home
directory: every script and every installed executable runs with an
environment from `isolatedEnv` (`isolation_test.go`), which moves `HOME`, the
XDG base directories, `ZDOTDIR`, `USERPROFILE`, `LOCALAPPDATA` and `APPDATA` under a
per-test temporary directory, drops inherited `PURLVIEW_*` settings, and fails
the test before anything runs if one of them is unset, still names the real
location or lies inside your home directory (outside the temporary directory,
where the throwaway homes live). The Windows user Path lives in the registry,
which no environment variable moves, so the PowerShell tests run with
`PURLVIEW_NO_MODIFY_PATH=1`, except `TestPsAddsDestToUserPath`, which runs
only where `CI` is set and puts the previous value back afterwards.
