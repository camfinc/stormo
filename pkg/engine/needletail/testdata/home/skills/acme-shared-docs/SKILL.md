---
name: acme-shared-docs
description: "Shared Acme document space: read briefs and handoffs other agents left, and publish documents for other agents (group-wide or sales unit). Use when a task mentions a shared doc, a handoff, a brief, or when another agent or a teammate should be able to read your output."
version: 1.0.0
author: acme-stormo
license: proprietary
metadata:
  tags: [acme, shared, documents, handoff]
  related_skills: [acme-knowledge]
---
# Acme shared documents (generated)

Two directories are mounted for you and persist across restarts:

| path | who can read and write | put here |
|---|---|---|
| `/shared/group` | every Acme agent, all units | cross-unit briefs, goals, policies, research everyone can use |
| `/shared/sales` | sales agents only | operational documents for the sales unit |

Other units' directories do not exist for you. To share with another unit, write to `group`.

## Conventions

- One Markdown file per document with frontmatter (`title`, `author`, `unit`, `created`,
  `tags`, optional `to`). Create documents with the script so the frontmatter and a unique,
  collision-free file name are always right:
  `python3 ${SKILL_DIR}/scripts/shared_docs.py new sales q4-plan --title "Q4 plan" --tags planning,brief < body.md`
- Handoffs to a specific agent: add `--to <agent>`; the file lands in `<layer>/handoffs/<agent>/`.
- Find documents: `shared_docs.py list` (newest first; `--layer`, `--tag`, `--to atlas`).
  Check `--to atlas` at the start of a task that mentions a handoff.
- Never overwrite another agent's document. To respond or revise, create a new document that
  names the original path in its body.
- Binary attachments (PDFs, images) go next to the document that references them, same name prefix.

## Hard rules

- **No credentials, ever**, in any shared file.
- **No client personal data in `group`** (names with contact details, identity documents, card data, account numbers).
  Client documents stay in the unit directory, and only when the task needs them there.
- Shared documents are working material, not memory: lessons belong in memory so they go through
  review. Do not copy shared documents into memory.
