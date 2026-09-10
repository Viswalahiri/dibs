# Dibs build specification

Build a single-binary Go daemon named `dibs`. It polls GitHub repositories for
newly opened issues, rejects the ones already spoken for, scores the rest with
Claude, and pushes the survivors to Slack.

Read `PLAN.md` for intent. This document is the contract. Where it is silent,
build the simplest thing that works. Where it conflicts with `PLAN.md`, this
document wins.

## 0. Inviolable constraint

**No writes to GitHub.** The token is read-only. Do not implement comment
posting, assignment, reactions, or labels. If any code path would issue a
`POST`, `PATCH`, `PUT`, or `DELETE` against `api.github.com`, delete that path
rather than making it work. Slack button handlers mutate local SQLite only.

## 1. Locked decisions

| Area | Decision |
|---|---|
| Language | Go 1.22+ |
| Database | SQLite via `modernc.org/sqlite`, pure Go, no cgo |
| Queue | The `issues` table. No channels between stages. |
| GitHub access | Hand-rolled `net/http` client for exact ETag and rate-limit control |
| Ingestion | REST polling with conditional requests |
| LLM | Claude via `net/http` POST, structured outputs |
| Notification | Slack Socket Mode |
| Deployment | systemd user unit with linger on Ubuntu, portable to a VPS |
| Scale target | 3 to 25 repos |

## 2. Layout

```
dibs/
├── cmd/dibs/main.go              subcommands, flags, signals, wiring
├── internal/
│   ├── app/supervisor.go         worker lifecycle, lease reaping on boot
│   ├── config/                   config.go, config_test.go
│   ├── store/                    schema.sql, store.go, repos.go, issues.go,
│   │                             lease.go, store_test.go
│   ├── gh/                       client.go, poller.go, enrich.go, types.go,
│   │                             gh_test.go
│   ├── filter/                   filter.go, filter_test.go
│   ├── triage/                   triage.go, prompt.txt, schema.go, score.go,
│   │                             replay.go, triage_test.go
│   ├── notify/                   slack.go, blocks.go, router.go, outbox.go
│   └── reaper/reaper.go
├── configs/                      dibs.example.yaml, repos.example.yaml
├── deploy/dibs.service
├── testdata/triage/              golden fixtures for `make eval`
├── go.mod
├── Makefile
└── README.md
```

## 3. Dependencies

Closed list. Do not add without a specific need.

```
github.com/slack-go/slack      Socket Mode and Block Kit
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
| `dibs run` | The daemon. Everything below. |
| `dibs status [--today\|--missed\|--repos]` | Read-only report to stdout. `--today` lists everything scored today with its state. `--missed` lists issues that scored below the floor in the last 24h, newest first. `--repos` prints per-repo counts and rejection reasons over 7 days. |
| `dibs replay [--config path]` | Recompute composites from stored triage responses under a candidate config. Prints how the push and reject sets change against recorded Track and Skip decisions. Makes no API calls. |
| `dibs backfill <owner/repo> --since <dur>` | Explicit opt-in. Ingests older issues, respects the daily call cap, never pushes to Slack. |

## 5. Configuration

### 5.1 Environment

| Variable | Required | Purpose |
|---|---|---|
| `DIBS_GITHUB_TOKEN` | yes | Fine-grained PAT, read-only, public repos |
| `DIBS_ANTHROPIC_KEY` | yes | Claude API key |
| `DIBS_SLACK_BOT_TOKEN` | yes | `xoxb-` bot token |
| `DIBS_SLACK_APP_TOKEN` | yes | `xapp-` token with `connections:write` |
| `DIBS_CONFIG` | no | Defaults to `~/.config/dibs/dibs.yaml` |
| `DIBS_DB` | no | Defaults to `~/.local/share/dibs/dibs.db` |
| `DIBS_GITHUB_API` | no | Alternate API host. Exists so the binary can run against a stub without a real token. |

Validate all four required variables at startup and exit naming the missing one.
Never log token values, not even prefixes.

At startup call `GET /rate_limit`, then `GET /user`. Log the authenticated
login and fail startup if it does not equal `profile.github_login`. Do not try
to verify scopes; fine-grained PATs do not expose them reliably.

### 5.2 `dibs.yaml`

```yaml
profile:
  github_login: my-gh-handle   # required; filter and reaper both need it
  stacks: [go, typescript, python, rust, postgres]
  effort_ceiling_hours: 16
  timezone: America/New_York

