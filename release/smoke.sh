#!/usr/bin/env bash
# Smoke-test Purlview release artifacts on the machine that runs this script.
#
# Usage:
#   release/smoke.sh --dist DIR --version VERSION [--os OS] [--arch ARCH]
#                          [--previous DIR] [--allow-unsigned] [--containers]
#                          [--no-packages]
#
# The script executes the archive for this OS/architecture, checks help,
# version, completion and placeholder behaviour, proves the executable starts
# and stops its own daemon in a private state directory (and, as root in the
# package containers, that another user cannot reach it), verifies checksums,
# and on Linux installs, upgrades (when --previous names an older artifact
# set) and removes the native packages. --containers additionally runs the Fedora (rpm)
# and Alpine (musl) checks in Docker for the same architecture, and on a
# non-Linux host also the Debian (deb) checks.
#
# On macOS the signature and notarization are verified unless --allow-unsigned
# is given, in which case the result is reported but does not fail the run.
set -euo pipefail

DIST=""
VERSION=""
OS=""
ARCH=""
PREVIOUS=""
ALLOW_UNSIGNED=false
CONTAINERS=false
PACKAGES=true

while [ $# -gt 0 ]; do
  case "$1" in
    --dist) DIST="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --os) OS="$2"; shift 2 ;;
    --arch) ARCH="$2"; shift 2 ;;
    --previous) PREVIOUS="$2"; shift 2 ;;
    --allow-unsigned) ALLOW_UNSIGNED=true; shift ;;
    --containers) CONTAINERS=true; shift ;;
    --no-packages) PACKAGES=false; shift ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "smoke: unknown argument: $1" >&2; exit 2 ;;
  esac
done

if [ -z "$DIST" ] || [ -z "$VERSION" ]; then echo "smoke: --dist and --version are required" >&2; exit 2; fi
DIST="$(cd "$DIST" && pwd)"
# Absolute so it can be bind-mounted into containers.
[ -z "$PREVIOUS" ] || PREVIOUS="$(cd "$PREVIOUS" && pwd)"

if [ -z "$OS" ]; then
  case "$(uname -s)" in
    Darwin) OS=darwin ;;
    Linux) OS=linux ;;
    *) echo "smoke: unsupported host $(uname -s); use smoke.ps1 on Windows" >&2; exit 2 ;;
  esac
fi
if [ -z "$ARCH" ]; then
  case "$(uname -m)" in
    x86_64|amd64) ARCH=amd64 ;;
    arm64|aarch64) ARCH=arm64 ;;
    *) echo "smoke: unsupported architecture $(uname -m)" >&2; exit 2 ;;
  esac
fi

