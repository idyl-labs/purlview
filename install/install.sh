#!/bin/sh
# Purlview installer for macOS and Linux (POSIX sh).
#
# Usage:
#   curl -fsSL https://purlview.com/install.sh | sh
#   curl -fsSL https://purlview.com/install.sh | sh -s -- [--version vX.Y.Z] [--dest DIR] [--arch amd64|arm64] [--no-modify-path]
#   curl -fsSL https://purlview.com/install.sh | sh -s -- --uninstall [--dest DIR]
#   sh install.sh [options]   (the same, from a downloaded copy)
#
# What it does: resolves the requested version (the latest stable release by
# default; prereleases are never chosen implicitly), downloads the archive and
# checksums.txt for this OS and architecture over HTTPS, verifies the SHA-256
# and the publisher's signature, checks that the extracted executable runs,
# stops the previous version's daemon (ending its active shares) while
# blocking new daemon starts, and only then replaces the file at DEST/purlview
# with a single rename. A previous installation is left intact if any step
# fails, but shares that were ended stay ended.
#
# If DEST is not on PATH, the installer adds it the way rustup and uv do: one
# marked block at the end of the startup file of your shell ($SHELL), which
# --uninstall removes again. Nothing else on the system is modified: no sudo,
# no file outside your home directory. --no-modify-path (or
# PURLVIEW_NO_MODIFY_PATH=1) leaves startup files alone.
#
# The checksum comes from the same release as the archive, so it detects
# corruption and truncation; it is not publisher authentication. That is the
# signature, checked before the downloaded executable runs for the first
# time, and an executable that fails it is neither run nor installed:
#   macOS  the executable's Apple Developer ID signature must be Purlview's.
#   both   when `cosign` is installed, the Sigstore signature on checksums.txt
#          must be the release workflow's for this version.
# Linux executables carry no signature of their own: without cosign a Linux
# install rests on HTTPS and the checksum. The APT and DNF repositories are
# signed.
#
# Environment:
#   PURLVIEW_RELEASE_BASE_URL  GitHub repository URL that serves releases
#   PURLVIEW_INSTALL_DIR       default destination (default: $HOME/.local/bin)
#   PURLVIEW_VERSION           default version
#   PURLVIEW_NO_MODIFY_PATH    1: never add DEST to PATH (as --no-modify-path)
#   PURLVIEW_VERIFY_PUBLISHER  1: check signatures of a release served from
#                               the loopback interface too (installer tests)
set -eu

# Everything below is one function, called on the last line, so a shell
# reading this script from a pipe runs nothing until it has all of it: a
# download cut short is a syntax error, never half an install.
main() {

RELEASE_BASE_URL="${PURLVIEW_RELEASE_BASE_URL:-https://github.com/idyl-labs/purlview-releases}"
# The repository whose release workflow signs checksums.txt (Sigstore).
SOURCE_REPOSITORY=idyl-labs/purlview
# The Apple Developer ID team that signs Purlview's macOS executables.
APPLE_TEAM_ID=2B6R3TC7HJ
DEST="${PURLVIEW_INSTALL_DIR:-$HOME/.local/bin}"
DEST_EXPLICIT=false
VERSION="${PURLVIEW_VERSION:-}"
ARCH=""
UNINSTALL=false
MODIFY_PATH=true
case "${PURLVIEW_NO_MODIFY_PATH:-}" in
  ''|0) ;;
  *) MODIFY_PATH=false ;;
esac

log() { printf 'purlview-install: %s\n' "$*"; }
fail() { printf 'purlview-install: error: %s\n' "$*" >&2; exit 1; }

