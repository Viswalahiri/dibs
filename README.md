# Dibs

Dibs watches a handful of GitHub repositories, throws out the issues that are
already spoken for, and pushes the rest to Slack fast enough that claiming one
is still possible.

Dibs holds a read-only GitHub token and never writes to GitHub. Claiming
happens in your browser, by hand.

`PLAN.md` explains what it is and why it is shaped this way. `SETUP.md` is how
to run it, written for someone who has not used Go before.

## Status

Built and running as a systemd user service.

A poller finds new issues, an enricher fetches the thread and applies a
deterministic filter, and whatever survives goes to Slack. A reaper sets each
repository's polling rate from how many issues it actually produces and warns
when one goes dark.

## What Dibs is not

It is a notifier. It has no opinion about which surviving issue is better than
another, it does not rank them, and it does not track what you do next. An
alert arrives, you open it or you don't, and that is the end of the issue as
far as Dibs is concerned.

An earlier version scored every issue with a model and put Track, Skip, and
Snooze buttons on each alert. Scoring only pays off if you tune it, tuning it
needs a week of pressing buttons to build a corpus, and nobody wanted to press
the buttons. The scoring was never filtering anything in practice, so it went,
and the buttons went with it. What remains costs nothing to run and needs no
API key.

## What is a claim

Only two things reject an issue for being taken. A linked pull request means
somebody has written code. A comment saying "taking this" means somebody said
so out loud. Both are people acting.

An assignee is not either of those. Projects hand assignments out by
round-robin, by CODEOWNERS, and by bot, and an assigned issue with no other
activity is usually still open in practice. The alert reports the assignment
and leaves the call to you.

What does count is an assignee arriving after Dibs first recorded the issue.
That is somebody taking it during the window, and the push worker drops the
issue when it sees one.

## What survives the filter

Four checks, and no more. A linked pull request, a killfile label (`wontfix`,
`duplicate`, `invalid`, `stale`), someone else claiming it in the thread, or a
body under 80 characters with no code fence.

`needs-triage` and `blocked` are deliberately missing from the killfile. Both
mean nobody has acted yet, which describes exactly the fresh unclaimed issue
this thing exists to find, and plenty of repositories apply `needs-triage`
automatically.

The rule that keeps the list short: a bad issue reaching Slack costs one click,
and a good issue killed here costs the issue. When in doubt, let it through.

## What the reaper does

Every fifteen minutes it returns rows abandoned by a crashed worker and expires
issues the push worker never reached. In practice the second one only fires if
Slack was unreachable for a fortnight.

Once a day at `reaper.cadence_recompute_hour` it resets each repository's
polling interval from the issues that repository has actually produced, and
warns about any repository that rejected over 90% of a week's issues. That last
one is almost always a new label bot rather than a quiet week, and it is
invisible from the alert stream because the symptom is silence.

The reaper makes no requests. Everything it reads is already in SQLite.

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
| `dibs run` | The daemon. Add `--dry-run` to print alerts to stdout instead of Slack, which needs only the GitHub token. |
| `dibs status --repos` | Each repository's waterline and issue counts by state. |
| `dibs status --today` | Everything notified in the last 24 hours. |

## Adding a repository

A repository joins at the waterline. Dibs records what is currently open,
surfaces none of it, and starts from there. Adding one costs zero enrichment
requests. Past that line an issue must be under `freshness_cutoff_min` minutes
old when Dibs first sees it, or it is recorded and dropped. In steady state
this gate almost never fires, because the poller comes round every 45 seconds
and sees issues that are seconds old. It matters after a gap, when the laptop
was asleep and an hour of issues arrives at once.

`SIGHUP` reloads both config files without a restart.

## What it costs

Nothing. GitHub is free at this volume and Slack's free tier is enough.

An issue costs two GitHub requests to screen, and a repository being watched
costs one conditional request per poll, which a `304` answers without spending
rate-limit quota at all. At four repositories on a 45-second interval that is a
few hundred requests an hour against a limit of five thousand.
