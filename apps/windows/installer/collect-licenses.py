"""Collect resolved NuGet license/notice files and package provenance for the installer."""
import json
from pathlib import Path
import shutil

root = Path(__file__).resolve().parents[1]
assets = json.loads((root / "Stormo.Desktop/obj/project.assets.json").read_text(encoding="utf-8-sig"))
target = root / "assets/licenses"
target.mkdir(parents=True, exist_ok=True)
packages = []
for name, metadata in assets["libraries"].items():
    if metadata.get("type") != "package":
        continue
    packages.append(name)
    for folder in assets["packageFolders"]:
        package = Path(folder) / metadata["path"]
        if not package.is_dir():
            continue
        for file in package.rglob("*"):
            if file.is_file() and any(word in file.name.lower() for word in ("license", "notice", "copying")):
                relative = file.relative_to(package)
                destination = target / name / relative
                destination.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(file, destination)
(target / "PACKAGES.txt").write_text("Resolved build packages (licenses in package subdirectories):\n" + "\n".join(packages) + "\n\n.NET runtime: https://github.com/dotnet/runtime/blob/main/LICENSE.TXT\nWindows App SDK: https://github.com/microsoft/WindowsAppSDK\n", encoding="utf-8")