# The usage is written out, not read from this file: piped, there is no file.
usage() {
  cat <<'USAGE'
Purlview installer for macOS and Linux.

  curl -fsSL https://purlview.com/install.sh | sh
  curl -fsSL https://purlview.com/install.sh | sh -s -- [options]

Options:
  --version vX.Y.Z   install this release (default: the latest stable)
  --dest DIR         install to DIR (default: $PURLVIEW_INSTALL_DIR or ~/.local/bin)
  --arch ARCH        amd64 or arm64 (default: detected)
  --no-modify-path   do not add DIR to PATH (or set PURLVIEW_NO_MODIFY_PATH=1)
  --uninstall        remove purlview from DIR, its runtime state (unless
                     another purlview stays on PATH) and the
                     PATH lines this installer added
  -h, --help         show this help

Downloads the archive and checksums.txt over HTTPS, verifies the SHA-256
and the publisher's signature (macOS: Apple Developer ID; with cosign
installed: the release's Sigstore signature), checks that the executable
runs, then replaces DIR/purlview in one step.
If DIR is not on PATH, adds it with one marked block at the end of your
shell's startup file (~/.zshrc, ~/.bashrc, ~/.profile or fish's conf.d).
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --version) [ $# -ge 2 ] || fail "--version needs a value"; VERSION="$2"; shift 2 ;;
    --version=*) VERSION="${1#--version=}"; shift ;;
    --dest) [ $# -ge 2 ] || fail "--dest needs a value"; DEST="$2"; DEST_EXPLICIT=true; shift 2 ;;
    --dest=*) DEST="${1#--dest=}"; DEST_EXPLICIT=true; shift ;;
    --arch) [ $# -ge 2 ] || fail "--arch needs a value"; ARCH="$2"; shift 2 ;;
    --arch=*) ARCH="${1#--arch=}"; shift ;;
    --no-modify-path) MODIFY_PATH=false; shift ;;
    --uninstall) UNINSTALL=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) fail "unknown argument: $1 (see --help)" ;;
  esac
done

# Only HTTPS is accepted, except plain HTTP to the loopback interface, which
# the installer tests use to serve local fixtures. Nothing signs a fixture, so
# its signatures are checked only when PURLVIEW_VERIFY_PUBLISHER=1 asks.
CURL_PROTO='=https'
VERIFY_PUBLISHER=true
case "$RELEASE_BASE_URL" in
  https://*) ;;
  http://127.0.0.1:*|http://127.0.0.1/*|http://localhost:*|http://localhost/*|http://\[::1\]:*)
    CURL_PROTO='=https,http'
    [ "${PURLVIEW_VERIFY_PUBLISHER:-}" = 1 ] || VERIFY_PUBLISHER=false ;;
  *) fail "PURLVIEW_RELEASE_BASE_URL must be an https:// URL, got: $RELEASE_BASE_URL" ;;
esac
RELEASE_BASE_URL="${RELEASE_BASE_URL%/}"

# --- host detection ------------------------------------------------------------

detect_os() {
  case "$(uname -s 2>/dev/null || echo unknown)" in
    Darwin) echo darwin ;;
    Linux) echo linux ;;
    MINGW*|MSYS*|CYGWIN*|Windows_NT) fail "this is a Windows shell; use install.ps1 from PowerShell instead" ;;
    *) fail "unsupported operating system: $(uname -s). Purlview supports macOS, Linux and Windows." ;;
  esac
}

detect_arch() {
  machine="$(uname -m 2>/dev/null || echo unknown)"
  case "$machine" in
    x86_64|amd64) arch=amd64 ;;
    arm64|aarch64) arch=arm64 ;;
    *) fail "unsupported architecture: $machine. Purlview supports amd64 and arm64; pass --arch to override." ;;
  esac
  # A shell running under Rosetta reports x86_64 on Apple silicon.
  if [ "$arch" = amd64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
    arch=arm64
  fi
  echo "$arch"
}

OS="$(detect_os)"
if [ -z "$ARCH" ]; then
  ARCH="$(detect_arch)"
else
  case "$ARCH" in
    amd64|x86_64) ARCH=amd64 ;;
    arm64|aarch64) ARCH=arm64 ;;
    *) fail "unsupported --arch $ARCH; use amd64 or arm64" ;;
  esac
fi

# --- managed-install detection -------------------------------------------------

