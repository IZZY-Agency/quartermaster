<p align="center">
  <a href="https://github.com/IZZY-Agency/quartermaster/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/IZZY-Agency/quartermaster/ci.yml?branch=main&amp;label=CI" alt="CI"></a>
  <a href="https://github.com/IZZY-Agency/quartermaster/releases/latest"><img src="https://img.shields.io/github/v/release/IZZY-Agency/quartermaster?label=release&amp;color=f5c518" alt="Latest release"></a>
  <a href="https://github.com/IZZY-Agency/homebrew-tap"><img src="https://img.shields.io/badge/brew-izzy--agency%2Ftap%2Fquartermaster-f5c518" alt="Homebrew formula"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/IZZY-Agency/quartermaster?color=f5c518" alt="MIT licence"></a>
</p>

# quartermaster

Right-size every Claude Code agent before it starts.

quartermaster (`qm`) is for people who run a coordinator Claude Code session
that spawns worker and reviewer sessions. Before each spawn it picks the model
and effort the task needs, so small tasks stop running on the biggest model and
your usage limits last longer without losing quality. It also keeps private text
on your machine, sends reviews to Codex while your Codex limits last and to
Claude after, and logs every decision so you can check it against what really
happened.

[Releases](https://github.com/IZZY-Agency/quartermaster/releases) ·
[Changelog](CHANGELOG.md) ·
[Issues](https://github.com/IZZY-Agency/quartermaster/issues)

Examples in this file use placeholders: Example Corp (a client), `<email>`,
`<repo>` (the path of a repository), `<key>` and `ClientA`. Replace them with
your own values.

## Why

The numbers below are aggregates measured on one developer's own history before
quartermaster existed: 249 Claude Code worker sessions and 1,051 Codex sessions
over 40 days. Costs are real tokens at API list prices, a yardstick for how fast
limits drain, not what a plan bills.

- 67% of what workers cost was cache reads: long sessions re-reading their
  growing context. About 72% of the re-reads that tool output caused was
  reading code.
- Almost every worker was spawned on the biggest model at high effort,
  whatever the task. Small tasks were cheap in total, but each one was overpaid.
- The fixed start block of a worker (system prompt, instructions, plugins, MCP
  tools, the brief) was about 63k tokens, 16% of all the context tokens
  processed.
- 221 of 418 Codex reviews ran at the highest effort (xhigh), and about 70%
  of them started Codex sub-agents of their own.

quartermaster acts on each of these: it picks the model and effort from the task,
starts workers with a lean profile, pushes code reading into a cheap explorer
agent, and sets an explicit model and effort on every review.

## Install

With Homebrew (macOS and Linux):

```sh
brew install izzy-agency/tap/quartermaster
```

From source, with Go 1.27 or newer. On a Mac, `swiftc` (the Xcode command-line
tools) is needed only for the optional privacy helper; without it `make install`
installs `qm` and says the helper was skipped.

```sh
git clone https://github.com/IZZY-Agency/quartermaster.git
cd quartermaster
make build install
```

`make install` copies `qm`, the helper (macOS) and the worker plugin into
`~/.local` by default; use `make install PREFIX=<folder>` for another place.
`<PREFIX>/bin`, by default `~/.local/bin`, must be on your `PATH`; it is not in
a stock macOS shell. Add it with `export PATH="$HOME/.local/bin:$PATH"` in your
POSIX shell's profile, or `fish_add_path ~/.local/bin` in fish. The plugins do
nothing while `qm` is not on your `PATH`.

quartermaster needs the `claude` command (Claude Code). Everything else is
optional (see [Optional providers](#optional-providers)).

## Quick start

1. Run the setup. It writes the config, reports which providers it found, asks
   for an optional TypeSafe key, and offers to wrap your Claude Code status line
   (it shows the change, asks, and saves a backup first):

   ```sh
   qm init
   ```

   Run it again after you install `qm` another way (from source, then from
   Homebrew): it offers to repoint a status line that still runs the earlier
   `qm`, and `qm status` says so when that one is gone.

2. Inside Claude Code, add this repository as a plugin marketplace and install
   the coordinator plugin (`qm init` prints these two lines; a `qm` you built
   yourself and run from the checkout's `bin/` prints the checkout's path in
   the first line instead, which works the same):

   ```
   /plugin marketplace add IZZY-Agency/quartermaster
   /plugin install qm-coordinator@quartermaster
   ```

   The worker plugin, `qm-worker`, needs no install: `qm spawn worker` loads it
   into each worker.

3. Check what is on and off, and why:

   ```sh
   qm status
   ```

4. See what a spawn would do, without launching anything. Put a task brief in
   `brief.md` and run it from your coordinator's terminal:

   ```sh
   qm spawn worker --dry-run --brief brief.md --type docs --size S \
     --hint sonnet/low --name example-docs --cwd <repo>
   ```

   It prints the decision (model, effort, the table row and the reasons) and the
   launch script, and starts nothing. `qm route worker` and `qm route review`
   print only the decision.

From then on the coordinator spawns workers with `qm spawn worker` and runs
reviews with `qm spawn review`; the plugin denies the raw ways of doing both and
says what to run instead.

To give qm a TypeSafe key without storing it, set it in the environment before
`qm init`: `export TYPESAFE_API_KEY=<key>` in a POSIX shell, `set -x
TYPESAFE_API_KEY <key>` in fish.

## How decisions are made

Code decides, from facts; Jev, TypeSafe's model, only answers typed questions
(is this brief mostly mechanical? does it change authentication or payments?).
The tables, thresholds and question wording live in one place in the source,
`internal/policy`.

### Workers

`qm spawn worker` takes the brief, a `--type` (`feature`, `fix`, `fix-pass`,
`docs`, `release-paperwork`, `research`, `migration`, `ui` or `review`), a
`--size` (`S`: one or two files or under about 50 lines; `M`: one feature in one
area; `L`: several areas or systems) and `--hint`, the coordinator's own pick as
`model/effort`. A worker of `--type review` is routed with the review table
below (its Claude column), in `qm spawn worker` and `qm route worker` alike. For
every other type, the first row of this table that matches wins:

| # | Condition | Model / effort |
|---|---|---|
| 1 | `--force m/e --why TEXT` | what you forced; the reason is logged |
| 2 | the privacy gate kept the brief local, or it or Jev failed | the hint, lowered to at most opus/high (no hint: opus/high) |
| 3 | research (Jev's role, or `--type research`) | sonnet/medium; opus/medium if the design is left open |
| 4 | risk, and size L or the design left open | opus/xhigh |
| 5 | risk | opus/high |
| 6 | `--type ui`, or visual quality is central | opus/medium |
| 7 | size S, and docs, release-paperwork or mostly mechanical | sonnet/low |
| 8 | size S | sonnet/medium |
| 9 | the design left open, or an unknown root cause to find | opus/medium |
| 10 | size L | opus/medium |
| 11 | size M | sonnet/high |

**Risk** is true when the brief names authentication, access control,
credentials or secrets, payments, deployment, migrations or exposure (a fixed
word pattern, checked by code, which skips standing boilerplate such as "do not
deploy"), when the type is `migration`, or when Jev's answer to "does this
change security, access or payments?" or "does it act on a live system or real
data?" is above 0.3. Jev answers on a 0 to 1 scale: 0.7 and above is yes, 0.3
and below is no, and anything between counts as risk. "Design left open",
"unknown root cause" and "visual quality is central" are yes at 0.7 and above.
A brief too long for one request is split into sections and the answers are
combined on the safe side.

Fable is never picked by the table; it is only available with `--force`.

**Downshift.** When your Claude 5-hour usage is known and above 90%, a decision for a brief
without risk, that is not forced, not local and not the `ui` row, moves one step
down this ladder: opus/medium, sonnet/high, sonnet/medium, sonnet/low. The
usage comes from the status-line wrapper (`qm statusline`); a limit file older
than an hour counts as unknown, and nothing is shifted.

**Launch profile.** `qm spawn worker` starts the worker with `--model` and
`--effort` from the decision; `--autocompact` set to 250,000 tokens for Sonnet
and 400,000 for Opus and Fable (`worker_window`); only the MCP servers in the
config file `project_mcp` names for the repository (default none); plugins limited to `qm-worker`,
the official LSP plugin for the repository's main language when it is installed,
and the ones you list; Bash output capped at 20,000 characters and file reads
at 12,000 tokens; and the coordinator's own permission flag (bypass for bypass,
the same `--permission-mode` for a mode, nothing for nothing, never a stronger
one). The decision is appended to the decision log.

### Reviews

`qm spawn review --prompt FILE` routes a review. By default the round (1 for a
first review, 2 and up for a re-check) is counted from qm's decision log for the
branch, and the size of the diff from `git diff --numstat BASE...HEAD`; `--round`
and `--base` override them. Both are read for the branch checked out in the
folder qm runs in (`--repo` names another), so a review of a branch that lives in
a worktree runs there; when that branch has no diff against its base, qm prints a
note. The first row that matches wins:

| Condition | Codex | Claude, when Codex is off or fails |
|---|---|---|
| round 2 or more (a fix check) | gpt-6.2-sol / medium | sonnet / medium |
| risk, diff over 1,000 lines | gpt-6.2-astra / xhigh | opus / xhigh |
| risk | gpt-6.2-sol / xhigh | opus / high |
| diff under 300 lines | gpt-6.2-sol / medium | sonnet / high |
| larger diff | gpt-6.2-sol / high | opus / medium |

When Codex is not available, the review goes to Claude. When a Codex run fails
after it started, the same prompt goes to Claude too, so it reaches both.

Risk for a review is the same word pattern over the prompt and the changed
paths, or Jev's answer above 0.3 to "does the change touch security, access,
secrets, payments or exposure?" or "could a bug corrupt real data or break a
live system?" (asked on a first review only).

Codex is available when the `codex` command is installed, its latest primary
limit is under 95% used, and no usage-limit error is still in effect; both are
read for the account Codex is logged in to, so switching accounts takes effect
at once. The limit is read from the sessions Codex saves when you use it
yourself; qm's reviews save none, so an account only qm uses counts as
available until a review hits the limit. Paid
credits do not count: when the limit is reached, reviews go to Claude even if
credits remain. Every Codex call carries an explicit model and effort, a
read-only sandbox and a findings schema; a Claude review runs `claude -p` with
reading tools only (`Read`, `Grep`, `Glob`, and Bash for `git diff`, `git log`,
`gh pr view` and `gh pr diff`) and no MCP servers: edits, writes and MCP tools
are denied, and in its `dontAsk` permission mode any other call that would ask
for permission is denied too. Your own Claude Code allow rules and hooks still
apply in it. The Claude column moves one tier down above 90% of the
5-hour limit, except on the risk rows.

`qm spawn review` starts the review in the background and prints its id; add
`--wait` to wait and print the findings (`verdict`, and per finding its
severity, file, line, scenario and fix), or run `qm spawn review --status ID`
later. A review that takes longer than 45 minutes fails.

### Without Jev

With no TypeSafe key, every Jev question is read as "no" (not as uncertain, which
would send every task to opus/high), and the type, size and hint stand in:
`docs` and `release-paperwork` count as mechanical, `research` as the research
role, `ui` as visual, and on size M or L a hint of opus/medium or above leaves
the design open. The code risk pattern still applies. The decision's reasons say
"Jev off (no TypeSafe key)". Reviews use the round, the diff size and the risk
pattern.

### The plugins' rules

`qm-coordinator` (installed by you) and `qm-worker` (loaded into workers) enforce
these rules. Each has a switch in `rules` of the config: `on`, `note` (say it,
do not block) or `off`; a rule not listed is on.

| Rule | Plugin | What it does |
|---|---|---|
| `raw-launch-gate` | both | denies a raw `codex` run (`exec`, `review` or a prompt), a worker started by hand in agterm or tmux, or a raw `claude` launch, also behind wrappers such as `env`, `sudo`, `xargs` or `eval`; says "use qm spawn". It is a guard rail for a cooperating session, not a sandbox: a command inside an interpreter's string (`python3 -c`) is not read |
| `reviewer-model` | both | sets the model and effort of a review sub-agent that has none |
| `ask-gate` | coordinator | denies a question to you that Jev judges answerable from the context, cheap to undo and not a product ruling (needs a TypeSafe key) |
| `feedback`, `feedback-deliver` | coordinator | when your message states a lasting rule no written rule covers, tells the coordinator to save it; never saves anything itself (needs a TypeSafe key) |
| `read-budget` | worker | denies the next exploration read past `read_budget` in one phase: a `Read` of a whole file or of more than 80 lines, any `Grep`, or a Bash command that starts with `cat`, `sed`, `grep`, `rg`, `head`, `tail` or `find` (a `sed -n` range of up to 80 lines does not count), telling the worker to use the explorer agent; a write or edit starts a new phase |
| `explorer-model` | worker | runs explorer sub-agents on Haiku |
| `lsp-note` | worker | notes that the LSP tool is there when a search looks like a symbol lookup |
| `write-note` | worker | notes that `Edit` is for changes to a large existing file |
| `context-limit` | worker | at 85% of the compact window (`worker.handoff_percent`), tells the worker to finish the step, write a handoff note and send the coordinator a message |
| `turn-start`, `turn-stop` | worker | record, silently and in their own files, whether the worker's turn is open, for [qm watch](#watching-workers-qm-watch); they act only in workers (`QM_ROLE=worker`) |

The `read-budget` and `raw-launch-gate` rules, after blocking work three times
in one phase, soften to a note for the rest of the phase or 30 minutes,
whichever ends first; the softening is logged and sends a message through your
`notify` command if you set one. The `ask-gate` never softens. `qm report` lists the softenings.

## Privacy

Redact first; once redacted, text may leave. Before any text goes to TypeSafe,
the privacy gate:

- replaces secrets (found by gitleaks, when installed, and by patterns for
  tokens and keys), emails (except the Anthropic no-reply address), phone
  numbers, IPv4 and IPv6 addresses other than loopback, and tokens (long
  high-entropy or hex runs other than a 40-character commit id, and keys
  with well-known vendor prefixes) with placeholders
  such as `<SECRET_1>`, `<EMAIL_1>`, `<PHONE_1>`, `<IP_1>` and `<TOKEN_1>`;
- turns your home folder into `~`;
- replaces each client or supplier name you list in `privacy.clients` with a
  stable pseudonym (Example Corp becomes `ClientA` everywhere);
- on a Mac with the helper, finds person names with Apple's name detector,
  confirms each with the on-device Foundation Model, and replaces the confirmed
  ones with `<PERSON_1>`; then asks the same model, over the redacted text,
  whether anything private is left.

If a secret cannot be redacted whole, or the model says something private is
left, the text stays on your machine: routing uses the coordinator's hint, the
ask gate lets the question through, and the feedback hook tells the coordinator
the message was not checked. Redaction is best effort; list your
clients and suppliers in `privacy.clients`, especially on Linux, where names are
not detected.

**What reaches TypeSafe**, only with a key and only redacted: a worker brief, a
review prompt, the recent conversation and the question of the ask gate, your
message and the written rules the feedback hook compares it with.

**What never leaves your machine:** unredacted text; text the gate keeps local;
the config; the decision log (it records project, branch, folder, type, size,
tier, reasons and Jev's answers, worker names, session ids, `--why` text, the
launch profile and error details, never the text of a brief); the rest of the
state folder, which keeps in plain text the review jobs (the full prompt,
Claude's raw reply, the findings, Codex's log), the feedback hook's copy of your
message until it is checked, handoff notes and `qm up` summaries. Reviews go to
the `codex` and `claude` commands on your machine, which talk to their own
services under your own logins. [SECURITY.md](SECURITY.md) lists everything
that leaves your machine.

`privacy.local_model` sets how the Foundation Model check is used: `auto` (the
default) uses it when it is available, `required` keeps text local whenever it
is not available or fails (`qm init` sets this when it finds the model), and
`off` never uses it.

## Configuration

The config is `~/.config/quartermaster/config.json` (mode 0600, in a folder of
mode 0700; `XDG_CONFIG_HOME` moves it). A key you leave out keeps its default.
`qm init` creates it. A later `qm init` adds the keys that are missing, and
also sets `privacy.local_model` to `required` when it finds the local model,
asks before adding the superpowers plugin to `worker.plugins` when you have it
enabled, and stores a TypeSafe key you type in.

```json
{
  "privacy": {
    "clients": [
      { "pattern": "example[\\s-]?corp", "replace": "ClientA" }
    ],
    "local_model": "auto"
  },
  "worker": { "plugins": ["superpowers@claude-plugins-official"] },
  "rules": { "read-budget": "note" },
  "read_budget": 8,
  "launcher": "auto",
  "notify": ["tg", "send", "--level", "info", "--title", "quartermaster"],
  "up": { "claude_args": ["--permission-mode", "acceptEdits"] }
}
```

(`example[\s-]?corp` and `ClientA` are placeholders, and so is the notifier:
the `tg` command in it comes from
[claude-tg](https://github.com/IZZY-Agency/claude-tg), which sends Claude Code
messages to your phone over Telegram; any command works.) A
bypass flag in `up.claude_args` would start every coordinator, and so every
worker, without permission prompts.

| Key | Default | Meaning |
|---|---|---|
| `privacy.clients` | `[]` | list of `{"pattern": REGEX, "replace": PSEUDONYM}`; a name matching the regex (case-insensitive; an invalid regex is read as plain text) is replaced by the pseudonym before text leaves |
| `privacy.local_model` | `"auto"` | `auto`, `required` or `off`; see [Privacy](#privacy) |
| `worker.plugins` | `[]` | plugins kept on in every worker, as `name@marketplace` (all others are switched off) |
| `project_plugins` | `{}` | extra plugins to keep per repository: `{"<repo>": ["name@marketplace"]}`, keyed by the repository's absolute path (its git top level) |
| `project_mcp` | `{}` | an MCP config file for a repository's workers: `{"<repo>": "<path to a .mcp.json>"}`; without one, workers get no MCP servers |
| `rules` | `{}` | a rule's switch, `on`, `note` or `off`; see [The plugins' rules](#the-plugins-rules) |
| `read_budget` | `8` | exploration reads a worker may do in one phase |
| `worker.handoff_percent` | `85` | the share of its compact window at which a worker is told to finish its step, write a handoff note and report; a whole number from 1 to 99 (another value is shown by `qm status` and the default is used). Lower means shorter, cheaper sessions and more handoffs |
| `worker_window` | `{"sonnet": 250000, "opus": 400000, "fable": 400000}` | each worker model's auto-compact window, in tokens |
| `typesafe_api_key` | unset | the TypeSafe key; the `TYPESAFE_API_KEY` environment variable wins, and `qm init` never stores a key it read from there |
| `launcher` | `"auto"` | `auto`, `agterm` or `tmux` |
| `notify` | `[]` | a command that receives a message as its last argument (as `<title>: <text>`) when a rule softens or `qm watch` reports a worker (its name and state, never a path); no shell is used |
| `report.worker_markers` | `[]` | extra regexes that mark a session as a worker for `qm report --baseline`, next to the built-in ones |
| `up.claude_args` | `[]` | `claude` arguments for a new project's coordinator when `qm up` gets none after `--` |
| `up.coordinator_tab` | `"new"` | where `qm up` starts a coordinator: `new` (a new tab) or `here` (the tab `qm up` runs in, as `--here`; without a terminal, a new tab with a note); `--here` and `--new-tab` win over it |
| `watch.silent_minutes` | `10` | minutes without a write to a worker's transcript after which `qm watch` and `qm status` call it silent; a whole number of at least 1 (`0` or a negative number is an error `qm watch` and the Workers section show; a value of the wrong type, like any key's, fails the whole config) |

`launcher: auto` picks agterm when `AGTERM_SESSION_ID` is set and tmux when
`TMUX` is set; `qm up` also tries a running agterm, then a `tmux` command. With
neither, `qm spawn worker` stops with an error that names both and this key.

The tmux launcher is experimental: it is covered by the automated tests, and
agterm is the launcher qm is used with day to day. Report what breaks.

State, the decision log and everything else qm records live in
`~/.local/state/quartermaster/` (`XDG_STATE_HOME` moves it).

## Optional providers

Every outside provider is optional. A missing one is a state, not an error: its
feature turns off, and `qm status` shows it as off with the reason. A provider
that is present but fails at run time is logged, hooks let the action through,
and routing falls back as described above.

| Missing | What changes |
|---|---|
| TypeSafe key | no Jev questions; routing uses type, size, hint and the risk pattern; the ask gate and feedback hook are off |
| `codex` | every review goes to Claude |
| Foundation Model helper (`qm-privcheck`; macOS only) | privacy check is redaction and gitleaks only: on Linux names are not detected; with `local_model: required`, text stays local instead |
| `gitleaks` | secrets are found by patterns only |
| agterm and tmux | `qm spawn worker` stops with a clear error before launching; routing and reviews still work |
| status-line wrapper | no Claude limit state, so no downshift |
| `notify` | softenings are logged and listed by `qm report`, with no message |
| the official LSP plugin | no LSP plugin is loaded in workers |
| `gh` | `qm report` and `qm up` cannot read pull-request state, and say so themselves (`qm status` does not list `gh`); `qm up` falls back to git to see whether a branch is merged |

## Projects: qm up and qm down

`qm up` starts a project's coordinator and, after a restart of your machine
or terminal, restores the coordinator and its workers. A project is the git
top level of the current folder (the folder itself outside git; a linked
worktree is a project of its own).

```sh
qm up              # first run: start the coordinator; later runs: restore
qm up --dry-run    # print the plan, change nothing
qm up --full       # resume every unfinished session in full
qm up --adopt      # first run: take over the sessions already running here
qm up --here       # start the coordinator in this tab, after the workers
qm up done NAME    # mark a worker finished; it is never restored
qm down            # record the state and close the project's sessions
```

**First run.** `qm up` opens a workspace named after the folder and starts
the coordinator in it. Arguments after `--` are the coordinator's own claude
arguments (for example its permission flag); they are kept for later runs, and
a later `qm up -- ...` replaces them. With none, `up.claude_args` of the config
is used. The folder must be one Claude Code already trusts. Workers the
coordinator spawns with `qm spawn worker` open in the same workspace and are
tied to the project. The coordinator's plugin tells it to run `qm up done NAME`
when it accepts a worker's report as finished (in a coordinator started by
`qm up`).

**The coordinator in this tab.** `qm up --here` starts the coordinator in the
tab you ran it from, in place of a new one: qm starts the workers first,
saves the state and prints its summary, then replaces itself with the
coordinator, so the tab ends up running it with nothing of qm left behind. In
agterm the tab is moved into the project's workspace and pinned; it must not
be split. In tmux the window must have that one pane, and is renamed after the
coordinator. qm records the tab only when it sees qm itself running in it, so
`qm down` closes it last. A tab another project or another session holds is never taken: the
coordinator still starts here, with no tab recorded, and a note says why
(that is also what happens in a terminal that is neither agterm nor tmux).
`--here` needs a terminal, and only goes with a run that starts a coordinator:
a first run, or a restore that resumes or restarts it; next to a coordinator
that is still running, or with `--adopt`, it is refused. Set
`up.coordinator_tab` to `here` to make it the default; `--new-tab` overrides
it for one run, and without a terminal (a script, another Claude session) the
config's `here` falls back to a new tab with a note. `--dry-run` says which
tab the coordinator would start in.

**Ending a session.** Tell the coordinator to wrap up and run `qm down`: it
writes its handoff note first, then runs `qm down --yes`, which closes the
workers' tabs and then its own.

**Later runs.** `qm up` reads its own records, Claude Code's session files and
transcripts, and git (and `gh`, when installed), shows one row per session and
asks before it starts anything (`--yes` skips the question). It needs `ps` to
tell which sessions are still running. In short, for each session, the first
row that matches wins:

| Session | What qm up does |
|---|---|
| still running | leave it alone (never a second copy of a live session) |
| worker finished (marked done, branch merged or closed, worktree gone) | skip |
| folder not trusted by Claude Code, or unknown | skip, and say what to do |
| transcript missing (or unreadable) | start fresh from the handoff note (worker) or the summary (coordinator); a worker with no handoff note is skipped |
| `--full` | resume in full |
| prompt cache still warm (last activity within 55 minutes) | resume |
| context up to 150k tokens (worker) or 200k (coordinator) | resume |
| larger context | start fresh from the handoff note or the summary; a worker with none is resumed in full |

A fresh coordinator starts from a summary qm writes: the workers, the
previous coordinator's handoff note, your last messages and the git state.
Resuming a cold session writes its whole context to the cache again, which is
why the plan shows the cost before you answer.

**qm down.** Records each session's context and last activity in the decision
log, saves the state, then closes the workers' terminal sessions and the
coordinator's last, each only while it is still in the project's workspace or
tmux session. `--keep-open` records and closes nothing; `--yes` skips the
question. A session you already closed by hand is not an error, a tab that
no longer runs its session and now runs something else is left open, and so is
an agterm tab split into several panes (closing it would end what runs beside
the session). Coordinators
are told not to run `qm down` themselves unless you ask.

**Adopting running sessions.** When the first `qm up` finds Claude sessions
already running in the project folder or its git worktrees, it does not start a
second coordinator. It lists them and asks which one is the coordinator and
which are workers, shows what it will record, and asks to confirm. Nothing is
restarted and nothing is typed into any session; a session you do not pick
stays yours and qm never touches it.

- `qm up --adopt` skips the questions: the coordinator is the session that runs
  the command (a coordinator can adopt itself), else the only session in the
  project root that qm did not spawn as a worker; every worker started with
  `qm spawn worker` is adopted too. `--adopt-coordinator ID` names the
  coordinator by its session id or a unique prefix.
  With nothing to adopt, `--adopt` fails; it never starts a new coordinator.
- `qm up --new` starts a new coordinator beside the running sessions and adopts
  nothing.
- With `--yes`, or without a terminal, and neither flag, qm refuses and lists
  the sessions: a script never starts a second coordinator by accident.
- The coordinator must run in the project root; a session running in a
  subfolder is listed but cannot be adopted. In agterm the project's workspace
  is the one the coordinator's tab is in, and adopted workers in other
  workspaces are moved into it (a move restarts nothing). qm records a tab for
  a session only when agterm shows that session's own claude process in it,
  and never a tab a project already records for another session.
- A worker you started by hand keeps its own settings: on a later restore it is
  resumed with its `--model`, `--effort` and permission flags only (never more
  permission than the coordinator has: its saved arguments, lowered to the
  flags of its own process while it runs; a worker whose mode has nothing in
  common with the coordinator's, such as `plan` against `dontAsk`, is not
  started) and is never started fresh.
- Until its next restart, an adopted coordinator has no project variables in
  its environment. `qm spawn worker` run by the coordinator, or by a session
  the project already holds, finds the project from the folder (a spawn from
  any other session stays untied), `qm up done` and `qm down` find it the same
  way, and `qm up handoff`, for the coordinator only, prints where it writes
  its handoff note.
- **Later.** `qm up --adopt` on a project that already has state adds sessions
  you started by hand since: the running ones in the project's folders that the
  project does not hold (its coordinator, adopted sessions, and the workers it
  has recorded, finished ones included). It adopts workers only and restores
  nothing. In a terminal it lists them and asks which (row numbers, `a` for all
  the rows, `q` for the ones `qm spawn worker` started, empty for none), then
  asks to confirm. With `--yes`, or without a terminal, it adopts only the
  sessions a worker Record names and lists the rest as not adopted, so a script
  never takes over a session nobody named. `--adopt-coordinator`, `--new`,
  `--full` and coordinator arguments after `--` do not go with it. A plain
  `qm up` prints one line when such sessions are running, pointing at
  `qm up --adopt`.
- Two adoptions never take the same session: after the confirmation each one
  checks again, under one lock for the machine, which sessions other projects
  hold, and leaves those with a note. When the decision log cannot be
  written, an adoption saves nothing and moves no tab.

**Terminals.** In agterm, qm pins every session it starts to a plain shell on
agterm's own restore, so `qm up` is the only thing that restores a session.
After an agterm restart, `qm up` resumes each session inside the tab agterm
brought back, so tabs keep their place; a tab that is busy with something else
is left open and a new one is opened. It checks the tab again right before it
types, and clears the line first. qm never closes a tab by its name.
In tmux, the project is a tmux session named after the workspace, with one
window per Claude session; outside tmux it is created detached and qm prints
the `tmux attach` line. With neither agterm nor tmux (the `launcher` setting
and `--launcher agterm|tmux` choose one), qm writes a launch script per
session and prints one line to run in a terminal of your choice; the new
session ids are saved with no terminal session, so the next `qm up` knows
them and never starts them twice.

**State.** One file per project, `~/.local/state/quartermaster/projects/<id>.json`
(mode 0600), with a lock file next to it: a second `qm up`, `qm up done` or
`qm down` for the same project fails at once. The file holds no worker list;
workers come from the decision log. `qm up` and `qm down` never write Claude
Code's own files.

## Watching workers: qm watch

A worker that ends its turn without a message (it handed off at its context
limit, asked a question, crashed) leaves the coordinator waiting. `qm watch`
ends when that happens, so a coordinator runs it in the background and its
exit wakes it; the `qm-coordinator` spawning skill says to, and
`qm spawn worker` prints `watch it: qm watch` after each launch.

The workers are those of the project the working folder belongs to (found
the way `qm up done` finds it): the ones `qm up` would restore, without the
ones marked done or replaced. Each has a state:

| State | When |
|---|---|
| `gone` | no live Claude process runs its session (a worker spawned less than 2 minutes ago counts as `working` until its process shows) |
| `stopped` | its turn ended (the `turn-stop` hook of this very process recorded it) |
| `silent` | the process lives, but nothing was written to its transcript, or its sub-agents' transcripts, for `watch.silent_minutes` (10) or longer; a long tool call looks the same, and so does a worker interrupted with Esc (an interrupt fires no stop hook) |
| `working` | otherwise |
| `unknown` | the process list could not be read |

A worker without the `qm-worker` plugin (adopted, started by hand) has no turn
hooks, so it is never `stopped`: it goes from `working` to `silent`. After
`/clear` or a compaction a worker's process runs a new session id; qm follows
it through Claude Code's PID file.

`qm status` lists them after its other sections:

```
Workers (<project id>):
  NAME           STATE     SINCE  CONTEXT  MODEL        FOLDER
  later-adopt-2  working   20s    95k      sonnet/high  .claude/worktrees/later-adopt
  watch          stopped   12m    180k     opus/high    .claude/worktrees/watch
    handoff note: ~/.local/state/quartermaster/handoff/<project id>/watch.md
```

`qm watch` prints the same table, then checks every 15 seconds (`--interval`)
and exits 0 at the first check where it reported a watched worker that is
`stopped`, `silent` or `gone`, with one line per worker it reported (an event
another watch reported first is that watch's; this one keeps waiting); your
`notify` command gets the worker's name and state, without paths:

```
watch stopped 12s ago; handoff note: ~/.local/state/quartermaster/handoff/<project id>/watch.md
later-adopt-2 silent for 10 min
```

Each stop, silence and disappearance is reported once, by the first watch that
sees it, even one that happened before the watch started; a later watch lists
it as `(reported)` and does not wait for it until the worker works again (qm
keeps these marks in `~/.local/state/quartermaster/watch/`). Keep one watch per
project; restart it after dealing with what it reported. Without names it watches every worker;
with names, only those. `--silent` overrides the threshold for one run, and
`--timeout` gives up after a while, exiting 0: it is one deadline for the
whole watch, the marks lock and the notifier included. It runs no daemon and
types into no session.

## Commands

| Command | What it does |
|---|---|
| `qm init [--yes] [--undo] [--marketplace PATH\|OWNER/REPO]` | set up qm, or put back what it changed |
| `qm status [--json]` | what is on and off and why: providers, rules, compact windows, Claude limits, Codex, and the project's workers |
| `qm route worker\|review ...` | print the decision a spawn would make; writes no log |
| `qm spawn worker ...` | route, launch a worker and log it (`--dry-run` launches nothing) |
| `qm spawn review ...` | route, run a review and return structured findings |
| `qm report [--since DAY] [--baseline [--before DAY] [--project TEXT]]` | what workers cost and how their reviews went, by tier and by worker; with `--baseline`, flags rows that did worse than your earlier sessions |
| `qm statusline [-- COMMAND]` | record Claude's usage limits from the status line, then run your own status-line command |
| `qm up`, `qm up done NAME`, `qm down` | see [Projects](#projects-qm-up-and-qm-down) |
| `qm watch [--silent D] [--timeout D] [--interval D] [NAME...]` | wait until a worker of this project stops, goes silent or goes away; see [Watching workers](#watching-workers-qm-watch) |
| `qm version` | print the version and commit |

`qm hook NAME` and `qm feedback-analyze` are run by the plugins; you never run
them. `qm report` reads your decision log and Claude Code's transcripts, and
asks `gh` about each pull request it finds. Run any command with `--help` for
its flags.

## Uninstall

1. Undo what `qm init` changed (the status line, the files it created). The
   decision log is kept:

   ```sh
   qm init --undo
   ```

2. Inside Claude Code: `/plugin uninstall qm-coordinator@quartermaster`.
3. Remove the program: `brew uninstall quartermaster`, or delete `bin/qm`,
   `bin/qm-privcheck` and `share/quartermaster` under your install prefix.
4. To delete the decision log and every other record, remove
   `~/.local/state/quartermaster/` (and `~/.config/quartermaster/` if `qm init
   --undo` kept an edited config).

## Thanks

Special thanks to [agterm](https://github.com/umputun/agterm), the terminal
quartermaster was built in and works best with: its workspaces, session
sidebar and `agtermctl` are what `qm spawn worker`, `qm up` and `qm down`
drive.

## How this repository is maintained

quartermaster is developed in a private repository. Each release is published
here as a single commit tagged with its version, so a release tag always points
at the tree its build came from. Small updates between releases (this README,
CI, tooling) arrive as commits without a tag. Bug reports and feature requests
go in [Issues](https://github.com/IZZY-Agency/quartermaster/issues).

## Licence

[MIT](LICENSE). The third-party notices quartermaster ships with are in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
