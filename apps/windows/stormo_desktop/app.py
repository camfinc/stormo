from __future__ import annotations

import base64
from pathlib import Path
import sys

from PySide6.QtCore import QObject, QRunnable, QThreadPool, QTimer, QUrl, Signal, Qt, QSize
from PySide6.QtGui import QDesktopServices, QFont, QIcon
from PySide6.QtWidgets import (QAbstractItemView, QApplication, QHBoxLayout, QHeaderView,
    QLabel, QListWidget, QMainWindow, QMessageBox, QProgressBar, QPushButton, QSplitter,
    QStackedWidget, QTableWidget, QTableWidgetItem, QTextEdit, QVBoxLayout, QWidget)

from .runtime import Runtime, RuntimeFailure, run


def assets_path() -> Path:
    return Path(getattr(sys, "_MEIPASS", Path(__file__).resolve().parents[1])) / "assets"


class Signals(QObject):
    done = Signal(object)
    failed = Signal(str)


class Task(QRunnable):
    def __init__(self, function):
        super().__init__()
        self.function, self.signals = function, Signals()

    def run(self):
        try:
            self.signals.done.emit(self.function())
        except Exception as exc:
            self.signals.failed.emit(str(exc))


class Window(QMainWindow):
    def __init__(self, runtime=None, start_checks=True):
        super().__init__()
        self.runtime = runtime or Runtime(assets_path())
        self.busy = False
        self.ready = False
        self.checks = []
        self.setWindowTitle("Stormo — Windows preview")
        self.setWindowIcon(QIcon(str(assets_path() / "mark.svg")))
        self.resize(1080, 720)
        self.setMinimumSize(820, 560)
        self.pool = QThreadPool(self)
        self.pool.setMaxThreadCount(1)
        self.sidebar = QListWidget()
        self.sidebar.addItems(["Setup", "Agents", "Core"])
        self.sidebar.setAccessibleName("Sections")
        for index in range(self.sidebar.count()):
            self.sidebar.item(index).setSizeHint(QSize(150, 44))
        self.sidebar.setFixedWidth(160)
        self.pages = QStackedWidget()
        self.sidebar.currentRowChanged.connect(self.pages.setCurrentIndex)
        content = QSplitter()
        content.addWidget(self.sidebar)
        content.addWidget(self.pages)
        content.setChildrenCollapsible(False)
        self.setCentralWidget(content)
        self.controls = []
        self.setup_page()
        self.agents_page()
        self.core_page()
        self.sidebar.setCurrentRow(0)
        self.progress = QProgressBar()
        self.progress.setRange(0, 0)
        self.progress.setMaximumWidth(160)
        self.progress.hide()
        self.statusBar().addPermanentWidget(self.progress)
        self.statusBar().showMessage("Checking this PC…")
        self.timer = QTimer(self)
        self.timer.setInterval(5000)
        self.timer.timeout.connect(self.poll)
        if start_checks:
            self.timer.start()
            QTimer.singleShot(0, self.check_setup)

    def page(self, title, description):
        page = QWidget()
        layout = QVBoxLayout(page)
        layout.setContentsMargins(28, 24, 28, 24)
        layout.setSpacing(16)
        heading = QLabel(title)
        font = QFont(heading.font())
        font.setPointSize(23)
        font.setBold(True)
        heading.setFont(font)
        layout.addWidget(heading)
        label = QLabel(description)
        label.setWordWrap(True)
        layout.addWidget(label)
        self.pages.addWidget(page)
        return layout

    def button(self, label, callback, layout):
        button = QPushButton(label)
        button.setMinimumHeight(34)
        button.clicked.connect(callback)
        layout.addWidget(button)
        self.controls.append(button)
        return button

    def setup_page(self):
        layout = self.page("Make yourself at home", "Stormo runs your agents on this PC. We'll help you prepare Windows and its container runtime. No terminal required.")
        self.checklist = QListWidget()
        self.checklist.setAccessibleName("Setup progress")
        self.checklist.setFocusPolicy(Qt.FocusPolicy.NoFocus)
        self.checklist.setMaximumHeight(280)
        layout.addWidget(self.checklist)
        self.instruction = QLabel("Checking what is already installed…")
        self.instruction.setWordWrap(True)
        self.instruction.setTextFormat(Qt.TextFormat.PlainText)
        self.instruction.setMinimumHeight(100)
        layout.addWidget(self.instruction)
        buttons = QHBoxLayout()
        self.next_button = self.button("Checking…", self.next_step, buttons)
        self.button("Check again", self.check_setup, buttons)
        buttons.addStretch()
        layout.addLayout(buttons)
        note = QLabel("Docker Desktop is a separate app with its own license terms. Setup may need administrator approval, internet access and a Windows restart. Your workspace is kept when Stormo is uninstalled.")
        note.setWordWrap(True)
        layout.addWidget(note)
        layout.addStretch()

    def agents_page(self):
        layout = self.page("Agents", "Your local fleet. Select an agent to start, stop, restart or save a nap.")
        self.fleet_status = QLabel("Finish setup to connect your workspace.")
        self.fleet_status.setWordWrap(True)
        self.fleet_status.setTextFormat(Qt.TextFormat.PlainText)
        layout.addWidget(self.fleet_status)
        self.table = QTableWidget(0, 5)
        self.table.setHorizontalHeaderLabels(["Agent", "Unit", "State", "Health", "Model"])
        self.table.horizontalHeader().setSectionResizeMode(QHeaderView.ResizeMode.Stretch)
        self.table.setSelectionBehavior(QAbstractItemView.SelectionBehavior.SelectRows)
        self.table.setSelectionMode(QAbstractItemView.SelectionMode.SingleSelection)
        self.table.setEditTriggers(QAbstractItemView.EditTrigger.NoEditTriggers)
        self.table.setAccessibleName("Agents in this workspace")
        self.table.verticalHeader().hide()
        self.table.verticalHeader().setDefaultSectionSize(40)
        self.table.itemSelectionChanged.connect(self.update_controls)
        layout.addWidget(self.table)
        buttons = QHBoxLayout()
        self.agent_buttons = []
        for label, action in [("Start", "start"), ("Stop", "stop"), ("Restart", "restart"), ("Nap now", "nap-now")]:
            self.agent_buttons.append(self.button(label, lambda checked=False, a=action: self.agent_action(a), buttons))
        buttons.addStretch()
        self.refresh_button = self.button("Refresh", self.refresh_fleet, buttons)
        layout.addLayout(buttons)

    def core_page(self):
        layout = self.page("Core", "The core coordinates this workspace. Closing the app leaves it and your agents running.")
        buttons = QHBoxLayout()
        self.core_buttons = [self.button("Start core", lambda: self.core_action("core up"), buttons),
                             self.button("Stop core", lambda: self.core_action("core down"), buttons)]
        buttons.addStretch()
        layout.addLayout(buttons)
        self.details = QTextEdit()
        self.details.setReadOnly(True)
        self.details.setAccessibleName("Current operation details")
        self.details.setPlaceholderText("Operation results appear here. They are not saved by the app.")
        layout.addWidget(self.details)

    def submit(self, label, function, done):
        if self.busy:
            return
        self.busy = True
        self.update_controls()
        self.progress.show()
        self.statusBar().showMessage(label)
        task = Task(function)
        self.task = task  # Keep Python signal wrappers alive through completion.
        def success(value):
            self.finish()
            done(value)
        task.signals.done.connect(success)
        task.signals.failed.connect(self.failed)
        self.pool.start(task)

    def finish(self):
        self.busy = False
        self.progress.hide()
        self.update_controls()
        self.statusBar().showMessage("Ready")

    def failed(self, message):
        self.finish()
        self.statusBar().showMessage("Could not complete this step. See the details and retry.")
        self.details.setPlainText(message)
        self.table.setRowCount(0)
        if self.pages.currentIndex() == 0:
            self.instruction.setText(message + "\nYou can retry with Check again.")
        else:
            self.fleet_status.setText(message)
            self.table.setRowCount(0)
        self.update_controls()

    def update_controls(self):
        for control in self.controls:
            control.setEnabled(not self.busy)
        if hasattr(self, "agent_buttons"):
            for control in self.agent_buttons:
                control.setEnabled(not self.busy and self.ready and self.table.currentRow() >= 0)
        if hasattr(self, "core_buttons"):
            for control in self.core_buttons + [self.refresh_button]:
                control.setEnabled(not self.busy and self.ready)
        if hasattr(self, "next_button"):
            self.next_button.setEnabled(not self.busy and bool(self.checks) and
                (self.ready or self.checks[-1].key not in ("windows", "repair")))

    def check_setup(self):
        self.submit("Checking this PC…", self.runtime.detect, self.show_checks)

    def show_checks(self, checks):
        self.checks = checks
        self.ready = bool(checks) and all(c.ready for c in checks)
        self.checklist.clear()
        for check in checks:
            self.checklist.addItem(("Ready: " if check.ready else "Next: ") + check.title)
            self.checklist.item(self.checklist.count() - 1).setSizeHint(QSize(400, 38))
        self.instruction.setText(checks[-1].detail if checks else "Could not check setup.")
        key = checks[-1].key if checks else "repair"
        self.next_button.setText("Start core and open Agents" if self.ready else {
            "wsl": "Prepare Windows…", "import": "Prepare Stormo", "docker": "Get Docker Desktop…",
            "verify": "Verify runtime", "instance": "Create workspace", "repair": "Repair required",
            "windows": "Unsupported PC"}.get(key, "Check again"))
        self.update_controls()

    def next_step(self):
        if self.ready:
            def opened(_):
                self.sidebar.setCurrentRow(1)
                self.refresh_fleet()
            self.submit("Starting your workspace…", lambda: self.runtime.action("core up"), opened)
            return
        key = self.checks[-1].key
        if key == "docker":
            QDesktopServices.openUrl(QUrl("https://docs.docker.com/desktop/setup/install/windows-install/"))
        elif key == "wsl":
            self.submit("Preparing Windows. Approve the Windows prompt if you want to continue…", install_wsl, self.wsl_finished)
        elif key == "import":
            self.submit("Preparing Stormo's workspace…", self.runtime.import_runtime, lambda _: self.check_setup())
        elif key == "verify":
            self.submit("Testing the container runtime. The first check downloads a small test image…", self.runtime.verify_container, lambda _: self.check_setup())
        elif key == "instance":
            self.submit("Creating your workspace…", self.runtime.create_instance, lambda _: self.check_setup())

    def wsl_finished(self, _):
        self.instruction.setText("Windows setup has finished. Restart Windows if requested, then reopen Stormo and select Check again. No automatic restart will occur.")

    def poll(self):
        if not self.busy and self.ready and self.pages.currentIndex() == 1:
            self.refresh_fleet()

    def refresh_fleet(self):
        self.submit("Refreshing agents…", self.runtime.fleet, self.show_fleet)

    def show_fleet(self, fleet):
        selected = self.selected_agent()
        agents = fleet.get("agents", [])
        self.table.setRowCount(len(agents))
        for row, agent in enumerate(agents):
            for col, key in enumerate(("name", "unit", "state", "health", "model")):
                item = QTableWidgetItem(str(agent.get(key) or "—"))
                item.setData(Qt.ItemDataRole.UserRole, agent["id"])
                self.table.setItem(row, col, item)
            if agent["id"] == selected:
                self.table.selectRow(row)
        self.fleet_status.setText(f"{len(agents)} agents in this workspace." if agents else
            "Your workspace is ready and empty. Agent creation is coming to this preview; existing agents created with the CLI appear here.")
        self.update_controls()

    def selected_agent(self):
        row = self.table.currentRow()
        return self.table.item(row, 0).data(Qt.ItemDataRole.UserRole) if row >= 0 and self.table.item(row, 0) else None

    def agent_action(self, action):
        agent = self.selected_agent()
        if not agent:
            return
        if action in ("stop", "restart") and QMessageBox.question(self, "Confirm action",
                f"{action.capitalize()} {agent}? Active work may be interrupted.") != QMessageBox.StandardButton.Yes:
            return
        self.submit(f"{action.capitalize()}: {agent}…", lambda: self.runtime.action(action, agent), lambda _: self.refresh_fleet())

    def core_action(self, action):
        if action == "core down" and QMessageBox.question(self, "Stop core?",
                "Agents stay running, but model calls and coordination will pause until you start the core again.") != QMessageBox.StandardButton.Yes:
            return
        self.submit("Starting core…" if action == "core up" else "Stopping core…",
                    lambda: self.runtime.action(action), lambda result: self.details.setPlainText(str(result)))

    def closeEvent(self, event):
        if self.busy:
            QMessageBox.information(self, "Setup or action in progress", "Wait for the current step to finish before closing Stormo.")
            event.ignore()
        else:
            event.accept()


def install_wsl():
    # Fixed command only; UAC applies to WSL installation, not the app.
    script = "$p = Start-Process wsl.exe -ArgumentList '--install','--no-distribution' -Verb RunAs -Wait -PassThru; exit $p.ExitCode"
    encoded = base64.b64encode(script.encode("utf-16-le")).decode("ascii")
    try:
        run(["powershell.exe", "-NoProfile", "-NonInteractive", "-EncodedCommand", encoded], timeout=1800)
    except RuntimeFailure as exc:
        raise RuntimeFailure("Windows setup did not finish, or approval was cancelled. If Windows requested a restart, restart and check again. Otherwise retry Prepare Windows.\n" + str(exc)) from exc


def main():
    app = QApplication(sys.argv)
    app.setApplicationName("Stormo")
    app.setOrganizationName("CAMF")
    window = Window()
    window.show()
    return app.exec()