scoring:
  junk_floor: 0                # below this, stored but never pushed; 0 pushes everything
  veto_confidence: 1.00        # a veto kills only above this; 1.00 makes every veto a -10 penalty
  weights:                     # equal until replay data says otherwise
    scope_clarity: 20
    concreteness: 20
    blast_radius: 20
    maintainer_invitation: 20
    contention_risk: 20
  multipliers:
    stack_match: 1.00
    stack_mismatch: 0.70
    receptivity_high: 1.10
    receptivity_normal: 1.00
    receptivity_cautious: 0.85

polling:
  default_interval_sec: 45
  min_interval_sec: 30
  max_concurrent: 4
  freshness_cutoff_min: 60     # older than this at first sight: dropped free
  gap_warn_min: 15             # poll gap that triggers one Slack warning
  rate_limit_slow_at: 500
  rate_limit_pause_at: 100

triage:
  model: claude-sonnet-5
  thinking: disabled           # "adaptive" spends reasoning tokens and time
  max_body_chars: 4000
  max_thread_chars: 2500
  max_doc_chars: 1500
  max_retries: 2
  timeout_sec: 45
  daily_call_cap: 400
  cost:
    input_per_mtok_usd: 2.00   # Sonnet 5 rates
    output_per_mtok_usd: 10.00
    monthly_budget_usd: 25.00

slack:
  deliver_to: dm               # "dm" or a channel ID

reaper:
  expire_after_days: 14
  cadence_recompute_hour: 3
  lease_ttl_sec: 300
```

### 5.3 `repos.yaml`

```yaml
repos:
  - slug: owner/repo-one
    receptivity: high          # high | normal | cautious
    stacks: [go]               # optional override
    notes: "maintainer responsive, PRs merge in ~2 days"
```

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
  receptivity       TEXT    NOT NULL DEFAULT 'normal',
  stacks            TEXT    NOT NULL DEFAULT '[]',
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
  score             INTEGER,
  effort_low_h      REAL,
  effort_high_h     REAL,
  enrichment_json   TEXT,    -- fetched once, read by both the filter and triage
  triage_input      TEXT,    -- exact rendered user message, for replay
  triage_json       TEXT,    -- raw model response
  lease_owner       TEXT,
  lease_expires_at  INTEGER,
  surfaced_at       INTEGER, -- when the Slack push was sent
  decided_at        INTEGER, -- when the operator pressed a button
  UNIQUE(repo_id, number)
);

CREATE TABLE IF NOT EXISTS tracked (
  issue_id      INTEGER PRIMARY KEY REFERENCES issues(id),
  tracked_at    INTEGER NOT NULL,
  assigned_at   INTEGER,
  pr_url        TEXT,
  closed_at     INTEGER,
  outcome       TEXT NOT NULL DEFAULT 'pending',
  last_checked  INTEGER
);

CREATE TABLE IF NOT EXISTS triage_runs (
  id          INTEGER PRIMARY KEY,
  issue_id    INTEGER NOT NULL REFERENCES issues(id),
  model       TEXT    NOT NULL,
  input_tok   INTEGER NOT NULL DEFAULT 0,
  output_tok  INTEGER NOT NULL DEFAULT 0,
  latency_ms  INTEGER NOT NULL DEFAULT 0,
  ok          INTEGER NOT NULL DEFAULT 1,
  error       TEXT,
  created_at  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS doc_cache (
  repo_id    INTEGER NOT NULL REFERENCES repos(id),
  path       TEXT    NOT NULL,
  etag       TEXT,
  content    TEXT    NOT NULL DEFAULT '',
  fetched_at INTEGER NOT NULL,
  PRIMARY KEY (repo_id, path)
);

-- prs_opened is how many pull requests this author has opened in this
-- repository, not how many of their own issues they fixed. Correlating each
-- issue with a later PR would cost a request per issue, and the PR count
-- carries the same signal for a two-request cache entry. The prompt states
-- what is actually measured.
CREATE TABLE IF NOT EXISTS author_stats (
  repo_id       INTEGER NOT NULL REFERENCES repos(id),
  login         TEXT    NOT NULL,
  issues_opened INTEGER NOT NULL DEFAULT 0,
  prs_opened    INTEGER NOT NULL DEFAULT 0,
  computed_at   INTEGER NOT NULL,
  PRIMARY KEY (repo_id, login)
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
CREATE INDEX IF NOT EXISTS idx_issues_score
  ON issues(score DESC) WHERE state = 'scored';
CREATE INDEX IF NOT EXISTS idx_outbox_pending
  ON outbox(sent_at) WHERE sent_at IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_outbox_dedupe
  ON outbox(dedupe_key) WHERE dedupe_key IS NOT NULL;
```

