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

# The window: apps/macos/dmg/background*.png (rendered from docs/brand/dmg-background.svg, which
# places the icons) behind the app and the Applications link, laid out by Finder on a writable
# copy, then compressed.
mkdir -p "$out" "$work/dmg/.background"
cp -R "$app" "$work/dmg/"
ln -s /Applications "$work/dmg/Applications"
tiffutil -cathidpicheck "$here/dmg/background.png" "$here/dmg/background@2x.png" \
  -out "$work/dmg/.background/background.tiff" 2>/dev/null
# The volume icon is the app's own (an Icon Composer icon has no .icns in the bundle).
mkdir -p "$work/vol.iconset"
swift - "$app" "$work/vol.iconset" <<'SWIFT'
import AppKit
let icon = NSWorkspace.shared.icon(forFile: CommandLine.arguments[1])
for (name, px) in [("16x16", 16), ("16x16@2x", 32), ("32x32", 32), ("32x32@2x", 64), ("128x128", 128),
                   ("128x128@2x", 256), ("256x256", 256), ("256x256@2x", 512), ("512x512", 512), ("512x512@2x", 1024)] {
  let rep = NSBitmapImageRep(bitmapDataPlanes: nil, pixelsWide: px, pixelsHigh: px, bitsPerSample: 8,
                             samplesPerPixel: 4, hasAlpha: true, isPlanar: false, colorSpaceName: .deviceRGB,
                             bytesPerRow: 0, bitsPerPixel: 0)!
  NSGraphicsContext.saveGraphicsState()
  NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
  icon.draw(in: NSRect(x: 0, y: 0, width: px, height: px))
  NSGraphicsContext.restoreGraphicsState()
  try! rep.representation(using: .png, properties: [:])!.write(to: URL(fileURLWithPath: "\(CommandLine.arguments[2])/icon_\(name).png"))
}
SWIFT
iconutil -c icns "$work/vol.iconset" -o "$work/VolumeIcon.icns"
[ -d /Volumes/Stormo ] && { echo "eject the mounted Stormo volume first" >&2; exit 1; }
hdiutil create -volname Stormo -srcfolder "$work/dmg" -fs HFS+ -format UDRW -ov "$work/rw.dmg" >/dev/null
hdiutil attach "$work/rw.dmg" -readwrite -noverify -noautoopen -mountpoint /Volumes/Stormo >/dev/null
osascript <<'APPLESCRIPT'
tell application "Finder"
  tell disk "Stormo"
    open
    set w to container window
    set current view of w to icon view
    set toolbar visible of w to false
    set statusbar visible of w to false
    set bounds of w to {200, 120, 840, 548}
    set o to icon view options of w
    set arrangement of o to not arranged
    set icon size of o to 112
    set text size of o to 13
    set background picture of o to file ".background:background.tiff"
    set position of item "Stormo.app" to {170, 190}
    set position of item "Applications" to {470, 190}
    update without registering applications
    delay 1
    close
  end tell
end tell
APPLESCRIPT
# After Finder, which drops a .VolumeIcon.icns it finds while laying out.
cp "$work/VolumeIcon.icns" /Volumes/Stormo/.VolumeIcon.icns
SetFile -a C /Volumes/Stormo
sync
hdiutil detach /Volumes/Stormo >/dev/null
dmg="$out/Stormo-$version.dmg"
rm -f "$dmg"
hdiutil convert "$work/rw.dmg" -format UDZO -imagekey zlib-level=9 -o "$dmg" >/dev/null
[ "$identity" != - ] && codesign --force --timestamp --sign "$identity" "$dmg"

if [ -n "${NOTARY_PROFILE:-}" ]; then
  xcrun notarytool submit "$dmg" --keychain-profile "$NOTARY_PROFILE" --wait
  xcrun stapler staple "$dmg"
  spctl -a -t open --context context:primary-signature -v "$dmg"
fi
echo "built $dmg"
