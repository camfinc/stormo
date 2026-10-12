"""Runs on Windows CI: native detached core lifecycle, ownership and paths with spaces."""
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import time
from urllib.request import build_opener, ProxyHandler

engine = (Path(__file__).resolve().parents[1] / "assets/engine/bin/stormo.exe").resolve()
with socket.socket() as sock:
    sock.bind(("127.0.0.1", 0))
    port = sock.getsockname()[1]
env = dict(os.environ, SWARM_CORE_PORT=str(port), SWARM_CORE_BIND="none")
env.pop("STORMO_INSTANCE", None)
env.pop("SWARM_TARGET", None)
opener = build_opener(ProxyHandler({}))

def cli(instance, *args):
    result = subprocess.run([str(engine), "--json", "--target", "local", "--instance", str(instance), *args],
                            env=env, capture_output=True, text=True, encoding="utf-8", timeout=45, check=True)
    events = [json.loads(line) for line in result.stdout.splitlines()]
    return [event["data"] for event in events if event["event"] == "result"][-1]

def core():
    with opener.open(f"http://127.0.0.1:{port}/api/core", timeout=2) as response:
        return json.load(response)

with tempfile.TemporaryDirectory(prefix="Stormo native test ") as directory:
    instance = Path(directory) / "Workspace with spaces"
    other = Path(directory) / "Other workspace"
    for path in (instance, other):
        cli(path, "new", "instance", str(path), "--name", "Test", "--template", "empty", "--no-git")
    try:
        for cycle in range(2):
            started = cli(instance, "core", "up")
            assert started["started"], started
            info = core()
            assert info["pid"] == started["pid"]
            assert os.path.normcase(info["instance"]["root"]) == os.path.normcase(str(instance))
            assert os.path.normcase(info["exe"]) == os.path.normcase(str(engine))
            assert not cli(instance, "core", "up")["started"]
            assert cli(other, "core", "down")["reason"] == "not_ours"
            assert core()["pid"] == started["pid"]
            assert cli(instance, "core", "down")["stopped"]
            for _ in range(100):
                try:
                    core()
                except OSError:
                    break
                time.sleep(0.1)
            else:
                raise AssertionError("core did not close gracefully")
            time.sleep(0.3)  # Let Close release SQLite/files before the next start.
    finally:
        cli(instance, "core", "down")
print("Native Windows core start, detach, ownership, stop and restart passed.")