### 6.1 States

| State | Meaning |
|---|---|
| `baseline` | Open when the repo was adopted. Never processed, never surfaced. |
| `new` | Inserted, awaiting enrichment |
| `enriched` | Context fetched, awaiting filter and triage |
| `scored` | Has a composite, awaiting the push decision |
| `pushed` | Individual Slack alert sent |
| `tracked` | Operator pressed Track |
| `skipped` | Operator pressed Skip |
| `snoozed` | Operator pressed Snooze, will resurface |
| `rejected` | Killed by filter, veto, or the junk floor. See `reject_reason`. |
| `aged_out` | Older than `freshness_cutoff_min` at first sight |
| `claimed_before_push` | The pre-push re-check found it taken |
| `expired` | Aged out of `scored` or `snoozed` |
| `backfilled` | Ingested by `dibs backfill`. Scored for calibration, never surfaced. Terminal, and claimed by no worker, which is what keeps it off Slack. Still carries the `reject_reason` the filter or the floor would have given it. |

`reject_reason` values: `already_assigned`, `linked_pr_exists`, `killfile_label`,
`claimed_in_thread`, `too_thin`, `veto_self_fixing`, `veto_already_taken`,
`veto_poorly_scoped`, `below_floor`.

`already_assigned` is retained for rows written before assignment became a score
penalty. Nothing produces it now.

## 7. Pipeline

Workers are independent goroutines. They never talk to each other. Each polls
the database for claimable rows, leases them, works, and commits the result and
the state change in one transaction.

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

Workers: poller (one goroutine per enabled repo), enricher, triager, notifier,
router, reaper.

### 7.1 Repo adoption

The first time a repo is seen in `repos.yaml`:

1. `GET /repos/{owner}/{name}/issues?state=open&sort=created&direction=desc&per_page=100`
2. Insert every returned item with `state = 'baseline'`, skipping any with a
   non-null `pull_request` field.
3. Set `waterline_at` to the newest `created_at` returned, or to `now` if the
   repo has no open issues.
4. Set `adopted_at = now`.

Adoption produces zero enrichment requests and zero model calls. This applies on
first run and on every `SIGHUP` that adds a repo.

### 7.2 Poll tick

Per repo, on a ticker at `poll_interval_sec`:

1. Read `Budget()`. Discard the reading if its reset time has already passed:
   only a response refreshes the budget, and a paused poller sends none, so a
   stale reading would hold the pause open forever. Below `rate_limit_slow_at`,
   double this repo's interval **for this tick only** rather than persisting
   it, so a recovered budget restores the normal cadence instead of
   compounding. Below `rate_limit_pause_at`, skip the tick and enqueue one
   deduplicated Slack warning per reset window.
2. If `last_polled_at` is set and `now - last_polled_at > gap_warn_min`, enqueue
   one Slack line: `gap: {n}m, resuming`. This is the only sleep handling.
   There is no backfill.
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

### 7.3 Enrichment

Claims rows in `new`. For each, fetch concurrently:

1. **Timeline.** `GET /repos/{o}/{n}/issues/{num}/timeline?per_page=100` with
   `Accept: application/vnd.github.mockingbird-preview+json`. Set `HasLinkedPR`
   if any event is `cross-referenced` or `connected` and its source is an open
   pull request.
2. **Comments.** `GET /repos/{o}/{n}/issues/{num}/comments?per_page=20`
3. **Docs.** `CONTRIBUTING.md`, falling back to `.github/CONTRIBUTING.md`, via
   `GET /repos/{o}/{n}/contents/{path}`. Cache in `doc_cache` with ETags,
   refresh only when `fetched_at` is over 7 days old. Base64-decode `content`.
   Prefer the section on claiming or assignment if a heading identifies one;
   most of a CONTRIBUTING file is setup boilerplate that costs tokens and says
   nothing useful. Truncate to `max_doc_chars`.