FAILED=0
pass() { printf 'ok    %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*" >&2; FAILED=1; }
note() { printf 'note  %s\n' "$*"; }
# check LABEL CMD... : pass or fail LABEL depending on CMD's exit status.
check() { local label="$1"; shift; if "$@"; then pass "$label"; else fail "$label"; fi; }

WORK="$(mktemp -d "${TMPDIR:-/tmp}/purlview-smoke.XXXXXX")"
trap 'rm -rf "$WORK"' EXIT

sha256_tool() {
  if command -v sha256sum >/dev/null 2>&1; then echo "sha256sum"; else echo "shasum -a 256"; fi
}

# --- artifact discovery -------------------------------------------------------

ARCHIVE="$DIST/purlview_${VERSION}_${OS}_${ARCH}.tar.gz"
[ -f "$ARCHIVE" ] || { echo "smoke: archive not found: $ARCHIVE" >&2; exit 1; }
DEB="$DIST/purlview_${VERSION}_linux_${ARCH}.deb"
RPM="$DIST/purlview_${VERSION}_linux_${ARCH}.rpm"

echo "== checksums (${OS}/${ARCH}) =="
(
  cd "$DIST"
  grep -E "purlview_${VERSION}_(${OS}_${ARCH}\.tar\.gz|linux_${ARCH}\.(deb|rpm))$" checksums.txt >"$WORK/checksums.subset"
  [ -s "$WORK/checksums.subset" ] || { echo "no checksum entries for ${OS}/${ARCH}" >&2; exit 1; }
  $(sha256_tool) -c "$WORK/checksums.subset"
) && pass "checksums.txt matches the ${OS}/${ARCH} artifacts"
[ "${PIPESTATUS[0]:-0}" -eq 0 ] || fail "checksum verification"

# --- binary behaviour ---------------------------------------------------------

check_binary() { # <binary> <label>
  local bin="$1" label="$2" out err code first
  out="$WORK/out.txt"; err="$WORK/err.txt"

  code=0; "$bin" --version >"$out" 2>"$err" || code=$?
  first="$(head -n 1 "$out")"
  if [ "$code" -eq 0 ] && [ "$first" = "purlview $VERSION" ] && [ ! -s "$err" ]; then
    pass "$label: --version reports $VERSION"
  else
    fail "$label: --version exit=$code first line='$first' stderr='$(cat "$err")'"
  fi
  if grep -q "built by: goreleaser" "$out"; then pass "$label: build identity present"; else fail "$label: build identity missing: $(cat "$out")"; fi

  code=0; "$bin" --help >"$out" 2>"$err" || code=$?
  if [ "$code" -eq 0 ] && grep -q '^Usage:' "$out" && [ ! -s "$err" ]; then pass "$label: --help"; else fail "$label: --help exit=$code"; fi

  code=0; "$bin" >"$out" 2>"$err" || code=$?
  if [ "$code" -eq 0 ] && grep -q '^Usage:' "$out"; then pass "$label: no arguments shows help"; else fail "$label: bare invocation exit=$code"; fi

  for shell in bash zsh fish powershell; do
    code=0; "$bin" completion "$shell" >"$out" 2>"$err" || code=$?
    if [ "$code" -eq 0 ] && [ -s "$out" ] && [ ! -s "$err" ]; then pass "$label: completion $shell"; else fail "$label: completion $shell exit=$code"; fi
  done
  if command -v bash >/dev/null 2>&1; then
    if "$bin" completion bash >"$out" && bash -n "$out"; then pass "$label: bash completion parses"; else fail "$label: bash completion does not parse"; fi
  fi

  # The account commands read this user's remembered credential, so they run
  # against a private, empty state directory: signed out, they give login
  # instructions; login with closed stdin must stop without issuing credentials. share starts a
  # daemon and is exercised in check_daemon.
  local astate
  astate="$(mktemp -d "${TMPDIR:-/tmp}/purlview-smoke-account.XXXXXX")"
  for placeholder in list whoami; do
    code=0; env PURLVIEW_STATE_DIR="$astate" "$bin" "$placeholder" >"$out" 2>"$err" || code=$?
    if [ "$code" -eq 1 ] && grep -q 'Not signed in — run purlview login first' "$err" && [ ! -s "$out" ]; then pass "$label: $placeholder signed out gives login instructions (exit 1, stderr only)"; else fail "$label: $placeholder exit=$code stdout='$(cat "$out")' stderr='$(cat "$err")'"; fi
  done
  code=0; env PURLVIEW_STATE_DIR="$astate" "$bin" login </dev/null >"$out" 2>"$err" || code=$?
  if [ "$code" -eq 1 ] && grep -q '^  Email: $' "$err" && grep -q 'Sign-in was not finished' "$err" && [ ! -s "$out" ]; then pass "$label: email login stops on closed input (exit 1, stderr only)"; else fail "$label: login exit=$code stdout='$(cat "$out")' stderr='$(cat "$err")'"; fi
  if [ -e "$astate" ] && [ -n "$(ls -A "$astate" 2>/dev/null)" ]; then fail "$label: account commands created state"; fi
  rm -rf "$astate"

  code=0; "$bin" bogus >"$out" 2>"$err" || code=$?
  if [ "$code" -eq 2 ] && grep -q "isn't a purlview command" "$err"; then pass "$label: unknown command exits 2"; else fail "$label: unknown command exit=$code"; fi

  check_daemon "$bin" "$label"

  if [ "$OS" = linux ] && command -v unshare >/dev/null 2>&1; then
    if unshare -rn true 2>/dev/null; then
      if unshare -rn "$bin" --version >/dev/null 2>&1 && unshare -rn "$bin" completion zsh >/dev/null 2>&1; then
        pass "$label: version and completion work without a network namespace"
      else
        fail "$label: version/completion failed in a network-less namespace"
      fi
    else
      note "$label: unshare -rn unavailable here; offline check skipped"
    fi
  fi
}

# check_daemon proves the packaged executable starts, reuses and stops its
# own daemon in a private state directory. The share command must start the
# daemon and then stop honestly (no credential, no terminal); nothing may
# reach the network (the update check is pointed at a closed loopback port).
check_daemon() { # <binary> <label>
  local bin="$1" label="$2" state out err code
  state="$(mktemp -d "${TMPDIR:-/tmp}/purlview-smoke-state.XXXXXX")"
  out="$WORK/d-out.txt"; err="$WORK/d-err.txt"
  local -a env=(env "PURLVIEW_STATE_DIR=$state" "PURLVIEW_UPDATE_API_URL=http://127.0.0.1:1" "PURLVIEW_DAEMON_IDLE_EXIT=30s" "PURLVIEW_DAEMON_START_TIMEOUT=20s")

  code=0; "${env[@]}" "$bin" daemon status >"$out" 2>"$err" || code=$?
  if [ "$code" -eq 1 ] && grep -q 'not running' "$err"; then pass "$label: daemon status reports not running"; else fail "$label: daemon status before start exit=$code $(cat "$err")"; fi
  if [ -e "$state" ] && [ -n "$(ls -A "$state" 2>/dev/null)" ]; then fail "$label: daemon status created state"; fi

  # Signed out and without a terminal, share bootstraps the daemon and then
  # stops with login instructions; nothing is claimed on stdout.
  code=0; "${env[@]}" "$bin" share localhost:3000 >"$out" 2>"$err" || code=$?
  if [ "$code" -eq 1 ] && grep -q 'Not signed in — run purlview login first' "$err" && [ ! -s "$out" ]; then
    pass "$label: share starts the daemon and stops honestly (signed out)"
  else
    fail "$label: share with daemon exit=$code stdout='$(cat "$out")' stderr='$(cat "$err")'"
  fi
  code=0; "${env[@]}" "$bin" daemon status >"$out" 2>"$err" || code=$?
  if [ "$code" -eq 0 ] && grep -q "version $VERSION" "$out" && grep -q 'console:    none' "$out"; then pass "$label: daemon running after share ($VERSION, no console)"; else fail "$label: daemon status after share exit=$code $(cat "$out" "$err")"; fi
  code=0; "${env[@]}" "$bin" daemon start >"$out" 2>"$err" || code=$?
  if [ "$code" -eq 0 ] && grep -q 'daemon reused' "$out"; then pass "$label: second start reuses the daemon"; else fail "$label: daemon start exit=$code $(cat "$out" "$err")"; fi
  code=0; "${env[@]}" "$bin" daemon stop >"$out" 2>"$err" || code=$?
  if [ "$code" -eq 0 ] && grep -q 'stopped' "$out"; then pass "$label: daemon stop"; else fail "$label: daemon stop exit=$code $(cat "$out" "$err")"; fi
  code=0; "${env[@]}" "$bin" daemon status >"$out" 2>"$err" || code=$?
  if [ "$code" -eq 1 ]; then pass "$label: daemon gone after stop"; else fail "$label: daemon still reported after stop: $(cat "$out")"; fi
  if [ -e "$state/runtime/endpoint.json" ]; then fail "$label: endpoint record left after stop"; fi
  if grep -q "daemon .* started" "$state/logs/daemon.log" 2>/dev/null && grep -q "exited (requested)" "$state/logs/daemon.log" 2>/dev/null; then pass "$label: daemon log records start and exit"; else fail "$label: daemon log incomplete: $(cat "$state/logs/daemon.log" 2>/dev/null)"; fi
  if [ "$(id -u)" -eq 0 ] && [ "$OS" = linux ]; then check_daemon_other_user "$bin" "$label" "$state"; fi
  rm -rf "$state"
}

# check_daemon_other_user (root only): a daemon started by root must not be
# reachable by another local user, and that user gets their own state. It
# runs inside the package-check containers, which have root.
check_daemon_other_user() { # <binary> <label> <root state dir>
  local bin="$1" label="$2" state="$3" other=purlview-smoke otherhome out err code
  if ! id "$other" >/dev/null 2>&1; then
    if command -v useradd >/dev/null 2>&1; then useradd -m "$other" 2>/dev/null || true; elif command -v adduser >/dev/null 2>&1; then adduser -D "$other" 2>/dev/null || true; fi
  fi
  id "$other" >/dev/null 2>&1 || { note "$label: cannot create a second user here; cross-user check skipped"; return; }
  # run_as CMD: run a shell command as the other user with whichever
  # switch-user tool the image has.
  if command -v su >/dev/null 2>&1; then run_as() { su -s /bin/sh "$other" -c "$1"; }
  elif command -v runuser >/dev/null 2>&1; then run_as() { runuser -u "$other" -- sh -c "$1"; }
  elif command -v setpriv >/dev/null 2>&1; then run_as() { setpriv --reuid="$other" --regid="$(id -g "$other")" --init-groups sh -c "$1"; }
  else note "$label: no su/runuser/setpriv; cross-user check skipped"; return
  fi
  otherhome="$(getent passwd "$other" | cut -d: -f6)"
  out="$WORK/u-out.txt"; err="$WORK/u-err.txt"
  # The executable under test may sit in root's private work directory;
  # give the other user a readable copy so the check exercises the daemon's
  # protection, not the temporary directory's mode.
  local shared
  shared="$(mktemp -d /tmp/purlview-smoke-shared.XXXXXX)"
  chmod 755 "$shared" && cp "$bin" "$shared/purlview" && chmod 755 "$shared/purlview"
  env PURLVIEW_STATE_DIR="$state" PURLVIEW_UPDATE_API_URL=http://127.0.0.1:1 "$bin" daemon start >/dev/null 2>&1 || { fail "$label: root daemon start for cross-user check"; rm -rf "$shared"; return; }
  # The other user is pointed at root's state directory: the OS must refuse.
  code=0; run_as "HOME='$otherhome' PURLVIEW_STATE_DIR='$state' '$shared/purlview' daemon status" >"$out" 2>"$err" || code=$?
  if [ "$code" -eq 1 ] && ! grep -q 'daemon: running' "$out" && grep -qi 'not accessible\|permission denied' "$err"; then pass "$label: another user cannot reach the daemon ($(head -n1 "$err"))"; else fail "$label: another user reached root's daemon or got an unexpected error (exit $code): $(cat "$out" "$err")"; fi
  # And gets their own, separate daemon in their own state.
  code=0; run_as "HOME='$otherhome' PURLVIEW_UPDATE_API_URL=http://127.0.0.1:1 PURLVIEW_DAEMON_IDLE_EXIT=30s '$shared/purlview' daemon start && '$shared/purlview' daemon stop" >"$out" 2>"$err" || code=$?
  if [ "$code" -eq 0 ] && grep -q 'daemon started' "$out" && grep -q 'stopped' "$out"; then pass "$label: another user starts and stops their own daemon"; else fail "$label: other user's daemon exit=$code $(cat "$out" "$err")"; fi
  env PURLVIEW_STATE_DIR="$state" "$bin" daemon stop >/dev/null 2>&1 || true
  rm -rf "$shared"
}

HOST_OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
if [ "$HOST_OS" != "$OS" ]; then
  $CONTAINERS || { echo "smoke: host is $HOST_OS but --os $OS was requested; add --containers" >&2; exit 2; }
  note "host is $HOST_OS; the $OS archive runs only inside containers"
else
echo "== archive =="
mkdir -p "$WORK/archive"
tar -xzf "$ARCHIVE" -C "$WORK/archive"
for f in purlview completions/purlview.bash completions/_purlview completions/purlview.fish completions/purlview.ps1 man/purlview.1; do
  check "archive contains $f" test -f "$WORK/archive/$f"
done
check "archive binary is executable" test -x "$WORK/archive/purlview"
check_binary "$WORK/archive/purlview" "archive"

fi # host archive checks

# --- macOS trust --------------------------------------------------------------

if [ "$OS" = darwin ] && [ "$HOST_OS" = darwin ]; then
  echo "== macOS signature and notarization =="
  BIN="$WORK/archive/purlview"
  # Go ad-hoc signs arm64 executables, so a verifiable signature is not
  # enough: require a Developer ID authority.
  SIGNED=false
  if codesign --verify --strict --verbose=2 "$BIN" 2>"$WORK/codesign.txt" && codesign -dvv "$BIN" 2>&1 | grep -q '^Authority=Developer ID Application:'; then
    SIGNED=true
    pass "codesign verifies: $(codesign -dvv "$BIN" 2>&1 | grep '^Authority=Developer ID Application:' | head -n1)"
  elif $ALLOW_UNSIGNED; then
    note "binary has no Developer ID signature (allowed): $(codesign -dvv "$BIN" 2>&1 | grep -E '^(Signature|Authority)=' | head -n1)"
  else
    fail "binary has no Developer ID signature: $(codesign -dvv "$BIN" 2>&1 | grep -E '^(Signature|Authority)=' | head -n1)"
  fi
  # A bare executable is assessed as an install; --type execute only accepts
  # app bundles.
  if spctl --assess --type install --verbose=2 "$BIN" 2>"$WORK/spctl.txt" && grep -q '^source=Notarized Developer ID$' "$WORK/spctl.txt"; then
    pass "spctl accepts the executable: $(tr '\n' ' ' <"$WORK/spctl.txt")"
  elif $ALLOW_UNSIGNED && [ "$SIGNED" = false ]; then
    note "spctl rejects the unsigned executable (allowed)"
  else
    fail "spctl rejects the executable: $(cat "$WORK/spctl.txt")"
  fi
  # A browser download carries the quarantine attribute; emulate it.
  cp "$BIN" "$WORK/quarantined"
  xattr -w com.apple.quarantine "0083;$(printf '%x' "$(date +%s)");smoke;" "$WORK/quarantined"
  if "$WORK/quarantined" --version >/dev/null 2>"$WORK/q.txt"; then
    pass "quarantined copy runs (Gatekeeper accepted it)"
  elif $ALLOW_UNSIGNED && [ "$SIGNED" = false ]; then
    note "quarantined unsigned copy was blocked by Gatekeeper (allowed for this rehearsal)"
  else
    fail "quarantined copy blocked: $(cat "$WORK/q.txt")"
  fi
fi

# --- Linux packages -----------------------------------------------------------

deb_checks() { # runs on a Debian-family host as root
  local prev=""
  if [ -n "$PREVIOUS" ]; then prev="$(find "$PREVIOUS" -maxdepth 1 -name "purlview_*_linux_${ARCH}.deb" | head -n1)"; fi
  if [ -n "$prev" ]; then
    if dpkg -i "$prev" >/dev/null; then pass "deb: previous version installed ($(dpkg-query -W -f='${Version}' purlview))"; else fail "deb: previous install"; fi
  fi
  if dpkg -i "$DEB" >/dev/null; then pass "deb: installed ($(dpkg-query -W -f='${Version}' purlview))"; else fail "deb: install"; fi
  check_binary /usr/bin/purlview "deb"
  for f in /usr/share/bash-completion/completions/purlview /usr/share/zsh/vendor-completions/_purlview /usr/share/fish/vendor_completions.d/purlview.fish /usr/share/man/man1/purlview.1.gz; do
    check "deb: package ships $f" sh -c "dpkg-deb -c '$DEB' | grep -q ' \\.$f\$'"
    # Minimized images exclude documentation paths through dpkg path-exclude.
    if [ -f "$f" ]; then pass "deb: $f installed"; elif grep -qs '^path-exclude' /etc/dpkg/dpkg.cfg.d/* 2>/dev/null; then note "deb: $f excluded by this image's dpkg configuration"; else fail "deb: missing $f"; fi
  done
  if dpkg -r purlview >/dev/null && [ ! -e /usr/bin/purlview ]; then pass "deb: removed cleanly"; else fail "deb: removal"; fi
}

rpm_checks() { # runs on a Fedora-family host as root
  local prev=""
  if [ -n "$PREVIOUS" ]; then prev="$(find "$PREVIOUS" -maxdepth 1 -name "purlview_*_linux_${ARCH}.rpm" | head -n1)"; fi
  if [ -n "$prev" ]; then
    if rpm -i "$prev"; then pass "rpm: previous version installed ($(rpm -q purlview))"; else fail "rpm: previous install"; fi
    if rpm -U "$RPM"; then pass "rpm: upgraded to $(rpm -q purlview)"; else fail "rpm: upgrade"; fi
  else
    if rpm -i "$RPM"; then pass "rpm: installed ($(rpm -q purlview))"; else fail "rpm: install"; fi
  fi
  check_binary /usr/bin/purlview "rpm"
  for f in /usr/share/bash-completion/completions/purlview /usr/share/zsh/site-functions/_purlview /usr/share/fish/vendor_completions.d/purlview.fish /usr/share/man/man1/purlview.1.gz; do
    check "rpm: package ships $f" sh -c "rpm -qlp '$RPM' | grep -qx '$f'"
    if [ -f "$f" ]; then pass "rpm: $f installed"; elif grep -qs 'nodocs' /etc/dnf/dnf.conf /etc/rpm/macros* 2>/dev/null; then note "rpm: $f excluded by this image's rpm configuration"; else fail "rpm: missing $f"; fi
  done
  if rpm -e purlview && [ ! -e /usr/bin/purlview ]; then pass "rpm: removed cleanly"; else fail "rpm: removal"; fi
}

case "${SMOKE_ROLE:-}" in
  deb) echo "== deb (host) =="; deb_checks; exit $FAILED ;;
  rpm) echo "== rpm (host) =="; rpm_checks; exit $FAILED ;;
  alpine) echo "== alpine (host) =="; exit $FAILED ;;
esac

as_root() { if [ "$(id -u)" -eq 0 ]; then "$@"; else sudo -E "$@"; fi; }

if [ "$OS" = linux ] && [ "$HOST_OS" = linux ] && $PACKAGES; then
  if command -v dpkg >/dev/null 2>&1 && [ -f "$DEB" ]; then
    echo "== deb (this host) =="
    if [ "$(id -u)" -eq 0 ] || sudo -n true 2>/dev/null; then
      as_root env SMOKE_ROLE=deb PREVIOUS="$PREVIOUS" "$0" --dist "$DIST" --version "$VERSION" --os linux --arch "$ARCH" ${PREVIOUS:+--previous "$PREVIOUS"} || FAILED=1
    else
      note "not root and no passwordless sudo; deb install skipped on this host"
    fi
  fi
fi

if $CONTAINERS; then
  command -v docker >/dev/null 2>&1 || { fail "--containers requires docker"; exit 1; }
  run_container() { # <image> <role>
    local image="$1" role="$2" mounts=(-v "$DIST:/dist:ro" -v "$(cd "$(dirname "$0")" && pwd)/smoke.sh:/smoke.sh:ro")
    [ -n "$PREVIOUS" ] && mounts+=(-v "$PREVIOUS:/previous:ro")
    docker run --rm --platform "linux/$ARCH" "${mounts[@]}" -e SMOKE_ROLE="$role" -e PREVIOUS="${PREVIOUS:+/previous}" "$image" \
      sh -c 'command -v bash >/dev/null 2>&1 || apk add --no-cache bash >/dev/null 2>&1; exec bash /smoke.sh --dist /dist --version "$0" --os linux --arch "$1" '"${PREVIOUS:+--previous /previous}" "$VERSION" "$ARCH"
  }
  if $PACKAGES && { [ "$HOST_OS" != linux ] || ! command -v dpkg >/dev/null 2>&1; }; then
    echo "== deb (ubuntu:24.04 container, linux/$ARCH) =="; run_container ubuntu:24.04 deb || FAILED=1
  fi
  if $PACKAGES; then echo "== rpm (fedora:42 container, linux/$ARCH) =="; run_container fedora:42 rpm || FAILED=1; fi
  echo "== alpine:3.22 container (musl, linux/$ARCH) =="
  docker run --rm --platform "linux/$ARCH" -v "$DIST:/dist:ro" -v "$(cd "$(dirname "$0")" && pwd)/smoke.sh:/smoke.sh:ro" alpine:3.22 \
    sh -c 'apk add --no-cache bash tar >/dev/null 2>&1 && exec bash /smoke.sh --dist /dist --version "$0" --os linux --arch "$1" --no-packages' "$VERSION" "$ARCH" || FAILED=1
fi

if [ "$FAILED" -eq 0 ]; then echo "smoke: all checks passed for ${OS}/${ARCH} ${VERSION}"; else echo "smoke: FAILURES for ${OS}/${ARCH} ${VERSION}" >&2; fi
exit $FAILED