# managed_by PATH prints the package manager that owns PATH, if any.
managed_by() {
  path="$1"
  if command -v brew >/dev/null 2>&1; then
    prefix="$(brew --prefix 2>/dev/null || true)"
    case "$path" in
      "$prefix"/bin/*|"$prefix"/Caskroom/*) echo "Homebrew"; return ;;
    esac
    case "$(readlink "$path" 2>/dev/null || true)" in
      */Caskroom/*|*/Cellar/*) echo "Homebrew"; return ;;
    esac
  fi
  if command -v dpkg >/dev/null 2>&1 && dpkg -S "$path" >/dev/null 2>&1; then echo "dpkg/APT"; return; fi
  if command -v rpm >/dev/null 2>&1 && rpm -qf "$path" >/dev/null 2>&1; then echo "rpm/DNF"; return; fi
  echo ""
}

# --- PATH --------------------------------------------------------------------------

# The block this installer adds to a startup file is this comment line and
# the line after it (path_line). It is found, and removed, by both lines.
PATH_MARK='# Added by the Purlview installer (https://purlview.com); remove with install.sh --uninstall'

# absolute_dest makes DEST absolute, as startup files need it, without a
# trailing slash, as PATH spells it.
absolute_dest() {
  case "$DEST" in
    /*) ;;
    *) if [ -d "$DEST" ]; then DEST="$(CDPATH='' cd -- "./$DEST" && pwd)"; fi ;;
  esac
  case "$DEST" in
    ?*/) DEST="${DEST%/}" ;;
  esac
}

# on_path DIR: DIR is on this shell's PATH.
on_path() {
  case ":$PATH:" in
    *":$1:"*|*":$1/:"*) return 0 ;;
  esac
  return 1
}

# plain_dest: DEST can go into a startup file inside double quotes as it is.
# A DEST that would need escaping there is left to the user.
plain_dest() {
  case "$DEST" in
    *[\"\$\`\\]*|*'
'*) return 1 ;;
  esac
  return 0
}

# path_line FILE prints the line of the block for FILE, with $HOME kept
# literal when DEST is below it. The sh line adds DEST once however often
# the file is read; fish_add_path --path does the same and, unlike plain
# fish_add_path, leaves no universal variable behind.
# shellcheck disable=SC2016 # $HOME and $PATH are for the startup file
path_line() {
  p_dir="$DEST"
  case "$HOME" in
    ''|/) ;;
    *) case "$DEST" in "$HOME"/*) p_dir='$HOME/'"${DEST#"$HOME"/}" ;; esac ;;
  esac
  case "$1" in
    *.fish) printf 'fish_add_path --path "%s"\n' "$p_dir" ;;
    *) printf 'case ":$PATH:" in *":%s:"*) ;; *) export PATH="%s:$PATH" ;; esac\n' "$p_dir" "$p_dir" ;;
  esac
}

# startup_files prints the startup files of the user's shell that a new
# terminal reads, one per line: where the block goes.
startup_files() {
  p_shell="${SHELL:-}"
  case "${p_shell##*/}" in
    zsh) printf '%s\n' "${ZDOTDIR:-$HOME}/.zshrc" ;;
    bash)
      if [ "$OS" = darwin ]; then
        # Terminal windows on macOS start login shells, which read the first
        # of these that exists and not ~/.bashrc.
        if [ -f "$HOME/.bash_profile" ]; then printf '%s\n' "$HOME/.bash_profile"
        elif [ -f "$HOME/.bash_login" ]; then printf '%s\n' "$HOME/.bash_login"
        else printf '%s\n' "$HOME/.profile"
        fi
        if [ -f "$HOME/.bashrc" ]; then printf '%s\n' "$HOME/.bashrc"; fi
      else
        printf '%s\n' "$HOME/.bashrc"
      fi
      ;;
    fish) printf '%s\n' "${XDG_CONFIG_HOME:-$HOME/.config}/fish/conf.d/purlview.fish" ;;
    *) printf '%s\n' "$HOME/.profile" ;;
  esac
}

# has_block FILE: FILE holds the block for DEST.
has_block() {
  [ -f "$1" ] || return 1
  MARK="$PATH_MARK" LINE="$(path_line "$1")" awk '
    prev == ENVIRON["MARK"] && $0 == ENVIRON["LINE"] { found = 1 }
    { prev = $0 }
    END { exit !found }' "$1" 2>/dev/null
}