4. **Author stats.** Use `author_stats` when computed within 7 days. Otherwise
   `GET /search/issues?q=repo:{o}/{n}+author:{login}+type:issue`. **The search
   endpoint has a separate 30/min limit.** Cache aggressively, never call it in
   a loop, and degrade to zeroed stats on failure rather than erroring the row.

Commit the context and set `state = 'enriched'`. Budget is 3 to 4 requests per
new issue.

### 7.4 Deterministic filter

Pure functions, no I/O, fully unit-testable. Runs before any Claude spend.

```go
type Result struct {
    Rejected bool
    Reason   string
}
func Apply(iss Issue, ctx gh.Context, cfg Config) Result
```

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

`question`, `discussion`, and `rfc` are also absent from the killfile. They
apply a `-10` score penalty each in `score.go` instead.

An assignee is not a rejection either. Projects hand assignments out by
round-robin, by CODEOWNERS, and by bot, so an assigned issue with no other
activity is usually still open in practice, and killing on the field costs real
issues. `score.go` applies a `-10` penalty instead, and the model is shown the
assignee list so its `already_taken` veto can weigh it. The strong signals stay
here: a linked pull request, and a person saying in the thread that they have
taken it.

`already_assigned` remains a valid `reject_reason` because rows recorded before
this change still carry it. Nothing produces it now.

Strip fenced code blocks and blockquotes from comment bodies before running the
claim regex, so an issue quoting the phrase "working on this" is not rejected.

```go
var claimRe = regexp.MustCompile(`(?i)\b(i'?ll take (this|it)|i am taking|taking this|` +
    `working on (this|it)|i'?m on (this|it)|/assign|\.take|pr (is )?incoming|` +
    `i have a (patch|fix)|opened a pr|submitted a pr|will submit)\b`)
```

Every check here rests on somebody having acted, not on a field having been
set. Do not add checks beyond this list.

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
- Parse `X-RateLimit-Remaining` and `X-RateLimit-Reset` on every response
- On `403` or `429` carrying `Retry-After`, sleep that long and retry once
- On `403` with `X-RateLimit-Remaining: 0`, sleep until reset plus 5s jitter
- On 5xx, exponential backoff with full jitter at 1s, 2s, 4s, three attempts max
- Never cache an absolute deadline across a possible suspend. Read `time.Now()`
  fresh at every use.

## 9. Triage

### 9.1 Request

`POST https://api.anthropic.com/v1/messages`

Headers: `x-api-key`, `anthropic-version: 2023-06-01`,
`content-type: application/json`.

Body fields:

- `model` from config
- `max_tokens: 1200`
- `system`: the embedded prompt
- `messages`: one user message, §9.3
- `output_config.format`: the JSON schema in §9.4, so the API constrains the
  response shape

**Do not send `temperature`, `top_p`, or `top_k`.** Sampling parameters are
rejected with a 400 on Sonnet 5 and every current model generation. Do not send
a `thinking` block either; the default is correct here.

Retry up to `max_retries` on 429, 5xx, or a response that fails schema
validation, with exponential backoff. Record every attempt in `triage_runs`
including failures.

On terminal failure, set `state = 'scored'` with `score = 50` and a
`triage_json` marked degraded. **Fail open.** A model outage must never silently
swallow issues.

Enforce `daily_call_cap`. On breach the worker fails open: it stops calling the
model and stores the issue at the degraded score of 50, exactly as a model
outage does. Parking the row until the window rolls over would surface it hours
late, which for this system is indistinguishable from losing it. The cap still
bounds spend, because no further call is made.

On breach, log at error level, enqueue one Slack
warning, and stop triaging until the next local midnight. Enforce
`monthly_budget_usd` the same way at 80% (warn) and 100% (stop), computed by
summing `triage_runs` for the calendar month against the configured rates. Both
warnings deduplicate per period.

Store the exact rendered user message in `triage_input` before the call.
`dibs replay` depends on it.

### 9.2 System prompt

Embed verbatim via `go:embed prompt.txt`.

