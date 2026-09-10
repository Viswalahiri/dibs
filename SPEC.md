# Dibs build specification

Build a single-binary Go daemon named `dibs`. It polls GitHub repositories for
newly opened issues, rejects the ones already spoken for, and pushes the rest to
Slack.

Read `PLAN.md` for intent. This document is the contract. Where it is silent,
build the simplest thing that works. Where it conflicts with `PLAN.md`, this
document wins.

## 0. Inviolable constraint

**No writes to GitHub.** The token is read-only. Do not implement comment
posting, assignment, reactions, or labels. If any code path would issue a
`POST`, `PATCH`, `PUT`, or `DELETE` against `api.github.com`, delete that path
rather than making it work.

Dibs is a notifier. It has no opinion about which surviving issue is better than
another, and it stops at the notification. Do not add ranking, scoring, tiering,
message buttons, or any record of what the operator did next.

## 1. Locked decisions

| Area | Decision |
|---|---|
| Language | Go 1.22+ |
| Database | SQLite via `modernc.org/sqlite`, pure Go, no cgo |
| Queue | The `issues` table. No channels between stages. |
| GitHub access | Hand-rolled `net/http` client for exact ETag and rate-limit control |
| Ingestion | REST polling with conditional requests |
| Notification | Slack, posting only. No Socket Mode, no interactivity. |
| Deployment | systemd user unit with linger on Ubuntu, portable to a VPS |
| Scale target | 3 to 25 repos |

## 2. Layout

```
dibs/
├── cmd/dibs/                     main.go, status.go
├── internal/
│   ├── app/supervisor.go         worker lifecycle
│   ├── config/                   config.go, config_test.go
│   ├── store/                    schema.sql, store.go, repos.go, issues.go,
│   │                             state.go, stats.go, outbox.go, and tests
│   ├── gh/                       client.go, poller.go, enrich.go, types.go,
│   │                             paginate.go, timeline.go, and tests
│   ├── filter/                   filter.go, filter_test.go
│   ├── notify/                   slack.go, blocks.go, push.go, sender.go
│   └── reaper/reaper.go
├── configs/                      dibs.example.yaml, repos.example.yaml, env.example
├── deploy/dibs.service
├── go.mod
├── Makefile
└── README.md
```

## 3. Dependencies

Closed list. Do not add without a specific need.

```
github.com/slack-go/slack      Block Kit and posting
modernc.org/sqlite             pure-Go SQLite driver
gopkg.in/yaml.v3               config
```

Standard library for everything else, including all HTTP.

Concurrency is capped with a buffered channel, not a rate limiter. GitHub's
secondary rate limit punishes concurrent requests, and a requests-per-second
limiter does not bound those.

## 4. Subcommands

| Command | Behaviour |
|---|---|
| `dibs run` | The daemon. Everything below. `--dry-run` prints alerts to stdout and needs no Slack token. `--debug` logs every poll. |
| `dibs status --repos` | Per-repository waterline and issue counts by state. |
| `dibs status --today` | Everything notified in the last 24 hours, newest first. |

## 5. Configuration

### 5.1 Environment

| Variable | Required | Purpose |
|---|---|---|
| `DIBS_GITHUB_TOKEN` | yes | Fine-grained PAT, read-only, public repos |
| `DIBS_SLACK_BOT_TOKEN` | unless `--dry-run` | `xoxb-` bot token, scope `chat:write` |
| `DIBS_CONFIG` | no | Defaults to `~/.config/dibs/dibs.yaml` |
| `DIBS_DB` | no | Defaults to `~/.local/share/dibs/dibs.db` |
| `DIBS_GITHUB_API` | no | Alternate API host. Exists so the binary can run against a stub without a real token. |

Report every missing variable at once and exit naming all of them. Never log
token values, not even prefixes.

At startup call `GET /rate_limit`, then `GET /user`. Log the authenticated login
and fail startup if it does not equal `profile.github_login`. Do not try to
verify scopes; fine-grained PATs do not expose them reliably.

### 5.2 `dibs.yaml`

