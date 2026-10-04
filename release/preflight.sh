#!/usr/bin/env bash
# Copyright 2026 Idyl Labs
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Release preflight: validates the tag, the source commit, the required
# checks, the release destination and the credentials, and writes the
# decisions to $GITHUB_OUTPUT for the other release jobs.
#
# Expected environment (set by .github/workflows/release.yml):
#   TAG, GITHUB_REF, GITHUB_SHA, GITHUB_REPOSITORY
#   GH_TOKEN          reads this repository's checks and workflow runs
#   RELEASE_GH_TOKEN  reads the release repository (the release app's token
#                     when that is another repository; defaults to GH_TOKEN)
#   ALLOW_UNSIGNED_INPUT ("true"/"false"), ALLOW_UNSIGNED_VAR ("true"/"")
#   ALLOW_UNSIGNED_WINDOWS_INPUT ("true"/"false")
#   RELEASE_TARGET (owner/name of the release repository; empty: this one),
#   PRERELEASE_CHANNELS_VAR
#   PREVIOUS_TAG_INPUT
#   HAS_APPLE_SIGNING, HAS_APPLE_NOTARY, HAS_AZURE_OIDC, HAS_ARTIFACT_SIGNING,
#   HAS_RELEASE_APP ("true"/"false", computed from secret presence)
#   HAS_WINGET_FORK, HAS_PACKAGES_ROLE, HAS_PACKAGES_CONFIG ("true"/"false",
#   from release-environment variables, which job-level conditions cannot read)
set -euo pipefail

out() { printf '%s=%s\n' "$1" "$2" | tee -a "${GITHUB_OUTPUT:-/dev/null}"; }
die() { echo "preflight: $*" >&2; exit 1; }
# release_gh runs gh against the release repository, which a cross-repository
# release reaches only through the release app's token.
release_gh() { GH_TOKEN="${RELEASE_GH_TOKEN:-$GH_TOKEN}" gh "$@"; }
summary() { printf '%s\n' "$*" >>"${GITHUB_STEP_SUMMARY:-/dev/null}"; }

# --- tag and version ---------------------------------------------------------

[[ "$TAG" =~ ^v([0-9]+)\.([0-9]+)\.([0-9]+)(-(alpha|beta|rc)\.([0-9]+))?$ ]] \
  || die "tag '$TAG' must be vMAJOR.MINOR.PATCH or vMAJOR.MINOR.PATCH-(alpha|beta|rc).N"
VERSION="${TAG#v}"
PRERELEASE=false
[[ "$VERSION" == *-* ]] && PRERELEASE=true
[[ "$GITHUB_REF" == "refs/tags/$TAG" ]] \
  || die "this workflow must run on the tag ref refs/tags/$TAG (got $GITHUB_REF); dispatch it with the tag selected as the ref so the signing identity names the tag"

git fetch --tags --quiet origin
git rev-parse -q --verify "refs/tags/$TAG^{commit}" >/dev/null || die "tag $TAG does not exist on origin"
COMMIT="$(git rev-parse "refs/tags/$TAG^{commit}")"
[[ "$COMMIT" == "$GITHUB_SHA" ]] || die "checked-out commit $GITHUB_SHA is not the tag's commit $COMMIT"
[[ -z "$(git status --porcelain)" ]] || die "working tree is not clean"

# --- trusted release path ---------------------------------------------------

git fetch --quiet origin main
if ! $PRERELEASE; then
  git merge-base --is-ancestor "$COMMIT" origin/main \
    || die "stable release $TAG must point at a commit on main"
fi

# --- required checks on this exact commit -------------------------------------