```
You are triaging GitHub issues for an experienced open-source contributor who
wants to find issues worth claiming. He watches a small set of repositories he
already knows and trusts. Your job is not to protect him from these projects.
It is to save him from three specific kinds of dead end, and otherwise to rank
what remains.

VETOES. For each of the three below, judge whether it applies and how confident
you are on a 0.0 to 1.0 scale. Cite the specific text that convinced you in the
"evidence" field. If nothing in the issue supports a veto, set confidence to 0.0
and leave evidence empty. Do not speculate.

Fire a veto only on visible evidence in the issue or thread. Absence of
information is not evidence. When genuinely unsure, report a confidence between
0.3 and 0.5 rather than committing. The scoring layer treats that band as a
penalty rather than a kill, which is the right outcome for an ambiguous case.

  self_fixing     The author or a maintainer indicates they are already writing
                  a fix. Look for stated intent such as "PR incoming", "I have a
                  patch", "will open a PR shortly". Weigh the author's history:
                  if they routinely open issues and also open pull requests in
                  this repository, raise confidence.

  already_taken   Someone other than the contributor has claimed this in the
                  comments, or a maintainer has said it is spoken for.

                  An assignee alone is not a claim. Many projects assign
                  automatically, by round-robin, by CODEOWNERS, or by a bot, and
                  an assigned issue with no other activity is usually still open
                  in practice. Raise confidence when the assignee has also
                  commented, or when the thread shows work under way. Otherwise
                  leave it low and let the score carry the doubt.

  poorly_scoped   There is no way to tell what "done" looks like. No
                  reproduction, no expected behaviour, no acceptance criteria,
                  or a request so broad it is really a project. A short issue is
                  not automatically poorly scoped. "Typo in README line 40" is
                  perfectly scoped.

DIMENSIONS. Score each 1 to 5, where 5 is always the most favourable. Justify
each in the "why" field, which must reference specific issue text and must be at
most 20 words. Terse is required, not preferred. These fields dominate the cost
of running this system. Evidence fields are capped at 20 words too.

  scope_clarity          5 = the finish line is unambiguous.
                         1 = you could not tell when you were done.

  concreteness           5 = reproduction steps, versions, expected versus
                             actual, or explicit acceptance criteria.
                         1 = a vague assertion that something is wrong.

  blast_radius           5 = a change confined to one function or file.
                         1 = threads through the architecture, touches public
                             API, or needs a migration.

  maintainer_invitation  5 = a maintainer explicitly invited outside help,
                             sketched the approach, or labelled it for
                             contributors.
                         1 = no maintainer has engaged at all.

  contention_risk        5 = unlikely anyone else is racing for it.
                         1 = popular repo, many reactions, high-visibility issue
                             that will be taken within the hour.

EFFORT. Estimate a range in hours for a competent engineer unfamiliar with this
codebase, including reading the surrounding code and writing tests. State
confidence as low, medium, or high. The contributor's ceiling is
{{EFFORT_CEILING}} hours. Exceeding it is informational, not disqualifying.

STACK. List the languages and technologies the fix would touch, lowercase.

SUMMARY. Write two short positives, the strongest reasons to take this, and one
top_risk, the single biggest reason to hesitate. These are read on a phone.
Under 60 characters each, no trailing punctuation.
```

### 9.3 User message

```
REPOSITORY: {owner}/{name}
RECEPTIVITY: {receptivity}
CONTRIBUTOR STACKS: {comma-separated}

ISSUE #{number}: {title}
AUTHOR: {login} (association: {author_assoc})
LABELS: {comma-separated}
OPENED: {relative, e.g. "4 minutes ago"}

--- BODY ---
{body, truncated to max_body_chars}

--- COMMENTS ({n}) ---
{login} ({assoc}, {relative}): {body}
... truncated to max_thread_chars

--- AUTHOR HISTORY IN THIS REPO ---
Opened {issues_opened} issues and {prs_opened} pull requests.

--- CONTRIBUTING.md EXCERPT ---
{cached, truncated to max_doc_chars, or "not available"}
```

Always include `author_assoc`. `OWNER`, `MEMBER`, and `COLLABORATOR` authors are
far more likely to be filing self-tracking tickets, and it is the single most
predictive field in the payload.

### 9.4 Response schema

