# Purlview

**Share your running app. Improve it together.**

Purlview gives the people you are working with a link to the app running on
your machine. They open it while you watch, you change the code, and they see
the change through the app's own hot reload, on the same link, until you stop
the share or it expires.

```sh
purlview share 3000                       # anyone with the link
purlview share 3000 --to ana@example.com  # only Ana, after she verifies her email
purlview share 5173 8000                  # a front end and its API, on one link
```

This repository holds the `purlview` command-line interface, the per-user
daemon that serves your shares, the Go SDK for Purlview's account API, and
the installer scripts. Releases are published at
[idyl-labs/purlview-releases](https://github.com/idyl-labs/purlview-releases).

## Install

macOS, Linux and WSL:

```sh
curl -fsSL https://purlview.com/install.sh | sh
```

Windows PowerShell:

```powershell
irm https://purlview.com/install.ps1 | iex
```

Homebrew, WinGet, Scoop, APT, DNF, the release archives, and how to verify
a download, are in [docs/installation.md](docs/installation.md).

## How it works

`purlview share` starts a daemon that belongs to your OS user. The daemon
creates the share through Purlview's account API, then keeps an outbound
connection to Purlview's edge (QUIC, with TCP as a fallback) on which the
edge forwards your visitors' requests to the app on your machine. Nothing
listens on a port of yours, and a share never outlives its expiry, at most one
hour. Each share has its own key; its private half never leaves the daemon.

- [docs/daemon.md](docs/daemon.md): the daemon's lifecycle, its private
  control protocol, its files, and the update check.
- [docs/updates.md](docs/updates.md): how Purlview tells you about a new
  version and how updating works.
- [docs/cli-scenarios.md](docs/cli-scenarios.md): generated transcripts of
  every command against a fake platform.
- [sdk/README.md](sdk/README.md): the Go SDK and version 1 of the account
  API.

## Layout

| Path | Contents |
| --- | --- |
| `cmd/purlview` | The executable's entry point. |
| `internal/command` | The command tree, its output and exit codes. |
| `internal/daemon`, `internal/ipc` | The per-user daemon and its private control protocol. |
| `internal/share`, `internal/share/engine` | Share requests and the daemon's share engine. |
| `internal/tunnel` | Docking a share on the edge and serving its targets. |
| `internal/scenario`, `internal/testplatform` | The scenario runner's fake world and the fake platform for integration tests. |
| `internal/tools` | The scenario runner and the generator of completions and manual pages. |
| `sdk/` | The Go SDK: `api`, `resource`, `purlview` (client) and `apiserver` (server binding). |
| `install/` | `install.sh`, `install.ps1` and their end-to-end tests. |

## Build from source

You need Go 1.27.1 or later (with `GOTOOLCHAIN=auto`, any Go 1.21 or later
fetches it) and Git.

```sh
git clone https://github.com/idyl-labs/purlview.git
cd purlview
go build -trimpath -o purlview ./cmd/purlview
./purlview --version
```

`go install github.com/idyl-labs/purlview/cmd/purlview@latest` builds and
installs it in one step.

A build from source speaks the same protocol to the Purlview service as a
release. It reports `built by: source` (or `go install`) and never shows an
update notice. A plain `go build` of a checkout reports its version as
`devel`; `go install …@vX.Y.Z` reports that version. When the service
requires a minimum client version, it refuses new shares from an older or
`devel` build with `update_required`. Release builds set their version with
`-ldflags "-X github.com/idyl-labs/purlview/internal/buildinfo.version=<version>"`,
and so can a source build of a release tag.

### The Hyperplane Go client

The daemon docks shares on Purlview's edge with the Hyperplane Go client,
`github.com/idyl-labs/hyperplane-go`, an ordinary public Go module. Building
needs nothing beyond Go and the public module proxy.

## Configuration

An installed `purlview` needs no configuration: it uses the Purlview service.
For development against another deployment:

| Variable | Effect |
| --- | --- |
| `PURLVIEW_PLATFORM_ENDPOINT` | The account API origin. Setting it turns every production default off; `none` runs with no platform at all. |
| `PURLVIEW_ENVIRONMENT` | A name for that deployment. Any platform other than production gets a daemon, logs, cache and credential of its own. |
| `PURLVIEW_CA_FILE` | An extra CA certificate to trust for that deployment. TLS verification is never disabled. |
| `PURLVIEW_STATE_DIR` | Moves every runtime, log, cache and credential path under one directory. |
| `PURLVIEW_NO_UPDATE_CHECK` | Turns the update check off, as `DO_NOT_TRACK` and `CI` do. |
| `PURLVIEW_DEBUG` | `1` adds the underlying cause to an error. |

## Contributing and security

See [CONTRIBUTING.md](CONTRIBUTING.md) for building, testing and the
Developer Certificate of Origin. Report vulnerabilities privately, as
[SECURITY.md](SECURITY.md) describes.

## Licence

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE).
