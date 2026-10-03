#!/usr/bin/env bash
# Sign macOS executables with a Developer ID Application identity in an
# ephemeral keychain, notarize them with notarytool, and keep the notarization
# log. Bare executables cannot be stapled (Apple supports stapling for app
# bundles, disk images and installer packages only); Gatekeeper checks the
# notarization ticket online instead.
#
# Usage: sign-macos.sh --in DIR --out DIR
#   DIR/darwin_<arch>/purlview and DIR/darwin_<arch>/unsigned.sha256
#
# Environment:
#   APPLE_DEVELOPER_ID_CERTIFICATE_P12       base64 of the .p12 export
#   APPLE_DEVELOPER_ID_CERTIFICATE_PASSWORD  its password
#   APPLE_NOTARY_KEY_ID, APPLE_NOTARY_ISSUER_ID, APPLE_NOTARY_PRIVATE_KEY (p8)
#   APPLE_TEAM_ID                            optional; the identity must match
set -euo pipefail

IN=""; OUT=""
while [ $# -gt 0 ]; do
  case "$1" in
    --in) IN="$2"; shift 2 ;;
    --out) OUT="$2"; shift 2 ;;
    *) echo "sign-macos: unknown argument $1" >&2; exit 2 ;;
  esac
done
[[ -n "$IN" && -n "$OUT" ]] || { echo "usage: sign-macos.sh --in DIR --out DIR" >&2; exit 2; }
for v in APPLE_DEVELOPER_ID_CERTIFICATE_P12 APPLE_DEVELOPER_ID_CERTIFICATE_PASSWORD APPLE_NOTARY_KEY_ID APPLE_NOTARY_ISSUER_ID APPLE_NOTARY_PRIVATE_KEY; do
  [[ -n "${!v:-}" ]] || { echo "sign-macos: $v is required" >&2; exit 1; }
done

