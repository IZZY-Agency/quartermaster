# Changelog

## 0.3.0

- **Colours in agterm.** Coordinator tabs are purple and worker tabs teal,
  in `qm up` (also `--here` and `--adopt`) and `qm spawn worker`. `qm watch`
  turns a worker's tab orange when it stops, yellow when it goes silent and
  red when it is gone, and gives its colour back when it works again. A tab
  you tinted by hand is left alone; `qm up done` and `qm down` take qm's
  colours off the tabs that stay open. `tint.*` changes or removes them.
- **The coordinator's model.** A coordinator `qm up` starts from scratch runs
  on `up.coordinator_model` (`fable`) and falls back to
  `up.coordinator_fallback` (`opus`); `qm up` prints both. An explicit
  `--model` or `--fallback-model` wins. One that started on the default gets
  the fallback again when it is resumed. Needs Claude Code 2.1.170; an
  earlier one starts without either.
- **After agterm quits without `qm down`.** `qm up note` lists, from any
  folder, the projects whose sessions ended that way and the line that
  restores each. `qm init` offers an agterm launch hook that sends their names
  as one notification, and `qm init --undo` removes it.
- **Codex usage is read from Codex.** Before a review and in `qm status`, qm
  asks Codex for its current usage (`codex app-server`, cached for 5 minutes;
  `codex.usage_read: false` turns it off). Both limit windows count, and `qm
  status` shows the window, where the figure is from and how old it is.
- **Several Codex accounts.** A usage limit counts only for the account that
  hit it, so switching accounts takes effect at once (within 5 minutes with a
  login kept in a keyring). When you switch during a review and the run
  fails, the review is tried once more on Codex under the new account. Two
  members of one workspace count as two.
- **`waiting` workers.** A worker that ended its turn while its own background
  task runs, or a wakeup is scheduled, shows as `waiting` in `qm watch` and
  `qm status`, and is no longer reported as stopped. `qm watch` keeps waiting
  for a live worker it reported as silent.
- **Briefs are off the command line.** qm keeps a copy of a worker's brief in
  its state folder and the first prompt points to it; so do the note and the
  summary `qm up` starts a fresh session with. A brief with a line longer than
  one Read returns is refused; a large one is read in parts.
- **Workers cannot end processes by a pattern.** A new worker rule,
  `pattern-kill`, refuses `pkill`, `killall`, and `kill` fed by `pgrep` or
  `ps`: they match other Claude sessions too.
- **Starts that cannot be taken or repeated.** `qm up` and `qm spawn worker`
  hold a session's id before it starts, so an adoption never claims it in
  between, and a start that stopped in the middle is recovered by the next `qm
  up`. A worker spawned again after a launch without an answer is restored
  once, and `qm up done` ends both.
- **Nothing starts on incomplete data.** While the session file of a running
  claude process, or Claude Code's sessions folder, cannot be read, `qm up`
  starts and adopts nothing, and names the file and how to recover. An
  adoption records, moves and pins an agterm tab only when the session's own
  process is in its foreground.
- **tmux, tested against a real one** (in a container; still experimental). A
  window is idle only when nothing but a waiting shell, or a finished command,
  is in it. Workspace names and folders are never expanded or run by tmux, and
  a name it cannot take is refused. A window moved, renamed, linked or changed
  by hand is left open, and qm says so. A socket that may not be used is an
  error.
- The launch gate also finds a launch after a shift in arithmetic, or after a
  here-document in a substitution that bash ends early.
- A hook whose input never ends gives up at its time limit.
- The release scripts check the version and submission name they are given and
  check signatures before running a binary; CI actions are pinned by commit.
- **A coordinator started by a version file** of Claude Code's installer
  (`.../claude/versions/<version>`) is recognised, so its workers get its
  permission flag. One run by `node`, or by the npm package's own binary, is
  not: its workers prompt, as before.
- **Inherited variables no longer steer git.** A `GIT_*` variable in qm's
  environment does not decide which repository git answers for, or whether a
  folder counts as trusted. `qm up` asks GitHub about a worker's branch only
  in the worker's own repository, whatever `GH_REPO` says.
- **Control characters and line breaks** in names, folders and titles no
  longer reach the terminal or a notification.
- A project state file whose folder does not match its id is refused as
  unreadable, with the recovery; `qm up` refuses a project folder whose name
  is not valid UTF-8; `qm report` refuses positional arguments.
- Terminal ids are read only in their own shape and only from a process's
  environment; a session id with glob characters finds no other session's
  transcript; an over-long line in a Codex rollout is skipped, and a rollout
  whose account cannot be told gives no usage reading.

## 0.2.2

- **The coordinator in the tab `qm up` runs in.** `qm up --here` starts the
  workers first, then replaces itself with the coordinator in the current
  tab, and records that tab so `qm down` closes it last. `--new-tab` keeps a
  new tab for one run, and `up.coordinator_tab` (`new` or `here`) sets the
  default. Without a terminal, `here` from the config falls back to a new
  tab with a note. In a terminal that is neither agterm nor tmux the
  coordinator still starts in place, so `--here` also works with no
  launcher.
- **Ending a session from the coordinator.** Tell the coordinator to wrap up
  and run `qm down`: it writes its handoff note, then runs `qm down --yes`,
  which closes the workers' tabs and then its own.
- **Workers hand off later.** A worker is told to finish its step and write
  a handoff note at 85% of its compact window, up from 70%, which stopped
  workers that were close to done. `worker.handoff_percent` sets the share,
  and `qm status` shows it.
- `qm down` leaves an agterm tab open when it is split into several panes:
  closing it would end what runs beside the session.
- A lone `--` among the coordinator's claude arguments is refused: it would
  end claude's options before the ones `qm up` sets.
- `qm up --dry-run` says where the coordinator would start.

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