Pass this as `output_config.format` with `strict` validation and
`additionalProperties: false` throughout.

```json
{
  "vetoes": {
    "self_fixing":   {"confidence": 0.0, "evidence": ""},
    "already_taken": {"confidence": 0.0, "evidence": ""},
    "poorly_scoped": {"confidence": 0.0, "evidence": ""}
  },
  "dimensions": {
    "scope_clarity":         {"score": 0, "why": ""},
    "concreteness":          {"score": 0, "why": ""},
    "blast_radius":          {"score": 0, "why": ""},
    "maintainer_invitation": {"score": 0, "why": ""},
    "contention_risk":       {"score": 0, "why": ""}
  },
  "effort": {"low_hours": 0.0, "high_hours": 0.0, "confidence": "medium"},
  "stack": [],
  "positives": ["", ""],
  "top_risk": ""
}
```

### 9.5 Composite

**Go computes the score. The model never returns a final number.** Model
composites drift between calls and make retuning impossible.

```go
func Composite(t Response, iss Issue, repo Repo, cfg Config) (
    score int, rejected bool, reason string)
```

```
1. Hard veto: for each of the three, if confidence > veto_confidence,
   return 0, true, "veto_<name>".

   The comparison is exclusive so that a veto_confidence of 1.00 disables
   killing entirely: no confidence can exceed 1.00, so every veto lands as the
   penalty in step 5 instead. That is the shipped default.

2. Weighted base, weights summing to 100, each dimension 1..5:
     base  = Σ (weight_i × score_i)      // 100..500
     score = base / 5.0                  // 20..100

3. Stack multiplier:
     effective = repo.stacks if non-empty else profile.stacks
     score *= stack_match if intersect(response.stack, effective) else
              stack_mismatch

4. Receptivity multiplier: score *= receptivity_<repo.receptivity>

5. Soft-veto penalty: for each veto with 0.35 <= confidence <= veto_confidence,
   score -= 10.

6. Label penalty: -10 for each of question, discussion, rfc present on the
   issue.

7. Assignee penalty: -10 once if anyone other than profile.github_login holds
   the assignment. His own assignment costs nothing.

8. Clamp to [0, 100], round to nearest int.
```

Routing:

| Score | Result |
|---|---|
| `>= junk_floor` | `state = 'scored'`, queued for the push worker |
| `< junk_floor` | `state = 'rejected'`, `reject_reason = 'below_floor'` |

Effort over `effort_ceiling_hours` is not a veto. It shows as a badge in the
alert and nothing more.

### 9.6 Guard against a compliant model

- Reject any dimension scoring 5 whose `why` is under 20 characters. Retry once
  with an appended instruction to cite specific text. If it fails again,
  downgrade that dimension to 4.
- The reaper logs the daily score distribution. If the median exceeds 75 for
  three consecutive days, emit a warning that the rubric needs recalibration.

## 10. Slack

### 10.1 Connection

Use `slack-go/slack/socketmode`. Outbound WebSocket only, so no tunnel, no
static IP, no inbound firewall rule.

App scopes: `chat:write`, `im:write`, `commands`. Enable Socket Mode with an
app-level token carrying `connections:write`. Enable Interactivity; Socket Mode
needs no Request URL.

Every outbound message is written to `outbox` before send and marked `sent_at`
on acknowledgement. On startup and on reconnect, flush unsent rows older than 30
seconds. Warning messages set `dedupe_key` so the unique index enforces
deduplication rather than application code.

### 10.2 Push worker

Claims rows in `scored`. For each, in order:

1. **Re-check freshness.** `GET /repos/{o}/{n}/issues/{num}`. If it has picked
   up an assignee it did not carry when dibs scored it, or is closed, or a new
   comment matches the claim regex, set `state = 'claimed_before_push'` and
   stop. Do not notify.

   The comparison is against the recorded assignee list, not against emptiness.
   An issue that was already assigned when the filter passed it was surfaced on
   purpose, and rejecting it here would undo that decision one stage later.
   `profile.github_login` never counts as a new assignee: him taking the issue
   is the outcome. A name that arrived during the window is a real claim, which
   is why the change is watched rather than the field.
2. Build the Block Kit message and enqueue it to `outbox`.
3. Set `state = 'pushed'`, `surfaced_at = now`.

