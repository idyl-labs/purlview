#!/usr/bin/env bash
# Compose the GitHub release notes from GoReleaser's changelog plus the
# verification instructions. Usage: release-notes.sh TAG DIST OUTFILE
#   Environment: RELEASE_REPOSITORY (owner/name), SOURCE_REPOSITORY,
#                UNSIGNED ("true" marks an unsigned rehearsal),
#                UNSIGNED_WINDOWS ("true": the Windows executables are unsigned)
set -euo pipefail
TAG="$1"; DIST="$2"; OUT="$3"
VERSION="${TAG#v}"
{
  if [[ "${UNSIGNED:-false}" == true ]]; then
    echo "> **Unsigned rehearsal.** The macOS and Windows executables in this prerelease carry no publisher signature or notarization. It exists to exercise the release pipeline; do not use it as a product release."
    echo
  fi
  if [[ "${UNSIGNED_WINDOWS:-false}" == true ]]; then
    echo "> The Windows executables in this release are not yet Authenticode signed, so a browser download of the zip may show a SmartScreen prompt. The macOS executables are signed and notarized, and every file is covered by checksums.txt and its Sigstore signature."
    echo
  fi
  if [[ "$VERSION" == *-* ]]; then
    echo "> Prerelease. Package managers do not pick prereleases up; install with an explicit version."
    echo
  fi
  echo "## Changes"
  echo
  if [[ -s "$DIST/CHANGELOG.md" ]]; then sed '/^## Changelog/d' "$DIST/CHANGELOG.md"; else echo "_No changelog entries._"; fi
  echo
  echo "## Verify a download"
  echo
  echo '```sh'
  echo "sha256sum --ignore-missing -c checksums.txt"
  echo "cosign verify-blob \\"
  echo "  --certificate-identity 'https://github.com/${SOURCE_REPOSITORY}/.github/workflows/release.yml@refs/tags/${TAG}' \\"
  echo "  --certificate-oidc-issuer https://token.actions.githubusercontent.com \\"
  echo "  --bundle checksums.txt.sigstore.json checksums.txt"
  echo '```'
  echo
  echo "checksums.txt lists every archive and package; the Sigstore bundle proves that the release workflow of \`${SOURCE_REPOSITORY}\` produced it at tag \`${TAG}\`. SBOMs (SPDX JSON) accompany each archive and package."
} >"$OUT"
