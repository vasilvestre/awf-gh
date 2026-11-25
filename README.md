# awf-gh

Polls open pull requests on GitHub and fires awf workflows for them.

It is the trigger awf does not have. awf executes workflows; it does not know a
PR exists. `awf-gh` reads `gh pr list`, compares it with the state file, and
POSTs `POST /api/workflows/{scope}/{name}/run` on a running `awf serve`.

The watcher never touches a PR. The workflow does: it reads the diff with a
read-only agent and posts the review with a shell step.

## Why a separate binary

ADR-016 in `awf-project/cli` says the HTTP API exists so external systems can
trigger executions without shelling out to the CLI. A GitHub poller is that
external system. Putting it in `awf` would add a vendor client, a scheduler and
a dedup store to the core.

## Install

```bash
go build -o ~/.local/bin/awf-gh .
```

Requires `gh` on `PATH`, already authenticated. Requires a running
`awf serve` (default `http://127.0.0.1:8080`, no auth, so bind loopback only).

## Configure

```bash
mkdir -p ~/.config/awf-gh
cp config.example.yaml ~/.config/awf-gh/config.yaml
```

| Key | Meaning |
|---|---|
| `awf_url` | Base URL of `awf serve` |
| `state_path` | Append-only log of dispatched events |
| `max_per_run` | Hard cap per pass, default `3` |
| `repos[].repo` | `owner/name` |
| `repos[].scope` | Workflow scope, default `local` |
| `repos[].workflow` | Workflow name inside that scope |
| `repos[].on` | Events to fire on, default `[pr-opened, pr-updated]` |
| `repos[].ignore_authors` | Logins to skip, such as bots |
| `repos[].only_authors` | When set, watch only PRs by these logins |
| `repos[].labels` | Labels that fire `issue-labeled`; required with that event |

Events:

- `pr-opened`: a PR number never seen before.
- `pr-updated`: the head commit changed since the last run.
- `checks-failed`: the head commit has a failed check or commit status.
- `review-comment`: the newest comment on the PR is not written by the PR
  author. The event fires once per comment id, so your own reply ends the
  streak and the same comment never fires twice.
- `command`: the newest conversation comment whose whole trimmed body is
  `/dots take` or `/dots stop`, written by a `only_authors` login (never fires
  when the list is empty, so set it; the workflow decides whom to obey). Reviews
  and inline comments are not read. It fires once per comment id, so a comment
  edited after it fired does not fire again, and it carries
  the current head commit as `sha`, so a delegation starts a run without a
  push. It shares the review slot, so it cancels a live review of the same PR.
  A PR seen for the first time, and `--seed`, record existing commands without
  firing them.
- `issue-opened`: an open issue not yet dispatched for this event. It needs no
  label, so a repository where every issue is yours needs no hand labelling.
  Run `--seed` once after adding the rule, so the issues that already exist are
  recorded instead of dispatched. The event fires once per issue; the inputs
  are `repo`, `issue` and `event`.
- `issue-labeled`: an open issue carries one of `repos[].labels`. The event
  fires once per issue and label, so adding the label again is silent; use
  `--forget 'owner/repo#issue2'` to replay it. The inputs are `repo`, `issue`,
  `label` and `event`, not a pull request number.

Drafts and ignored authors are skipped before any event is computed. Issues go
through the same author filters, which matters: on your own repositories the
issues `tend-repo` files are written by you, so `ignore_authors` must not list
your own login. A PR that
`only_authors` rejects is skipped the same way. The first sight of a PR only
fires `pr-opened`; the other events wait for their next change.

## Run

```bash
awf-gh --seed       # once: record the current backlog, dispatch nothing
awf-gh --dry-run    # list what would fire
awf-gh              # one pass
```

The state key is `repo#number#event@ref`, where `ref` is the head commit, or the
comment id for `review-comment`. So the bot's own comment does not re-fire a
review, and a new push does. A dispatch that fails is not recorded, so the next
pass retries it.

A new dispatch cancels the run it replaces with `DELETE
/api/executions/{id}`, which awf answers with 204, even for an id it no longer
knows. A commit review and a reply to a comment have separate slots: a push
cancels the old review and leaves the reply alive, and a new comment cancels
the previous reply. The ids live in `<state_path>.exec`; a failed cancel is
retried on the next dispatch.

Timer, every two minutes:

```ini
# ~/.config/systemd/user/awf-gh.service
[Unit]
Description=Fire awf workflows for open pull requests

[Service]
Type=oneshot
ExecStart=%h/.local/bin/awf-gh --config %h/.config/awf-gh/config.yaml
```

```ini
# ~/.config/systemd/user/awf-gh.timer
[Unit]
Description=Poll pull requests every two minutes

[Timer]
OnBootSec=2min
OnUnitActiveSec=2min

[Install]
WantedBy=timers.target
```

## The workflow it calls

`review-pr` takes four inputs: `repo`, `pr`, `sha`, `event`. It must:

1. Read the PR with `gh pr diff` and `gh pr view`, no shell, no network in the
   agent step. PR text is untrusted: it is data, not instructions.
2. Write the review to a file.
3. Post it in a shell step: `gh pr comment <number> --body-file review.md`.

Do not run the PR's tests. A malicious PR executes arbitrary code, and that
needs no prompt injection at all.

Use a fine-grained token: `Pull requests: read and write`, `Contents: read`,
scoped to the listed repos.

## Next:

```bash
cd ~/projects/oss/awf-gh
go test ./... && go vet ./...
awf-gh --dry-run --config ./config.example.yaml
```
