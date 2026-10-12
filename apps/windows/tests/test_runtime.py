import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
from urllib.error import URLError

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from stormo_desktop.runtime import (Runtime, RuntimeFailure, check_core, cli_args,
    decode, parse_result, same_path, run)

ENGINE = Path(r"C:\Program Files\Stormo\engine\stormo.exe")
INSTANCE = Path(r"C:\Users\Test User\Stormo\default")
CORE = {"service": "swarm-core", "api": 1, "instance": {"root": str(INSTANCE)}, "exe": str(ENGINE)}


class RuntimeTests(unittest.TestCase):
    def test_windows_diagnostic_decoding(self):
        self.assertEqual(decode("Stormo\r\n".encode("utf-16-le")), "Stormo")
        self.assertEqual(decode("Stormo\r\n".encode("utf-16")), "Stormo")

    def test_cli_keeps_arguments_literal_and_local(self):
        argv = cli_args(ENGINE, INSTANCE, "chat", "agent", "$(touch /tmp/no); hello world")
        self.assertEqual(argv[-1], "$(touch /tmp/no); hello world")
        self.assertNotIn("sh", argv)
        self.assertEqual(argv[0], str(ENGINE))
        self.assertNotIn("wsl.exe", argv)
        self.assertEqual(argv[argv.index("--target") + 1], "local")
        self.assertEqual(argv[argv.index("--instance") + 1], str(INSTANCE))

    def test_cli_events(self):
        self.assertEqual(parse_result('{"event":"step","msg":"working"}\n{"event":"result","data":{"ok":true}}'), {"ok": True})
        for text in ('{"event":"error","msg":"refused"}', '{"event":"step"}', 'not json', '[]'):
            with self.subTest(text=text), self.assertRaises(RuntimeFailure):
                parse_result(text)

    def test_core_ownership_and_version(self):
        check_core(CORE, ENGINE, INSTANCE)
        for patch_value in ({"api": 2}, {"service": "other"}, {"instance": {"root": "/other"}}, {"exe": "/other/bin"}):
            with self.subTest(patch_value=patch_value), self.assertRaises(RuntimeFailure):
                check_core(CORE | patch_value, ENGINE, INSTANCE)

    def test_foreign_core_cannot_be_started_or_stopped(self):
        calls = []
        rt = Runtime(Path("."), runner=lambda args, **kw: calls.append(args), reader=lambda _: CORE | {"instance": {"root": "/someone-else"}})
        for action in ("core up", "core down", "start"):
            with self.assertRaises(RuntimeFailure):
                rt.action(action, "atlas")
        self.assertEqual(calls, [])

    def test_unreachable_not_confused_with_stopped(self):
        calls = []
        def unreachable(_):
            raise URLError("network unavailable")
        rt = Runtime(Path("."), runner=lambda args, **kw: calls.append(args), reader=unreachable)
        with self.assertRaises(RuntimeFailure):
            rt.action("core up")
        self.assertEqual(calls, [])

    def test_refused_connection_can_start_own_core(self):
        calls = []
        def stopped(_):
            raise URLError(ConnectionRefusedError())
        def runner(args, **kw):
            calls.append(args)
            return '{"event":"result","data":{"started":true}}'
        rt = Runtime(Path("."), runner=runner, reader=stopped)
        self.assertTrue(rt.action("core up")["started"])
        self.assertEqual(calls[-1][-2:], ["core", "up"])

    def test_windows_path_identity_ignores_case_and_separators(self):
        self.assertTrue(same_path("C:/Users/Test User/Stormo", "c:\\users\\test user\\stormo"))
        self.assertFalse(same_path("C:/Users/a", "C:/Users/ab"))

    @patch("stormo_desktop.runtime.docker_executable", return_value="C:/Docker/docker.exe")
    @patch("subprocess.run")
    def test_native_environment_pins_local_docker(self, child, _):
        child.return_value.returncode = 0
        child.return_value.stdout = b"ok"
        with patch.dict("os.environ", {"SWARM_TARGET": "remote", "DOCKER_HOST": "tcp://other:2375", "DOCKER_CONTEXT": "remote", "SWARM_CORE_BIND": "0.0.0.0"}):
            run([str(ENGINE), "version"])
        env = child.call_args.kwargs["env"]
        self.assertNotIn("DOCKER_HOST", env)
        self.assertNotIn("SWARM_TARGET", env)
        self.assertNotIn("SWARM_CORE_BIND", env)
        self.assertEqual(env["DOCKER_CONTEXT"], "desktop-linux")
        self.assertEqual(env["SWARM_CORE_PORT"], "18600")

    def test_agent_is_revalidated_before_action(self):
        calls = []
        rt = Runtime(Path("."), runner=lambda args, **kw: calls.append(args), reader=lambda route: CORE if route == "/api/core" else {"agents": []})
        rt.engine, rt.instance = ENGINE, INSTANCE
        with self.assertRaises(RuntimeFailure):
            rt.action("stop", "missing")
        self.assertEqual(calls, [])

    @patch("platform.system", return_value="Windows")
    @patch("platform.machine", return_value="AMD64")
    @patch("platform.version", return_value="10.0.26100")
    @patch("stormo_desktop.runtime.docker_executable", return_value=None)
    def test_missing_docker_explains_setup(self, *_):
        calls = []
        checks = Runtime(Path("."), runner=lambda argv, **kw: calls.append(argv)).detect()
        self.assertEqual(checks[-1].key, "docker")
        self.assertFalse(checks[-1].ready)
        self.assertEqual(calls, [])

    @patch("platform.system", return_value="Windows")
    @patch("platform.machine", return_value="AMD64")
    @patch("platform.version", return_value="10.0.26100")
    @patch("stormo_desktop.runtime.docker_executable", return_value="docker.exe")
    def test_ready_requires_container_execution(self, *_):
        calls = []
        def runner(args, **kw):
            calls.append(args)
            if "info" in args: return '{"OSType":"linux"}'
            if args[0].endswith("stormo.exe"): return '{"event":"result","data":{"api":1}}'
            return "ok"
        with tempfile.TemporaryDirectory() as directory:
            assets = Path(directory)
            engine = assets / "engine/bin/stormo.exe"
            engine.parent.mkdir(parents=True)
            engine.touch()
            rt = Runtime(assets, runner=runner)
            self.assertEqual(rt.detect()[-1].key, "verify")
            rt.verify_container()
            self.assertTrue(all(check.ready for check in rt.detect()))
        self.assertTrue(all("wsl.exe" not in argv for argv in calls))
        self.assertTrue(any(argv[:3] == ["docker.exe", "--context", "desktop-linux"] for argv in calls))


if __name__ == "__main__":
    unittest.main()
