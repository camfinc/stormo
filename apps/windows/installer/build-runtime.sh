#!/usr/bin/env bash
# Requires Go and Docker. Only archives tracked source, never instance data or local secrets.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
context=$(mktemp -d)
container=""
cleanup() {
  if [[ -n "$container" ]]; then docker rm "$container" >/dev/null; fi
  rm -rf "$context"
}
trap cleanup EXIT
# CI builds a clean commit; this intentionally does not include uncommitted changes.
git archive HEAD | tar -x -C "$context"
mkdir -p "$context/apps/windows/build" apps/windows/assets
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -C "$context" -trimpath -o "$context/apps/windows/build/stormo" ./cmd/stormo
docker build --platform linux/amd64 -f "$context/apps/windows/installer/runtime.Dockerfile" \
  -t stormo-windows-runtime:preview "$context"
container=$(docker create --platform linux/amd64 stormo-windows-runtime:preview)
docker export "$container" | gzip > apps/windows/assets/runtime.tar.gz
python3 - <<'PY'
import hashlib
from pathlib import Path
p = Path('apps/windows/assets/runtime.tar.gz')
with p.open('rb') as stream:
    checksum = hashlib.file_digest(stream, 'sha256').hexdigest()
p.with_name('runtime.sha256').write_text(checksum + '  runtime.tar.gz\n')
PY
