# Dibs

Dibs watches a handful of GitHub repositories, throws out the issues that are
already spoken for, and pushes the rest to Slack fast enough that claiming one
is still possible.

This document explains what Dibs is and why it is shaped this way. It is for a
human. The build instructions live in `SPEC.md`, which is what an implementing
agent reads.

## What it is for

I follow a small number of open-source projects. Three or four, realistically,
even though the design tolerates more. When a good issue appears in one of them,
the window to claim it is short. Somebody comments "taking this" and it is gone.

So the job is narrow. Get a genuinely new, genuinely unclaimed issue in front of
me within a couple of minutes, and do not interrupt me for anything else.

Two things follow from that, and they drive every decision below.

**Speed is the product.** If an issue reaches me an hour late, Dibs did nothing.
An hour-old issue was already visible to everyone watching the repo.

**Stale issues cost nothing.** Not a GitHub request, not a Slack line. Anything
that appeared while Dibs was asleep or throttled is recorded and dropped. It is
not worth the ping.

## The one hard rule

**Dibs never writes to GitHub.**

The token is read-only. No comments, no assignment, no reactions, no labels. If
a code path needs write scope, that path is wrong and gets deleted rather than
fixed.

Claiming happens in my browser, by hand. Dibs' whole job is to make sure the
right issue is in front of me while claiming it is still possible.

## Why the scoring is gone

Dibs used to score every surviving issue with Claude and put Track, Skip, and
Snooze buttons on each Slack alert. Both halves are gone now, and they went
together, because each one was the other's reason to exist.

The score only earns its keep if something acts on it, which meant setting a
junk floor. Setting that floor honestly meant a week of pressing Track and Skip
so `dibs replay` had real decisions to sweep candidate floors against. I never
pressed the buttons. The floor stayed at zero, the veto threshold stayed at
1.00, and every model call bought a number that decorated a message and
filtered nothing.

Two guesses were possible at that point. Guess a floor without evidence, which
is the thing the calibration week existed to avoid. Or admit that a system I
will not label is a system that cannot be tuned, and stop paying for the half
that needs tuning.

I picked the second one. What is left has no recurring cost, no API key, and
nothing to calibrate. It is a strainer and a notifier.

The buttons had a second job worth naming, since it also died: an issue you
pressed Track on got followed until it closed, so Dibs could tell you whether
your contribution landed. That was pleasant and it was never the point. It also
never worked properly, because an issue that stays open forever stays pending
forever.

## Strainer, not gate

I already trust these repos. I do not need protection from them. I need to skip
the issues somebody has already taken, and nothing else.

The error costs are lopsided. A bad issue reaching Slack costs me two seconds
and a glance. A good issue I never see costs me the issue. So when the judgment
is close, let it through.

That asymmetry is why the filter is four checks and stays four checks. Every one
of them rests on a person having acted, not on a field having been set.

## How it works

A poller checks each repo every 45 seconds using conditional requests, so a repo
with nothing new costs no rate-limit quota at all. New issues go to an enricher
that fetches the thread, runs the deterministic filter, and either drops the
issue or hands it to the push worker.

Everything hangs off one SQLite file. There is no message broker, no in-memory
queue, no separate worker process.

### SQLite is the queue

Each pipeline stage is a worker that claims rows in a given state, takes a short
lease on them, does its work, and advances the state in the same transaction it
commits the result. Crash recovery is expiring stale leases on boot.

I originally specified six goroutines wired by Go channels. That version could
not survive `kill -9`: a row handed off on a channel and not yet processed was
simply gone, and nothing re-drove it. The durable outbox for Slack was already
solving that same problem in one specific place. Making the database the queue
solves it everywhere, deletes the channel topology and the backpressure
warnings, and is less code than what it replaces.

### Fetching and filtering are one stage

The filter is four pure predicates over bytes the enricher has just fetched.
Persisting that context, releasing the lease, and re-claiming the row to run
them would buy nothing, so the enricher applies the filter itself and writes the
verdict with the state change.

This is what the model call used to sit between. With it gone there was no
reason to keep the seam, and closing it deleted a worker, a state, and the
column that carried the fetched context between them.

### Only new issues, ever

When a repo is added, Dibs records the issue numbers currently open and the
newest creation time, surfaces none of them, and starts from that line. Adding a
repo costs nothing. Without this, the first run would treat 30 issues per repo
as new and push a pile of stale garbage.

Past that line, an issue must be under an hour old when Dibs first sees it.
Older than that, it is recorded and dropped before any enrichment request.

That one check deleted a surprising amount of the design. There is no backfill,
no `Link` header pagination, no sleep-and-wake recovery path, no separate stale
routing. After an outage of any length, Dibs advances the line and carries on,
because everything in that window is stale by definition.

### The freshness check happens twice

