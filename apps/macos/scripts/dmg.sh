#!/bin/sh
# Builds Stormo.app (Release, universal) and packs it into dist/Stormo-<version>.dmg.
#   apps/macos/scripts/dmg.sh [version]        version defaults to `git describe`, e.g. v0.1.0
# Signing, inside out (helper, app, DMG), with SIGN_IDENTITY: a "Developer ID Application: ..."
# identity for distribution, else ad-hoc ("-", runs only on the Mac that built it or after the
# Gatekeeper override). Notarizes and staples when NOTARY_PROFILE names a notarytool keychain
# profile (xcrun notarytool store-credentials). STORMO_BINARY: a universal, version-stamped CLI to
# bundle instead of compiling this checkout.
set -eu
here="$(cd "$(dirname "$0")/.." && pwd)"
engine="$(cd "$here/../.." && pwd)"
version="${1:-$(git -C "$engine" describe --tags --always --dirty)}"
identity="${SIGN_IDENTITY:-$(security find-identity -v -p codesigning | sed -n 's/.*"\(Developer ID Application: .*\)"/\1/p' | head -1)}"
identity="${identity:--}"
out="$engine/dist"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

echo "version $version, signing with: $identity"
xcodebuild -project "$here/Stormo.xcodeproj" -scheme Stormo -configuration Release \
  -derivedDataPath "$work/dd" ARCHS="arm64 x86_64" ONLY_ACTIVE_ARCH=NO \
  MARKETING_VERSION="${version#v}" CODE_SIGN_IDENTITY="$identity" OTHER_CODE_SIGN_FLAGS="--timestamp" \
  build >"$work/build.log" 2>&1 || { tail -40 "$work/build.log"; exit 1; }
app="$work/dd/Build/Products/Release/Stormo.app"

# The build phase signs the helper without a timestamp; notarization needs one.
if [ "$identity" != - ]; then
  codesign --force --options runtime --timestamp --identifier "$(defaults read "$app/Contents/Info" CFBundleIdentifier).cli" \
    --sign "$identity" "$app/Contents/Helpers/stormo"
  codesign --force --options runtime --timestamp --sign "$identity" "$app"
fi
codesign --verify --deep --strict "$app"

mkdir -p "$out" "$work/dmg"
cp -R "$app" "$work/dmg/"
ln -s /Applications "$work/dmg/Applications"
dmg="$out/Stormo-$version.dmg"
rm -f "$dmg"
hdiutil create -volname Stormo -srcfolder "$work/dmg" -fs HFS+ -format UDZO -ov "$dmg" >/dev/null
[ "$identity" != - ] && codesign --force --timestamp --sign "$identity" "$dmg"

if [ -n "${NOTARY_PROFILE:-}" ]; then
  xcrun notarytool submit "$dmg" --keychain-profile "$NOTARY_PROFILE" --wait
  xcrun stapler staple "$dmg"
  spctl -a -t open --context context:primary-signature -v "$dmg"
fi
echo "built $dmg"
