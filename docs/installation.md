# Installing Purlview

Purlview is a single native executable, `purlview`, for macOS, Linux and
Windows on 64-bit Intel/AMD (`amd64`) and 64-bit Arm (`arm64`). No language
runtime is needed.

## Supported systems

| OS | Architectures | Minimum version | Basis |
| --- | --- | --- | --- |
| macOS | arm64 (Apple silicon), amd64 (Intel) | macOS 13 Ventura | Go 1.27 deployment baseline |
| Linux | arm64, amd64 | kernel 3.2+, any libc | statically linked |
| Windows | arm64, amd64 | Windows 10 | Go 1.27 deployment baseline |

Older systems may work but are not tested.

## The local daemon and upgrades

The plain-language summary is [How Purlview updates](updates.md).

The first `purlview share` starts a small per-user background process, the
daemon, that will own your shares; you install nothing extra and need no
administrator rights. **Upgrading or uninstalling Purlview stops that
daemon and ends any active shares**; start a share again afterwards. Each
share also checks, best effort and metadata only, whether a newer release
exists and prints a notice on stderr with the upgrade command for your
installation. Purlview never downloads or installs an update by itself.

When the daemon is stopped depends on the route:

| Route | Daemon stop during the upgrade | Notes |
| --- | --- | --- |
| `install.sh` / `install.ps1` | yes: after the new release is downloaded and verified, the script runs `purlview daemon stop --maintenance`, replaces the executable, then `purlview daemon resume` | a failed replacement keeps the previous executable; ended shares stay ended |
| Homebrew cask | yes: the cask's `uninstall_preflight` hook runs `purlview daemon stop --maintenance` as your user before the old files are removed (`brew upgrade` and `brew uninstall`), and the new cask's `postflight` runs `purlview daemon resume` | |
| `.deb` / `.rpm` download | no hook: `dpkg`/`rpm` run as root and must not stop other users' daemons | the file is replaced in place; the next `purlview share` (or `purlview daemon start`) notices the outdated daemon, stops it and starts the installed version; a share active during the package transaction keeps running until then |
| Scoop | yes: the manifest's `pre_install` runs the installed copy's `purlview daemon stop --maintenance` before Scoop replaces it, and `post_install` runs the new copy's `purlview daemon resume` | errors never fail the install, and the maintenance marker expires after 3 minutes |
| WinGet (portable; not yet available) | no hook | `winget upgrade` cannot replace a running executable; an idle daemon has exited within 60 seconds, otherwise run `purlview daemon stop` first |
| `go install` | no hook | the next `purlview share` retires the old daemon |

`purlview daemon stop` is the manual escape hatch for every route; it is a
hidden maintenance command, documented in [daemon.md](daemon.md). Uninstall
through the installer scripts also removes the daemon's runtime state
(sockets, locks, logs, update cache) but not account credentials, which a
`purlview logout` removes locally; package managers remove only their
own files.

## Routes

| Audience | Preferred route | Fallback |
| --- | --- | --- |
| macOS | `curl -fsSL https://purlview.com/install.sh \| sh` | `brew install --cask idyl-labs/tap/purlview`; signed, notarized archive |
| Linux (and WSL) | `curl -fsSL https://purlview.com/install.sh \| sh` | APT or DNF repository; portable `.tar.gz`, `.deb`, `.rpm` |
| Windows | `irm https://purlview.com/install.ps1 \| iex` | `scoop bucket add idyl-labs https://github.com/idyl-labs/scoop-bucket` then `scoop install purlview`; signed archive; WinGet once it is available (below) |
| Go developers | `go install github.com/idyl-labs/purlview/cmd/purlview@<tag>` | build from a checkout (see the README) |

After any route, check that the executable runs:

```sh
purlview --version
```

### Native installer (recommended: macOS, Linux, WSL, Windows)

The scripts are `install/install.sh` and `install/install.ps1` in this
repository. They download the archive for your
system over HTTPS, verify its SHA-256 against the release's `checksums.txt`,
check that the executable runs, and then replace the previous file in one
step. They refuse to overwrite an executable owned by Homebrew, Scoop,
WinGet, dpkg or rpm.

Like rustup and uv, they also put the install folder on your PATH when it
is not there yet, so there is no manual step:

