# Restart summary: demo (qm up, 2026-11-20 10:00Z)

You are the coordinator of this project, started fresh by qm up: the previous
coordinator's context was 231k tokens (threshold 200k). Its transcript is
/claude/projects/demo/coord.jsonl; read from it only what
this summary lacks. Workers are addressed by their session names, as before.

## Workers
The states are qm up's plan for this run, not what happened: a worker
whose launch failed is still listed as planned. A worker
that does not answer may not have started: check the workspace's sessions.

| name | state | branch | pull request | last activity | context | qm up plan |
|---|---|---|---|---|---|---|
| alpha | finished (merged) | feature-a | - | 2026-11-20 07:00Z | 310k | left alone |
| mid | started fresh | feature-m | - | 2026-11-20 08:00Z | 151k | started fresh: context 151k, handoff note |
| zeta | resumed | feature-z | #12 | 2026-11-20 09:48Z | 92k | resumed: cache warm (last activity 12 min ago) |

## Previous coordinator's handoff note
/state/handoff/coord.md

Finish the importer; tests for the parser are still open.

## The owner's last messages
Message 1:
> first kept

Message 2:
> second kept
> over two lines

Message 3:
> third kept

## Git
- branch: main
- uncommitted files: 2
- worktrees:
  - /work/demo
  - /work/demo/.claude/worktrees/feature-z
- open pull requests:
  - #12 Add the parser (feature-z)
  - #13 Other (feature-q)
