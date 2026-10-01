#!/usr/bin/env python3
"""Generate plugins.json (the apt search index) from plugin Metadata."""
import json
import pathlib
import re
import sys

root = pathlib.Path(__file__).resolve().parent.parent / "plugins-external"


def field(block: str, name: str) -> str:
    m = re.search(rf'\b{name}:\s*"([^"]*)"', block)
    return m.group(1) if m else ""


entries = []
for d in sorted(root.iterdir()):
    src = d / "main.go"
    if not src.is_file():
        continue
    text = src.read_text(encoding="utf-8")
    m = re.search(r"var Metadata = &plugin\.PluginMetadata\{(.*?)\n\}", text, re.S)
    block = m.group(1) if m else ""
    desc = field(block, "Description")
    if not desc:
        m = re.search(r'Description\(\) string\s*\{\s*return "([^"]*)"', text)
        desc = m.group(1) if m else ""
    desc_en = field(block, "DescEN")
    if not desc_en:
        m = re.search(r'DescEN\(\) string\s*\{\s*return "([^"]*)"', text)
        desc_en = m.group(1) if m else ""
    entries.append({
        "name": d.name,
        "description": desc,
        "desc_en": desc_en,
        "version": field(block, "Version") or "1.0.0",
    })

out = sys.argv[1] if len(sys.argv) > 1 else "plugins.json"
pathlib.Path(out).write_text(json.dumps(entries, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
print(f"{len(entries)} plugins -> {out}")