```yaml
profile:
  github_login: my-gh-handle   # required; the filter needs it
  timezone: America/New_York   # decides when the reaper's daily pass runs

polling:
  default_interval_sec: 45
  min_interval_sec: 30
  max_concurrent: 4
  freshness_cutoff_min: 60     # older than this at first sight: dropped free
  gap_warn_min: 15             # poll gap that triggers one Slack warning
  rate_limit_slow_at: 500
  rate_limit_pause_at: 100

slack:
  deliver_to: dm               # "dm" or a channel ID

reaper:
  expire_after_days: 14
  cadence_recompute_hour: 3
  lease_ttl_sec: 300
```

Decode with `KnownFields(true)`. An unrecognised key is a config error, not
something to ignore, because a silently dropped setting is worse than a refused
start.

### 5.3 `repos.yaml`

```yaml
repos:
  - slug: owner/repo-one
    notes: "maintainer responsive, PRs merge in ~2 days"
  - slug: owner/repo-two
```

`notes` is stored and never read. It is there for whoever edits the file.

On `SIGHUP`, reload both files. A newly listed repo is adopted per §7.1.
Removing a repo sets `enabled = 0` rather than deleting rows.

## 6. Schema

Embed `schema.sql` via `go:embed`. Run at startup, idempotent. Set
`PRAGMA journal_mode=WAL` and `PRAGMA busy_timeout=5000`. All timestamps are
Unix seconds, UTC.

```sql
CREATE TABLE IF NOT EXISTS repos (
  id                INTEGER PRIMARY KEY,
  owner             TEXT    NOT NULL,
  name              TEXT    NOT NULL,
  notes             TEXT    NOT NULL DEFAULT '',
  poll_interval_sec INTEGER NOT NULL DEFAULT 45,
  etag              TEXT,
  last_polled_at    INTEGER,
  waterline_at      INTEGER NOT NULL DEFAULT 0,
  adopted_at        INTEGER,
  issues_last_30d   INTEGER NOT NULL DEFAULT 0,
  enabled           INTEGER NOT NULL DEFAULT 1,
  UNIQUE(owner, name)
);

CREATE TABLE IF NOT EXISTS issues (
  id                INTEGER PRIMARY KEY,
  repo_id           INTEGER NOT NULL REFERENCES repos(id),
  number            INTEGER NOT NULL,
  node_id           TEXT    NOT NULL,
  title             TEXT    NOT NULL,
  body              TEXT    NOT NULL DEFAULT '',
  html_url          TEXT    NOT NULL,
  author            TEXT    NOT NULL,
  author_assoc      TEXT    NOT NULL,
  labels            TEXT    NOT NULL DEFAULT '[]',
  assignees         TEXT    NOT NULL DEFAULT '[]',
  comment_count     INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL,
  first_seen_at     INTEGER NOT NULL,
  state             TEXT    NOT NULL,
  reject_reason     TEXT,
  lease_owner       TEXT,
  lease_expires_at  INTEGER,
  surfaced_at       INTEGER,
  UNIQUE(repo_id, number)
);

CREATE TABLE IF NOT EXISTS outbox (
  id         INTEGER PRIMARY KEY,
  kind       TEXT    NOT NULL,
  dedupe_key TEXT,
  payload    TEXT    NOT NULL,
  created_at INTEGER NOT NULL,
  sent_at    INTEGER
);

CREATE INDEX IF NOT EXISTS idx_issues_lease
  ON issues(state, lease_expires_at, first_seen_at);
CREATE INDEX IF NOT EXISTS idx_outbox_pending
  ON outbox(sent_at) WHERE sent_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_outbox_dedupe
  ON outbox(dedupe_key) WHERE dedupe_key IS NOT NULL;
```

The fetched context is deliberately not a column. The filter is the only thing
that reads it, and the filter runs in the same stage that fetches it.

### 6.1 States

| State | Meaning |
|---|---|
| `baseline` | Open when the repo was adopted. Never screened, never surfaced. |
| `new` | Inserted, awaiting the enricher |
| `ready` | Survived the filter, awaiting the push worker |
| `pushed` | Slack alert sent. The end of the line. |
| `rejected` | Killed by the filter. See `reject_reason`. |
| `aged_out` | Older than `freshness_cutoff_min` at first sight |
| `claimed_before_push` | The pre-push re-check found it taken |
| `expired` | Sat in `ready` past `expire_after_days` |

`reject_reason` values: `linked_pr_exists`, `killfile_label`,
`claimed_in_thread`, `too_thin`. Every rejection carries one, and writing a
rejection without one is an error rather than a null column.