| Shell | What the installer changes |
| --- | --- |
| zsh | appends a marked block to `~/.zshrc` (`$ZDOTDIR/.zshrc` if set) |
| bash on Linux and WSL | appends a marked block to `~/.bashrc` |
| bash on macOS | appends a marked block to `~/.bash_profile` if it exists, otherwise `~/.profile`, and to `~/.bashrc` if it exists |
| fish | writes `~/.config/fish/conf.d/purlview.fish`, which runs `fish_add_path --path` |
| any other shell | appends a marked block to `~/.profile` |
| PowerShell | adds the folder to your user Path (never the machine Path) and to the PATH of the window it runs in |

The block is two lines: a comment naming the Purlview installer, and a line
that adds the folder once however often the file is read. It is written only
when the folder is not on PATH, never twice, and never with `sudo` or outside
your home directory. The installer prints each file it changed. A running
shell cannot pick up the change from a piped script, so on macOS, Linux and
WSL open a new terminal (or run `exec $SHELL -l`) before the first
`purlview login`; `irm ... | iex` updates the PowerShell window it runs in.
Uninstalling removes exactly what was added. To leave PATH alone, pass
`--no-modify-path` (sh) or `-NoModifyPath` (PowerShell), or set
`PURLVIEW_NO_MODIFY_PATH=1`.

macOS, Linux and WSL:

```sh
curl -fsSL https://purlview.com/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://purlview.com/install.ps1 | iex
```

Both are safe to pipe. `install.sh` is one function called on its last
line, so a download cut short is a syntax error and runs nothing;
PowerShell parses the whole script before running it, and `install.ps1`
runs in a scope of its own, so it leaves no settings or functions in your
session. With options, pass them after the script:

```sh
curl -fsSL https://purlview.com/install.sh | sh -s -- --version v0.2.0
curl -fsSL https://purlview.com/install.sh | sh -s -- --uninstall
```

```powershell
& ([scriptblock]::Create((irm https://purlview.com/install.ps1))) -Version v0.2.0
& ([scriptblock]::Create((irm https://purlview.com/install.ps1))) -Uninstall
```

To read a script before it runs, download it first:

```sh
curl -fsSLo install.sh https://purlview.com/install.sh && sh install.sh
```

```powershell
Invoke-WebRequest https://purlview.com/install.ps1 -OutFile install.ps1; powershell -ExecutionPolicy Bypass -File install.ps1
```

| Option (sh) | Option (PowerShell) | Effect |
| --- | --- | --- |
| `--version vX.Y.Z` | `-Version vX.Y.Z` | Pin a release. The default is the latest stable release; prereleases are only installed when named explicitly. |
| `--dest DIR` | `-Destination DIR` | Install location. Defaults: `~/.local/bin`; `%LOCALAPPDATA%\Programs\purlview`. |
| `--arch amd64|arm64` | `-Architecture` | Override detection. A shell under Rosetta or x64 emulation still gets the native build. |
| `--no-modify-path` | `-NoModifyPath` | Leave PATH and shell startup files alone; the installer says how to add the folder yourself. |
| `--uninstall` | `-Uninstall` | Remove the executable installed by the script, and the PATH entry it added. |
| `PURLVIEW_NO_MODIFY_PATH=1` | same | As `--no-modify-path`. `-AddToPath` (PowerShell, from older instructions) overrides it. |
| `PURLVIEW_RELEASE_BASE_URL` | same | Release repository URL, for mirrors and tests. |

Update by running the script again; the script stops the daemon (ending
active shares) only after the new release is downloaded and verified, and
blocks daemon starts while it replaces the file. Corporate proxies are
honoured through the usual `HTTPS_PROXY` settings and the Windows system
proxy; TLS checks are never disabled. On Windows, if another `purlview.exe`
process is still running after the daemon stopped, the installer renames the
old file aside; if even that fails it reports it and leaves the previous
version in place; close the process and retry.

### Homebrew (macOS, also Linux)

```sh
brew install --cask idyl-labs/tap/purlview
brew upgrade --cask purlview
brew uninstall --cask purlview
```

The cask installs the executable, the manual pages and shell completions.
It selects the architecture automatically. Prereleases are never published
to the tap, `idyl-labs/homebrew-tap`. Upgrading or uninstalling stops your daemon first (the cask says so in its
caveats).

### WinGet (Windows; not yet available)

The `IdylLabs.Purlview` package is not yet in the WinGet community
repository (`microsoft/winget-pkgs`), so `winget install` does not find it.
Use the installer script or Scoop meanwhile. Once the package has been
accepted there:

```powershell
winget install IdylLabs.Purlview
winget upgrade IdylLabs.Purlview
winget uninstall IdylLabs.Purlview
```

The manifest uses WinGet's portable package support (zip with a nested
portable executable, alias `purlview`, user scope) for x64 and arm64.

