#!/usr/bin/env bash
# Add one release's Linux packages to the signed APT and DNF repositories
# served at https://pkg.purlview.com, then rebuild and sign their
# metadata. It works on a local copy of the repository tree; --publish uploads
# that tree afterwards.
#
# A stable release enters the "stable" and "prerelease" suites; a prerelease
# enters "prerelease" only. A version older than one a suite already carries is
# refused, a package already present with the same content is a no-op, and a
# package already present with different content is refused.
#
# Usage: package-repository.sh --dist DIR --version 0.2.0 --repo DIR
#          --public-key FILE --fingerprint HEX [--publish s3://bucket]
#   DIR (--dist) holds purlview_<version>_linux_{amd64,arm64}.{deb,rpm} and
#   checksums.txt, whose Sigstore bundle the caller has already verified.
# Environment: GNUPGHOME holding the secret key named by --fingerprint.
# Needs: dpkg-deb, dpkg-scanpackages, apt-ftparchive, rpm, rpmsign,
# createrepo_c, gpg, gpgv; aws for --publish.
set -euo pipefail

DIST=""; VERSION=""; REPO=""; PUBLIC_KEY=""; FINGERPRINT=""; PUBLISH=""
while [ $# -gt 0 ]; do
  case "$1" in
    --dist) DIST="$2"; shift 2 ;;
    --version) VERSION="$2"; shift 2 ;;
    --repo) REPO="$2"; shift 2 ;;
    --public-key) PUBLIC_KEY="$2"; shift 2 ;;
    --fingerprint) FINGERPRINT="$2"; shift 2 ;;
    --publish) PUBLISH="$2"; shift 2 ;;
    *) echo "package-repository: unknown argument $1" >&2; exit 2 ;;
  esac
done
[[ -n "$DIST" && -n "$VERSION" && -n "$REPO" && -n "$PUBLIC_KEY" && -n "$FINGERPRINT" ]] \
  || { echo "usage: package-repository.sh --dist DIR --version X.Y.Z --repo DIR --public-key FILE --fingerprint HEX [--publish s3://bucket]" >&2; exit 2; }
die() { echo "package-repository: $*" >&2; exit 1; }
log() { echo "package-repository: $*"; }