Legal transitions, and no others:

```
new    -> ready | rejected
ready  -> pushed | claimed_before_push | expired
```

Everything else is terminal.

## 7. Pipeline

Workers are independent goroutines. They never talk to each other. Each polls
the database for claimable rows, leases them, works, and commits the result and
the state change in one transaction.

Workers: poller (one goroutine per enabled repo), enricher, pusher, sender,
reaper.

### 7.0 Lease protocol

```sql
UPDATE issues
   SET lease_owner = ?, lease_expires_at = ?
 WHERE id IN (
   SELECT id FROM issues
    WHERE state = ?
      AND (lease_expires_at IS NULL OR lease_expires_at < ?)
    ORDER BY first_seen_at
    LIMIT ?
 )
RETURNING id;
```

On completion, clear `lease_owner` and `lease_expires_at` in the same
transaction that sets the new state. On startup, clear every expired lease
before starting workers. A crash mid-work leaves a lease that expires after
`lease_ttl_sec` and the row is retried. Every stage must therefore be safe to
run twice on the same row.

Claimable states are `new` and `ready`.

### 7.1 Repo adoption

The first time a repo is seen in `repos.yaml`:

1. `GET /repos/{owner}/{name}/issues?state=open&sort=created&direction=desc&per_page=100`
2. Insert every returned item with `state = 'baseline'`, skipping any with a
   non-null `pull_request` field.
3. Set `waterline_at` to the newest `created_at` returned, or to `now` if the
   repo has no open issues.
4. Set `adopted_at = now`.

Adoption produces zero enrichment requests. This applies on first run and on
every `SIGHUP` that adds a repo.

### 7.2 Poll tick

Per repo, on a ticker at `poll_interval_sec`:

1. Read `Budget()`. Discard the reading if its reset time has already passed:
   only a response refreshes the budget, and a paused poller sends none, so a
   stale reading would hold the pause open forever. Below `rate_limit_slow_at`,
   double this repo's interval **for this tick only** rather than persisting it,
   so a recovered budget restores the normal cadence instead of compounding.
   Below `rate_limit_pause_at`, skip the tick and enqueue one deduplicated Slack
   warning per reset window.
2. If `last_polled_at` is set and `now - last_polled_at > gap_warn_min`, enqueue
   one Slack line: `gap: {n}m, resuming`. This is the only sleep handling. There
   is no backfill.
3. `GET /repos/{owner}/{name}/issues?state=open&sort=created&direction=desc&per_page=30`
   with `If-None-Match`.
4. On `304`, update `last_polled_at` and return.
5. On `200`, store the new ETag, then for each item:
   - **Skip any item with a non-null `pull_request` field.** The issues endpoint
     returns pull requests. This is the most common bug in this pattern.
   - Skip if `created_at` is **strictly before** `waterline_at`. Not at or
     before: an issue opened in the same second the waterline was drawn has
     never been seen, and `<=` loses it silently. The tie falls through to the
     insert, where the `(repo_id, number)` constraint decides.
   - Skip if the `(repo_id, number)` row already exists.
   - If `now - created_at > freshness_cutoff_min`, insert with
     `state = 'aged_out'` and continue. No further work, no cost.
   - Otherwise insert with `state = 'new'`.
   - Advance `waterline_at` to the newest `created_at` seen this tick.

A `304` does not consume rate-limit quota. That is why this cadence works.

### 7.3 Enrichment and filtering

One stage, one lease. Claims rows in `new`, fetches concurrently:

1. **Timeline.** `GET /repos/{o}/{n}/issues/{num}/timeline?per_page=100` with
   `Accept: application/vnd.github.mockingbird-preview+json`. Set `HasLinkedPR`
   if any event is `cross-referenced` or `connected` and its source is an open
   pull request. Only an open one counts. A closed pull request is usually an
   abandoned attempt, which leaves the issue available.
2. **Comments.** `GET /repos/{o}/{n}/issues/{num}/comments?per_page=20`

Then apply §7.4 and commit the verdict with the state change: `ready` when the
issue survives, `rejected` with its reason when it does not.

Budget is exactly 2 requests per new issue. Anything that raises that number is
a request nothing reads, and `internal/app` has a test that pins it.

### 7.4 Deterministic filter

Pure functions, no I/O, fully unit-testable.

