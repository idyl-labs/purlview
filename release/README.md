# Release helpers

Used by [`.goreleaser.yaml`](../.goreleaser.yaml) and the workflows in
[`.github/workflows`](../.github/workflows); [docs/releasing.md](../docs/releasing.md)
describes the whole release.

| File | Role |
| --- | --- |
| `preflight.sh` | tag, commit, CI checks, destination and credential validation |
| `sign-macos.sh` | Developer ID signing and notarization in an ephemeral keychain |
| `apple/DeveloperIDG2CA.crt` | Apple's public Developer ID G2 intermediate, fingerprint-pinned in `sign-macos.sh` |
| `release-notes.sh` | release notes with verification instructions |
| `publish.sh` | idempotent draft, upload, publish |
| `publish-manifest.sh` | commit a cask or Scoop manifest, refusing downgrades |
| `publish-winget.sh` | WinGet pull request through a fork |
| `package-repository.sh` | add a release's `.deb` and `.rpm` to the signed APT and DNF repositories at pkg.purlview.com |
| `test-package-repository.sh` | container test: build the repositories with a throwaway key, then install with apt and dnf |
| `packages/purlview.asc` | the APT and DNF repositories' public signing key |
| `smoke.sh`, `smoke.ps1` | execute and inspect the artifacts on the current machine, including a daemon start, reuse and stop in a private state directory (and a cross-user check when running as root in the Linux containers) |
| `channel-check.sh`, `channel-check.ps1` | install a published release through one public channel and check it (`install-check.yml`) |

The Go helpers live with the other tools:

| Package | Role |
| --- | --- |
| `internal/tools/applysigned` | GoReleaser post-build hook that swaps in natively signed binaries after a rebuild hash check |
| `internal/tools/semvercmp` | SemVer comparison for the scripts, prerelease-aware; shares `internal/semver` with the CLI's update check |
| `internal/tools/gendocs` | shell completions and manual pages for the archives and packages |
| `internal/tools/notices` | `THIRD_PARTY_NOTICES` from the modules linked into the executable; the release build refuses a stale one |