[[ "$VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-(alpha|beta|rc)\.[0-9]+)?$ ]] || die "version $VERSION is not X.Y.Z or X.Y.Z-(alpha|beta|rc).N"
[[ "$FINGERPRINT" =~ ^[0-9A-F]{40}$ ]] || die "fingerprint must be 40 upper-case hex digits"
[[ -z "$PUBLISH" || "$PUBLISH" =~ ^s3://[a-z0-9.-]+$ ]] || die "--publish must be s3://bucket"
[[ -f "$PUBLIC_KEY" ]] || die "$PUBLIC_KEY not found"
mkdir -p "$REPO"
DIST="$(cd "$DIST" && pwd)"; REPO="$(cd "$REPO" && pwd)"

# Debian orders "~" before anything, which is how a SemVer prerelease sorts
# below its release; nfpm writes 0.2.0-rc.1 as 0.2.0~rc.1.
DEB_VERSION="${VERSION/-/\~}"
if [[ "$VERSION" == *-* ]]; then SUITES=(prerelease); else SUITES=(stable prerelease); fi
ARCHES=(amd64 arm64)
rpm_arch() { case "$1" in amd64) echo x86_64 ;; arm64) echo aarch64 ;; esac; }

WORK="$(mktemp -d)"; trap 'rm -rf "$WORK"' EXIT

# --- signing key ----------------------------------------------------------------

gpg --batch --yes --dearmor -o "$WORK/keyring.gpg" <"$PUBLIC_KEY"
key_fprs="$(gpg --batch --with-colons --show-keys "$WORK/keyring.gpg" | awk -F: '$1 == "fpr" { print $10; exit }')"
[[ "$key_fprs" == "$FINGERPRINT" ]] || die "$PUBLIC_KEY is key $key_fprs, not $FINGERPRINT"
gpg --batch --list-secret-keys "$FINGERPRINT" >/dev/null 2>&1 || die "GNUPGHOME has no secret key $FINGERPRINT"
sign() { gpg --batch --yes --pinentry-mode loopback --local-user "$FINGERPRINT" "$@"; }

# --- incoming packages ----------------------------------------------------------

[[ -f "$DIST/checksums.txt" ]] || die "$DIST/checksums.txt not found"
for arch in "${ARCHES[@]}"; do
  for ext in deb rpm; do
    name="purlview_${VERSION}_linux_${arch}.${ext}"
    [[ -f "$DIST/$name" ]] || die "$DIST/$name not found"
    want="$(awk -v n="$name" '$2 == n { print $1 }' "$DIST/checksums.txt")"
    [[ -n "$want" ]] || die "$name is not listed in checksums.txt"
    [[ "$(sha256sum "$DIST/$name" | cut -d' ' -f1)" == "$want" ]] || die "$name does not match checksums.txt"
  done
  deb="$DIST/purlview_${VERSION}_linux_${arch}.deb"
  [[ "$(dpkg-deb -f "$deb" Package)" == purlview ]] || die "$deb is not the purlview package"
  [[ "$(dpkg-deb -f "$deb" Version)" == "$DEB_VERSION" ]] || die "$deb is not version $DEB_VERSION"
  [[ "$(dpkg-deb -f "$deb" Architecture)" == "$arch" ]] || die "$deb is not for $arch"
  rpm="$DIST/purlview_${VERSION}_linux_${arch}.rpm"
  [[ "$(rpm -qp --qf '%{NAME} %{ARCH}' "$rpm" 2>/dev/null)" == "purlview $(rpm_arch "$arch")" ]] || die "$rpm is not the purlview package for $(rpm_arch "$arch")"
done

# newest_in prints the highest Debian version among a suite's .deb files.
newest_in() {
  local newest="" v f
  for f in "$1"/*.deb; do
    [[ -e "$f" ]] || continue
    v="$(dpkg-deb -f "$f" Version)"
    if [[ -z "$newest" ]] || dpkg --compare-versions "$v" gt "$newest"; then newest="$v"; fi
  done
  echo "$newest"
}

# --- APT ------------------------------------------------------------------------

for suite in "${SUITES[@]}"; do
  pool="$REPO/deb/pool/$suite/p/purlview"
  mkdir -p "$pool"
  newest="$(newest_in "$pool")"
  if [[ -n "$newest" ]] && dpkg --compare-versions "$DEB_VERSION" lt "$newest"; then
    die "APT suite $suite already carries $newest, newer than $DEB_VERSION; refusing to roll it back"
  fi
  added=false
  for arch in "${ARCHES[@]}"; do
    name="purlview_${VERSION}_linux_${arch}.deb"
    if [[ -f "$pool/$name" ]]; then
      cmp -s "$pool/$name" "$DIST/$name" || die "APT suite $suite already has a different $name"
      log "APT $suite: $name already present"
    else
      cp "$DIST/$name" "$pool/$name"
      added=true
      log "APT $suite: added $name"
    fi
  done

  # Signed metadata is rewritten only when the suite gained a package, so a
  # repeated run leaves the repository byte for byte as it was.
  dists="$REPO/deb/dists/$suite"
  if ! $added && [[ -f "$dists/InRelease" ]]; then
    log "APT $suite: unchanged"
    continue
  fi
  rm -rf "$dists"
  for arch in "${ARCHES[@]}"; do
    mkdir -p "$dists/main/binary-$arch"
    (cd "$REPO/deb" && dpkg-scanpackages --multiversion --arch "$arch" "pool/$suite" /dev/null 2>/dev/null) >"$dists/main/binary-$arch/Packages"
    gzip -9n <"$dists/main/binary-$arch/Packages" >"$dists/main/binary-$arch/Packages.gz"
  done
  (cd "$dists" && apt-ftparchive \
    -o APT::FTPArchive::Release::Origin=Purlview \
    -o APT::FTPArchive::Release::Label=Purlview \
    -o APT::FTPArchive::Release::Suite="$suite" \
    -o APT::FTPArchive::Release::Codename="$suite" \
    -o APT::FTPArchive::Release::Architectures="amd64 arm64" \
    -o APT::FTPArchive::Release::Components=main \
    -o APT::FTPArchive::Release::Description="Purlview command-line tool, $suite releases" \
    release . >"$WORK/Release")
  mv "$WORK/Release" "$dists/Release"
  sign --clearsign -o "$dists/InRelease" "$dists/Release"
  sign --armor --detach-sign -o "$dists/Release.gpg" "$dists/Release"
  gpgv --keyring "$WORK/keyring.gpg" "$dists/InRelease" 2>/dev/null || die "APT $suite: InRelease does not verify"
  gpgv --keyring "$WORK/keyring.gpg" "$dists/Release.gpg" "$dists/Release" 2>/dev/null || die "APT $suite: Release.gpg does not verify"
  log "APT $suite: signed $(grep -c '^Package:' "$dists/main/binary-amd64/Packages") amd64 and $(grep -c '^Package:' "$dists/main/binary-arm64/Packages") arm64 entries"

  cat >"$REPO/deb/purlview-$suite.sources" <<EOF
Types: deb
URIs: https://pkg.purlview.com/deb
Suites: $suite
Components: main
Architectures: amd64 arm64
Signed-By: /usr/share/keyrings/purlview-archive-keyring.gpg
EOF
done

# --- DNF ------------------------------------------------------------------------

mkdir -p "$WORK/rpmdb"
rpmkeys --root "$WORK/rpmdb" --import "$PUBLIC_KEY"
for suite in "${SUITES[@]}"; do
  packages="$REPO/rpm/$suite/packages"
  mkdir -p "$packages"
  newest=""
  for f in "$packages"/*.rpm; do
    [[ -e "$f" ]] || continue
    v="$(rpm -qp --qf '%{VERSION}' "$f" 2>/dev/null)"
    if [[ -z "$newest" ]] || dpkg --compare-versions "$v" gt "$newest"; then newest="$v"; fi
  done
  if [[ -n "$newest" ]] && dpkg --compare-versions "$DEB_VERSION" lt "$newest"; then
    die "DNF suite $suite already carries $newest, newer than $DEB_VERSION; refusing to roll it back"
  fi
  added=false
  for arch in "${ARCHES[@]}"; do
    name="purlview_${VERSION}_linux_${arch}.rpm"
    if [[ -f "$packages/$name" ]]; then
      # The repository keeps the signed copy, so compare what signing leaves alone.
      [[ "$(rpm -qp --qf '%{PAYLOADDIGEST}' "$packages/$name" 2>/dev/null)" == "$(rpm -qp --qf '%{PAYLOADDIGEST}' "$DIST/$name" 2>/dev/null)" ]] \
        || die "DNF suite $suite already has a different $name"
      log "DNF $suite: $name already present"
    else
      cp "$DIST/$name" "$WORK/$name"
      rpmsign --addsign --define "__gpg $(command -v gpg)" --define "_gpg_name $FINGERPRINT" \
        --define "_gpg_path ${GNUPGHOME:-$HOME/.gnupg}" "$WORK/$name" >/dev/null 2>"$WORK/rpmsign.log" \
        || { cat "$WORK/rpmsign.log" >&2; die "DNF $suite: could not sign $name"; }
      mv "$WORK/$name" "$packages/$name"
      added=true
      log "DNF $suite: added $name"
    fi
    rpmkeys --root "$WORK/rpmdb" --checksig "$packages/$name" | grep -q 'digests signatures OK' || die "DNF $suite: $name signature does not verify"
  done
  if ! $added && [[ -f "$REPO/rpm/$suite/repodata/repomd.xml.asc" ]]; then
    log "DNF $suite: unchanged"
    continue
  fi
  createrepo_c --quiet --update --general-compress-type=gz "$REPO/rpm/$suite" >/dev/null
  rm -f "$REPO/rpm/$suite/repodata/repomd.xml.asc"
  sign --armor --detach-sign -o "$REPO/rpm/$suite/repodata/repomd.xml.asc" "$REPO/rpm/$suite/repodata/repomd.xml"
  gpgv --keyring "$WORK/keyring.gpg" "$REPO/rpm/$suite/repodata/repomd.xml.asc" "$REPO/rpm/$suite/repodata/repomd.xml" 2>/dev/null \
    || die "DNF $suite: repomd.xml.asc does not verify"
  log "DNF $suite: signed repository metadata"

  cat >"$REPO/rpm/purlview-$suite.repo" <<EOF
[purlview-$suite]
name=Purlview ($suite)
baseurl=https://pkg.purlview.com/rpm/$suite
enabled=1
gpgcheck=1
repo_gpgcheck=1
gpgkey=https://pkg.purlview.com/purlview.asc
EOF
done

cp "$PUBLIC_KEY" "$REPO/purlview.asc"
cp "$WORK/keyring.gpg" "$REPO/purlview-archive-keyring.gpg"

# --- publication ------------------------------------------------------------------

# Packages first and metadata last, so a client never sees an index that names
# a package the origin does not have yet. Metadata is cached briefly; packages
# never change once written.
if [[ -n "$PUBLISH" ]]; then
  immutable="public, max-age=31536000, immutable"
  brief="public, max-age=60"
  aws s3 sync "$REPO/deb/pool" "$PUBLISH/deb/pool" --only-show-errors --cache-control "$immutable"
  for suite in "${SUITES[@]}"; do
    aws s3 sync "$REPO/rpm/$suite/packages" "$PUBLISH/rpm/$suite/packages" --only-show-errors --cache-control "$immutable"
  done
  aws s3 cp "$REPO/purlview.asc" "$PUBLISH/purlview.asc" --only-show-errors --cache-control "$brief" --content-type application/pgp-keys
  aws s3 cp "$REPO/purlview-archive-keyring.gpg" "$PUBLISH/purlview-archive-keyring.gpg" --only-show-errors --cache-control "$brief" --content-type application/octet-stream
  for suite in "${SUITES[@]}"; do
    aws s3 cp "$REPO/deb/purlview-$suite.sources" "$PUBLISH/deb/purlview-$suite.sources" --only-show-errors --cache-control "$brief" --content-type text/plain
    aws s3 cp "$REPO/rpm/purlview-$suite.repo" "$PUBLISH/rpm/purlview-$suite.repo" --only-show-errors --cache-control "$brief" --content-type text/plain
    aws s3 sync "$REPO/rpm/$suite/repodata" "$PUBLISH/rpm/$suite/repodata" --only-show-errors --cache-control "$brief" --exclude 'repomd.xml*'
    aws s3 cp "$REPO/rpm/$suite/repodata/repomd.xml" "$PUBLISH/rpm/$suite/repodata/repomd.xml" --only-show-errors --cache-control "$brief"
    aws s3 cp "$REPO/rpm/$suite/repodata/repomd.xml.asc" "$PUBLISH/rpm/$suite/repodata/repomd.xml.asc" --only-show-errors --cache-control "$brief"
    aws s3 sync "$REPO/deb/dists/$suite/main" "$PUBLISH/deb/dists/$suite/main" --only-show-errors --cache-control "$brief"
    aws s3 cp "$REPO/deb/dists/$suite/Release" "$PUBLISH/deb/dists/$suite/Release" --only-show-errors --cache-control "$brief"
    aws s3 cp "$REPO/deb/dists/$suite/Release.gpg" "$PUBLISH/deb/dists/$suite/Release.gpg" --only-show-errors --cache-control "$brief"
    aws s3 cp "$REPO/deb/dists/$suite/InRelease" "$PUBLISH/deb/dists/$suite/InRelease" --only-show-errors --cache-control "$brief"
  done
  log "published to $PUBLISH"
fi
log "done: $VERSION in ${SUITES[*]}"