WORK="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/sign-macos.XXXXXX")"
KEYCHAIN="$WORK/release.keychain-db"
KEYCHAIN_PASSWORD="$(openssl rand -hex 24)"
# G2 Developer ID Application certificates chain through Apple's G2
# intermediate. Runner images do not reliably carry it, and without it the
# identity is not valid for code signing.
INTERMEDIATE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/apple/DeveloperIDG2CA.crt"
INTERMEDIATE_SHA256="F1:6C:D3:C5:4C:7F:83:CE:A4:BF:1A:3E:6A:08:19:C8:AA:A8:E4:A1:52:8F:D1:44:71:5F:35:06:43:D2:DF:3A"
cleanup() {
  security delete-keychain "$KEYCHAIN" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

printf '%s' "$APPLE_DEVELOPER_ID_CERTIFICATE_P12" | base64 -d >"$WORK/certificate.p12"
printf '%s' "$APPLE_NOTARY_PRIVATE_KEY" >"$WORK/notary.p8"
chmod 0600 "$WORK/certificate.p12" "$WORK/notary.p8"

security create-keychain -p "$KEYCHAIN_PASSWORD" "$KEYCHAIN"
security set-keychain-settings -lut 21600 "$KEYCHAIN"
security unlock-keychain -p "$KEYCHAIN_PASSWORD" "$KEYCHAIN"
security import "$WORK/certificate.p12" -k "$KEYCHAIN" -P "$APPLE_DEVELOPER_ID_CERTIFICATE_PASSWORD" -f pkcs12 -T /usr/bin/codesign -T /usr/bin/security >/dev/null
security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$KEYCHAIN_PASSWORD" "$KEYCHAIN" >/dev/null
fingerprint="$(openssl x509 -in "$INTERMEDIATE" -noout -fingerprint -sha256 | cut -d= -f2)"
[[ "$fingerprint" == "$INTERMEDIATE_SHA256" ]] || { echo "sign-macos: $INTERMEDIATE fingerprint $fingerprint is not Apple's Developer ID G2 intermediate" >&2; exit 1; }
# A .p12 exported from a keychain that already holds the intermediate carries
# it along; importing it again fails, so import only when it is missing.
if ! security find-certificate -a -Z "$KEYCHAIN" | grep -qx "SHA-256 hash: ${INTERMEDIATE_SHA256//:/}"; then
  security import "$INTERMEDIATE" -k "$KEYCHAIN" -f openssl -t cert >/dev/null
fi
security list-keychains -d user -s "$KEYCHAIN" login.keychain-db

IDENTITY_LINE="$(security find-identity -v -p codesigning "$KEYCHAIN" | grep 'Developer ID Application:' | head -n1 || true)"
[[ -n "$IDENTITY_LINE" ]] || { echo "sign-macos: no 'Developer ID Application' identity in the certificate" >&2; exit 1; }
if [[ -n "${APPLE_TEAM_ID:-}" ]] && [[ "$IDENTITY_LINE" != *"($APPLE_TEAM_ID)"* ]]; then
  echo "sign-macos: identity does not belong to team $APPLE_TEAM_ID: $IDENTITY_LINE" >&2; exit 1
fi
IDENTITY_HASH="$(awk '{print $2}' <<<"$IDENTITY_LINE")"
echo "sign-macos: using identity ${IDENTITY_LINE#*\) }"

mkdir -p "$OUT/notarization"
targets=()
for dir in "$IN"/darwin_*; do
  [[ -f "$dir/purlview" ]] || continue
  target="$(basename "$dir")"
  targets+=("$target")
  mkdir -p "$OUT/$target"
  cp "$dir/purlview" "$OUT/$target/purlview"
  [[ -f "$dir/unsigned.sha256" ]] && cp "$dir/unsigned.sha256" "$OUT/$target/unsigned.sha256"
  codesign --force --sign "$IDENTITY_HASH" --keychain "$KEYCHAIN" --options runtime --timestamp --verbose=2 "$OUT/$target/purlview"
  codesign --verify --strict --verbose=2 "$OUT/$target/purlview"
  codesign -dvv "$OUT/$target/purlview" 2>&1 | grep -E '^(Authority|TeamIdentifier|Timestamp)=' | sed "s/^/sign-macos: $target: /"
done
(( ${#targets[@]} > 0 )) || { echo "sign-macos: no darwin executables under $IN" >&2; exit 1; }

# One notarization submission covering every executable.
( cd "$OUT" && zip -q -r "$WORK/submission.zip" "${targets[@]/%//purlview}" )
echo "sign-macos: submitting ${#targets[@]} executables for notarization"
xcrun notarytool submit "$WORK/submission.zip" \
  --key "$WORK/notary.p8" --key-id "$APPLE_NOTARY_KEY_ID" --issuer "$APPLE_NOTARY_ISSUER_ID" \
  --wait --timeout 45m --output-format json >"$OUT/notarization/submission.json"
SUBMISSION_ID="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])' <"$OUT/notarization/submission.json")"
STATUS="$(python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])' <"$OUT/notarization/submission.json")"
xcrun notarytool log "$SUBMISSION_ID" \
  --key "$WORK/notary.p8" --key-id "$APPLE_NOTARY_KEY_ID" --issuer "$APPLE_NOTARY_ISSUER_ID" \
  "$OUT/notarization/log.json" || true
echo "sign-macos: notarization $SUBMISSION_ID status: $STATUS"
[[ "$STATUS" == Accepted ]] || { cat "$OUT/notarization/log.json" >&2 || true; exit 1; }

# Gatekeeper assesses a bare command-line executable as an install, not an
# app launch: --type execute rejects anything that is not an app bundle.
for target in "${targets[@]}"; do
  assessment="$(spctl --assess --type install --verbose=2 "$OUT/$target/purlview" 2>&1)" \
    || { echo "sign-macos: $target: Gatekeeper rejects it: $assessment" >&2; exit 1; }
  while IFS= read -r line; do echo "sign-macos: $target: $line"; done <<<"$assessment"
  grep -q '^source=Notarized Developer ID$' <<<"$assessment" \
    || { echo "sign-macos: $target: Gatekeeper does not see the notarization" >&2; exit 1; }
done
echo "sign-macos: done"
