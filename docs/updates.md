# How Purlview updates

Purlview is one executable. It runs as the `purlview` command and, while
something is shared, as a background **daemon** that holds the tunnels.
Updating replaces the executable; the daemon then has to be replaced too,
because it is still running the old one.

## The short version

- **Purlview never updates itself.** You choose when, with your usual
  install method (table below). Nothing is downloaded behind your back.
- **It tells you when there is an update.** After a `share`, a two-line
  notice on stderr names the new version and the exact command for your
  install method. It is checked at most once a day, shown at most once a day
  per version, and only in a terminal.
- **Updating ends running shares when the old daemon stops.** Most install
  methods stop it during the update; with the others it keeps serving until
  your next `purlview share` or `purlview daemon stop` (table below). Either
  way, run `purlview share` again afterwards. Nothing else is lost: you stay
  signed in, and the new daemon starts on the next share.

## Update commands

| Installed with | Update with | The old daemon is stopped |
| --- | --- | --- |
| `install.sh` (macOS, Linux, WSL) | run the same install command again | by the installer, before replacing the executable |
| `install.ps1` (Windows) | run the same install command again | by the installer, before replacing the executable |
| Homebrew | `brew upgrade --cask purlview` | by the cask, before the old files are removed |
| Scoop | `scoop update purlview` | by the manifest, before the old copy is removed |
| WinGet (once the package is available) | `winget upgrade IdylLabs.Purlview` | not by WinGet: stop it first with `purlview daemon stop` if a share is running |
| `.deb` / `.rpm` | install the new package | not by the package (it runs as root, across users): your next `purlview share` stops the old daemon and starts the new one |
| `go install` | `go install github.com/idyl-labs/purlview/cmd/purlview@latest` | your next `purlview share` does it |

`purlview daemon stop` always works as a manual stop, on any install method.

## What happens in detail

1. **The check.** On a `share`, the daemon asks GitHub's releases API for the
   latest release: one small request, metadata only, answered from a cache
   for 24 hours. Builds from a checkout (`built by: source`) never check; a
   `go install` of a release tag does.
2. **The notice.** A newer version prints, after the share's own output:

   ```
   A newer Purlview is available: 0.2.0 (you have 0.1.0)
   Update: brew upgrade --cask purlview — updating stops running shares
   ```
3. **The update.** The installers, Homebrew and Scoop stop the daemon in
   *maintenance* mode first: new daemon starts are refused for up to three
   minutes, so a share started mid-update cannot bring back the old
   executable. They resume when the new executable is in place. WinGet,
   `.deb`/`.rpm` and `go install` replace the executable without stopping the
   daemon; its shares stay live until step 4 or a manual stop.
4. **Afterwards.** The next `purlview share` (or `purlview daemon start`)
   compares the running daemon with the executable on disk. If they differ in
   version, build or file, it stops the old daemon, starts the new one, and
   says `Purlview was updated — shares started before the update have
   stopped`. Other commands (`list`, `stop`) talk to whichever daemon is
   running and leave it alone.

## Turning the check off

Set any of these to `1` (or anything but empty, `0` or `false`):

- `PURLVIEW_NO_UPDATE_CHECK`: Purlview's own switch.
- `DO_NOT_TRACK`: the common convention for tools that phone home.
- `CI`: set by CI systems; nobody reads the notice there.

With any of them set, no request is made and no notice is shown.

See [installation](installation.md) for install methods and verifying
downloads, and [the daemon](daemon.md) for the daemon's protocol, logs and
maintenance commands.
