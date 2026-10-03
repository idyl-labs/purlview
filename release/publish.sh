#!/usr/bin/env bash
# Publish the assembled artifact set as an immutable GitHub release:
# draft -> upload every asset -> (attestation happens in the workflow) ->
# publish. Safe to re-run: an existing draft is completed, and an already
# published release is accepted only when its checksums.txt is identical to
# the local one. Never modifies a published release.
#
# Usage: publish.sh --tag TAG --dist DIR --repository owner/name --commit SHA
#                   --notes FILE [--prerelease] [--phase draft|publish]
# Environment: GH_TOKEN with contents:write on the repository.
set -euo pipefail

TAG=""; DIST=""; REPO=""; COMMIT=""; NOTES=""; PRERELEASE=false; PHASE=all
while [ $# -gt 0 ]; do
  case "$1" in
    --tag) TAG="$2"; shift 2 ;;
    --dist) DIST="$2"; shift 2 ;;
    --repository) REPO="$2"; shift 2 ;;
    --commit) COMMIT="$2"; shift 2 ;;
    --notes) NOTES="$2"; shift 2 ;;
    --prerelease) PRERELEASE=true; shift ;;
    --phase) PHASE="$2"; shift 2 ;;
    *) echo "publish: unknown argument $1" >&2; exit 2 ;;
  esac
done
[[ -n "$TAG" && -n "$DIST" && -n "$REPO" && -n "$NOTES" ]] || { echo "usage: publish.sh --tag --dist --repository --notes [--commit] [--prerelease] [--phase draft|publish|all]" >&2; exit 2; }

assets=()
while IFS= read -r -d '' f; do assets+=("$f"); done < <(find "$DIST" -maxdepth 1 -type f \( -name '*.tar.gz' -o -name '*.zip' -o -name '*.deb' -o -name '*.rpm' -o -name '*.sbom.json' -o -name 'checksums.txt' -o -name 'checksums.txt.sigstore.json' \) -print0 | sort -z)
(( ${#assets[@]} >= 21 )) || { echo "publish: expected at least 21 assets (6 archives, 4 packages, 10 SBOMs, checksums), found ${#assets[@]}" >&2; exit 1; }
grep -q 'checksums.txt.sigstore.json' <<<"$(printf '%s\n' "${assets[@]}")" || { echo "publish: checksums.txt.sigstore.json is missing" >&2; exit 1; }

state=none
if json="$(gh release view "$TAG" --repo "$REPO" --json isDraft,tagName,url 2>/dev/null)"; then
  if [[ "$(jq -r .isDraft <<<"$json")" == true ]]; then state=draft; else state=published; fi
fi
echo "publish: release $TAG in $REPO is currently: $state"

if [[ "$state" == published ]]; then
  tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
  gh release download "$TAG" --repo "$REPO" --pattern checksums.txt --dir "$tmp"
  if cmp -s "$tmp/checksums.txt" "$DIST/checksums.txt"; then
    echo "publish: already published with identical checksums; nothing to do"
    echo "url=$(jq -r .url <<<"$json")" >>"${GITHUB_OUTPUT:-/dev/null}"
    exit 0
  fi
  echo "publish: $TAG is already published in $REPO with DIFFERENT checksums. A published release is never modified; fix forward with a new version." >&2
  exit 1
fi

if [[ "$PHASE" == draft || "$PHASE" == all ]]; then
  if [[ "$state" == none ]]; then
    args=(--repo "$REPO" --draft --title "$TAG" --notes-file "$NOTES")
    $PRERELEASE && args+=(--prerelease)
    [[ -n "$COMMIT" ]] && args+=(--target "$COMMIT")
    gh release create "$TAG" "${args[@]}"
    echo "publish: created draft $TAG"
  fi
  gh release upload "$TAG" --repo "$REPO" --clobber "${assets[@]}"
  echo "publish: uploaded ${#assets[@]} assets to the draft"
fi

if [[ "$PHASE" == publish || "$PHASE" == all ]]; then
  # Every asset must be present before the draft is published (immutable).
  remote="$(gh release view "$TAG" --repo "$REPO" --json assets --jq '.assets[].name' | sort)"
  local_names="$(printf '%s\n' "${assets[@]}" | xargs -n1 basename | sort)"
  if [[ "$remote" != "$local_names" ]]; then
    diff <(echo "$remote") <(echo "$local_names") >&2 || true
    echo "publish: draft asset set differs from the local artifact set; refusing to publish" >&2
    exit 1
  fi
  # "latest" is what the CLI's update checker and the installers resolve, so
  # it must only ever advance: a prerelease is never latest, and a stable
  # release published after a newer stable one (a hotfix for an older line,
  # or a retried older workflow) leaves the newer one in place.
  latest_flag=--latest
  if $PRERELEASE; then
    latest_flag=--latest=false
  elif current="$(gh release view --repo "$REPO" --json tagName --jq .tagName 2>/dev/null)" && [[ -n "$current" && "$current" != "$TAG" ]]; then
    if [[ "$(go run ./internal/tools/semvercmp "$TAG" "$current" 2>/dev/null || echo 1)" == -1 ]]; then
      echo "publish: $current stays the latest release; $TAG is older and will not replace it"
      latest_flag=--latest=false
    fi
  fi
  gh release edit "$TAG" --repo "$REPO" --draft=false "$latest_flag"
  url="$(gh release view "$TAG" --repo "$REPO" --json url --jq .url)"
  immutable="$(gh release view "$TAG" --repo "$REPO" --json isImmutable --jq '.isImmutable' 2>/dev/null || echo unknown)"
  echo "publish: published $url (immutable: $immutable)"
  echo "url=$url" >>"${GITHUB_OUTPUT:-/dev/null}"
  echo "immutable=$immutable" >>"${GITHUB_OUTPUT:-/dev/null}"
fi