# add_block FILE appends the block for DEST to FILE after a blank line,
# creating FILE (and fish's conf.d directory) if it does not exist yet.
add_block() {
  case "$1" in
    *.fish) mkdir -p "${1%/*}" 2>/dev/null || return 1 ;;
  esac
  p_line="$(path_line "$1")"
  p_sep=''
  if [ -s "$1" ]; then
    # A last line without a newline is ended first.
    if [ -n "$(tail -c 1 "$1")" ]; then p_sep=2; else p_sep=1; fi
  fi
  {
    if [ "$p_sep" = 2 ]; then printf '\n\n'; elif [ "$p_sep" = 1 ]; then printf '\n'; fi
    printf '%s\n%s\n' "$PATH_MARK" "$p_line"
  } 2>/dev/null >>"$1"
}

# remove_block FILE removes the blocks for DEST from FILE, each with the
# blank line add_block put before it. FILE is rewritten in place, so a
# symbolic link stays a link; the fish file, which holds nothing else, is
# deleted once it is empty.
remove_block() {
  has_block "$1" || return 1
  p_tmp="$(mktemp "${TMPDIR:-/tmp}/purlview-install.XXXXXX")" || return 1
  if MARK="$PATH_MARK" LINE="$(path_line "$1")" awk '
      { line[NR] = $0 }
      END {
        for (i = 1; i <= NR; i++) {
          if (line[i] == ENVIRON["MARK"] && i < NR && line[i + 1] == ENVIRON["LINE"]) {
            if (i > 1 && line[i - 1] == "") drop[i - 1] = 1
            drop[i] = 1
            drop[i + 1] = 1
            i++
          }
        }
        for (i = 1; i <= NR; i++) if (!(i in drop)) print line[i]
      }' "$1" >"$p_tmp" && cat "$p_tmp" 2>/dev/null >"$1"; then
    rm -f "$p_tmp"
  else
    rm -f "$p_tmp"
    return 1
  fi
  case "$1" in
    *.fish) if [ -z "$(tr -d ' \t\n' <"$1")" ]; then rm -f "$1"; fi ;;
  esac
  return 0
}

# add_to_path adds the block for DEST to the startup files of the user's
# shell and says what changed and what is left to do.
add_to_path() {
  p_done=false
  p_ifs="$IFS"
  IFS='
'
  set -f
  for p_file in $(startup_files); do
    if has_block "$p_file"; then
      log "$p_file already adds $DEST to PATH"
      p_done=true
    elif add_block "$p_file"; then
      log "added $DEST to PATH in $p_file"
      p_done=true
    else
      log "warning: could not write to $p_file"
    fi
  done
  set +f
  IFS="$p_ifs"
  $p_done
}

# path_advice is what to do when the installer leaves PATH alone.
path_advice() {
  log "$DEST is not on your PATH. Add it in your shell profile, for example:"
  log "  export PATH=\"$DEST:\$PATH\""
}

# remove_from_path removes the blocks for DEST from every startup file this
# installer writes to, and fails if there were none.
remove_from_path() {
  p_removed=false
  for p_file in "${ZDOTDIR:-$HOME}/.zshrc" "$HOME/.zshrc" "$HOME/.bashrc" "$HOME/.bash_profile" "$HOME/.bash_login" "$HOME/.profile" \
      "${XDG_CONFIG_HOME:-$HOME/.config}/fish/conf.d/purlview.fish"; do
    if remove_block "$p_file"; then
      log "removed the PATH line for $DEST from $p_file"
      p_removed=true
    elif has_block "$p_file"; then
      log "warning: could not edit $p_file; remove the Purlview installer's comment and the line after it yourself"
    fi
  done
  $p_removed
}

# --- uninstall -------------------------------------------------------------------

# stop_daemon EXE: stop the daemon owned by the current user through the
# installed executable, ending its active shares. --maintenance blocks new
# daemon starts until resume_daemon runs or the marker expires (minutes), so a
# share command cannot start the old executable while it is being replaced.
# An executable too old to know the daemon commands exits 2; that is fine.
stop_daemon() {
  [ -x "$1" ] || return 0
  "$1" --version >/dev/null 2>&1 || return 0
  if out="$("$1" daemon stop --maintenance --timeout 20s 2>&1)"; then
    case "$out" in
      *"was not running"*) ;;
      *) log "stopped the running Purlview daemon (active shares ended)" ;;
    esac
  else
    case "$out" in
      *"unknown command"*|*"isn't a purlview command"*) ;; # a purlview from before the daemon command
      *) log "warning: could not stop the Purlview daemon: $out" ;;
    esac
  fi
}