### Scoop (Windows)

```powershell
scoop bucket add idyl-labs https://github.com/idyl-labs/scoop-bucket
scoop install purlview
scoop update purlview
scoop uninstall purlview
```

### APT (Debian, Ubuntu)

```sh
sudo curl -fsSL https://pkg.purlview.com/purlview-archive-keyring.gpg -o /usr/share/keyrings/purlview-archive-keyring.gpg
sudo curl -fsSL https://pkg.purlview.com/deb/purlview-stable.sources -o /etc/apt/sources.list.d/purlview.sources
sudo apt update && sudo apt install purlview
```

`sudo apt upgrade` then keeps it current, and `sudo apt remove purlview`
removes it. The repository and its packages are
signed with the key at `https://pkg.purlview.com/purlview.asc`.

### DNF (Fedora, RHEL family)

```sh
sudo curl -fsSL https://pkg.purlview.com/rpm/purlview-stable.repo -o /etc/yum.repos.d/purlview.repo
sudo dnf install purlview
```

`sudo dnf upgrade` then keeps it current, and `sudo dnf remove purlview`
removes it; dnf asks you to confirm the key's fingerprint the first time.

Replace `stable` with `prerelease` in either address to follow prereleases too.

### Linux packages

Each release ships `purlview_<version>_linux_<arch>.deb` and `.rpm` with the
executable, manual pages and completions for bash, zsh and fish.

```sh
sudo dpkg -i purlview_<version>_linux_amd64.deb      # Debian, Ubuntu
sudo dnf install ./purlview_<version>_linux_amd64.rpm # Fedora, RHEL family
sudo dpkg -r purlview                                 # remove
sudo dnf remove purlview
```

A downloaded package does not give you automatic updates; install the next
release the same way, or use the APT or DNF repository above.

### From source

```sh
go install github.com/idyl-labs/purlview/cmd/purlview@latest
```

The result reports `built by: go install` and the version it was installed
at. It checks for updates as a release does, and its update notice gives the
`go install` command above.
It is a developer route, not the user route; the README describes building
from a checkout.

## Verifying a download

`checksums.txt` is published with every release and lists every archive and
package. It proves integrity (nothing was corrupted or truncated), not who
published it, because it comes from the same place as the archive.

```sh
sha256sum --ignore-missing -c checksums.txt                                    # Linux
shasum -a 256 --ignore-missing -c checksums.txt                                # macOS
```

`checksums.txt.sigstore.json` is a keyless Sigstore signature made by
Purlview's release workflow. Verifying it with the identity below proves the
file came from that workflow at that tag.

```sh
cosign verify-blob \
  --certificate-identity "https://github.com/idyl-labs/purlview/.github/workflows/release.yml@refs/tags/v<version>" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --bundle checksums.txt.sigstore.json checksums.txt
```

[releasing.md](releasing.md) describes how releases are built and signed.

macOS executables are signed with a Developer ID certificate (team
`2B6R3TC7HJ`) and notarized; Windows executables carry a timestamped
Authenticode signature of `Idyl Labs, Inc.`. Neither requires extra tools
from users: Gatekeeper and Windows verify them.

On macOS and Windows the installer scripts verify the publisher before the
downloaded executable runs for the first time, and neither run nor install
one that fails. On Linux they can only when `cosign` is installed:

- `install.sh` on macOS requires the executable's Developer ID signature to be
  that team's;
- `install.ps1` requires a valid Authenticode signature of that publisher;
- `install.sh`, on macOS and Linux, runs the `cosign` check above when
  `cosign` is on PATH, and then requires it to pass.

Linux executables carry no signature of their own. Without `cosign`, a script
install on Linux rests on HTTPS and the checksum, and says so; for a verified
publisher on Linux, install `cosign` first or use the APT and DNF
repositories, which are signed.

`--uninstall` (`-Uninstall`) removes the executable in its directory. It stops
the daemon and removes its runtime, log and cache directories only when no
other `purlview` stays on PATH: those belong to the user, not to one copy.

## Shell completion

Archives and packages include completion files (`completions/`) and manual
pages (`man/`). Homebrew and the Linux packages install them. Otherwise:

```sh
purlview completion bash > ~/.local/share/bash-completion/completions/purlview
purlview completion zsh > "${fpath[1]}/_purlview"
purlview completion fish > ~/.config/fish/completions/purlview.fish
```

```powershell
purlview completion powershell | Out-String | Invoke-Expression   # or add to $PROFILE
```

`purlview completion <shell> --help` prints the loading instructions for
that shell.