```go
type Context struct {
    HasLinkedPR bool
    Comments    []Comment
}
func Apply(iss store.Issue, ctx Context, cfg *config.Config) Result
```

`Context` lives in `internal/filter` rather than `internal/gh`, because the
filter is its only reader and nothing persists it.

Reject if any of:

| Check | Reason |
|---|---|
| `ctx.HasLinkedPR` | `linked_pr_exists` |
| Labels intersect the killfile | `killfile_label` |
| A comment by anyone other than `profile.github_login` matches the claim regex | `claimed_in_thread` |
| `len(body) < 80` and the body has no code fence | `too_thin` |

Killfile, case-insensitive substring match:
`wontfix`, `duplicate`, `invalid`, `stale`

`needs-triage` and `blocked` are deliberately absent. Both mean nobody has acted
yet, which describes exactly the fresh unclaimed issue this system exists to
find, and many repos apply `needs-triage` to every issue automatically.

`question`, `discussion`, and `rfc` are also absent. They were scored down
rather than killed when there was scoring, and with the scoring gone they are
simply let through.

An assignee is not a rejection either. Projects hand assignments out by
round-robin, by CODEOWNERS, and by bot, so an assigned issue with no other
activity is usually still open in practice, and killing on the field costs real
issues. The alert reports the assignee list and the operator decides. The strong
signals stay here: a linked pull request, and a person saying in the thread that
they have taken it.

Strip fenced code blocks and blockquotes from comment bodies before running the
claim regex, so an issue quoting the phrase "working on this" is not rejected.

```go
var claimRe = regexp.MustCompile(`(?i)\b(i'?ll take (this|it)|i am taking|taking this|` +
    `working on (this|it)|i'?m on (this|it)|/assign|\.take|pr (is )?incoming|` +
    `i have a (patch|fix)|opened a pr|submitted a pr|will submit)\b`)
```

Every check here rests on somebody having acted, not on a field having been set.
Do not add checks beyond this list.

## 8. GitHub client

```go
type Client struct {
    http    *http.Client
    token   string
    baseURL string
    sem     chan struct{} // caps requests in flight at max_concurrent
    sleep   func(context.Context, time.Duration) error

    mu        sync.RWMutex
    remaining int // -1 until the first response
    limit     int
    resetAt   time.Time
}

type Response struct {
    Body        []byte
    ETag        string
    Status      int
    NotModified bool
    Link        string
}

func New(token string, maxConcurrent int, opts ...Option) *Client

// Get performs a conditional GET, retrying transient failures.
func (c *Client) Get(ctx context.Context, path, etag string) (Response, error)

// GetJSON decodes into v, leaving it untouched on a 304.
func (c *Client) GetJSON(ctx context.Context, path, etag string, v any) (
    resp Response, notModified bool, err error)

func (c *Client) Budget() (remaining int, resetAt time.Time)
```

`sleep` is injectable so the retry and rate-limit paths are testable without
real waits.

- Base URL `https://api.github.com`
- Every request carries `Authorization: Bearer <token>`,
  `Accept: application/vnd.github+json`,
  `X-GitHub-Api-Version: 2022-11-28`, `User-Agent: dibs/1.0`
- Send `If-None-Match` when a non-empty etag is supplied
- Parse `X-RateLimit-Remaining` and `X-RateLimit-Reset` on every response, and
  record only readings for the core resource. Other buckets have their own
  limits and a small remaining count on one of them must not pause the poller.
- On `403` or `429` carrying `Retry-After`, sleep that long and retry once
- On `403` with `X-RateLimit-Remaining: 0`, sleep until reset plus 5s jitter
- On 5xx, exponential backoff with full jitter at 1s, 2s, 4s, three attempts max
- Never cache an absolute deadline across a possible suspend. Read `time.Now()`
  fresh at every use.

## 9. Slack

### 9.1 Connection

Posting only, over `slack-go/slack`. The app needs one scope, `chat:write`.
Leave interactivity off. There is no app-level token, no Socket Mode, and no
inbound connection, because there is nothing for Slack to send back.

`deliver_to` is `dm` or a channel ID. A DM has to be opened before it can be
posted to, so resolve it once through `AuthTest` and `OpenConversation` and
cache the channel ID.

