"""No Qt dependency: runtime checks and mutations are independently testable."""
from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
from dataclasses import dataclass
from urllib.request import build_opener, ProxyHandler, HTTPRedirectHandler

DISTRO = "Stormo"
INSTANCE = "/home/stormo/Instances/default"
ENGINE = "/opt/stormo/bin/stormo"
MARKER = "stormo-desktop-runtime-v1"


class RuntimeFailure(Exception):
    pass


def decode(data: bytes) -> str:
    # wsl.exe's inventory is UTF-16LE on Windows, command output is UTF-8.
    return data.decode("utf-16" if data.startswith(b"\xff\xfe") else
                       "utf-16-le" if b"\0" in data else "utf-8", errors="replace").strip()


def run(argv: list[str], timeout: int = 30) -> str:
    env = dict(os.environ)
    for key in ("SWARM_TARGET", "STORMO_INSTANCE", "WSLENV"):
        env.pop(key, None)
    try:
        result = subprocess.run(argv, input=b"", stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                env=env, timeout=timeout, check=False,
                                creationflags=getattr(subprocess, "CREATE_NO_WINDOW", 0))
    except subprocess.TimeoutExpired as exc:
        raise RuntimeFailure("This step took too long. Check setup and retry.") from exc
    except OSError as exc:
        raise RuntimeFailure(f"Could not start {Path(argv[0]).name}: {exc}") from exc
    if result.returncode:
        raise RuntimeFailure(decode(result.stderr or result.stdout)[-2000:] or
                             f"{Path(argv[0]).name} exited with code {result.returncode}.")
    return decode(result.stdout)


def wsl(*args: str) -> list[str]:
    return ["wsl.exe", "--distribution", DISTRO, "--user", "stormo", "--exec", *args]


def cli_args(*command: str) -> list[str]:
    return wsl("env", "-u", "SWARM_TARGET", "-u", "STORMO_INSTANCE",
               "SWARM_CORE_PORT=18600", ENGINE, "--json", "--target", "local",
               "--instance", INSTANCE, *command)


def parse_result(output: str):
    value = None
    found = False
    for line in output.splitlines():
        try:
            event = json.loads(line)
        except json.JSONDecodeError as exc:
            raise RuntimeFailure("The engine returned an invalid response.") from exc
        if not isinstance(event, dict):
            raise RuntimeFailure("The engine returned an invalid event.")
        if event.get("event") == "error":
            raise RuntimeFailure(event.get("msg") or "The engine could not complete this action.")
        if event.get("event") == "result":
            value, found = event.get("data"), True
    if not found:
        raise RuntimeFailure("The engine returned no result. Check its version and retry.")
    return value


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise RuntimeFailure("Unexpected redirect from the local engine.")


def read_api(route: str):
    if route not in ("/health", "/api/core", "/api/fleet"):
        raise ValueError("Unsupported desktop API route")
    opener = build_opener(ProxyHandler({}), NoRedirect())
    with opener.open("http://127.0.0.1:18600" + route, timeout=3) as response:
        return json.load(response)


def check_core(core: dict) -> None:
    if core.get("service") != "swarm-core" or core.get("api") != 1:
        raise RuntimeFailure("Another service or an incompatible Stormo version is using the core port.")
    if core.get("instance", {}).get("root") != INSTANCE or core.get("exe") != ENGINE:
        raise RuntimeFailure("Another workspace owns the core. Stop it from its own app before continuing.")


@dataclass(frozen=True)
class Check:
    key: str
    title: str
    ready: bool
    detail: str


