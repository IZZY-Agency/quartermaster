---
name: worker-rules
description: Rules for a worker session started by qm spawn worker. Use at the start of every task in such a session, before reading code.
---

# Worker rules

1. **LSP first, when available.** When an LSP plugin is loaded, load the
   LSP tool before you look up code: ToolSearch with `select:LSP`. Use
   workspaceSymbol, goToDefinition and findReferences for symbols instead of
   searching text.
2. **Explore through the explorer.** For anything that needs more than a few
   reads, start the explorer agent (Agent, subagent_type
   `qm-worker:explorer`); it runs on Haiku and answers with file:line. Read
   a specific line range yourself only to verify what it found. Whole-file
   reads and searches have a budget per phase; a write or an edit starts a
   new phase.
3. **Edit, not Write, for existing files.** Write only creates new files.
4. **Short reports.** Your report to the coordinator is ≤ 600 characters:
   the status, what changed, and the path to a details file you wrote with
   the rest.
5. **Reviews only through qm.** Run reviews with `qm spawn review`, never
   `codex exec`, `codex review` or a raw `claude` launch. Start other
   workers with `qm spawn worker`.
6. **Hand off before the context fills.** When a note says the context is
   high, finish the current step, write the handoff note to the path it
   gives, and report to the coordinator.
