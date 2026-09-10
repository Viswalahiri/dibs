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
