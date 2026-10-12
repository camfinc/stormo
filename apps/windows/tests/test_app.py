import os
os.environ.setdefault("QT_QPA_PLATFORM", "offscreen")
from pathlib import Path
import sys
import unittest
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from PySide6.QtWidgets import QApplication
from stormo_desktop.app import Window
from stormo_desktop.runtime import Check


class AppTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.app = QApplication.instance() or QApplication([])

    def setUp(self):
        class FakeRuntime:
            def detect(self):
                return [Check("docker", "Docker Desktop", False, "Restart Windows, then check again.")]
        self.window = Window(FakeRuntime(), start_checks=False)
        self.window.timer.stop()

    def tearDown(self):
        self.window.pool.waitForDone()
        self.app.processEvents()
        self.window.close()

    def test_window_icon_renders_at_windows_shell_sizes(self):
        icon = self.window.windowIcon()
        self.assertFalse(icon.isNull())
        self.assertEqual({(s.width(), s.height()) for s in icon.availableSizes()},
                         {(s, s) for s in (16, 24, 32, 48, 64, 128, 256)})
        for size in (16, 32, 48, 256):
            self.assertFalse(icon.pixmap(size, size).isNull())

    def test_setup_actions_disabled_until_detected(self):
        self.window.show_checks([Check("docker", "Docker Desktop", False, "Restart Windows, then check again.")])
        self.assertEqual(self.window.next_button.text(), "Get Docker Desktop…")
        self.assertFalse(self.window.core_buttons[0].isEnabled())
        self.assertFalse(self.window.agent_buttons[0].isEnabled())

    def test_ready_empty_then_selected_fleet(self):
        self.window.show_checks([Check("instance", "Workspace", True, "Ready")])
        self.window.show_fleet({"agents": []})
        self.assertIn("empty", self.window.fleet_status.text())
        self.window.show_fleet({"agents": [{"id": "atlas", "name": "Atlas", "state": "running"}]})
        self.window.table.selectRow(0)
        self.assertEqual(self.window.selected_agent(), "atlas")
        self.assertTrue(self.window.agent_buttons[0].isEnabled())
        self.window.failed("Core disconnected")
        self.assertEqual(self.window.table.rowCount(), 0)
        self.assertFalse(self.window.agent_buttons[0].isEnabled())

    def test_busy_disables_duplicate_actions(self):
        self.window.show_checks([Check("instance", "Workspace", True, "Ready")])
        self.window.busy = True
        self.window.update_controls()
        self.assertFalse(self.window.next_button.isEnabled())
        self.assertFalse(self.window.core_buttons[0].isEnabled())
        self.window.busy = False

    def test_removed_agent_does_not_select_replacement(self):
        self.window.show_checks([Check("instance", "Workspace", True, "Ready")])
        self.window.show_fleet({"agents": [{"id": "atlas", "name": "Atlas"}]})
        self.window.table.selectRow(0)
        self.window.show_fleet({"agents": [{"id": "byte", "name": "Byte"}]})
        self.assertIsNone(self.window.selected_agent())
        self.assertFalse(self.window.agent_buttons[0].isEnabled())