resume_daemon() {
  [ -x "$1" ] || return 0
  "$1" daemon resume >/dev/null 2>&1 || true
}

# runtime_state_dirs prints the daemon's runtime, log and cache directories
# for this user; these are installation artifacts, not user data.
runtime_state_dirs() {
  if [ -n "${PURLVIEW_STATE_DIR:-}" ]; then
    printf '%s\n' "$PURLVIEW_STATE_DIR"
    return
  fi
  case "$OS" in
    darwin)
      printf '%s\n' "$HOME/Library/Application Support/purlview/runtime" "$HOME/Library/Logs/purlview" "$HOME/Library/Caches/purlview" ;;
    linux)
      [ -n "${XDG_RUNTIME_DIR:-}" ] && printf '%s\n' "$XDG_RUNTIME_DIR/purlview"
      printf '%s\n' "${XDG_STATE_HOME:-$HOME/.local/state}/purlview" "${XDG_CACHE_HOME:-$HOME/.cache}/purlview" ;;
  esac
}

# other_copy EXE prints the first purlview executable on PATH that is not EXE.
other_copy() {
  own="$(cd "$(dirname "$1")" 2>/dev/null && pwd -P)"
  old_ifs="$IFS"; IFS=:
  for dir in $PATH; do
    IFS="$old_ifs"
    if [ -z "$dir" ] || [ ! -x "$dir/purlview" ] || [ -d "$dir/purlview" ]; then continue; fi
    if [ "$(cd "$dir" 2>/dev/null && pwd -P)" = "$own" ]; then continue; fi
    printf '%s\n' "$dir/purlview"
    return
  done
  IFS="$old_ifs"
}

if $UNINSTALL; then
  absolute_dest
  target="$DEST/purlview"
  if [ ! -e "$target" ]; then
    # The executable is gone already; the PATH lines may not be.
    remove_from_path && exit 0
    fail "nothing to remove at $target"
  fi
  owner="$(managed_by "$target")"
  [ -z "$owner" ] || fail "$target is managed by $owner; remove it with that package manager"
  # The daemon and its runtime state are this user's, not this copy's: while
  # another purlview stays on PATH they are that copy's too, and are left.
  other="$(other_copy "$target")"
  if [ -n "$other" ]; then
    rm -f "$target"
    log "removed $target"
    log "another purlview remains at $other: the daemon and its runtime state were left alone"
  else
    stop_daemon "$target"
    rm -f "$target"
    log "removed $target"
    runtime_state_dirs | while IFS= read -r dir; do
      [ -d "$dir" ] || continue
      rm -rf "$dir" && log "removed runtime state $dir"
    done
  fi
  if ! remove_from_path && on_path "$DEST"; then
    log "if you added $DEST to PATH only for Purlview, you can remove it from your shell profile"
  fi
  log "account credentials (if any) are not removed by uninstall; sign out with 'purlview logout' before uninstalling to revoke them"
  exit 0
fi

# --- prerequisites ---------------------------------------------------------------

command -v curl >/dev/null 2>&1 || fail "curl is required (it is used for HTTPS downloads)"
command -v tar >/dev/null 2>&1 || fail "tar is required to extract the archive"

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
  elif command -v openssl >/dev/null 2>&1; then openssl dgst -sha256 "$1" | awk '{print $NF}'
  else fail "sha256sum, shasum or openssl is required to verify the download"
  fi
}

# fetch URL FILE downloads with bounded retries and timeouts, HTTPS only.
fetch() {
  curl --fail --silent --show-error --location \
    --proto "$CURL_PROTO" --tlsv1.2 \
    --retry 3 --retry-delay 2 --connect-timeout 20 --max-time 300 \
    --output "$2" "$1"
}

# --- version resolution --------------------------------------------------------

