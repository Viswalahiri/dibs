# Dibs

Dibs watches a handful of GitHub repositories, throws out the issues that are
already spoken for, ranks what is left, and pushes the good ones to Slack fast
enough that claiming one is still possible.

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

**Stale issues cost nothing.** Not a GitHub request, not a model call, not a
Slack line. Anything that appeared while Dibs was asleep or throttled is
recorded and dropped. It is not worth the money and it is not worth the ping.

## The one hard rule

**Dibs never writes to GitHub.**

The token is read-only. No comments, no assignment, no reactions, no labels. If
a code path needs write scope, that path is wrong and gets deleted rather than
fixed. Slack buttons change local SQLite rows and nothing else.

Claiming happens in my browser, by hand. Dibs' whole job is to make sure the
right issue is in front of me while claiming it is still possible.

## Strainer, not gate

I already trust these repos. I do not need protection from them. I need to skip
three specific dead ends:

1. Somebody already claimed it
2. The author is visibly about to fix it themselves
3. It is too vague to know what "done" means

Those three are hard vetoes. Everything else survives and gets ranked.

The error costs are lopsided. A bad issue reaching Slack costs me two seconds
and a skip click. A good issue I never see costs me the issue. So when the
judgment is close, let it through.

I considered a fourth veto for "the maintainers have not settled on an approach"
and cut it. It is indistinguishable from an ordinary feature discussion, and it
kills issues worth seeing. The `maintainer_invitation` score already captures
part of what it was reaching for.

## How it works

A poller checks each repo every 45 seconds using conditional requests, so a
repo with nothing new costs no rate-limit quota at all. New issues go through a
deterministic filter that rejects the obvious dead ends for free, then to Claude
for scoring, then to Slack.

Everything hangs off one SQLite file. There is no message broker, no in-memory
queue, no separate worker process.

### SQLite is the queue

Each pipeline stage is a worker that claims rows in a given state, takes a
short lease on them, does its work, and advances the state in the same
transaction it commits the result. Crash recovery is expiring stale leases on
boot.

I originally specified six goroutines wired by Go channels. That version could
not survive `kill -9`: a row handed off on a channel and not yet processed was
simply gone, and nothing re-drove it. The durable outbox for Slack was already
solving that same problem in one specific place. Making the database the queue
solves it everywhere, deletes the channel topology and the backpressure
warnings, and is less code than what it replaces.

### Only new issues, ever

When a repo is added, Dibs records the issue numbers currently open and the
newest creation time, surfaces none of them, and starts from that line. Adding
a repo produces zero model calls. Without this, the first run would treat 30
issues per repo as new, blow through the daily call cap in minutes, and score a
pile of stale garbage.

Past that line, an issue must be under 15 minutes old when Dibs first sees it.
Older than that, it is recorded and dropped before any enrichment request or
model call.

That one check deleted a surprising amount of the design. There is no backfill,
no `Link` header pagination, no sleep-and-wake recovery path, no separate stale
routing. After an outage of any length, Dibs advances the line and carries on,
because everything in that window is stale by definition.

### The freshness check happens twice

Between polling an issue and pushing it, Dibs spends five to fifteen seconds on
enrichment and a model call. On a busy repo that is enough time for someone to
claim it. So immediately before sending to Slack, Dibs re-fetches the issue. If
it has picked up an assignee it did not have before, or is closed, or has a
comment matching the claim patterns, it is dropped silently.

The comparison is against the assignees Dibs already recorded, not against an
empty list. An issue that was assigned all along is one I decided to look at
anyway. An assignee that appeared in the last fifteen seconds is somebody taking
it in front of me.

At a few pushes a day this costs a handful of API requests and buys the thing I
actually want: a ping means the issue was unclaimed seconds ago. There is a
second check when I press Track, covering the minutes I spend deciding.

### The floor starts at zero, not high

I had this backwards. The original plan was a high junk floor so the first week
would be quiet, on the theory that a noisy tool gets ignored.

A quiet week produces nothing to calibrate against. The floor and the veto
threshold are the two numbers M4 exists to fit, and fitting them needs rows
where I pressed Track or Skip on something. A floor that suppresses the issue
before I see it produces a row with no decision attached, which is exactly the
data the fit cannot use. The first run of this bore that out. Four issues
scored, zero pushed, and nothing at all learned.

So both numbers ship wide open. Everything the deterministic filter passes goes
to Slack, no veto kills anything, and the arithmetic still runs and still gets
recorded. A week of that gives `dibs replay` a corpus with real clicks on it.
The numbers that come out of the sweep are the ones worth committing.

The cost is a week of skipping most of what arrives. That is a click each, and
a click is the cheap failure. The expensive failure is a good issue I never saw,
and that failure leaves no trace anywhere I would think to look.

### An assignee is not a claim

The filter used to kill any issue with an assignee. That was the first check it
ran, and on the repositories I watch it was wrong more often than right.
Kubernetes projects assign by round-robin, by CODEOWNERS, and by bot, so the
field says almost nothing about whether a person is writing code.

The strong signals are somebody having acted. A linked pull request means code
exists. A comment saying "taking this" means somebody said it out loud. Those
still reject. Assignment now costs ten points in scoring, and the model sees the
assignee list so it can read the thread around it.

