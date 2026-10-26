"""Build a native engine and a tracked-source sidecar context; no Linux runtime image."""
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile

root = Path(__file__).resolve().parents[3]
target = root / "apps/windows/assets/engine"
target.mkdir(parents=True, exist_ok=True)
# No untracked files, secrets or instance data can enter the bundled build context.
with tempfile.TemporaryDirectory() as tmp:
    archive = Path(tmp) / "source.tar"
    subprocess.run(["git", "archive", "HEAD", "-o", str(archive)], cwd=root, check=True)
    with tarfile.open(archive) as source:
        source.extractall(target, filter="data")
env = dict(os.environ, CGO_ENABLED="0", GOOS="windows", GOARCH="amd64")
(target / "bin").mkdir(exist_ok=True)
subprocess.run(["go", "build", "-trimpath", "-o", str(target / "bin/stormo.exe"), "./cmd/stormo"], cwd=target, env=env, check=True)
