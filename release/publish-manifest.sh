#!/usr/bin/env bash
# Commit one generated package manifest (Homebrew cask or Scoop manifest) to
# its distribution repository. Idempotent: identical content is a no-op, and a
# manifest that already carries a newer version is never overwritten.
#
# Usage: publish-manifest.sh --repository owner/name --branch main
#          --path Casks/purlview.rb --file dist/homebrew/Casks/purlview.rb
#          --version 0.2.0 --message "purlview v0.2.0"
# Run it from the root of the source checkout.
# Environment: GH_TOKEN with contents:write on the repository.
set -euo pipefail

REPO=""; BRANCH=main; DEST_PATH=""; FILE=""; VERSION=""; MESSAGE=""
while [ $# -gt 0 ]; do
  case "$1" in
    --repository) REPO="$2"; shift 2 ;;
    --branch) BRANCH="$2"; shift 2 ;;
    --path) DEST_PATH="$2"; shift 2 ;;
    --file) FILE="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --message) MESSAGE="$2"; shift 2 ;;
    *) echo "publish-manifest: unknown argument $1" >&2; exit 2 ;;
  esac
done
[[ -n "$REPO" && -n "$DEST_PATH" && -n "$FILE" && -n "$VERSION" && -n "$MESSAGE" ]] || { echo "usage: publish-manifest.sh --repository --path --file --version --message [--branch]" >&2; exit 2; }
[[ -f "$FILE" ]] || { echo "publish-manifest: $FILE not found" >&2; exit 1; }
grep -q "$VERSION" "$FILE" || { echo "publish-manifest: $FILE does not mention version $VERSION" >&2; exit 1; }

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT
# Built here, in the source checkout, because the comparison below runs in
# the clone of the distribution repository.
go build -o "$WORK/semvercmp" ./internal/tools/semvercmp
git -c credential.helper='!gh auth git-credential' clone --quiet --depth 1 --branch "$BRANCH" "https://github.com/$REPO.git" "$WORK/repo"
cd "$WORK/repo"

if [[ -f "$DEST_PATH" ]]; then
  current="$(grep -Eo -m1 '(version "[^"]+"|"version": "[^"]+")' "$DEST_PATH" | grep -Eo '[0-9][^"]*' | head -n1 || true)"
  if [[ -n "$current" ]]; then
    order="$("$WORK/semvercmp" "$VERSION" "$current")" \
      || { echo "publish-manifest: cannot compare $VERSION with $REPO/$DEST_PATH's $current" >&2; exit 1; }
    case "$order" in
      -1) echo "publish-manifest: $REPO/$DEST_PATH already carries $current, newer than $VERSION; refusing to roll the channel back" >&2; exit 1 ;;
      0) if cmp -s "$DEST_PATH" "$OLDPWD/$FILE"; then echo "publish-manifest: $REPO/$DEST_PATH already matches $VERSION; nothing to do"; exit 0; fi ;;
    esac
  fi
fi

mkdir -p "$(dirname "$DEST_PATH")"
cp "$OLDPWD/$FILE" "$DEST_PATH"
git add "$DEST_PATH"
if git diff --cached --quiet; then echo "publish-manifest: no changes for $REPO/$DEST_PATH"; exit 0; fi
git -c user.name='purlview-release' -c user.email='41898282+github-actions[bot]@users.noreply.github.com' commit --quiet -m "$MESSAGE"
git -c credential.helper='!gh auth git-credential' push --quiet origin "HEAD:$BRANCH"
echo "publish-manifest: pushed $DEST_PATH ($VERSION) to $REPO@$BRANCH"
