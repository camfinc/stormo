"""No Qt dependency: runtime checks and mutations are independently testable."""
from __future__ import annotations

import json
import os
import ntpath
import shutil
from pathlib import Path
import platform
import subprocess
from dataclasses import dataclass
from urllib.request import build_opener, ProxyHandler, HTTPRedirectHandler



class RuntimeFailure(Exception):
    pass


def decode(data: bytes) -> str:
    # Native engine/Docker output is UTF-8; tolerate BOM-marked Windows diagnostics.
    return data.decode("utf-16" if data.startswith(b"\xff\xfe") else
                       "utf-16-le" if b"\0" in data else "utf-8", errors="replace").strip()


def run(argv: list[str], timeout: int = 30) -> str:
    env = dict(os.environ)
    for key in ("SWARM_TARGET", "STORMO_INSTANCE", "DOCKER_HOST", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "STORMO_SRC", "SWARM_CORE_BIND"):
        env.pop(key, None)
    env["SWARM_CORE_PORT"] = "18600"
    env["DOCKER_CONTEXT"] = "desktop-linux"
    docker = docker_executable()
    if docker:
        env["PATH"] = str(Path(docker).parent) + os.pathsep + env.get("PATH", "")
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


def docker_executable() -> str | None:
    for folder in (Path(os.environ.get("ProgramFiles", "C:/Program Files")) / "Docker/Docker/resources/bin",
                   Path(os.environ.get("LOCALAPPDATA", "")) / "Programs/DockerDesktop/resources/bin"):
        candidate = folder / "docker.exe"
        if candidate.is_file():
            return str(candidate)
    return shutil.which("docker.exe")


def cli_args(engine: Path, instance: Path, *command: str) -> list[str]:
    return [str(engine), "--json", "--target", "local", "--instance", str(instance), *command]


def same_path(a: str, b: str) -> bool:
    return ntpath.normcase(ntpath.normpath(a)) == ntpath.normcase(ntpath.normpath(b))


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


def check_core(core: dict, engine: Path, instance: Path) -> None:
    if core.get("service") != "swarm-core" or core.get("api") != 1:
        raise RuntimeFailure("Another service or an incompatible Stormo version is using the core port.")
    if not same_path(core.get("instance", {}).get("root", ""), str(instance)) or not same_path(core.get("exe", ""), str(engine)):
        raise RuntimeFailure("Another workspace owns the core. Stop it from its own app before continuing.")


@dataclass(frozen=True)
class Check:
    key: str
    title: str
    ready: bool
    detail: str


class Runtime:
    def __init__(self, assets: Path, runner=run, reader=read_api, instance: Path | None = None):
        self.assets, self.run, self.read = assets, runner, reader
        self.engine = (assets / "engine/bin/stormo.exe").resolve()
        self.instance = (instance or Path(os.environ.get("LOCALAPPDATA", Path.home())) / "Stormo/Instances/default").resolve()
        self.verified = False

    def command(self, *args: str, timeout: int = 300):
        return parse_result(self.run(cli_args(self.engine, self.instance, *args), timeout=timeout))

    def docker(self, *args: str, timeout: int = 30):
        executable = docker_executable()
        if not executable:
            raise RuntimeFailure("Docker Desktop is not installed yet. Install it, open it and check again.")
        return self.run([executable, "--context", "desktop-linux", *args], timeout=timeout)

    def detect(self) -> list[Check]:
        checks = []
        if platform.system() != "Windows" or platform.machine().lower() not in ("amd64", "x86_64") or int(platform.version().split(".")[-1]) < 22000:
            return [Check("windows", "Windows 11", False, "This preview needs Windows 11 on an Intel or AMD 64-bit PC.")]
        checks.append(Check("windows", "Windows 11", True, "This PC meets the app's OS target."))
        try:
            info = json.loads(self.docker("info", "--format", "{{json .}}"))
            if info.get("OSType") != "linux":
                raise RuntimeFailure("Switch Docker Desktop to Linux containers.")
            self.docker("compose", "version")
        except (RuntimeFailure, ValueError) as exc:
            self.verified = False
            return checks + [Check("docker", "Docker Desktop", False, "Install and open Docker Desktop, finish its setup, then check again. Docker manages the Linux containers for your agents. Stormo itself runs directly on Windows.\n" + str(exc))]
        checks.append(Check("docker", "Docker Desktop", True, "Docker's Linux engine and Compose are ready."))
        if not self.engine.is_file():
            return checks + [Check("repair", "Stormo engine", False, "The bundled Windows engine is missing. Reinstall the complete preview build.")]
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

    def verify_container(self):
        self.docker("run", "--rm", "--network", "none", "hello-world:latest", timeout=300)
        self.verified = True

    def create_instance(self):
        return self.command("new", "instance", str(self.instance), "--name", "My Stormo", "--slug", "my-stormo", "--template", "empty", "--no-git")

    def fleet(self):
        check_core(self.read("/api/core"), self.engine, self.instance)
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
                check_core(core, self.engine, self.instance)
            return self.command("core", "up")
        core = self.read("/api/core")
        check_core(core, self.engine, self.instance)
        if action == "core down":
            return self.command("core", "down")
        if not agent or agent.startswith("-"):
            raise RuntimeFailure("Select one agent first.")
        if agent not in {a["id"] for a in self.fleet().get("agents", [])}:
            raise RuntimeFailure("That agent is no longer in this workspace. Refresh and select it again.")
        return self.command(action, agent, timeout=900)