# The jobs of the CI workflow (.github/workflows/ci.yml) must have succeeded
# on the tagged commit. A commit can carry several CI runs (its pull request,
# its push to main, a dispatch); a check is satisfied by any successful run of
# that name. While a CI run on the commit is still going, preflight waits for
# it rather than failing.
required=("formatting, modules and scripts" "ubuntu-24.04" "ubuntu-24.04-arm" "macos-15" "macos-15-intel" "windows-2025" "windows-11-arm")
deadline=$((SECONDS + ${PREFLIGHT_CHECK_WAIT_SECONDS:-2700}))
while :; do
  checks="$(gh api --paginate "repos/$GITHUB_REPOSITORY/commits/$COMMIT/check-runs?filter=all&per_page=100" --jq '.check_runs[] | "\(.name)\t\(.status)\t\(.conclusion)"')"
  missing=()
  for name in "${required[@]}"; do
    runs="$(printf '%s\n' "$checks" | awk -F'\t' -v n="$name" '$1 == n')"
    if grep -q $'\tcompleted\tsuccess$' <<<"$runs"; then continue; fi
    if [[ -z "$runs" ]]; then missing+=("$name (not run)"); else missing+=("$name ($(cut -f2,3 <<<"$runs" | tr '\t' / | sort -u | paste -sd, -))"); fi
  done
  (( ${#missing[@]} == 0 )) && break
  running="$(gh api "repos/$GITHUB_REPOSITORY/actions/runs?head_sha=$COMMIT&per_page=100" --jq '[.workflow_runs[] | select(.path == ".github/workflows/ci.yml" and .status != "completed")] | length')"
  if (( running == 0 || SECONDS >= deadline )); then
    printf 'preflight: required CI checks are not green for %s:\n' "$COMMIT" >&2
    printf '  - %s\n' "${missing[@]}" >&2
    die "run CI on this commit (gh workflow run ci.yml --ref <branch>), then re-run this workflow"
  fi
  echo "preflight: a CI run on $COMMIT is still going; waiting for ${#missing[@]} required checks"
  sleep 30
done

# --- release destination -------------------------------------------------------

RELEASE_REPOSITORY="${RELEASE_TARGET:-$GITHUB_REPOSITORY}"
[[ "$RELEASE_REPOSITORY" =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die "the release repository must be owner/name, not '$RELEASE_REPOSITORY'"
CROSS_REPO=false
[[ "$RELEASE_REPOSITORY" != "$GITHUB_REPOSITORY" ]] && CROSS_REPO=true
if $CROSS_REPO; then
  [[ "$HAS_RELEASE_APP" == true ]] || die "publishing to $RELEASE_REPOSITORY needs the RELEASE_APP_ID and RELEASE_APP_PRIVATE_KEY secrets"
fi
RELEASE_PUBLIC=false
if visibility="$(release_gh api "repos/$RELEASE_REPOSITORY" --jq '.private' 2>/dev/null)"; then
  [[ "$visibility" == false ]] && RELEASE_PUBLIC=true
else
  echo "preflight: cannot read $RELEASE_REPOSITORY with the workflow token; treating it as private" >&2
fi

# --- signing credentials ----------------------------------------------------

ALLOW_UNSIGNED=false
if [[ "$ALLOW_UNSIGNED_INPUT" == true || "$ALLOW_UNSIGNED_VAR" == true ]]; then ALLOW_UNSIGNED=true; fi
# allow_unsigned_windows lets a release, stable included, go out before Windows
# signing exists; macOS must still be signed and notarized.
ALLOW_UNSIGNED_WINDOWS=false
[[ "${ALLOW_UNSIGNED_WINDOWS_INPUT:-false}" == true ]] && ALLOW_UNSIGNED_WINDOWS=true
SIGN_MACOS=false; SIGN_WINDOWS=false
[[ "$HAS_APPLE_SIGNING" == true && "$HAS_APPLE_NOTARY" == true ]] && SIGN_MACOS=true
[[ "$HAS_AZURE_OIDC" == true && "$HAS_ARTIFACT_SIGNING" == true ]] && SIGN_WINDOWS=true
UNSIGNED_WINDOWS=false
if $SIGN_MACOS && ! $SIGN_WINDOWS && $ALLOW_UNSIGNED_WINDOWS; then UNSIGNED_WINDOWS=true; fi
if $ALLOW_UNSIGNED && ! $PRERELEASE; then
  die "allow_unsigned is only valid for prereleases"
fi
if ! $SIGN_MACOS || ! $SIGN_WINDOWS; then
  if ! $UNSIGNED_WINDOWS && ! $ALLOW_UNSIGNED; then
    if $PRERELEASE; then
      die "prerelease $TAG lacks signing credentials (macOS: $SIGN_MACOS, Windows: $SIGN_WINDOWS); re-run with allow_unsigned=true for a labelled unsigned rehearsal, or allow_unsigned_windows=true when only Windows is missing"
    fi
    die "stable release $TAG requires macOS signing/notarization and Windows Authenticode credentials (macOS configured: $SIGN_MACOS, Windows configured: $SIGN_WINDOWS); with macOS configured, allow_unsigned_windows=true publishes Windows unsigned; see docs/releasing.md"
  fi
fi

# --- package channels ------------------------------------------------------

PUBLISH_CHANNELS=false
if $RELEASE_PUBLIC && $SIGN_MACOS && { $SIGN_WINDOWS || $UNSIGNED_WINDOWS; }; then
  if ! $PRERELEASE || [[ "$PRERELEASE_CHANNELS_VAR" == true ]]; then PUBLISH_CHANNELS=true; fi
fi

# --- previous release for the upgrade check ---------------------------------

PREVIOUS_TAG="${PREVIOUS_TAG_INPUT:-}"
if [[ -z "$PREVIOUS_TAG" ]]; then
  while read -r candidate; do
    [[ -n "$candidate" ]] || continue
    if [[ "$(go run ./internal/tools/semvercmp "$candidate" "$TAG" 2>/dev/null || echo 0)" == -1 ]]; then
      if [[ -z "$PREVIOUS_TAG" || "$(go run ./internal/tools/semvercmp "$candidate" "$PREVIOUS_TAG")" == 1 ]]; then PREVIOUS_TAG="$candidate"; fi
    fi
  done < <(release_gh release list --repo "$RELEASE_REPOSITORY" --exclude-drafts --limit 50 --json tagName --jq '.[].tagName' 2>/dev/null || true)
fi
if [[ -n "$PREVIOUS_TAG" ]]; then
  [[ "$PREVIOUS_TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta|rc)\.[0-9]+)?$ ]] || die "previous_tag must be an ordinary CLI release tag"
  [[ "$(go run ./internal/tools/semvercmp "$PREVIOUS_TAG" "$TAG")" == -1 ]] || die "previous_tag must be older than $TAG"
fi
mkdir -p previous
if [[ -n "$PREVIOUS_TAG" ]]; then
  if release_gh release download "$PREVIOUS_TAG" --repo "$RELEASE_REPOSITORY" --dir previous --pattern '*.deb' --pattern '*.rpm' --pattern 'checksums.txt' 2>/dev/null; then
    echo "preflight: upgrade check will start from $PREVIOUS_TAG"
  else
    echo "preflight: could not download packages of $PREVIOUS_TAG; upgrade check will be skipped" >&2
    PREVIOUS_TAG=""
  fi
fi
touch previous/.keep

out tag "$TAG"
out version "$VERSION"
out commit "$COMMIT"
out prerelease "$PRERELEASE"
out release_repository "$RELEASE_REPOSITORY"
out release_public "$RELEASE_PUBLIC"
out cross_repo "$CROSS_REPO"
out sign_macos "$SIGN_MACOS"
out sign_windows "$SIGN_WINDOWS"
out allow_unsigned "$ALLOW_UNSIGNED"
out unsigned_windows "$UNSIGNED_WINDOWS"
out publish_channels "$PUBLISH_CHANNELS"
PUBLISH_WINGET=false
$PUBLISH_CHANNELS && [[ "${HAS_WINGET_FORK:-false}" == true ]] && PUBLISH_WINGET=true
out publish_winget "$PUBLISH_WINGET"
# The APT and DNF repositories take prereleases too, into their prerelease
# suite.
PUBLISH_PACKAGES=false
if [[ "${HAS_PACKAGES_ROLE:-false}" == true ]]; then
  [[ "${HAS_PACKAGES_CONFIG:-false}" == true ]] \
    || die "PURLVIEW_PACKAGES_ROLE_ARN is set, so the APT and DNF repositories also need PURLVIEW_PACKAGES_AWS_REGION, PURLVIEW_PACKAGES_BUCKET, PURLVIEW_PACKAGE_SIGNING_SECRET_ID and PURLVIEW_PACKAGE_SIGNING_FINGERPRINT"
  PUBLISH_PACKAGES=true
fi
out publish_packages "$PUBLISH_PACKAGES"
out previous_tag "$PREVIOUS_TAG"

summary "## Preflight for $TAG"
summary ""
summary "| Decision | Value |"
summary "| --- | --- |"
summary "| Commit | \`$COMMIT\` |"
summary "| Prerelease | $PRERELEASE |"
summary "| Release repository | $RELEASE_REPOSITORY (public: $RELEASE_PUBLIC) |"
summary "| macOS signing + notarization | $SIGN_MACOS |"
summary "| Windows Authenticode | $SIGN_WINDOWS |"
summary "| Unsigned rehearsal allowed | $ALLOW_UNSIGNED |"
summary "| Windows published unsigned | $UNSIGNED_WINDOWS |"
summary "| Package channels will update | $PUBLISH_CHANNELS (WinGet: $PUBLISH_WINGET) |"
summary "| APT and DNF repositories will update | $PUBLISH_PACKAGES |"
summary "| Upgrade check from | ${PREVIOUS_TAG:-none} |"
