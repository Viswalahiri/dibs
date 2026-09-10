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
  enrichment_json   TEXT,    -- gh.Context, fetched once and reused by filter and triage
  triage_input      TEXT,    -- exact rendered user message, for replay
  triage_json       TEXT,    -- raw model response
  lease_owner       TEXT,
  lease_expires_at  INTEGER,
  surfaced_at       INTEGER,
  decided_at        INTEGER,
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
-- issue with a later PR would cost a request per issue; the PR count carries
-- the same signal for a two-request cache entry. The prompt states what is
-- actually measured.
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
