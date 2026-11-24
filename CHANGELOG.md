# Changelog

## 0.2.1

A hardening release, after an audit of the code.

- **The worker's permission flag comes from a verified coordinator.** `qm
  spawn worker` copies the flag only from the nearest `claude` among its own
  parent processes, checked to run a claude executable; `CLAUDE_PID` must
  name that same process. In every other case the worker gets no flag and
  prompts, and `qm status` says why.
- **Names and ids are checked before they become file names or command
  arguments.** Worker names, project ids, branches and folders read from
  qm's state and decision log are validated where they are read and again
  where files are written. An unusable field is cleared with a `note:` and
  the rest of the project goes on.
- **A restored worker follows the config, not its Record.** `qm up` gives it
  this qm's own plugin folder, and its MCP config only when `project_mcp`
  still names that file.
- **Redaction covers more.** Long hex keys, keys with well-known vendor
  prefixes, values of credential fields, IPv6 addresses, non-ASCII and
  fullwidth email forms, more phone forms, and person names regardless of
  case, width or zero-width characters. A placeholder already in the text
  can no longer pass for one redaction wrote. Versions after `@`, timestamps
  in file names and UUIDs are no longer redacted.
- **An invalid `privacy.clients` entry is a config error** (an empty
  `replace`, or a `pattern` that can match empty text), and keeps all text on
  your machine until it is fixed.
- **The launch gate reads wrapped launches.** It sees a `claude` or `codex`
  launch behind `env`, `sudo`, `nice`, `xargs`, `find -exec`, `eval` and
  similar wrappers, shell reserved words, command substitutions and quoting.
  A `codex` run with a prompt is denied like `codex exec`. Commands hidden in
  an interpreter's string (`python3 -c`, say) are outside what it reads.
- **One bad line or file no longer breaks a command.** An overlong line in
  the decision log is skipped; a very long last line in a transcript no
  longer hides the lines before it; the decision log is not opened through a
  symbolic link; two simultaneous appends can no longer leave a blank line.
- **An unreadable project state file says how to recover.** It still stops
  first runs and adoptions, since qm cannot tell which sessions that project
  holds, and the error now names the file and the command that moves it
  aside.
- Sweeps of qm's state folders remove only their own files, and never a lock
  file.

## 0.2.0

- **`qm watch` and worker states.** `qm watch` waits in the background until a
  worker stops, goes silent or goes away, then says which and exits, so a
  coordinator that runs it is woken instead of waiting on a worker that will
  never report. `qm status` lists the project's workers with their state,
  context size and model. Both read new turn hooks in the `qm-worker` plugin;
  every stop, silence and disappearance is reported once. With a notifier
  configured, each event is also sent through it (the worker's name and state
  only).
- **Adopting workers later.** `qm up --adopt` on a project that already has
  state adds running sessions started by hand as workers and restores nothing.
  In a terminal it asks which; with `--yes` or without a terminal it takes only
  the ones a worker Record names. A plain `qm up` prints a one-line note when
  such sessions exist.
- **Stricter adoption and restore.** Two projects cannot adopt the same
  session, workspace or tab; a worker is never given a tab another session
  owns; a restored worker's permission is capped at the running coordinator's
  and never raised; detection that runs out of time is an error, not an empty
  list. `qm up` and `qm down` look at a terminal again right before they close
  it and leave it open when it changed.
- **`qm init` repairs an earlier install.** It repoints a status line left by
  another `qm`, and `qm status` says when the status line runs a `qm` that is
  gone.
- The tmux launcher is labelled experimental: it is covered by the automated
  tests, and agterm is the launcher qm is used with day to day.
- `qm spawn review` prints a note when the branch it runs on has no diff
  against its base, the sign of a review started in the wrong folder.

## 0.1.1

- **`qm init` names the right plugin marketplace.** Run inside a quartermaster
  checkout, an installed `qm` printed the checkout's path in
  `/plugin marketplace add`. It now prints `IZZY-Agency/quartermaster` unless
  `qm` itself was built and run from that checkout's `bin/`.
- The README links claude-tg, thanks agterm, and has the badges and the
  "How this repository is maintained" section.

## 0.1.0

First release.

- **Worker routing.** `qm route worker` and `qm spawn worker` pick a model and
  effort for each worker from the task's type and size, the risk it carries and,
  when you have a TypeSafe key, Jev's judgments of the brief. `qm spawn worker`
  then starts a lean worker in agterm or tmux and logs the decision. A forced
  tier needs a reason.
- **Review routing.** `qm route review` and `qm spawn review` send a review to
  Codex, with an explicit model and effort, while your Codex limit has room, and
  to Claude after. The review runs in the background with reading tools only
  and returns structured findings (`--wait` waits for them). By default the
  round comes from qm's decision log and the diff size from git.
- **Privacy gate.** Before anything goes to TypeSafe, secrets, emails, phone
  numbers, IPv4 addresses other than loopback, tokens, your home folder, the
  client names you list in `privacy.clients` and (with the optional macOS
  helper) person names are replaced by placeholders. Text that is still private stays on your machine.
- **Two Claude Code plugins.** `qm-coordinator`, installed from this
  repository's plugin marketplace, gates raw worker and review launches, sets the
  model of review sub-agents, and (with a TypeSafe key) stops questions the
  coordinator can answer itself and flags lasting rules you state. `qm-worker`
  is loaded into workers by `qm spawn worker`: it sends code reading to a Haiku
  explorer, budgets exploration reads, nudges towards the LSP and `Edit`, and
  asks the worker to hand off before its context fills. Every rule has an
  on/note/off switch, and the read budget and the launch gate, after blocking
  work three times in a phase, soften to a note for a while.
- **Status-line limits.** `qm statusline` records Claude's 5-hour and 7-day
  usage from the status line and passes the line through to your own status
  command. Above 90% of the 5-hour limit, decisions for briefs without risk
  move one tier down.
- **`qm report`.** Prints what workers cost and how their reviews went, by tier
  and by worker, and with `--baseline` flags routing rows that did worse than
  your earlier sessions.
- **`qm init`, `qm init --undo` and `qm status`.** Set up the config, offer the
  status-line wrapper, and print the plugin commands; undo puts back what init
  changed; status shows what is on and off, and why.
- **`qm up` and `qm down`.** `qm up` starts a project's coordinator and, after a
  restart, restores the coordinator and its workers (resuming or starting fresh
  from a handoff note, whichever costs less), never starting a live session
  twice. `qm up done NAME` marks a worker finished. `qm down` records each
  session's context and last activity, then closes the project's terminal
  sessions.
- **Adopting running sessions.** A first `qm up` in a project that already
  has Claude sessions running takes them over (`--adopt`, or by asking)
  without restarting them; `--new` starts a new coordinator beside them. After
  an agterm restart, `qm up` resumes each session in its restored tab.
- **Optional providers.** TypeSafe, Codex, gitleaks, the Apple Foundation Model
  helper, agterm, tmux, a notifier and the official LSP plugins are all
  optional: a missing one turns its feature off and `qm status` says so.
- macOS (arm64, x86_64) and Linux (arm64, x86_64). The Foundation Model helper is
  macOS only.