resolve_latest() {
  probe="$(curl --silent --show-error --head --location --output /dev/null \
    --proto "$CURL_PROTO" --tlsv1.2 --retry 3 --retry-delay 2 --connect-timeout 20 --max-time 60 \
    --write-out '%{http_code} %{url_effective}' "$RELEASE_BASE_URL/releases/latest")" \
    || fail "could not reach $RELEASE_BASE_URL/releases/latest (check your network or proxy settings)"
  code="${probe%% *}"
  redirect="${probe#* }"
  case "$code" in
    200) ;;
    404) fail "no stable release is published at $RELEASE_BASE_URL yet; pass --version to install a specific release" ;;
    *) fail "unexpected HTTP status $code from $RELEASE_BASE_URL/releases/latest" ;;
  esac
  # A published stable release redirects to .../releases/tag/<tag>; with none,
  # GitHub redirects to the release list instead.
  case "$redirect" in
    */releases/tag/?*) ;;
    *) fail "no stable release is published at $RELEASE_BASE_URL yet; pass --version to install a specific release" ;;
  esac
  tag="${redirect##*/}"
  echo "$tag"
}

if [ -z "$VERSION" ]; then
  VERSION="$(resolve_latest)"
  log "latest stable release: $VERSION"
fi
case "$VERSION" in
  v*) ;;
  *) VERSION="v$VERSION" ;;
esac
printf '%s' "$VERSION" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' \
  || fail "invalid version $VERSION; expected vX.Y.Z or vX.Y.Z-prerelease"
BARE_VERSION="${VERSION#v}"
case "$BARE_VERSION" in
  *-*) log "installing prerelease $VERSION because it was requested explicitly" ;;
esac

# --- existing installation --------------------------------------------------

existing="$(command -v purlview 2>/dev/null || true)"
if [ -n "$existing" ] && [ "$existing" != "$DEST/purlview" ]; then
  owner="$(managed_by "$existing")"
  if [ -n "$owner" ]; then
    if $DEST_EXPLICIT; then
      log "warning: $existing is managed by $owner; installing a second copy to $DEST as requested"
    else
      fail "$existing is already installed by $owner. Update it with $owner instead, or pass --dest to install a separate copy."
    fi
  else
    log "note: another purlview is on PATH at $existing; the copy in $DEST wins only if $DEST comes first in PATH"
  fi
fi
dest_owner="$(managed_by "$DEST/purlview")"
[ -z "$dest_owner" ] || fail "$DEST/purlview is managed by $dest_owner; refusing to overwrite it"

# --- download and verify ----------------------------------------------------

ARCHIVE="purlview_${BARE_VERSION}_${OS}_${ARCH}.tar.gz"
DOWNLOAD_BASE="$RELEASE_BASE_URL/releases/download/$VERSION"

WORK="$(mktemp -d "${TMPDIR:-/tmp}/purlview-install.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT INT TERM

log "downloading $ARCHIVE from $DOWNLOAD_BASE"
fetch "$DOWNLOAD_BASE/checksums.txt" "$WORK/checksums.txt" \
  || fail "could not download checksums.txt for $VERSION from $RELEASE_BASE_URL (is the version published, and is the repository public?)"
fetch "$DOWNLOAD_BASE/$ARCHIVE" "$WORK/$ARCHIVE" \
  || fail "could not download $ARCHIVE; $VERSION may not provide ${OS}/${ARCH}"

expected="$(awk -v f="$ARCHIVE" '$2 == f || $2 == "*" f { print $1; exit }' "$WORK/checksums.txt")"
[ -n "$expected" ] || fail "$ARCHIVE is not listed in checksums.txt for $VERSION"
actual="$(sha256_of "$WORK/$ARCHIVE")"
[ "$actual" = "$expected" ] || fail "checksum mismatch for $ARCHIVE (expected $expected, got $actual); the download is corrupt or incomplete"
log "checksum verified"

# Every release publishes the Sigstore bundle of checksums.txt: with cosign
# installed, a release without one, or with another signer's, is refused.
if $VERIFY_PUBLISHER && command -v cosign >/dev/null 2>&1; then
  fetch "$DOWNLOAD_BASE/checksums.txt.sigstore.json" "$WORK/checksums.txt.sigstore.json" \
    || fail "could not download the Sigstore signature of checksums.txt for $VERSION; nothing was installed"
  cosign verify-blob \
    --certificate-identity "https://github.com/$SOURCE_REPOSITORY/.github/workflows/release.yml@refs/tags/$VERSION" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    --bundle "$WORK/checksums.txt.sigstore.json" "$WORK/checksums.txt" >/dev/null 2>&1 \
    || fail "Sigstore verification of checksums.txt failed for $VERSION; nothing was installed"
  log "checksums.txt signature verified with cosign"