Between polling an issue and pushing it, Dibs spends a few seconds fetching the
thread. On a busy repo that is enough time for someone to claim it. So
immediately before sending to Slack, Dibs re-fetches the issue. If it has picked
up an assignee it did not have before, or is closed, or has a comment matching
the claim patterns, it is dropped silently.

The comparison is against the assignees Dibs already recorded, not against an
empty list. An issue that was assigned all along is one I decided to look at
anyway. An assignee that appeared in the last few seconds is somebody taking it
in front of me.

This costs two requests per push and buys the thing I actually want: a ping
means the issue was unclaimed seconds ago.

There used to be a third check, when I pressed Track. It went with the buttons,
and it is no loss. It covered the minutes I spent deciding, and what I do in
those minutes is now my problem rather than Dibs'.

### An assignee is not a claim

The filter used to kill any issue with an assignee. That was the first check it
ran, and on the repositories I watch it was wrong more often than right.
Kubernetes projects assign by round-robin, by CODEOWNERS, and by bot, so the
field says almost nothing about whether a person is writing code.

The strong signals are somebody having acted. A linked pull request means code
exists. A comment saying "taking this" means somebody said it out loud. Those
still reject. Assignment is reported in the alert and decided by me.

The one place assignment still rejects outright is a change during the window,
which the freshness re-check watches for. That is not a field being set. That is
somebody arriving while I was deciding.

### One stream, no digest

Everything the filter passes goes to Slack as an individual push. There is no
tiering and no batched digest.

The digest was in the earlier design to catch mid-ranked issues at 09:00 and
17:00. Under a freshness rule that list is entirely issues between one and
sixteen hours old, which is precisely what I said I do not want to look at.
Cutting it removed a threshold, a code path, and a scheduler.

### What the alert says

Repo, number, age, title, labels, author, comment count, assignees, and how many
issues the repo produces a month. All of it comes off the row Dibs already has,
so composing an alert costs nothing.

There are no buttons. The title is a link, and everything past that link is a
browser action. A Slack app that only posts needs no interactivity, no
app-level token, and no inbound connection, which is one fewer thing to
configure and one fewer thing to break.

## Where it runs

On my laptop, for now. I do not want to pay for a VPS yet.

This works because the freshness rule turned sleep from a correctness problem
into a coverage problem. When the lid closes, Dibs misses whatever appears while
it is shut, and those issues were stale anyway. On wake it advances the line and
continues. There is nothing to recover.

The instrument that tells me when to stop tolerating this: any poll gap over 15
minutes logs and sends one Slack line reporting how long the gap was. If those
show up several times a week, the laptop is costing me the thing I care most
about and it is time to move.

Moving is an `scp` and a `systemctl enable`, because the whole system is one
static binary and one SQLite file. When I do move it, a $4-6/month VPS beats a
Raspberry Pi unless I already own one. A Pi means buying hardware, and an SD
card under a database that writes on every poll is a known way to lose the
database.

## What it costs

Nothing. GitHub is free at this volume and Slack's free tier is enough.

Screening one issue is two requests. Watching a repo is one conditional request
per poll, and a `304` answers most of them without spending quota at all. Four
repos at 45 seconds is a few hundred requests an hour against a limit of five
thousand.

The rate-limit guard still exists, because a bug that starts requesting in a
loop should slow down and then stop rather than get the token throttled. It
bounds a mistake, not a budget.

## Build order

Each milestone runs on its own and is useful on its own.

**M1, poller.** Config, schema, GitHub client with ETags, waterline adoption,
freshness cutoff, rate-limit guard. Prints new issues as JSON.

**M2, filter.** Timeline, comments, deterministic rejection with full unit
coverage, applied inside the enricher.

**M3, Slack.** Block Kit, outbox, sender, pre-push freshness re-check.

**M4, reaper and deployment.** Lease release, expiry, cadence recompute,
dark-repo warning, systemd unit.

## What Dibs will not do

Each of these was considered and cut.

**Any write to GitHub.** The rule above.

**Scoring, ranking, or tiering.** See above. If it comes back, it comes back
with something that acts on the number.

**Auto-claiming.** The point is that I claim.

**Tracking what I did next.** Dibs' job ends at the notification.

**Posting anything public.** Nothing Dibs generates is ever visible to a
maintainer.

**A repo receptivity engine.** I curate the list and already hold that judgment.

**GraphQL batching.** Unnecessary below fifty repos and it complicates ETag
handling.

**A TUI.** Slack is the notification surface. There are operational subcommands
(`run`, `status`) because a daemon needs them, but there is no interactive
interface.

**Webhooks.** Not available on repos I do not own.

**Backfill after downtime.** Everything missed is stale. See above.

**A batched digest.** See above.

**Extra filters beyond the specified list.** The strainer rule is load-bearing.
When in doubt, let it through.
