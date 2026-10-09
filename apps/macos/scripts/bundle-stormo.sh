#!/bin/sh
# Xcode build phase: puts a stormo binary at Contents/Helpers/stormo, signed like the app.
# Local builds compile this checkout for the build's architectures (its version is "dev": prefer an
# installed CLI for running agents). Release builds pass STORMO_BINARY, the universal,
# version-stamped binary scripts/build.sh made.
set -eu
dest="$TARGET_BUILD_DIR/$CONTENTS_FOLDER_PATH/Helpers"
mkdir -p "$dest"
if [ -n "${STORMO_BINARY:-}" ]; then
  cp "$STORMO_BINARY" "$dest/stormo"
else
  export PATH="/opt/homebrew/bin:/usr/local/bin:/usr/local/go/bin:$PATH"
  engine="$SRCROOT/../.."
  parts=""
  for arch in $ARCHS; do
    goarch="$arch"
    [ "$arch" = x86_64 ] && goarch=amd64
    (cd "$engine" && CGO_ENABLED=0 GOOS=darwin GOARCH="$goarch" go build -trimpath -o "$DERIVED_FILE_DIR/stormo-$arch" ./cmd/stormo)
    parts="$parts $DERIVED_FILE_DIR/stormo-$arch"
  done
  # shellcheck disable=SC2086
  lipo -create $parts -output "$dest/stormo"
fi
if [ "${CODE_SIGNING_ALLOWED:-YES}" = YES ]; then
  codesign --force --options runtime --timestamp=none --identifier "$PRODUCT_BUNDLE_IDENTIFIER.cli" \
    --sign "${EXPANDED_CODE_SIGN_IDENTITY:--}" "$dest/stormo"
fi
