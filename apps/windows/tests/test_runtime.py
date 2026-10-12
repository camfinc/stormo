import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch
from urllib.error import URLError

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from stormo_desktop.runtime import (Runtime, RuntimeFailure, check_core, cli_args,
    decode, parse_result, MARKER, INSTANCE, ENGINE)

CORE = {"service": "swarm-core", "api": 1, "instance": {"root": INSTANCE}, "exe": ENGINE}


class RuntimeTests(unittest.TestCase):
    def test_wsl_unicode_inventory(self):
        self.assertEqual(decode("Stormo\r\n".encode("utf-16-le")), "Stormo")
        self.assertEqual(decode("Stormo\r\n".encode("utf-16")), "Stormo")

    def test_cli_keeps_arguments_literal_and_local(self):
        argv = cli_args("chat", "agent", "$(touch /tmp/no); hello world")
        self.assertEqual(argv[-1], "$(touch /tmp/no); hello world")
        self.assertNotIn("sh", argv)
        self.assertIn("--exec", argv)
        self.assertEqual(argv[argv.index("--target") + 1], "local")
        self.assertEqual(argv[argv.index("--instance") + 1], INSTANCE)

    def test_cli_events(self):
        self.assertEqual(parse_result('{"event":"step","msg":"working"}\n{"event":"result","data":{"ok":true}}'), {"ok": True})
        for text in ('{"event":"error","msg":"refused"}', '{"event":"step"}', 'not json', '[]'):
            with self.subTest(text=text), self.assertRaises(RuntimeFailure):
                parse_result(text)

    def test_core_ownership_and_version(self):
        check_core(CORE)
        for patch_value in ({"api": 2}, {"service": "other"}, {"instance": {"root": "/other"}}, {"exe": "/other/bin"}):
            with self.subTest(patch_value=patch_value), self.assertRaises(RuntimeFailure):
                check_core(CORE | patch_value)

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
            return MARKER if args[-1] == "/etc/stormo-runtime" else '{"event":"result","data":{"started":true}}'
        rt = Runtime(Path("."), runner=runner, reader=stopped)
        self.assertTrue(rt.action("core up")["started"])
        self.assertEqual(calls[-1][-2:], ["core", "up"])

    def test_distro_collision_never_imports(self):
        calls = []
        def runner(args, **kw):
            calls.append(args)
            return "Stormo"
        rt = Runtime(Path("."), runner=runner)
        with self.assertRaises(RuntimeFailure):
            rt.import_runtime()
        self.assertEqual(len(calls), 1)

    def test_corrupt_bundle_never_imports(self):
        calls = []
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "runtime.tar.gz").write_bytes(b"broken")
            (root / "runtime.sha256").write_text("not-the-hash  runtime.tar.gz")
            rt = Runtime(root, runner=lambda args, **kw: calls.append(args) or "")
            with self.assertRaisesRegex(RuntimeFailure, "integrity"):
                rt.import_runtime()
        self.assertEqual(len(calls), 1)

    def test_agent_is_revalidated_before_action(self):
        calls = []
        rt = Runtime(Path("."), runner=lambda args, **kw: calls.append(args), reader=lambda route: CORE if route == "/api/core" else {"agents": []})
        with self.assertRaises(RuntimeFailure):
            rt.action("stop", "missing")
        self.assertEqual(calls, [])

    @patch("platform.system", return_value="Windows")
    @patch("platform.machine", return_value="AMD64")
    @patch("platform.version", return_value="10.0.26100")
    def test_setup_stops_at_missing_prerequisite(self, *_):
        def runner(args, **kw):
            if args == ["wsl.exe", "--status"]:
                raise RuntimeFailure("missing")
            self.fail("Must not probe downstream dependencies")
        checks = Runtime(Path("."), runner=runner).detect()
        self.assertEqual(checks[-1].key, "wsl")
        self.assertFalse(checks[-1].ready)

    @patch("platform.system", return_value="Windows")
    @patch("platform.machine", return_value="AMD64")
    @patch("platform.version", return_value="10.0.26100")
    def test_docker_server_not_enough_until_container_test(self, *_):
        def runner(args, **kw):
            if args[-1] == "--quiet": return "Stormo"
            if args[-1] == "/etc/stormo-runtime": return MARKER
            if args[-2:] == ["uname", "-r"]: return "6.6.0-microsoft-standard-WSL2"
            if "info" in args: return '{"OSType":"linux"}'
            if ENGINE in args: return '{"event":"result","data":{"api":1}}'
            return "ok"
        rt = Runtime(Path("."), runner=runner)
        self.assertEqual(rt.detect()[-1].key, "verify")
        rt.verify_container()
        self.assertTrue(all(check.ready for check in rt.detect()))


if __name__ == "__main__":
    unittest.main()
