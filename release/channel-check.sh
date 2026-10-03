#!/usr/bin/env bash
# Installs Purlview through one public channel exactly as purlview.com tells
# people to, then checks the installed CLI: it is the expected release, it
# runs, and it answers without a sign-in. install-check.yml runs it on every
# macOS and Linux runner a channel supports; channel-check.ps1 is Windows.
#
#   channel-check.sh <script|homebrew|apt|dnf> <vX.Y.Z>
#
# It changes the machine it runs on (packages, repositories), so it runs on
# throwaway CI runners and containers only.
set -euo pipefail
method="${1:?method}" want="${2:?version}"
fail() { echo "FAIL $*" >&2; exit 1; }
as_root() { if [ "$(id -u)" = 0 ]; then "$@"; else sudo "$@"; fi; }
# new_terminal runs a command the way a newly opened terminal would: the
# user's shell, interactive (zsh as a login shell, as macOS Terminal opens
# it), with none of this session's environment.
new_terminal() {
  local flags=-ic
  [ "$(uname -s)" = Darwin ] && flags=-lic
  env -i HOME="$HOME" USER="${USER:-$(id -un)}" SHELL="$SHELL" TERM=dumb PATH=/usr/bin:/bin "$SHELL" "$flags" "$1" 2>/dev/null || true
}
marks() { grep -c -F '# Added by the Purlview installer' "$1" || true; }

case "$method" in
  script)
    # A new user's default shell: zsh on macOS, bash on Linux. The installer
    # puts ~/.local/bin on PATH in that shell's startup file.
    if [ "$(uname -s)" = Darwin ]; then SHELL=/bin/zsh; else SHELL=/bin/bash; fi
    export SHELL
    # CI runners already have ~/.local/bin on PATH; a new user doesn't.
    fresh_path=/usr/bin:/bin:/usr/sbin:/sbin
    out="$(curl -fsSL https://purlview.com/install.sh | PATH="$fresh_path" sh 2>&1)"
    echo "$out"
    rc="$(sed -n "s|^purlview-install: added $HOME/.local/bin to PATH in ||p" <<<"$out" | head -n 1)"
    [ -n "$rc" ] || fail "the installer did not add ~/.local/bin to PATH"
    grep -q '^purlview-install: next: open a new terminal' <<<"$out" || fail "the installer did not say what to do next"
    # A new terminal, with nothing from this session, finds and runs purlview.
    found="$(new_terminal 'command -v purlview' | tail -n 1)"
    [ "$found" = "$HOME/.local/bin/purlview" ] || fail "a new terminal found '$found', want $HOME/.local/bin/purlview"
    fresh="$(new_terminal 'purlview --version' | grep '^purlview ' | head -n 1)"
    [ "${fresh#purlview }" = "$want" ] || [ "${fresh#purlview }" = "${want#v}" ] || fail "a new terminal ran '$fresh', want $want"
    echo "ok   a new terminal runs $fresh with no manual step ($rc)"
    # Installing again adds nothing more.
    again="$(curl -fsSL https://purlview.com/install.sh | PATH="$fresh_path" sh 2>&1)"
    grep -q "already adds $HOME/.local/bin to PATH" <<<"$again" || fail "a second install did not recognise its PATH line: $again"
    [ "$(marks "$rc")" = 1 ] || fail "$rc holds the installer's PATH line $(marks "$rc") times after a second install"
    echo "ok   a second install leaves one PATH line"
    export PATH="$HOME/.local/bin:$PATH"
    ;;
  homebrew)
    brew install --cask idyl-labs/tap/purlview
    ;;
  apt)
    as_root curl -fsSL https://pkg.purlview.com/purlview-archive-keyring.gpg -o /usr/share/keyrings/purlview-archive-keyring.gpg
    as_root curl -fsSL https://pkg.purlview.com/deb/purlview-stable.sources -o /etc/apt/sources.list.d/purlview.sources
    as_root apt-get update
    as_root apt-get install -y purlview
    ;;
  dnf)
    as_root curl -fsSL https://pkg.purlview.com/rpm/purlview-stable.repo -o /etc/yum.repos.d/purlview.repo
    as_root dnf install -y purlview
    ;;
  *) fail "unknown method $method" ;;
esac

bin="$(command -v purlview)" || fail "purlview is not on PATH after the $method install"
echo "ok   $method installed $bin"

# A fresh state directory, no update check and no platform traffic beyond
# the sign-in check below.
PURLVIEW_STATE_DIR="$(mktemp -d)"
export PURLVIEW_STATE_DIR PURLVIEW_NO_UPDATE_CHECK=1
got="$(purlview --version)"
got="${got%%$'\n'*}"
[ "${got#purlview }" = "$want" ] || [ "${got#purlview }" = "${want#v}" ] || fail "installed '$got', want $want"
echo "ok   $got"
if out="$(purlview list 2>&1)"; then fail "list succeeded without a sign-in: $out"; fi
grep -q "Not signed in" <<<"$out" || fail "list without a sign-in said: $out"
echo "ok   runs and asks for a sign-in"

if [ "$method" = script ]; then
  curl -fsSL https://purlview.com/install.sh | PATH="$fresh_path" sh -s -- --uninstall
  [ "$(marks "$rc")" = 0 ] || fail "$rc still holds the installer's PATH line after --uninstall"
  gone="$(new_terminal 'command -v purlview' | tail -n 1)"
  [ -z "$gone" ] || fail "a new terminal still finds $gone after --uninstall"
  echo "ok   --uninstall removes the PATH line and the command"
fi