class Runtime:
    def __init__(self, assets: Path, runner=run, reader=read_api):
        self.assets, self.run, self.read = assets, runner, reader
        self.verified = False

    def owned(self):
        if self.run(wsl("cat", "/etc/stormo-runtime")) != MARKER:
            raise RuntimeFailure("A different WSL distribution is named Stormo. It has been left unchanged.")

    def command(self, *args: str, timeout: int = 300):
        self.owned()
        return parse_result(self.run(cli_args(*args), timeout=timeout))

    def detect(self) -> list[Check]:
        checks = []
        if platform.system() != "Windows" or platform.machine().lower() not in ("amd64", "x86_64") or int(platform.version().split(".")[-1]) < 22000:
            return [Check("windows", "Windows 11", False, "This preview needs Windows 11 on an Intel or AMD 64-bit PC.")]
        checks.append(Check("windows", "Windows 11", True, "This PC meets the app's OS target."))
        try:
            self.run(["wsl.exe", "--status"])
            names = self.run(["wsl.exe", "--list", "--quiet"]).splitlines()
        except RuntimeFailure:
            return checks + [Check("wsl", "Windows runtime", False, "Enable Windows Subsystem for Linux. Windows may request administrator access and a restart. Reopen Stormo afterward.")]
        checks.append(Check("wsl", "Windows runtime", True, "Windows Subsystem for Linux is available."))
        if DISTRO not in [name.strip() for name in names]:
            return checks + [Check("import", "Stormo runtime", False, "Prepare Stormo's private Linux workspace. Your other Linux distributions stay unchanged.")]
        try:
            self.owned()
            release = self.run(wsl("uname", "-r"))
            if "wsl2" not in release.lower():
                raise RuntimeFailure("Stormo needs WSL 2. This distribution is not running a WSL 2 kernel.")
        except RuntimeFailure as exc:
            return checks + [Check("repair", "Stormo runtime", False, str(exc))]
        checks.append(Check("import", "Stormo runtime", True, "Stormo's WSL 2 workspace is available."))
        try:
            info = json.loads(self.run(wsl("docker", "info", "--format", "{{json .}}")))
            if info.get("OSType") != "linux":
                raise RuntimeFailure("Switch Docker Desktop to Linux containers.")
            self.run(wsl("docker", "compose", "version"))
        except (RuntimeFailure, ValueError) as exc:
            return checks + [Check("docker", "Container runtime", False, "Install and open Docker Desktop. In Settings → Resources → WSL Integration, enable Stormo and select Apply & restart. Then check again.\n" + str(exc))]
        checks.append(Check("docker", "Container runtime", True, "Docker's Linux engine and Compose answer inside Stormo."))
        try:
            version = self.command("version")
            if version.get("api") != 1:
                raise RuntimeFailure("The bundled engine API is incompatible with this app.")
        except (RuntimeFailure, AttributeError) as exc:
            return checks + [Check("repair", "Stormo engine", False, str(exc))]
        checks.append(Check("engine", "Stormo engine", True, "The engine speaks this app's API version."))
        if not self.verified:
            return checks + [Check("verify", "Test the runtime", False, "Run a small disposable test container to confirm agents can use the runtime. This downloads Docker's hello-world image on first use.")]
        try:
            self.command("instance")
        except RuntimeFailure:
            return checks + [Check("instance", "Your workspace", False, "Create your first workspace. It starts empty; no agents run or use a paid model during setup.")]
        return checks + [Check("instance", "Your workspace", True, "Your workspace is ready. Open Agents to start the core.")]

    def import_runtime(self):
        names = self.run(["wsl.exe", "--list", "--quiet"]).splitlines()
        if DISTRO in [name.strip() for name in names]:
            raise RuntimeFailure("Stormo is already registered. Check again; it will not be overwritten.")
        archive = self.assets / "runtime.tar.gz"
        digest = self.assets / "runtime.sha256"
        if not archive.is_file() or not digest.is_file():
            raise RuntimeFailure("The runtime bundle is missing. Install a complete Windows preview build.")
        with archive.open("rb") as stream:
            actual = hashlib.file_digest(stream, "sha256").hexdigest()
        if actual != digest.read_text().split()[0]:
            raise RuntimeFailure("The runtime bundle failed its integrity check. Download the installer again.")
        destination = Path(os.environ["LOCALAPPDATA"]) / "Stormo" / "Runtime"
        destination.mkdir(parents=True, exist_ok=True)
        self.run(["wsl.exe", "--import", DISTRO, str(destination), str(archive), "--version", "2"], timeout=600)
        self.owned()

    def verify_container(self):
        self.owned()
        self.run(wsl("docker", "run", "--rm", "--network", "none", "hello-world:latest"), timeout=300)
        self.verified = True

    def create_instance(self):
        return self.command("new", "instance", INSTANCE, "--name", "My Stormo", "--slug", "my-stormo", "--template", "empty", "--no-git")

    def fleet(self):
        check_core(self.read("/api/core"))
        return self.read("/api/fleet")

    def action(self, action: str, agent: str | None = None):
        if action not in ("core up", "core down", "start", "stop", "restart", "nap-now"):
            raise ValueError("Unsupported desktop action")
        if action == "core up":
            # Distinguish a refused connection from malformed/foreign HTTP responses.
            from urllib.error import URLError
            from socket import timeout as SocketTimeout
            try:
                core = self.read("/api/core")
            except URLError as exc:
                if not isinstance(exc.reason, ConnectionRefusedError):
                    raise RuntimeFailure("Could not verify the local core port. Check setup and retry.") from exc
            except SocketTimeout as exc:
                raise RuntimeFailure("The core port is not responding. It was left unchanged.") from exc
            else:
                check_core(core)
            return self.command("core", "up")
        core = self.read("/api/core")
        check_core(core)
        if action == "core down":
            return self.command("core", "down")
        if not agent or agent.startswith("-"):
            raise RuntimeFailure("Select one agent first.")
        if agent not in {a["id"] for a in self.fleet().get("agents", [])}:
            raise RuntimeFailure("That agent is no longer in this workspace. Refresh and select it again.")
        return self.command(action, agent, timeout=900)
