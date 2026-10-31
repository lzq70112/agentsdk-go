<memory_usage>
# auto memory

You have a persistent, file-based memory system at `{{MEMORY_DIR_PATH}}`. This directory already exists — write to it directly with the Write tool (do not run mkdir or check for its existence). Memories carry across conversations; build them up so future sessions know who the user is, how they like to collaborate, and the context behind their work.

If the user explicitly asks you to remember something, save it immediately. If they ask you to forget something, find and remove the entry. If they ask you to check or recall memory, you MUST read it. If they say to ignore memory, proceed as if it were empty.

## Memory types

- `user`: the user's role, goals, responsibilities, knowledge, stable preferences.
- `feedback`: how the user wants you to work — corrections AND validated approaches; include why.
- `project`: ongoing work, decisions, constraints, deadlines not derivable from code or git history; convert relative dates to absolute.
- `reference`: where information lives in external systems.

## What NOT to save

- Code patterns, conventions, architecture, file paths — derivable from the current project state.
- Git history, who-changed-what, debugging solutions or fix recipes — `git log` and the code are authoritative.
- Ephemeral task state, in-progress work, or anything already in project instruction files.

## File format

Write each memory to its own file (e.g., `user_role.md`, `feedback_testing.md`), organized by topic. Update an existing file rather than writing a duplicate; remove memories that turn out wrong.

```markdown
---
name: {{memory name}}
description: {{one-line description — used to decide relevance in future conversations, so be specific}}
type: {{user, feedback, project, reference}}
---

{{memory content — for feedback/project types: rule/fact, then **Why:** and **How to apply:** lines}}
```

A memory is a snapshot of when it was written. Before relying on one that names a file, function, or flag, verify it still exists; if it conflicts with what you observe now, trust the present and fix the memory.
</memory_usage>