The one place assignment still rejects outright is a change during the window,
which the freshness re-check watches for. That is not a field being set. That is
somebody arriving while I was deciding.

### One stream, no digest

Everything above the junk floor goes to Slack as an individual push. There is
no tiering and no batched digest.

The digest was in the earlier design to catch mid-ranked issues at 09:00 and
17:00. Under a 15-minute freshness rule that list is entirely issues between one
and sixteen hours old, which is precisely what I said I do not want to look at.
Cutting it removed a threshold, a code path, and a scheduler.

The floor starts at 40. If the volume annoys me, raising it is one line of
config, and I will have the data to pick the number.

## Scoring, kept deliberately dumb

Claude scores five dimensions from 1 to 5 and reports confidence on each of the
three vetoes. Go computes the composite. The model never returns a final number,
because model-produced composites drift between calls and make the weights
impossible to retune.

All five weights are equal. I have no evidence that maintainer engagement
matters more than blast radius, and equal weights are the honest starting point.
The stack and receptivity multipliers stay in Go where a replay can sweep them
later.

That leaves two numbers to tune: the junk floor and the veto confidence
threshold. Two parameters is a fit I can actually do against a hundred issues.
Eleven was not, and pretending otherwise would have meant adjusting numbers by
feel and calling it calibration.

`dibs replay` re-runs scoring over stored model responses under a candidate
config and shows how the push and reject sets change against my recorded Track
and Skip clicks. It costs no API spend and it is what makes the numbers
tunable at all.

## Where it runs

On my laptop, for now. I do not want to pay for a VPS yet.

This works because the 15-minute freshness rule turned sleep from a correctness
problem into a coverage problem. When the lid closes, Dibs misses whatever
appears while it is shut, and those issues were stale anyway. On wake it
advances the line and continues. There is nothing to recover.

I will set the two `logind` and GNOME settings as a best effort and not fight
the Wi-Fi power-saving question until it bites.

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

The only recurring cost is the Anthropic API. GitHub is free at this volume and
Slack's free tier is enough.

At four repos, expect roughly eight new issues a day. The deterministic filter
kills about half at no cost, leaving four or five model calls. Each call runs
about 3,300 input tokens and 350 output tokens under the truncation caps.

At Sonnet 5 rates of $2 and $10 per million tokens, that is **about $1.20 a
month**. At twenty-five repos it would be about $7.50. Both are noise, which
means the cost caps in the config exist to bound a bug, not to manage a budget.

The levers, in order of effect:

The deterministic filter matters most. Every issue it rejects is free. If it is
only killing 20% of volume, tighten it before touching anything else.

The three truncation caps come next, since input scales linearly with them.

The 20-word justification caps matter more than they look. Output is priced at
5x input, so it is a third of the bill despite being a tenth of the tokens.

Haiku 4.5 at $1 and $5 would roughly halve the bill. Worth trying once there is
calibration data to compare against, as a one-line config change. Not worth
building a two-model cascade for; at five calls a day the complexity never pays
back.

Prompt caching does not help here. The system prompt is cacheable, but at five
sparse calls a day the cache almost never gets a hit before it expires, and
cache writes cost more than plain input. It would raise the bill.

The Batch API is half price and takes hours, which defeats the entire point.

## Build order

Each milestone runs on its own and is useful on its own.

**M1, poller.** Config, schema, GitHub client with ETags, waterline adoption,
freshness cutoff, rate-limit guard. Prints new issues as JSON.

**M2, filter.** Timeline, comments, doc cache, author stats, deterministic
rejection with full unit coverage.

**M3, triage and Slack together.** Claude integration, scoring, Block Kit,
buttons, outbox. This ships Slack rather than holding it back, because
time-to-value is the whole point and a shadow-mode HTML file is a throwaway.
The junk floor starts at zero so the first week is loud on purpose.

**M4, calibration.** Run it for a week. Use `dibs replay` and the nightly
missed-issue list to set the floor and the veto threshold on evidence. The only
failure that matters is a veto killing something I would have taken; a bad issue
getting through costs one click.

**M5, reaper and deployment.** Outcome tracking, nudges, cadence recompute,
spend tracking, systemd unit.

## What Dibs will not do

Each of these was considered and cut.

**Any write to GitHub.** The rule above.

**Auto-claiming.** The point is that I claim.

**Posting anything public.** Nothing Dibs generates is ever visible to a
maintainer.

**A repo receptivity engine.** I curate the list and already hold that judgment.
It is a one-word config field.

**GraphQL batching.** Unnecessary below fifty repos and it complicates ETag
handling.

**A TUI.** Slack is the notification surface. There are operational
subcommands (`run`, `status`, `replay`, `backfill`) because a daemon needs them,
but there is no interactive interface.

**Webhooks.** Not available on repos I do not own.

**Backfill after downtime.** Everything missed is stale. See above.

**A batched digest.** See above.

**A cooldown hold before alerting.** Replaced by the two freshness re-checks.

**Extra filters beyond the specified list.** The strainer rule is load-bearing.
When in doubt, let it through.

**A two-model cost cascade.** The bill is a dollar.
