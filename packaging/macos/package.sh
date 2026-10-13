#!/usr/bin/env bash
#
# package.sh — sign, notarize and wrap a built Skim.app in a DMG (see `make package-macos`).
#
#   packaging/macos/package.sh <Skim.app> <out.dmg>
#
# Credential-optional; runs the same locally and in CI:
#
#   * No $MACOS_SIGNING_IDENTITY  -> an UNSIGNED DMG is still built.
#   * With a signing identity     -> the app and DMG are codesigned with a
#                                     hardened runtime + secure timestamp.
#   * With notarization creds      -> the app and DMG are notarized and stapled.
#
# MACOS_SIGNING_IDENTITY is a codesign identity: the certificate name
# ("Developer ID Application: …") or, when several certificates share a name,
# its SHA-1 hash from `security find-identity -v -p codesigning`.
#
# Notarization credentials — set ONE of these flows (checked in this order):
#   NOTARY_PROFILE                                notarytool keychain profile
#   NOTARY_APPLE_ID + NOTARY_PASSWORD + NOTARY_TEAM_ID   Apple ID app-specific password
#
# Notarization failures are loud but non-fatal (the DMG ships signed but not
# stapled); NOTARY_STRICT=1 makes them fatal.
set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 <Skim.app> <out.dmg>" >&2
  exit 2
fi
APP="$1"
DMG="$2"
APP_NAME="$(basename "$APP" .app)"
[[ -d "$APP" ]] || { echo "[error] $APP is not an app bundle" >&2; exit 1; }

step() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }
# Plain, greppable outcome banners for CI logs.
ok()   { printf '== %s ==\n' "$*"; }

SIGN_IDENTITY="${MACOS_SIGNING_IDENTITY:-}"

notary_args=(); notary_flow=""
if [[ -n "${NOTARY_PROFILE:-}" ]]; then
  notary_args=(--keychain-profile "$NOTARY_PROFILE")
  notary_flow="keychain profile '$NOTARY_PROFILE'"
elif [[ -n "${NOTARY_APPLE_ID:-}" && -n "${NOTARY_PASSWORD:-}" && -n "${NOTARY_TEAM_ID:-}" ]]; then
  notary_args=(--apple-id "$NOTARY_APPLE_ID" --password "$NOTARY_PASSWORD" --team-id "$NOTARY_TEAM_ID")
  notary_flow="Apple ID ($NOTARY_APPLE_ID)"
fi

notary_failed() {
  warn "== notarization FAILED: $1 =="
  [[ "${NOTARY_STRICT:-0}" == "1" ]] && die "NOTARY_STRICT=1 — treating notarization failure as fatal"
  warn "== continuing WITHOUT notarization — users may need right-click → Open on first launch =="
}

# Submit, wait, then staple. Returns 1 when the artifact was not notarized.
notarize() { # <submit_path> <staple_path> <label>
  local submit="$1" staple="$2" label="$3" out sid
  step "Notarizing $label via $notary_flow"
  if ! out="$(xcrun notarytool submit "$submit" "${notary_args[@]}" --wait 2>&1)"; then
    echo "$out" >&2
    notary_failed "notarytool submit failed for $label"
    return 1
  fi
  echo "$out"
  sid="$(awk '/^ *id:/{print $2; exit}' <<<"$out")"
  # notarytool exits 0 even when the status is Invalid — check it explicitly.
  if ! grep -q 'status: Accepted' <<<"$out"; then
    if [[ -n "$sid" ]]; then
      warn "Notarization log for submission $sid:"
      xcrun notarytool log "$sid" "${notary_args[@]}" >&2 || true
    fi
    notary_failed "notarization rejected for $label (submission id: ${sid:-unknown})"
    return 1
  fi
  ok "notarization Accepted: $label (submission id: ${sid:-unknown})"
  # Callers run this under `&&`/`||`, where set -e is off — check explicitly.
  xcrun stapler staple "$staple" || { notary_failed "stapling failed for $label"; return 1; }
  ok "staple OK: $label"
}

step "Packaging $APP -> $DMG"
echo "    signing:      ${SIGN_IDENTITY:-no (UNSIGNED build)}"
echo "    notarization: ${notary_flow:-no (not configured)}"
[[ -z "$SIGN_IDENTITY" ]] && warn "Building an UNSIGNED app — Gatekeeper will block it without right-click → Open."
[[ -z "$SIGN_IDENTITY" && -n "$notary_flow" ]] && warn "Notarization needs a signed app — skipping it."

notarized=0
if [[ -n "$SIGN_IDENTITY" ]]; then
  step "Signing $APP_NAME.app"
  # Contents/MacOS holds the only Mach-O executable — no --deep required.
  codesign --force --options runtime --timestamp --sign "$SIGN_IDENTITY" "$APP"
  codesign --verify --strict --verbose=2 "$APP"
  ok "codesign OK: $APP_NAME.app"

  if [[ -n "$notary_flow" ]]; then
    zip="$(mktemp -d)/$APP_NAME.zip"
    ditto -c -k --keepParent "$APP" "$zip"
    notarize "$zip" "$APP" "$APP_NAME.app" && notarized=1
    rm -rf "$(dirname "$zip")"
  fi
fi

step "Building DMG"
staging="$(mktemp -d)"
trap 'rm -rf "$staging"' EXIT
mkdir -p "$(dirname "$DMG")"
cp -R "$APP" "$staging/"
ln -s /Applications "$staging/Applications"
rm -f "$DMG"
hdiutil create -volname "$APP_NAME" -srcfolder "$staging" -ov -format UDZO "$DMG"

if [[ -n "$SIGN_IDENTITY" ]]; then
  step "Signing DMG"
  codesign --force --timestamp --sign "$SIGN_IDENTITY" "$DMG"
  ok "codesign OK: $(basename "$DMG")"
  if [[ $notarized == 1 ]]; then
    notarize "$DMG" "$DMG" "$(basename "$DMG")" || notarized=0
  fi
fi

step "Done: $DMG"
if [[ $notarized == 1 ]]; then
  ok "macOS package signed + notarized"
elif [[ -n "$SIGN_IDENTITY" ]]; then
  ok "macOS package signed (NOT notarized)"
else
  ok "macOS package UNSIGNED"
fi
