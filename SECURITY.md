# Security policy

## Reporting a vulnerability

Report it through GitHub's private vulnerability reporting on the
quartermaster repository: open the **Security** tab of
`github.com/IZZY-Agency/quartermaster`, then **Report a vulnerability**. Do not
open a public issue or pull request for it. You get an answer there, and the
fix is prepared in a private advisory until it is released.

Supported versions: the latest release.

## What qm sends where

quartermaster runs on your machine. It has no server of its own and sends no
telemetry. These are the only places where anything leaves it.

| What | Where | When |
|---|---|---|
| Redacted worker briefs, redacted review prompts, the redacted recent conversation and question of the ask gate, and redacted user messages and written rules of the feedback hook | TypeSafe (`api.typesafe.ai`), to be judged by the Jev model | Only when a TypeSafe key is set, and only after the privacy gate has redacted the text and found nothing private left. Text the gate keeps local is never sent. |
| A review prompt | The `codex` CLI on your machine, which talks to OpenAI under your own Codex login | `qm spawn review`, when Codex is installed and its limit has room. It runs read-only (`-s read-only`, `--ephemeral`). |
| A review prompt | The `claude` CLI on your machine, which talks to Anthropic under your own Claude Code login. It runs with reading tools only (`Read`, `Grep`, `Glob`, and Bash for `git diff`, `git log`, `gh pr view` and `gh pr diff`) and no MCP servers; edits, writes and MCP tools are denied, and so is any other call that would ask for permission. Your own Claude Code allow rules and hooks still apply in it. | `qm spawn review`, when Codex is not available, or when a Codex run fails (the same prompt then goes to both). |
| Requests to GitHub, through your own `gh` login | GitHub | When `gh` is installed. `qm up` runs `gh auth status`, `gh pr list --head <branch>` to see whether a worker's branch was merged or closed, and `gh pr list --author @me`, which reads the numbers, titles and branches of your open pull requests for the coordinator's summary. `qm report` runs `gh pr view` on each pull request URL found in your transcripts, to read its size and state. A Claude review may itself run `gh pr view` and `gh pr diff`. |
| The worker name and the rule name, in a message such as `<worker>: read-budget softened after 3 denials` | Your `notify` command, which may send it off the machine | Only when you set `notify` in the config, when a rule softens. |
| A worker's name and state, in a message such as `qm watch: <worker> stopped; handoff note written` or `qm watch: <worker> silent for 10 min`; never a path or the project id | Your `notify` command, which may send it off the machine | Only when you set `notify` in the config, when `qm watch` reports a worker that stopped, went silent or went away. |
| Everything a worker or coordinator session sends | Anthropic, as for any Claude Code session | Always, through Claude Code itself. qm does not change this. |

Without a TypeSafe key, nothing goes to TypeSafe: routing uses the type, size
and hint you give it and a fixed pattern for risky words, and the ask gate and
feedback hook are off. `qm status` shows which providers are on.

The privacy gate redacts secrets, emails, phone numbers, IP addresses other than loopback,
long tokens and keys, your home folder, the client names you list in
`privacy.clients` and, with the local Foundation Model helper, person names. If
a secret cannot be redacted, or the local check finds something private left,
the text stays on your machine and routing falls back to the coordinator's own
pick. See the README for the details.

Redaction is best effort. It lowers what leaves your machine; it does not
replace your judgement about what you put in a brief. Put the names of your
clients and suppliers in `privacy.clients`.

## Where the key and the data are kept

- The TypeSafe key is read from the `TYPESAFE_API_KEY` environment variable
  first. `qm init` never stores a key that came from the environment. A key you
  type into `qm init` is stored in `~/.config/quartermaster/config.json`, a file
  of mode 0600 in a folder of mode 0700.
- Everything qm records is kept in plain text under
  `~/.local/state/quartermaster/`, owner-only, and stays on your machine:
  - the decision log. It records the time, project, branch, folder, task type
    and size, model and effort, reasons and Jev's answers of each decision, and
    also worker names, session ids, the text you give `--why`, the launch
    profile (plugins, MCP config path, environment, permission flag) and the
    details of errors, which for a failed Claude review include its reply. It
    never records the text of a brief or a review prompt;
  - review job folders (`reviews/<id>/`): the full review prompt
    (`prompt.md`), Claude's raw reply (`reply.txt`), the findings
    (`findings.json`) and Codex's JSON log, which can quote your code;
  - the feedback hook's copy of your message (`sessions/<session>-fb-N.txt`),
    unredacted, until the background check has read it;
  - workers' handoff notes, the summaries `qm up` writes for a fresh
    coordinator (`up/`), the project state, launch scripts, the list of
    pending reviews, the Claude limit file and the Codex limit file
    (`limits/codex-limited.json`).
- qm reads these files back as its own. A worker's handoff note, written by
  that worker, becomes part of the first prompt of the session `qm up` starts
  in its place: that is how work is carried over, and it means the note's
  text steers the next session. Names, ids, branches and folders read from
  the state and the decision log are checked before they become file names or
  command arguments, but the folders are only as safe as their owner-only
  permissions: do not loosen them.
- `XDG_CONFIG_HOME` and `XDG_STATE_HOME` move both folders.

## What qm changes outside its own folders

- `qm init` may wrap `statusLine.command` in `~/.claude/settings.json` (only
  after showing the change and asking, or with `--yes`, and after saving a
  backup next to the file). `qm init --undo` puts it back.
- Workers started by `qm spawn worker` get the same permission flag as the
  coordinator that started them, never a stronger one. When qm cannot read the
  coordinator's flag, the worker gets no permission flag and follows your
  Claude Code settings.
- The two Claude Code plugins add hooks that can deny a tool call (a raw
  launch of a worker or a Codex review, reads past a budget) or rewrite a
  model or effort. Each rule has an off switch in the config.
