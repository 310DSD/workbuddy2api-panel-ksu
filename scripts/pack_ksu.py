"""pack_ksu.py 打包 KernelSU 模块 zip。

为什么用 Python 而不是 PowerShell Compress-Archive：.NET 的 ZipFile 在 Windows 上
写过反斜杠条目（bin\\wb2api），Android 的刷机脚本解不出来（上一轮真踩）。这里用
zipfile 显式构造正斜杠条目，条目顺序与时间戳固定 → 产物可复现、digest 稳定。

版本号与 versionCode 一律从 kernelsu/module.prop 读取（单一事实源），避免打包脚本
里再硬编码一份、发版时漂移。

用法：python scripts/pack_ksu.py [src_dir] [out_zip]
"""

import hashlib
import os
import re
import sys
import zipfile

DEFAULT_SRC = "kernelsu"
# 固定时间戳：同内容 → 同字节，便于对账（不参与任何运行时语义）。
FIXED_DATE = (2026, 10, 22, 18, 0, 0)


def read_module_prop(src: str) -> tuple[str, str]:
    """从 module.prop 读 (version, versionCode)。

    version 形如 v1.13.1-ksu；产物名沿用 wb2api_panel-<version>-<code>-arm64.zip。
    """
    prop = os.path.join(src, "module.prop")
    version, code = "", ""
    with open(prop, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if line.startswith("version="):
                version = line.split("=", 1)[1].strip()
            elif line.startswith("versionCode="):
                code = line.split("=", 1)[1].strip()
    if not version or not code:
        raise SystemExit(f"{prop}: 缺 version / versionCode")
    if not re.fullmatch(r"v[0-9A-Za-z._-]+", version):
        raise SystemExit(f"{prop}: version 形态异常：{version!r}")
    if not code.isdigit():
        raise SystemExit(f"{prop}: versionCode 必须是数字：{code!r}")
    return version, code


def default_out(src: str) -> str:
    version, code = read_module_prop(src)
    return f"dist/wb2api_panel-{version}-{code}-arm64.zip"


def build(src: str, out: str) -> None:
    entries = []
    for root, dirs, files in os.walk(src):
        dirs.sort()
        files.sort()
        rel = os.path.relpath(root, src)
        rel = "." if rel == "." else rel.replace(os.sep, "/")
        if rel != ".":
            entries.append((rel + "/", None))
        for f in files:
            arc = f if rel == "." else rel + "/" + f
            entries.append((arc, os.path.join(root, f)))

    os.makedirs(os.path.dirname(out), exist_ok=True)
    with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED, compresslevel=9) as z:
        for arc, path in entries:
            zi = zipfile.ZipInfo(arc, date_time=FIXED_DATE)
            if path is None:
                zi.external_attr = 0o40755 << 16
                zi.compress_type = zipfile.ZIP_STORED
                z.writestr(zi, b"")
            else:
                zi.external_attr = 0o100644 << 16
                zi.compress_type = zipfile.ZIP_DEFLATED
                with open(path, "rb") as fh:
                    z.writestr(zi, fh.read())

    data = open(out, "rb").read()
    print("zip:", out)
    print("size:", len(data))
    print("sha256:", hashlib.sha256(data).hexdigest())
    with zipfile.ZipFile(out) as z:
        names = z.namelist()
        print("entries:", len(names))
        bad = [n for n in names if "\\" in n]
        print("backslash_entries:", bad)
        for n in names:
            info = z.getinfo(n)
            print(f"  {n} {info.file_size}")


if __name__ == "__main__":
    src = sys.argv[1] if len(sys.argv) > 1 else DEFAULT_SRC
    out = sys.argv[2] if len(sys.argv) > 2 else default_out(src)
    build(src, out)