The message has to be decidable without opening GitHub. That is the whole speed
argument.

```
🟢 82 · owner/repo · #4821 · 4m ago
Fix race condition in worker pool drain

2–6h · go · ~48 open issues

✓ Repro steps and a failing test included
✓ Maintainer sketched the fix in a comment
⚠ Two 👀 reactions already

[ Open issue ↗ ]  [ Track ]  [ Skip ]  [ Snooze 1h ]  [ Why? ]
```

Blocks: a `section` with the title as a link and a context line above carrying
score badge, repo, number, and relative age; a `context` with effort range,
stack, and repo open-issue count; a `section` with two `✓` positives and one
`⚠` risk; an `actions` row of five buttons.

Badge: 🟢 at 70+, 🟡 below. Effort above the ceiling appends `⏳`.

Action IDs are `dibs_open`, `dibs_track`, `dibs_skip`, `dibs_snooze`,
`dibs_why`, each with `value` set to the issue's primary key.

**`Open issue ↗` is a plain URL button. It records nothing.** That is where
claiming happens, in the browser, by hand.

There is no cap on push volume.

### 10.3 Router

| Action | Behaviour |
|---|---|
| `dibs_track` | Re-fetch assignee and timeline first. If an assignee arrived that the issue did not carry when it was scored, or a PR appeared, replace the message with a warning and do not track. Otherwise insert into `tracked`, set `state = 'tracked'`, `decided_at = now`, and update the message to a compact tracked state. |
| `dibs_skip` | `state = 'skipped'`, `decided_at = now`, collapse the message to one grey line. Keep the score for calibration. |
| `dibs_snooze` | `state = 'snoozed'`, `decided_at = now`, requeue for +1h. On resurfacing, re-run the deterministic filter and the pre-push freshness check first. Most snoozed issues get claimed within the hour and should die silently. |
| `dibs_why` | Post the stored dimension breakdown as a threaded reply: each dimension with its score and `why`, plus all three veto confidences. Reads `triage_json`. Never re-calls the model. |

Every handler must be idempotent. Guard with a conditional update
(`UPDATE issues SET state=? WHERE id=? AND state IN (...)`) and check
`RowsAffected` so a double-tap cannot double-process.

## 11. Reaper

One ticker, every 15 minutes.

- **Expire leases** whose `lease_expires_at` has passed, returning rows to their
  prior state for retry.
- **Expire issues** in `scored` or `snoozed` older than `expire_after_days`.
- **Resurface** snoozed issues past their deadline, through the filter and the
  freshness check.
- **Track outcomes** for `tracked` rows where `last_checked` is over 6 hours
  old: fetch the issue, set `assigned_at` if `profile.github_login` is now an
  assignee, set `pr_url` from timeline cross-references, set `closed_at` and
  `outcome` on close. Read-only, always.
- **Nudge** on any `tracked` row older than 5 days with no `pr_url`. One Slack
  message asking whether it is still live. Releasing it is a browser action, not
  a Dibs action.
- **Recompute cadence** daily at `cadence_recompute_hour` from `issues_last_30d`:

| Issues per week | Interval |
|---|---|
| 10+ | 30s |
| 3–9 | 60s |
| 1–2 | 5m |
| 0 | 30m |

- **Log the daily score distribution** (count, median, p90) at info level.
- **Track spend** per §9.1.

## 12. Milestones

Each milestone runs on its own. Do not build ahead of the current one.

**M1, poller.** Config with validation, schema and migration, `gh.Client` with
ETags, repo adoption, poll tick with PR filtering and freshness cutoff, rate
limit guard, lease scaffolding. Prints new issues as JSON.

*Accepts when:* adding a repo produces zero enrichment requests and zero model
calls; a 24h run against the configured repos stays under 1,000 requests per
hour with 304s dominating the logs; no issue is emitted twice; an issue backdated
past the cutoff lands in `aged_out` without any follow-up request.

**M2, filter.** Timeline, comments, doc cache, author stats, deterministic
rejection.

*Accepts when:* every rejection carries a `reject_reason`; each killfile label
and each claim regex alternative has a passing test; an issue quoting "working
on this" inside a fenced code block is not rejected; an assigned issue with no
other activity survives both the filter and the pre-push re-check.