Every outbound message is written to `outbox` before send and marked `sent_at`
on acknowledgement. Warning messages set `dedupe_key` so the unique index
enforces deduplication rather than application code. A payload that will not
decode is marked sent and dropped, since it will never become readable and
would otherwise block the queue forever.

### 9.2 Push worker

Claims rows in `ready`. For each, in order:

1. **Re-check freshness.** `GET /repos/{o}/{n}/issues/{num}` and its comments.
   If it has picked up an assignee it did not carry when dibs first recorded it,
   or is closed, or a comment matches the claim regex, set
   `state = 'claimed_before_push'` and stop. Do not notify.

   The comparison is against the recorded assignee list, not against emptiness.
   An issue that was already assigned when the filter passed it was surfaced on
   purpose, and rejecting it here would undo that decision one stage later.
   `profile.github_login` never counts as a new assignee: him taking the issue
   is the outcome. A name that arrived during the window is a real claim, which
   is why the change is watched rather than the field.
2. Build the Block Kit message and enqueue it to `outbox`.
3. Set `state = 'pushed'`, `surfaced_at = now`.

The message has to be readable without opening GitHub. That is the whole speed
argument.

```
owner/repo · #4821 · 4m ago
Fix race condition in worker pool drain

bug, help wanted · by someone · 2 comments · ~48 issues/30d
```

Blocks: a `context` line with repo, number, and relative age; a `section` with
the title as a link; a `context` line with labels, author, comment count,
assignees, and the repository's monthly issue volume. Any part with nothing to
say is left out rather than printed empty.

Escape `&`, `<`, and `>` in every value that came from GitHub. Issue titles
contain them regularly and an unescaped one silently mangles the message.

There are no buttons and no action block. The title link is the only thing to
click, and everything past it happens in the browser.

There is no cap on push volume.

## 10. Reaper

One ticker, every 15 minutes. Every step reads and writes SQLite only. The
reaper makes no requests.

- **Release expired leases**, keeping a dead worker's name off rows it no longer
  owns. `Claim` already ignores an expired lease, so this is housekeeping rather
  than recovery.
- **Expire issues** sitting in `ready` longer than `expire_after_days`. In
  practice this only fires when Slack has been unreachable for a fortnight.
- **Recompute cadence** daily at `cadence_recompute_hour`, from the issues each
  repository has actually produced. Measure the rate over the time dibs has been
  watching rather than a flat thirty days, so a busy repository added yesterday
  is polled quickly today. Leave a repository alone for its first week, or a
  repo adopted this morning reads as silent and drops to half-hourly polling,
  slow enough that its next issue is stale on arrival.

| Issues per week | Interval |
|---|---|
| 10+ | 30s |
| 3-9 | 60s |
| 1-2 | 5m |
| 0 | 30m |

- **Warn about dark repos** daily: a repository that rejected 90% or more of at
  least ten issues over the past week has usually acquired a label bot rather
  than gone quiet, and the symptom is silence, so nothing in the alert stream
  would show it.

The daily pass reads whole days that have already ended, so its numbers cannot
change after the fact and a repeated pass composes a byte-identical message that
the outbox dedupe drops. It runs at or after the configured hour rather than
exactly on it, so a laptop asleep at three runs it on wake instead of skipping
the day.

## 11. Milestones

Each milestone runs on its own. Do not build ahead of the current one.

**M1, poller.** Config with validation, schema and migration, `gh.Client` with
ETags, repo adoption, poll tick with PR filtering and freshness cutoff, rate
limit guard, lease scaffolding. Prints new issues as JSON.

*Accepts when:* adding a repo produces zero enrichment requests; a 24h run
against the configured repos stays under 1,000 requests per hour with 304s
dominating the logs; no issue is emitted twice; an issue backdated past the
cutoff lands in `aged_out` without any follow-up request.

**M2, filter.** Timeline, comments, deterministic rejection, applied inside the
enricher.

*Accepts when:* every rejection carries a `reject_reason`; each killfile label
and each claim regex alternative has a passing test; an issue quoting "working
on this" inside a fenced code block is not rejected; an assigned issue with no
other activity survives both the filter and the pre-push re-check; enrichment
costs exactly two requests per issue.

**M3, Slack.** Block Kit, outbox, sender, pre-push freshness re-check.

*Accepts when:* a sleep and wake cycle loses no queued message; an issue newly
assigned between the filter and the push lands in `claimed_before_push` with no
Slack message; `--dry-run` runs the whole pipeline to stdout with no Slack token
present.