elif $VERIFY_PUBLISHER && [ "$OS" = linux ]; then
  log "note: the publisher's signature was not checked: cosign is not installed, and Linux executables carry none of their own. The APT and DNF packages are signed."
fi

mkdir "$WORK/extract"
tar -xzf "$WORK/$ARCHIVE" -C "$WORK/extract" || fail "could not extract $ARCHIVE"
[ -f "$WORK/extract/purlview" ] || fail "$ARCHIVE does not contain the purlview executable"
chmod 0755 "$WORK/extract/purlview"

# The executable has not run yet. On macOS it must carry a Developer ID
# signature of Purlview's team (the requirement is Apple's own for Developer
# ID code, with the team pinned); codesign is part of macOS.
if $VERIFY_PUBLISHER && [ "$OS" = darwin ]; then
  /usr/bin/codesign --verify --strict \
    -R="anchor apple generic and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = \"$APPLE_TEAM_ID\"" \
    "$WORK/extract/purlview" >/dev/null 2>&1 \
    || fail "the executable in $ARCHIVE is not signed by Purlview's publisher (Apple Developer ID team $APPLE_TEAM_ID); it was not run and nothing was installed"
  log "publisher signature verified (Apple Developer ID team $APPLE_TEAM_ID)"
fi

reported="$("$WORK/extract/purlview" --version 2>/dev/null | head -n 1 || true)"
[ "$reported" = "purlview $BARE_VERSION" ] \
  || fail "the downloaded executable did not run correctly on this machine (reported '$reported'); nothing was installed. Check the architecture with --arch, or see the installation guide for supported systems."

# --- stop the previous version's daemon --------------------------------------

# The new release is downloaded and verified; from here on daemon starts are
# blocked and the running daemon (with its shares) is stopped. If replacement
# fails, the previous executable stays usable; ended shares stay ended.
previous="$DEST/purlview"
if [ -x "$previous" ]; then
  log "note: upgrading Purlview ends any active shares"
  stop_daemon "$previous"
fi
release_maintenance() {
  if [ -x "$DEST/purlview" ]; then resume_daemon "$DEST/purlview"; else resume_daemon "$WORK/extract/purlview"; fi
}

# --- install atomically ------------------------------------------------------

mkdir -p "$DEST" 2>/dev/null || { release_maintenance; fail "cannot create $DEST"; }
[ -w "$DEST" ] || { release_maintenance; fail "cannot write to $DEST; choose a writable --dest or fix its permissions"; }
staged="$DEST/.purlview.new.$$"
cp "$WORK/extract/purlview" "$staged" 2>/dev/null || { release_maintenance; fail "cannot write to $DEST"; }
chmod 0755 "$staged"
if ! mv -f "$staged" "$DEST/purlview"; then
  rm -f "$staged"
  release_maintenance
  fail "could not replace $DEST/purlview; the previous installation is unchanged"
fi
resume_daemon "$DEST/purlview"

absolute_dest
log "installed purlview $BARE_VERSION to $DEST/purlview"

# --- PATH ----------------------------------------------------------------------

# A piped installer cannot change the PATH of the shell that runs it; a new
# terminal (or a new login shell in this one) reads the startup file.
next="run 'purlview login'"
if on_path "$DEST"; then
  :
elif $MODIFY_PATH && plain_dest && add_to_path; then
  if [ -n "${SHELL:-}" ]; then
    next="open a new terminal (or run 'exec $SHELL -l'), then run 'purlview login'"
  else
    next="open a new terminal, then run 'purlview login'"
  fi
else
  path_advice
  next="open a new terminal, then run 'purlview login'"
fi
log "update: run this installer again; pin: --version vX.Y.Z; remove: --uninstall"
log "next: $next"
}

main "$@"
