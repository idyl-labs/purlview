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

# End-to-end test of package-repository.sh in containers: build the APT and
# DNF repositories from a release's packages with a throwaway key, serve them,
# then install purlview with apt on Debian and dnf on Fedora through signature
# checks. Also checks that a repeated run changes nothing and that a
# conflicting package is refused.
#
# Usage: test-package-repository.sh --dist DIR --version X.Y.Z[-pre.N]
#   DIR holds purlview_<version>_linux_{amd64,arm64}.{deb,rpm} and checksums.txt.
# Needs: docker.
set -euo pipefail

DIST=""; VERSION=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dist) DIST="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    *) echo "test-package-repository: unknown argument $1" >&2; exit 2 ;;
  esac
done
[[ -n "$DIST" && -n "$VERSION" ]] || { echo "usage: test-package-repository.sh --dist DIR --version X.Y.Z" >&2; exit 2; }
DIST="$(cd "$DIST" && pwd)"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ "$VERSION" == *-* ]]; then SUITE=prerelease; else SUITE=stable; fi

WORK="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/package-repository-test.XXXXXX")"
NET="purlview-pkg-test-$$"
cleanup() {
  docker rm -f "$NET-server" >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$WORK" 2>/dev/null || docker run --rm -v "$WORK:/w" ubuntu:24.04 rm -rf /w/repo /w/conflict >/dev/null 2>&1 || true
}
trap cleanup EXIT
pass() { echo "ok   $*"; }

# Build twice (the second run must change nothing), then prove a conflicting
# package is refused. The key is generated inside the container and discarded.
docker run --rm -v "$HERE:/release:ro" -v "$DIST:/dist:ro" -v "$WORK:/w" -e VERSION="$VERSION" ubuntu:24.04 bash -euo pipefail -c '
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq >/dev/null && apt-get install -y -qq dpkg-dev apt-utils rpm createrepo-c gnupg >/dev/null
  export GNUPGHOME=$(mktemp -d)
  gpg --batch --quiet --passphrase "" --quick-generate-key "Purlview Packages Test <test@example.invalid>" rsa3072 sign never
  fpr=$(gpg --batch --with-colons --list-secret-keys | awk -F: "\$1 == \"fpr\" { print \$10; exit }")
  gpg --batch --armor --export "$fpr" >/w/key.asc
  /release/package-repository.sh --dist /dist --version "$VERSION" --repo /w/repo --public-key /w/key.asc --fingerprint "$fpr"
  find /w/repo -type f -exec sha256sum {} + | sort >/w/first.sum
  /release/package-repository.sh --dist /dist --version "$VERSION" --repo /w/repo --public-key /w/key.asc --fingerprint "$fpr" >/w/second.log
  find /w/repo -type f -exec sha256sum {} + | sort >/w/second.sum
  cmp -s /w/first.sum /w/second.sum || { echo "a repeated run changed the repository" >&2; exit 1; }
  grep -q "already present" /w/second.log || { echo "a repeated run did not recognise the packages" >&2; exit 1; }
  cp -a /w/repo /w/conflict
  deb=$(find /w/conflict/deb/pool -name "*_linux_amd64.deb" | head -n1)
  cp "$(find /w/conflict/deb/pool -name "*_linux_arm64.deb" | head -n1)" "$deb"
  if /release/package-repository.sh --dist /dist --version "$VERSION" --repo /w/conflict --public-key /w/key.asc --fingerprint "$fpr" 2>/w/conflict.log; then
    echo "a conflicting package was accepted" >&2; exit 1
  fi
  grep -q "already has a different" /w/conflict.log || { cat /w/conflict.log >&2; exit 1; }
  chmod -R a+rX /w
'
pass "repository built and signed; a repeated run changes nothing; a conflicting package is refused"

docker network create "$NET" >/dev/null
docker run -d --name "$NET-server" --network "$NET" -v "$WORK/repo:/srv:ro" python:3-alpine python -m http.server 8000 -d /srv >/dev/null
for _ in $(seq 1 30); do docker run --rm --network "$NET" alpine:3 wget -q -O /dev/null "http://$NET-server:8000/purlview.asc" 2>/dev/null && break; sleep 1; done
BASE="http://$NET-server:8000"

# The published .sources and .repo files name pkg.purlview.com; point them at
# the test server and otherwise use them exactly as a user would.
docker run --rm --network "$NET" -e BASE="$BASE" -e SUITE="$SUITE" -e VERSION="$VERSION" debian:12 bash -euo pipefail -c '
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq >/dev/null && apt-get install -y -qq curl ca-certificates >/dev/null
  curl -fsSL "$BASE/purlview-archive-keyring.gpg" -o /usr/share/keyrings/purlview-archive-keyring.gpg
  curl -fsSL "$BASE/deb/purlview-$SUITE.sources" | sed "s#https://pkg.purlview.com#$BASE#" >/etc/apt/sources.list.d/purlview.sources
  apt-get update -qq >/dev/null
  apt-get install -y -qq purlview >/dev/null
  purlview --version | head -n1 | grep -qx "purlview $VERSION"
'
pass "apt installs purlview $VERSION from the $SUITE suite on Debian 12 with signature checks"

docker run --rm --network "$NET" -e BASE="$BASE" -e SUITE="$SUITE" -e VERSION="$VERSION" fedora:41 bash -euo pipefail -c '
  curl -fsSL "$BASE/rpm/purlview-$SUITE.repo" | sed "s#https://pkg.purlview.com#$BASE#" >/etc/yum.repos.d/purlview.repo
  dnf install -y -q purlview >/dev/null
  purlview --version | head -n1 | grep -qx "purlview $VERSION"
  rpm -qi purlview | grep -Eq "^Signature *: RSA/SHA(256|512), .*Key ID"
'
pass "dnf installs purlview $VERSION from the $SUITE suite on Fedora 41 with package and metadata signature checks"
