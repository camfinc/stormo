---
name: zzsharedzz
description: "Shared ZZORGZZ document space: read briefs and handoffs other agents left, and publish documents for other agents (group-wide or zzunitzz unit). Use when a task mentions a shared doc, a handoff, a brief, or when another agent or a teammate should be able to read your output."
version: 1.0.0
author: zzresourcezz
license: proprietary
metadata:
  hermes:
    tags: [zzslugzz, shared, documents, handoff]
    related_skills: [zzknowledgezz]
---
# ZZORGZZ shared documents (generated)

Two directories are mounted for you and persist across restarts:

| path | who can read and write | put here |
|---|---|---|
| `/shared/group` | every ZZORGZZ agent, all units | cross-unit briefs, goals, policies, research everyone can use |
| `/shared/zzunitzz` | zzunitzz agents only | operational documents for the zzunitzz unit |

Other units' directories do not exist for you. To share with another unit, write to `group`.

## Conventions

- One Markdown file per document with frontmatter (`title`, `author`, `unit`, `created`,
  `tags`, optional `to`). Create documents with the script so the frontmatter and a unique,
  collision-free file name are always right:
  `python3 ${HERMES_SKILL_DIR}/scripts/shared_docs.py new zzunitzz q4-plan --title "Q4 plan" --tags planning,brief < body.md`
- Handoffs to a specific agent: add `--to <agent>`; the file lands in `<layer>/handoffs/<agent>/`.
- Find documents: `shared_docs.py list` (newest first; `--layer`, `--tag`, `--to zzagentzz`).
  Check `--to zzagentzz` at the start of a task that mentions a handoff.
- Never overwrite another agent's document. To respond or revise, create a new document that
  names the original path in its body.
- Binary attachments (PDFs, images) go next to the document that references them, same name prefix.

## Hard rules

- **No credentials, ever**, in any shared file.
- **No client personal data in `group`** (names with contact details, identity documents, card data, account numbers).
  Client documents stay in the unit directory, and only when the task needs them there.
- Shared documents are working material, not memory: lessons belong in memory so they go through
  review. Do not copy shared documents into memory.
