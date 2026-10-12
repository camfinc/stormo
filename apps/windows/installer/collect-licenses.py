"""Keep notices shipped in dependency wheels alongside the replaceable Qt DLLs."""
from importlib.metadata import distribution
from pathlib import Path
import shutil

root = Path(__file__).resolve().parents[1] / "assets" / "licenses"
for name in ("PySide6", "PySide6-Essentials", "PySide6-Addons", "shiboken6", "pyinstaller"):
    package = distribution(name)
    for file in package.files or []:
        if any(word in str(file).lower() for word in ("license", "copying", "notice")):
            source = Path(package.locate_file(file))
            if source.is_file():
                target = root / name / Path(str(file))
                # Distribution metadata paths should be relative, never traversal paths.
                if ".." in file.parts or file.is_absolute():
                    continue
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(source, target)
root.mkdir(parents=True, exist_ok=True)
(root / "SOURCES.txt").write_text(
    "Stormo: https://github.com/camfinc/stormo (Apache-2.0)\n"
    "Qt for Python 6.8.3: https://download.qt.io/official_releases/QtForPython/pyside6/PySide6-6.8.3-src/\n"
    "Qt 6.8.3: https://download.qt.io/official_releases/qt/6.8/6.8.3/single/\n"
    "Qt DLLs are distributed separately in this onedir build.\n"
    "Runtime Ubuntu notices: /usr/share/doc inside the Stormo WSL filesystem.\n", encoding="utf-8")
