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
| A review prompt | The `codex` CLI on your machine, which talks to OpenAI under your own Codex login | `qm spawn review`, when Codex is installed and its limit has room. It runs read-only (`-s read-only`, `--ephemeral`) and reads the change files in the review folder (the patch, the commit messages, the changed paths and the open pull request's text), whose contents it can send to OpenAI. |
| A request for the usage of your Codex account: qm's name and version, and nothing about you, your project or the review | A `codex app-server` process on your machine, which asks OpenAI under your own Codex login. Of that login qm reads the account id and, from the payload of the ID token, the user id (it opens Codex's `auth.json` for them), to tell logins apart: two members of one workspace have the same account id. It keeps only SHA-256 hashes of them, and keeps and sends none of its tokens. Codex may renew the login, and may start its plugin update, as at any start | Before a review is routed (`qm spawn review`, `qm route review`), before a review is tried again on Codex because its login was switched, and in `qm status`, when Codex is installed; when there is no `auth.json` (a login in a keyring), also before every Codex run of a review and after one that failed; not with `codex.usage_read: false`. Not again for 5 minutes after an answer and 2 after a failure, or sooner when a limit window has reset or Codex is logged in to another account; with no `auth.json`, a review's runner asks however recent the last answer or failure is; when the answer cannot be stored, at each command. It runs in the root folder, outside your projects, and what is left of its process group is ended with it, after 4 seconds at most. |
| A review prompt | The `claude` CLI on your machine, which talks to Anthropic under your own Claude Code login. The prompt carries a short section that names the change files qm wrote (the diff, commits, changed files and, when there is one, the open pull request), and what the reviewer reads of them is sent to Anthropic with the prompt. It runs with reading tools only (`Read`, `Grep` and `Glob`, no Bash) and no MCP servers; edits, writes and MCP tools are denied, and so is any other call that would ask for permission. It loads your user settings only, not the project or local settings of the repository under review, so that repository's settings and hooks do not load; its rule files at the top level (`CLAUDE.md`, `AGENTS.md`, `.claude/CLAUDE.md`, regular files only, reached through no symbolic link) are named in the prompt as files to read, and what the reviewer reads of them is sent with it. Your own Claude Code allow rules and hooks still apply in it. | `qm spawn review`, when Codex is not available, or when a Codex run fails (the same prompt then goes to both, and to Codex a second time first when its login was switched during the run). |
| Requests to GitHub, through your own `gh` login | GitHub | When `gh` is installed. `qm up` runs `gh auth status`, `gh pr list --head <branch>` to see whether a worker's branch was merged or closed, and `gh pr list --author @me`, which reads the numbers, titles and branches of your open pull requests for the coordinator's summary. `qm report` runs `gh pr view` on each pull request URL found in your transcripts, to read its size and state. `qm spawn review` runs `gh pr view <branch>` once, to put the open pull request's number, title and body in the review's folder for the reviewer. A Claude reviewer has no Bash and runs no `gh`. |
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
    (`findings.json`), the change under review as qm read it from git and
    `gh` (`change.diff`, the raw patch; `commits.txt`, the commit messages;
    `files.txt`, the changed paths; `pr.md`, the open pull request's title
    and body) and Codex's JSON log, which can quote your code;
  - the feedback hook's copy of your message (`sessions/<session>-fb-N.txt`),
    unredacted, until the background check has read it;
  - workers' handoff notes, the summaries `qm up` writes for a fresh
    coordinator (`up/`), the project state, launch scripts and a copy of each
    worker's brief (`launch/<session id>.md`, removed after 30 days), the workers
    being started (`spawning/`), the list of
    pending reviews, the Claude limit file, the Codex limit files
    (`limits/codex-limited-<hash>.json`, one for each Codex login that hit its
    limit; the name holds a SHA-256 hash of the account id and, when it is
    known, one of the account and user ids; `codex-limited-unknown.json` for a
    login that could not be told; an earlier version's `codex-limited.json`
    holds the account id itself, and is removed after its reset) and the last Codex usage
    (`limits/codex-usage.json`): for each limit window the percentage used, its
    length and its reset, whether ordinary usage is allowed, when that was
    read, the same hashes (to tell whether Codex still has that login), and the time and kind of the last failure. Its plan,
    credits, tokens and error messages are not kept.
- qm reads these files back as its own. A worker's handoff note, written by
  that worker, is what the first prompt points to in the session `qm up` starts
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
  launch of a worker or a Codex review, reads past a budget, processes ended
  by a pattern in a worker) or rewrite a
  model or effort. Each rule has an off switch in the config.
