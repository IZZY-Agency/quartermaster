---
name: spawning
description: How the coordinator starts workers and reviews through qm. Use before starting any worker session or any code review.
---

# Spawning workers and reviews

1. **Workers only through `qm spawn worker`.** Never start a worker with
   `agtermctl session new`, tmux or a raw `claude` command; the launch gate
   denies them. Write the brief to a file, then run:
   `qm spawn worker --brief FILE --type TYPE --size S|M|L --hint MODEL/EFFORT --name NAME --cwd DIR`
   - TYPE: feature, fix, fix-pass, docs, release-paperwork, research,
     migration, ui or review.
   - Size S: one or two files or under ~50 lines; M: one feature in one
     area; L: several areas or systems.
   - Always pass `--hint`, your own pick (for example `opus/high`): qm uses
     it when the brief must stay on this machine or Jev is off.
2. **Watch the workers with `qm watch`.** Keep one `qm watch` per project
   running in the background (Bash with `run_in_background`); start it
   after your first spawn and do not start a second one while it runs. Do
   not poll, and do not rely on the worker's message alone. Its exit wakes
   you: read its lines, deal with the workers they name (a stopped worker
   with a handoff note, a silent one, one gone), then start it again,
   unless it said `no worker to wait for` or `no worker left to wait for`,
   or failed with a usage error (outside a `qm up` project, for one); in
   those cases start it again only after your next spawn. Each stop,
   silence or disappearance wakes you once: a restarted watch lists the
   ones already reported as `(reported)`. A `waiting` worker ended its turn
   with a background task of its own running or a wakeup scheduled: leave
   it, the watch waits on. So it does for a live worker already reported
   silent.
3. **Reviews only through `qm spawn review`.** Never run `codex exec` or
   `codex review` yourself: `qm spawn review --prompt FILE [--repo DIR]`.
   qm counts the round and measures the diff itself, for the branch checked
   out in the folder it runs in: run it in the worktree of the branch under
   review, or name that folder with `--repo`. Run from another checkout it
   sizes no diff and counts the wrong branch's rounds.
4. **Overrides need a reason.** `--force MODEL/EFFORT` replaces qm's pick
   and must come with `--why "..."`; use it only when you know better than
   the table, and say why in one line.
5. **Preview with `qm route`.** `qm route worker|review` with the same
   flags shows the pick and its reasons without starting anything.
6. **Decide what you can.** When a hook says to decide a question yourself,
   state the decision and the reason in one line and continue.
7. **Mark finished workers.** Once you accept a worker's report as
   finished, run `qm up done NAME` with the worker's name, so `qm up`
   never restores it. qm finds the project from your folder when
   `QM_PROJECT_ID` is unset.
8. **Wrap up with a handoff note.** When the owner asks you to wrap up,
   write a handoff note to the file named by `$QM_HANDOFF`: the work in
   progress, each worker's state, open decisions and next steps, so the
   next coordinator can carry on from it. When `QM_HANDOFF` is unset (a
   coordinator `qm up` adopted, until its next restart), run
   `qm up handoff` and write the note to the path it prints. This step is
   only for the session qm up adopted as the project's coordinator: in any
   other session `qm up handoff` refuses, and you write no handoff note.
9. **Never run `qm down` yourself** unless the owner asks: it closes this
   project's terminal sessions, yours included. When the owner asks for
   `qm down`, write the handoff note first (rule 8), then run
   `qm down --yes`.
