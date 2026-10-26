"""Check final PE icon groups against the approved ICO (pefile ships with PyInstaller)."""
from pathlib import Path
import struct

import pefile

ROOT = Path(__file__).resolve().parents[1]


def ico_frames(path):
    data = path.read_bytes()
    reserved, kind, count = struct.unpack_from("<HHH", data)
    assert (reserved, kind) == (0, 1), "Expected an ICO file"
    frames = {}
    for index in range(count):
        entry = struct.unpack_from("<BBBBHHII", data, 6 + 16 * index)
        width, height = entry[0] or 256, entry[1] or 256
        length, offset = entry[-2:]
        frames[(width, height)] = data[offset:offset + length]
    assert set(frames) == {(size, size) for size in (16, 24, 32, 48, 64, 128, 256)}
    return frames


def check_exe(path, expected):
    with pefile.PE(str(path)) as pe:
        resources = {}
        for kind in pe.DIRECTORY_ENTRY_RESOURCE.entries:
            for name in kind.directory.entries:
                for language in name.directory.entries:
                    data = language.data.struct
                    resources[(kind.id, name.id, language.id)] = pe.get_data(
                        data.OffsetToData, data.Size)
        for (kind, _, language), group in resources.items():
            if kind != 14:  # RT_GROUP_ICON
                continue
            count = struct.unpack_from("<H", group, 4)[0]
            frames = {}
            for index in range(count):
                entry = struct.unpack_from("<BBBBHHIH", group, 6 + 14 * index)
                size = (entry[0] or 256, entry[1] or 256)
                frames[size] = resources.get((3, entry[-1], language))  # RT_ICON
            if frames == expected:
                print(f"{path.name}: all seven Stormo icon frames match")
                return
    raise AssertionError(f"{path}: approved Stormo icon group missing or changed")


if __name__ == "__main__":
    expected = ico_frames(ROOT / "assets/stormo.ico")
    check_exe(ROOT / "dist/Stormo/Stormo.exe", expected)
    check_exe(ROOT / "build/installer/Stormo-Setup-native-x64-preview.exe", expected)