**M4, reaper and deployment.** Lease release, expiry, cadence recompute,
dark-repo warning, systemd unit.

*Accepts when:* the daemon survives reboot; `kill -9` mid-enrichment loses no
row and the issue completes on the next lease cycle; `SIGHUP` reloads repos
without a restart and adopts new ones at the waterline.

## 12. Testing

- **`gh`**, `httptest` fixtures covering 304 handling, PR-in-issues filtering,
  `Retry-After` backoff, and the rate-limit pause.
- **`filter`**, table-driven over every killfile label and every claim regex
  alternative, with explicit negative cases. This is the highest-value test file
  in the repo, because it is the only thing deciding what the operator sees.
- **`store`**, in-memory SQLite. Assert that every state transition is
  idempotent and that a lease claimed twice yields one worker.
- **`app`**, one test running the full pipeline against a fixture GitHub server,
  asserting a push lands in a fake Slack sink. The same file pins the two-request
  enrichment budget by failing on any request path the fixture does not expect.

## 13. Deployment

Ubuntu LTS, systemd user service with lingering.

`deploy/dibs.service`, installed to `~/.config/systemd/user/dibs.service`:

```ini
[Unit]
Description=Dibs - GitHub issue radar
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Type=simple
ExecStart=%h/.local/bin/dibs run
EnvironmentFile=%h/.config/dibs/env
Restart=always
RestartSec=10
StandardOutput=append:%h/.local/state/dibs/dibs.log
StandardError=append:%h/.local/state/dibs/dibs.err

[Install]
WantedBy=default.target
```

Secrets live in `~/.config/dibs/env`, mode `0600`, never in the unit file.

```bash
mkdir -p ~/.local/state/dibs ~/.config/dibs
chmod 600 ~/.config/dibs/env
systemctl --user daemon-reload
systemctl --user enable --now dibs
loginctl enable-linger "$USER"
```

`loginctl enable-linger` is load-bearing. Without it the user manager is torn
down at logout and Dibs dies with it.

### 13.1 Laptop settings

Ubuntu suspends on lid close, which stops the process. Two changes, both best
effort. Nothing in Dibs depends on them working.

`/etc/systemd/logind.conf`:

```
HandleLidSwitch=ignore
HandleLidSwitchDocked=ignore
HandleLidSwitchExternalPower=ignore
```

Then `sudo systemctl restart systemd-logind`, which restarts the graphical
session on some Ubuntu versions. Save work first.

GNOME suspends on inactivity independently of the lid:

```bash
gsettings set org.gnome.settings-daemon.plugins.power sleep-inactive-ac-type 'nothing'
gsettings set org.gnome.settings-daemon.plugins.power sleep-inactive-battery-type 'suspend'
```

Leaving the battery case as `suspend` is deliberate. On battery it should sleep.

When suspend happens anyway, the gap warning in §7.2 reports it and Dibs
continues from the current waterline. Missed issues are stale and are not
recovered.

### 13.2 Makefile

`build`, `test`, `lint`, `setup` (copy the example configs without overwriting),
`install` (build, copy to `~/.local/bin`, restart the unit), `service`,
`cross` (linux/amd64 and linux/arm64), `logs` (`journalctl --user -u dibs -f`).

## 14. Failure modes

| Failure | Handling |
|---|---|
| Pull requests returned by the issues endpoint | Check `pull_request` on every item. The most common bug in this pattern. |
| Process killed mid-stage | Lease expires, row is retried. Every stage is safe to run twice. |
| Laptop sleeps | Gap warning, advance the waterline, no backfill |
| Clock jump after suspend | Never cache absolute deadlines. Read `time.Now()` fresh. |
| Slack unreachable | Durable outbox, the row stays pending and the sender retries |
| Undecodable outbox payload | Mark sent and drop. It will never become readable. |
| Duplicate warning messages | `outbox.dedupe_key` unique index |
| Secondary rate limit | Honour `Retry-After`, cap concurrency at `max_concurrent`. Concurrency is punished harder than volume. |
| A non-core rate-limit bucket reads low | Record only the core resource. Other buckets never pause the poller. |
| A repo goes dark from a label bot | `dibs status --repos` shows the histogram; reaper warns above 90% rejection over 7 days |
