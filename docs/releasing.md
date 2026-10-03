# Releasing Purlview

This is for maintainers. Users install Purlview as
[installation.md](installation.md) describes.

A release is cut by pushing a tag `vX.Y.Z` to this repository. The tag starts
[`.github/workflows/release.yml`](../.github/workflows/release.yml), which
builds every target, signs the macOS and Windows executables natively,
packages and checksums everything, tests the exact files on six native
runners, publishes them as a GitHub release in
[idyl-labs/purlview-releases](https://github.com/idyl-labs/purlview-releases)
and then updates the package channels.

[GoReleaser](https://goreleaser.com) (OSS, v2.18.1, configured in
[`.goreleaser.yaml`](../.goreleaser.yaml)) is the build and packaging
definition. It never publishes: the workflow runs it with `--skip=publish`
and performs draft, upload, attest, publish and distribute itself, in that
order, so that every stage can be retried safely.

## What a release contains

For version `<version>` (no leading `v`):

| File | Contents |
| --- | --- |
| `purlview_<version>_<os>_<arch>.tar.gz` | macOS and Linux archives: the executable, `LICENSE`, `NOTICE`, shell completions and manual pages |
| `purlview_<version>_windows_<arch>.zip` | Windows archives, with the same contents |
| `purlview_<version>_linux_<arch>.deb`, `.rpm` | Linux packages |
| `<artifact>.sbom.json` | an SPDX software bill of materials for each archive and package |
| `checksums.txt` | SHA-256 of every archive and package |
| `checksums.txt.sigstore.json` | keyless Sigstore signature of `checksums.txt` by the release workflow |

Targets: `darwin`, `linux` and `windows`, each on `amd64` and `arm64`.
Executables are static (`CGO_ENABLED=0`), built with `-trimpath`, with
conservative CPU baselines (`GOAMD64=v1`, `GOARM64=v8.0`) and the commit time
as the file time. The build identity (`purlview --version`) comes from the
linker: version, commit, commit date and `built by: goreleaser`.

The CLI's update check asks the same release repository for the latest
release; there is no other update channel. See [updates.md](updates.md).

## Versions and tags

- `vX.Y.Z` is a stable release. Its tag must point at a commit on `main`.
- `vX.Y.Z-alpha.N`, `vX.Y.Z-beta.N` and `vX.Y.Z-rc.N` are prereleases. Their
  tags may point at any commit, which is how the pipeline is rehearsed from a
  branch. Any other tag shape is rejected.
- A prerelease is marked as a GitHub prerelease, never becomes "latest", is
  never chosen by the installer scripts or the update check of a stable
  build, and never reaches Homebrew, Scoop or WinGet. It does enter the
  `prerelease` suite of the APT and DNF repositories.
- The "latest" mark only moves forward: a stable release older than the
  current latest one (a fix for an older line) is published without taking it.
- Tags are never moved and published releases are never changed. A bad
  release is fixed by a new version.

## Cutting a release

1. Merge the release commit to `main` (or, for a prerelease, push a branch).
   Preflight requires every job of the CI workflow
   ([`ci.yml`](../.github/workflows/ci.yml)) to have succeeded on that exact
   commit. A push to `main` runs them all. For a branch without a pull
   request, run them with `gh workflow run ci.yml --ref <branch>`.
2. Tag the commit and push the tag:

   ```sh
   git tag -a v1.2.3 -m "Purlview v1.2.3"
   git push origin v1.2.3
   ```

3. Approve the run when the `release` environment asks for it, then watch
   it. Every stage writes a summary. Preflight waits while a CI run on the
   commit is still going.

To re-run a release or pass one of the inputs below, dispatch the workflow
**with the tag selected as the ref** (`gh workflow run release.yml --ref
v1.2.3`). Preflight rejects any other ref, because the Sigstore identity must
name the tag.

| Input | Meaning |
| --- | --- |
| `allow_unsigned` | Prerelease only. Skips native signing when its credentials are missing and labels the release an unsigned rehearsal. |
| `allow_unsigned_windows` | Publishes the Windows executables unsigned when Windows signing is not configured, stable releases included. macOS must still be signed and notarized, and the release notes say so. |
| `previous_tag` | Published release to upgrade from in the Linux package check. Defaults to the newest published release older than this tag. |

## Stages

| Job | Runner | What it does |
| --- | --- | --- |
| `preflight` | Linux, `release` environment | Checks the tag format, that the tag's commit is the checked-out commit and the tree is clean, that a stable tag is on `main`, that every CI job succeeded on the commit, which signing credentials are present, and which channels this release will update. Missing signing credentials fail a stable release; a prerelease continues only with `allow_unsigned`. Downloads the previous release's Linux packages for the upgrade check. |
| `build` | Linux | `goreleaser build` for all six targets, recording each unsigned executable's SHA-256. |
| `sign-macos` | macOS, `release` environment | Imports the Developer ID certificate into an ephemeral keychain (with Apple's pinned Developer ID G2 intermediate), signs with the hardened runtime and a secure timestamp, notarizes both executables in one `notarytool` submission, keeps the notarization log, and checks the result with `spctl`. |
| `sign-windows` | Windows, `release` environment | Signs both executables with Microsoft Artifact Signing through Azure OIDC, with an RFC 3161 timestamp, and requires `Get-AuthenticodeSignature` to report them valid and timestamped. |
| `assemble` | Linux | `goreleaser release --skip=publish`: rebuilds every target, and the post-build hook `internal/tools/applysigned` swaps in the signed executable only after proving the rebuild is byte for byte the executable that was signed. Produces the archives, packages, checksums, SBOMs, the Sigstore signature of `checksums.txt` (verified on the spot), the Homebrew, Scoop and WinGet metadata, and the release notes. |
| `verify` | six native runners | Runs `release/smoke.sh` or `release/smoke.ps1` against the exact files: checksums, help, version, completions, signed-out behaviour, a daemon start, reuse and stop in a private state directory, native signature and notarization checks, deb install and upgrade from the previous release, rpm on Fedora, the musl build on Alpine, a cross-user check of the daemon's isolation, and WinGet manifest validation. |
| `publish` | Linux, `release` environment | Creates a draft release in the release repository, uploads every file, records a build provenance attestation for the archives, packages and `checksums.txt`, and publishes the draft. Re-running completes an existing draft and accepts an identical published release; a published release with different checksums fails the job. |
| `distribute-homebrew`, `distribute-scoop` | Linux, `release` environment | Commit the generated cask and manifest to the tap and the bucket. They refuse to move a channel to an older version. |
| `distribute-packages` | Linux, `release` environment | Verifies the Sigstore signature of `checksums.txt`, adds the `.deb` and `.rpm` packages to the signed APT and DNF repositories at `pkg.purlview.com` (stable releases to the `stable` and `prerelease` suites, prereleases to `prerelease` only), and then installs the new version from there with apt on Debian and dnf on Fedora. Refuses downgrades and changed content. |
| `distribute-winget` | Linux, `release` environment | Opens a pull request on `microsoft/winget-pkgs` through a fork. Its review there is asynchronous. |

Homebrew, Scoop and WinGet are updated only for a stable release, published
in a public release repository, whose macOS executables are signed and
notarized.

The unsigned executables are reproducible for a given Go toolchain and
commit, which is what lets `assemble` rebuild them and check them against
what was signed. Signatures, notarization tickets and the Sigstore bundle
depend on time and are not reproducible.

[`install-check.yml`](../.github/workflows/install-check.yml) checks a
published release afterwards: it installs it through every public channel on
every OS and CPU the channel supports, exactly as the installation guide
says, and checks what got installed. Dispatch it once the channels have
updated.

## Configuration

Everything the workflow needs comes from a protected GitHub environment named
`release`. Pull request checks never see it. Its required reviewers approve
each release at `preflight`, before anything is built, and its deployments
are restricted to `v*` tags.

Secrets:

| Name | Used by | Purpose |
| --- | --- | --- |
| `APPLE_DEVELOPER_ID_CERTIFICATE_P12` | sign-macos | base64 of the Developer ID Application certificate and key (.p12) |
| `APPLE_DEVELOPER_ID_CERTIFICATE_PASSWORD` | sign-macos | its password |
| `APPLE_NOTARY_KEY_ID`, `APPLE_NOTARY_ISSUER_ID`, `APPLE_NOTARY_PRIVATE_KEY` | sign-macos | App Store Connect API key for `notarytool` |
| `AZURE_CLIENT_ID`, `AZURE_TENANT_ID`, `AZURE_SUBSCRIPTION_ID` | sign-windows | Entra ID application with a federated credential for this repository's `release` environment and the Artifact Signing Certificate Profile Signer role; no client secret |
| `RELEASE_APP_ID`, `RELEASE_APP_PRIVATE_KEY` | preflight, publish, distribute | GitHub App with Contents read and write on the release repository, the tap and the bucket; each job mints a token for one repository |
| `WINGET_SUBMIT_TOKEN` | distribute-winget | token of the account that owns the `winget-pkgs` fork (a GitHub App cannot open pull requests on `microsoft/winget-pkgs`) |

Variables:

| Name | Purpose |
| --- | --- |
| `APPLE_TEAM_ID` | if set, the signing identity must belong to this Apple team |
| `ARTIFACT_SIGNING_ENDPOINT`, `ARTIFACT_SIGNING_ACCOUNT`, `ARTIFACT_SIGNING_CERTIFICATE_PROFILE` | Microsoft Artifact Signing account |
| `PURLVIEW_PACKAGES_ROLE_ARN` | AWS role the package job assumes through GitHub OIDC; the APT and DNF repositories are skipped while it is unset |
| `PURLVIEW_PACKAGES_AWS_REGION` | region of that role's bucket and secret |
| `PURLVIEW_PACKAGES_BUCKET` | bucket that serves `pkg.purlview.com` |
| `PURLVIEW_PACKAGE_SIGNING_SECRET_ID` | Secrets Manager secret holding the repositories' armored OpenPGP signing key |
| `PURLVIEW_PACKAGE_SIGNING_FINGERPRINT` | fingerprint of `release/packages/purlview.asc`; the key from the secret must match it |
| `PURLVIEW_WINGET_FORK` | `owner/winget-pkgs` fork used for submissions; WinGet is skipped while it is unset |
| `PURLVIEW_WINGET_FORK_OWNER` | owner named in the generated WinGet metadata (default: this organisation) |
| `PURLVIEW_TAP_REPOSITORY`, `PURLVIEW_SCOOP_REPOSITORY` | tap and bucket repository names in this organisation (default: `homebrew-tap`, `scoop-bucket`); point them at test repositories for a rehearsal |
| `PURLVIEW_PRERELEASE_CHANNELS` | `true` sends prereleases to the tap and bucket too; use only with test repositories |
| `PURLVIEW_ALLOW_UNSIGNED_PRERELEASE` | `true` allows unsigned prerelease rehearsals without the dispatch input |

Every job that needs a credential runs in the `release` environment and gets
the narrowest permission it needs. The workflow's own `GITHUB_TOKEN` only ever
reads this repository; writes to other repositories use the GitHub App's
per-job token.

The release repository should have
[immutable releases](https://docs.github.com/en/code-security/how-tos/secure-your-supply-chain/establish-provenance-and-integrity/prevent-release-changes)
enabled: the workflow drafts, attaches every file and only then publishes,
so a release locks with its complete file set.

## Verifying a release

Download `checksums.txt`, `checksums.txt.sigstore.json` and the files you want
from the release, then:

```sh
sha256sum --ignore-missing -c checksums.txt
cosign verify-blob \
  --certificate-identity "https://github.com/idyl-labs/purlview/.github/workflows/release.yml@refs/tags/<tag>" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --bundle checksums.txt.sigstore.json checksums.txt
```

The Sigstore signature proves that `checksums.txt` was produced by this
repository's release workflow at that tag, and the checksums cover every
archive and package. The build provenance attestation covers the same files:

```sh
gh attestation verify purlview_<version>_linux_amd64.tar.gz --repo idyl-labs/purlview
```

The native signatures:

```sh
codesign -dvv purlview                       # macOS: Authority=Developer ID Application: …
spctl --assess --type install -v purlview    # macOS: source=Notarized Developer ID
```

```powershell
Get-AuthenticodeSignature .\purlview.exe      # Windows: Status Valid
```

The APT and DNF repositories are signed with the key in
[`release/packages/purlview.asc`](../release/packages/purlview.asc).

## Rehearsing locally

Without any credential, GoReleaser builds the complete artifact set as a
snapshot, and the smoke test runs against it:

```sh
goreleaser check
goreleaser release --snapshot --clean --skip=publish,sign
release/smoke.sh --dist dist --version "$(jq -r .version dist/metadata.json)" --allow-unsigned
```

The `Release snapshot` workflow does the same, with the Linux package and
container checks, for every pull request that changes the release definition.
`release/test-package-repository.sh --dist dist --version <version>` builds
the APT and DNF repositories from a snapshot with a throwaway key and installs
from them in containers.

## When something fails

- **A job before `publish` failed:** re-run the failed jobs. Artifacts are kept
  for 30 days, so `assemble`, `verify` and `publish` re-run without
  rebuilding.
- **Notarization was rejected:** `sign-macos` fails and uploads the log as
  `signed-darwin/notarization/log.json`. Fix the cause and re-run from there.
- **`assemble` reports a rebuild mismatch:** the toolchain or inputs changed
  between `build` and `assemble`. Re-run the whole workflow.
- **`publish` failed part way:** re-run it. It completes the draft and
  publishes; an identical published release is accepted.
- **A channel failed:** re-run only that `distribute` job. It uses the
  published files and never rebuilds, and an older run cannot move a channel
  backwards.
- **A published version is bad:** release a new version. If a channel must be
  pointed away from the bad version at once, edit its manifest by hand to the
  last good version; the next release overwrites it.
