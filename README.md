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

Milestones 1, 2, 3, and 5 are built, and Dibs runs as a systemd user service.

A poller finds new issues, an enricher fetches the thread and the author's
history, a deterministic filter throws out the ones already taken, Claude
scores what is left, and survivors go to Slack with Track, Skip, Snooze, and
Why buttons. A reaper then follows what became of the issues you took, sets
each repository's polling rate from how many issues it actually produces, and
reports the few numbers that say whether the scoring is calibrated.

Milestone 4 is the one still open, and it is a week of watching rather than
code. The junk floor and the veto threshold are still the guessed numbers.
`dibs replay`, `dibs status --missed`, and `dibs backfill` are what set them on
evidence, and `make eval` fails until the golden fixture set that week produces
exists.

## Calibration mode

Dibs ships wide open. `junk_floor` is `0` and `veto_confidence` is `1.00`, so
every issue that survives the deterministic filter reaches Slack and no model
veto kills anything. Both numbers still do their arithmetic, and every score is
still recorded, which is the point. A week of running that way produces a
corpus where each row carries a real Track or Skip of yours, and `dibs replay`
sweeps candidate floors and thresholds over it without spending a cent. The
numbers that come out of that are worth committing. The numbers you would pick
today are guesses.

The cost of this posture is a noisier Slack. Expect to skip most of what
arrives during the first week. That is the arrangement working, not a fault,
which is why the skip-rate warning stays quiet while the floor is zero.

Once replay has an answer, set the two numbers and Dibs goes back to being
selective.

## What is a claim

Only two things reject an issue for being taken. A linked pull request means
somebody has written code. A comment saying "taking this" means somebody said
so out loud. Both are people acting.

An assignee is not either of those. Projects hand assignments out by
round-robin, by CODEOWNERS, and by bot, and an assigned issue with no other
activity is usually still open in practice. Assignment costs ten points in
scoring, and the model is shown the assignee list so it can weigh the thread
around it. It never kills the issue on its own.

What does count is an assignee arriving after Dibs first recorded the issue.
That is somebody taking it during the window, and the push worker drops the
issue when it sees one.

## What the reaper does

Every fifteen minutes it returns rows abandoned by a crashed worker and ages
out issues that sat for `expire_after_days` without a decision.

Every six hours it looks at the issues you pressed Track on. It records when
GitHub shows you as the assignee and when a pull request of yours appears, and
when the issue closes it writes the outcome: landed if your pull request was on
it, lost otherwise. All of that is a read. Dibs never assigns you, comments, or
opens anything.

After five days on a tracked issue with no pull request of yours, it asks once
whether the issue is still live. Releasing it is a browser action.

Once a day at `reaper.cadence_recompute_hour` it resets each repository's
polling interval from the issues that repository has actually produced, and
logs the previous day's score distribution, spend, and skip rate. Three of
those readings earn a Slack line rather than a log line, because the symptom is
otherwise silence: a scoring median above 75 for three days running, a skip
rate above half, and a repository that rejected over 90% of a week's issues.

The skip-rate line is the exception while `junk_floor` is `0`. A high skip rate
is what you asked for at that setting, so it stays in the log until a floor is
actually set.

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
| `dibs status --missed` | What the junk floor killed in the last 24 hours. Empty while the floor is `0`, which is the point of running that way. |
| `dibs replay [--config path] [-v]` | Re-scores stored model responses under a candidate config and reports how the push and reject sets move. Makes no API calls. |
| `dibs backfill <owner/repo> --since <dur>` | Scores a repository's recent history into the database and notifies nobody. Use it to build a corpus worth calibrating against, because a week of live running produces only a handful of scored issues. |

Backfilled scores are indicative rather than a replay of what would have been
pushed. An issue is scored as it stands today, comments and assignees and all,
not as it stood in the minute it was opened. Those rows are terminal, so no
worker can pick one up and surface it, and `dibs replay` counts them in its
totals but keeps them out of its change counts, because they carry no Track or
Skip of yours to compare against.

## Adding a repository

A repository joins at the waterline. Dibs records what is currently open,
surfaces none of it, and starts from there. Adding one costs zero enrichment
requests and zero model calls. Past that line an issue must be under
`freshness_cutoff_min` minutes old when Dibs first sees it, or it is recorded
and dropped. In steady state this gate almost never fires, because the poller
comes round every 45 seconds and sees issues that are seconds old. It matters
after a gap, when the laptop was asleep and an hour of issues arrives at once.

`SIGHUP` reloads both config files without a restart.

## What it costs

The only recurring cost is the Anthropic API, and at four repositories it is a
dollar or two a month. `PLAN.md` has the arithmetic. The caps in `dibs.yaml`
exist to bound a bug, not to manage a budget, which is why `daily_call_cap` sits
far above a normal day's traffic.

Opening the junk floor costs nothing extra, because the floor is applied after
scoring. The only change that adds model calls is letting assigned issues
through the filter, and that is a handful a day.

If the cap does trip, triage stops calling the model and stores each remaining
issue at the middle score of 50 so it still reaches Slack. A capped day is a
loud day, not a silent one.
