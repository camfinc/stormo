#!/usr/bin/env python3
"""List, create and read documents in the ZZORGZZ shared space (stdlib only).

  shared_docs.py list [--layer group|<unit>] [--tag t] [--to agent]
  shared_docs.py new <layer> <slug> --title "..." [--tags a,b] [--to agent] < body.md
  shared_docs.py path <layer>
"""
import argparse, datetime, os, re, sys

ROOT = os.environ.get("SWARM_SHARED_DIR", "/shared")
AGENT = os.environ.get("SWARM_AGENT", "unknown")
UNIT = os.environ.get("SWARM_UNIT", "")


def layers():
    return [d for d in ("group", UNIT) if d and os.path.isdir(os.path.join(ROOT, d))]


def frontmatter(path):
    try:
        with open(path, encoding="utf-8") as f:
            text = f.read(4096)
    except OSError:
        return {}
    m = re.match(r"^---\n(.*?)\n---\n", text, re.S)
    meta = {}
    for line in (m.group(1).splitlines() if m else []):
        if ":" in line:
            k, v = line.split(":", 1)
            meta[k.strip()] = v.strip().strip('"')
    return meta


def cmd_list(a):
    rows = []
    for layer in ([a.layer] if a.layer else layers()):
        base = os.path.join(ROOT, layer)
        for dirpath, _, files in os.walk(base):
            for name in files:
                if not name.endswith(".md"):
                    continue
                p = os.path.join(dirpath, name)
                meta = frontmatter(p)
                tags = [t.strip() for t in meta.get("tags", "").strip("[]").split(",") if t.strip()]
                if a.tag and a.tag not in tags:
                    continue
                if a.to and meta.get("to") != a.to:
                    continue
                rows.append((meta.get("created", ""), layer, os.path.relpath(p, ROOT), meta.get("author", "?"), meta.get("title", name)))
    for created, layer, rel, author, title in sorted(rows, reverse=True):
        print(f"{created[:16]:16}  {author:10}  {rel}  {title}")
    if not rows:
        print("(no documents)")


def cmd_new(a):
    if a.layer not in layers():
        sys.exit(f"layer {a.layer!r} is not mounted for this agent (available: {', '.join(layers())})")
    slug = re.sub(r"[^a-z0-9-]+", "-", a.slug.lower()).strip("-") or "doc"
    now = datetime.datetime.now(datetime.timezone.utc)
    folder = os.path.join(ROOT, a.layer, "handoffs", a.to) if a.to else os.path.join(ROOT, a.layer, "docs")
    os.makedirs(folder, exist_ok=True)
    # Date + author prefix: two agents never write the same file, so no locking is needed.
    path = os.path.join(folder, f"{now:%Y-%m-%d}-{AGENT}-{slug}.md")
    n = 2
    while os.path.exists(path):
        path = os.path.join(folder, f"{now:%Y-%m-%d}-{AGENT}-{slug}-{n}.md")
        n += 1
    tags = ", ".join(t.strip() for t in (a.tags or "").split(",") if t.strip())
    head = ["---", f'title: "{a.title}"', f"author: {AGENT}", f"unit: {UNIT}", f"created: {now.isoformat(timespec='seconds')}", f"tags: [{tags}]"]
    if a.to:
        head.append(f"to: {a.to}")
    body = sys.stdin.read() if not sys.stdin.isatty() else ""
    with open(path, "x", encoding="utf-8") as f:
        f.write("\n".join(head + ["---", "", body.strip(), ""]))
    print(path)


def main():
    p = argparse.ArgumentParser()
    sub = p.add_subparsers(dest="cmd", required=True)
    l = sub.add_parser("list"); l.add_argument("--layer"); l.add_argument("--tag"); l.add_argument("--to")
    n = sub.add_parser("new"); n.add_argument("layer"); n.add_argument("slug"); n.add_argument("--title", required=True)
    n.add_argument("--tags"); n.add_argument("--to")
    pa = sub.add_parser("path"); pa.add_argument("layer")
    a = p.parse_args()
    if a.cmd == "list":
        cmd_list(a)
    elif a.cmd == "new":
        cmd_new(a)
    else:
        print(os.path.join(ROOT, a.layer))


if __name__ == "__main__":
    main()
