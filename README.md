# Dibs

Dibs watches a handful of GitHub repositories, throws out the issues that are
already spoken for, ranks what is left, and pushes the good ones to Slack fast
enough that claiming one is still possible.

Dibs holds a read-only GitHub token and never writes to GitHub. Claiming
happens in your browser, by hand.

`PLAN.md` explains what it is and why it is shaped this way. `SPEC.md` is the
build contract. `SETUP.md` is how to run it, written for someone who has not
used Go before.

## Status

Milestones 1 through 3 are built and Dibs runs as a systemd user service. That
unit is M5 work pulled forward, because M4 is a week of watching real issues go
past and the week cannot start until the thing stays up on its own.

A poller finds new issues, an enricher fetches the thread and the author's
history, a deterministic filter throws out the ones already taken, Claude scores
what is left, and survivors go to Slack with Track, Skip, Snooze, and Why
buttons.

Still to come: the junk floor and the veto threshold set on real data rather
than guessed (M4), and the reaper, outcome tracking, and cadence recompute (M5).

## Quick start

```bash
make build
make setup                          # creates ~/.config/dibs/{env,dibs.yaml,repos.yaml}
$EDITOR ~/.config/dibs/env          # paste your tokens
$EDITOR ~/.config/dibs/dibs.yaml    # profile.github_login must match your token
$EDITOR ~/.config/dibs/repos.yaml   # the repositories to watch

set -a; . ~/.config/dibs/env; set +a
./bin/dibs run --dry-run            # alerts to stdout, no Slack app needed
make service                        # once you trust it, run it under systemd
```

`SETUP.md` covers all of this properly, including installing Go, getting each
token, and running Dibs as a background service.

## Commands

| Command | What it does |
|---|---|
| `dibs run` | The daemon. Add `--dry-run` to print alerts to stdout instead of Slack, which needs only the GitHub and Anthropic tokens. |
| `dibs status --repos` | Each repository's waterline and issue counts by state. |
| `dibs status --today` | Everything scored in the last 24 hours. |
| `dibs status --missed` | What the junk floor killed in the last 24 hours. |
| `dibs replay [--config path] [-v]` | Re-scores stored model responses under a candidate config and reports how the push and reject sets move. Makes no API calls. |

## Adding a repository

A repository joins at the waterline. Dibs records what is currently open,
surfaces none of it, and starts from there. Adding one costs zero enrichment
requests and zero model calls. Past that line an issue must be under
`freshness_cutoff_min` minutes old when Dibs first sees it, or it is recorded
and dropped: an issue that has been open for hours has been seen by everyone.

`SIGHUP` reloads both config files without a restart.

## What it costs

The only recurring cost is the Anthropic API, and at four repositories it is
about a dollar a month. `PLAN.md` has the arithmetic. The caps in `dibs.yaml`
exist to bound a bug, not to manage a budget.