**M3, triage and Slack.** Claude integration with structured outputs, composite
scoring, `dibs replay`, Socket Mode, Block Kit, router, outbox, pre-push
freshness re-check.

*Accepts when:* 100 issues score without a schema-validation failure; token
usage is recorded per run; the degraded fallback path works against a bad API
key; a sleep and wake cycle loses no queued message; double-tapping every button
is a no-op; Track refuses an issue newly assigned between push and click; an
issue newly assigned between scoring and push lands in `claimed_before_push` with no Slack
message.

**M4, calibration.** Run for a week. Use `dibs replay` and
`dibs status --missed` to set `junk_floor` and `veto_confidence` on evidence.

*Accepts when:* both numbers are changed or explicitly reaffirmed against
recorded decisions; every veto that killed something worth taking is identified;
the measured per-issue token cost replaces the estimate in `PLAN.md`.

**M5, reaper and deployment.** Outcome tracking, nudges, cadence recompute,
spend tracking, systemd unit.

*Accepts when:* the daemon survives reboot; `kill -9` mid-enrichment loses no
row and the issue completes on the next lease cycle; `SIGHUP` reloads repos
without a restart and adopts new ones at the waterline.

## 13. Testing

- **`gh`** — `httptest` fixtures covering 304 handling, PR-in-issues filtering,
  `Retry-After` backoff, and the rate-limit pause.
- **`filter`** — table-driven over every killfile label and every claim regex
  alternative, with explicit negative cases.
- **`score.go`** — pure-function tests for each multiplier, penalty, and veto
  path independently. This is the highest-value test file in the repo.
- **`store`** — in-memory SQLite. Assert that every state transition is
  idempotent and that a lease claimed twice yields one worker.
- **Integration** — one test running the full pipeline against fixture GitHub
  and Anthropic servers, asserting a push lands in a fake Slack sink.
- **`make eval`** — golden fixtures in `testdata/triage/`, at least 12 real
  issues with hand-assigned bands (veto, low, mid, high), one per veto category.
  Asserts band membership, never exact scores. **This runs manually, not in CI.**
  It calls a live model, so a model version change would otherwise redden the
  build for a reason unrelated to the code.

## 14. Deployment

Ubuntu LTS, systemd user service with lingering.

`deploy/dibs.service`, installed to `~/.config/systemd/user/dibs.service`:

```ini
[Unit]
Description=Dibs - GitHub issue radar
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%h/.local/bin/dibs run
EnvironmentFile=%h/.config/dibs/env
Restart=always
RestartSec=10
StartLimitIntervalSec=300
StartLimitBurst=5
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

### 14.1 Laptop settings

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

### 14.2 Makefile

`build`, `test`, `lint`, `eval` (the manual golden-fixture run),
`install` (build, copy to `~/.local/bin`, restart the unit),
`cross` (linux/amd64 and linux/arm64), `logs` (`journalctl --user -u dibs -f`).

## 15. Failure modes

| Failure | Handling |
|---|---|
| Pull requests returned by the issues endpoint | Check `pull_request` on every item. The most common bug in this pattern. |
| Process killed mid-stage | Lease expires, row is retried. Every stage is safe to run twice. |
| Laptop sleeps | Gap warning, advance the waterline, no backfill |
| Clock jump after suspend | Never cache absolute deadlines. Read `time.Now()` fresh. |
| Slack disconnects | Durable outbox, flush on reconnect |
| Button double-tap | Conditional `UPDATE ... WHERE state IN (...)`, check `RowsAffected` |
| Duplicate warning messages | `outbox.dedupe_key` unique index |
| Claude returns an invalid response | Retry twice, then fail open at 50 with a degraded marker |
| Claude outage | Fail open. Never silently swallow issues. |
| Search API rate limit | Cache author stats 7 days, degrade to zeros |
| Secondary rate limit | Honour `Retry-After`, cap concurrency at `max_concurrent`. Concurrency is punished harder than volume. |
| A repo goes dark from a label bot | `dibs status --repos` shows the histogram; reaper warns above 90% rejection over 7 days |
| Model scores everything high | The `why`-length check, plus the median-over-75 warning |
| Alert fatigue | Log the skip rate daily. Above 50% with a floor set, warn to raise `junk_floor`. At a floor of zero the rate is the operator's own choice, so it stays a log line. |
