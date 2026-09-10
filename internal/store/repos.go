package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
)

// Repo is a persisted repository row. It carries both the operator's config
// (receptivity, stacks, notes) and the poller's runtime state (etag,
// waterline, adoption).
type Repo struct {
	ID              int64
	Owner           string
	Name            string
	Receptivity     config.Receptivity
	Stacks          []string
	Notes           string
	PollIntervalSec int
	ETag            string
	LastPolledAt    time.Time // zero until the first poll
	WaterlineAt     time.Time // issues created at or before this are never surfaced
	AdoptedAt       time.Time // zero until adoption has run
	IssuesLast30d   int
	Enabled         bool
}

func (r Repo) Slug() string { return r.Owner + "/" + r.Name }

// Adopted reports whether the repository has passed through adoption. An
// unadopted repository must be adopted before it is ever polled, otherwise its
// entire open issue list would arrive as new work.
func (r Repo) Adopted() bool { return !r.AdoptedAt.IsZero() }

// PollInterval is how long to wait between ticks for this repository.
func (r Repo) PollInterval() time.Duration {
	return time.Duration(r.PollIntervalSec) * time.Second
}

// SyncRepos reconciles the database with repos.yaml. Listed repositories are
// inserted or have their operator-owned fields refreshed; runtime state
// (etag, waterline, adoption) is never touched, so a SIGHUP does not re-adopt
// or replay anything. Repositories no longer listed are disabled rather than
// deleted, keeping their issue history intact.
func (s *Store) SyncRepos(ctx context.Context, want []config.Repo, defaultIntervalSec int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	keep := make([]string, 0, len(want))
	for _, r := range want {
		stacks, err := json.Marshal(nonNil(r.Stacks))
		if err != nil {
			return fmt.Errorf("encode stacks for %s: %w", r.Slug, err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO repos (owner, name, receptivity, stacks, notes, poll_interval_sec, enabled)
			VALUES (?, ?, ?, ?, ?, ?, 1)
			ON CONFLICT(owner, name) DO UPDATE SET
				receptivity = excluded.receptivity,
				stacks      = excluded.stacks,
				notes       = excluded.notes,
				enabled     = 1`,
			r.Owner(), r.Name(), string(r.Receptivity), string(stacks), r.Notes, defaultIntervalSec)
		if err != nil {
			return fmt.Errorf("upsert repo %s: %w", r.Slug, err)
		}
		keep = append(keep, strings.ToLower(r.Slug))
	}

	// Disable anything no longer listed. Comparing lowercased slugs matches the
	// duplicate check in config, which is also case-insensitive.
	rows, err := tx.QueryContext(ctx, `SELECT id, owner, name FROM repos WHERE enabled = 1`)
	if err != nil {
		return err
	}
	var drop []int64
	for rows.Next() {
		var id int64
		var owner, name string
		if err := rows.Scan(&id, &owner, &name); err != nil {
			rows.Close()
			return err
		}
		slug := strings.ToLower(owner + "/" + name)
		if !slices.Contains(keep, slug) {
			drop = append(drop, id)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, id := range drop {
		if _, err := tx.ExecContext(ctx, `UPDATE repos SET enabled = 0 WHERE id = ?`, id); err != nil {
			return fmt.Errorf("disable repo %d: %w", id, err)
		}
	}
	return tx.Commit()
}

const repoColumns = `id, owner, name, receptivity, stacks, notes, poll_interval_sec,
	COALESCE(etag, ''), last_polled_at, waterline_at, adopted_at, issues_last_30d, enabled`

func scanRepo(sc interface{ Scan(...any) error }) (Repo, error) {
	var (
		r          Repo
		stacksJSON string
		lastPolled sql.NullInt64
		waterline  int64
		adopted    sql.NullInt64
		enabled    int
	)
	err := sc.Scan(&r.ID, &r.Owner, &r.Name, &r.Receptivity, &stacksJSON, &r.Notes,
		&r.PollIntervalSec, &r.ETag, &lastPolled, &waterline, &adopted, &r.IssuesLast30d, &enabled)
	if err != nil {
		return Repo{}, err
	}
	if err := json.Unmarshal([]byte(stacksJSON), &r.Stacks); err != nil {
		return Repo{}, fmt.Errorf("decode stacks for %s/%s: %w", r.Owner, r.Name, err)
	}
	r.LastPolledAt = timeOrZero(lastPolled)
	r.WaterlineAt = time.Unix(waterline, 0).UTC()
	r.AdoptedAt = timeOrZero(adopted)
	r.Enabled = enabled == 1
	return r, nil
}

func (s *Store) EnabledRepos(ctx context.Context) ([]Repo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+repoColumns+` FROM repos WHERE enabled = 1 ORDER BY owner, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) RepoBySlug(ctx context.Context, owner, name string) (Repo, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+repoColumns+` FROM repos WHERE owner = ? AND name = ?`, owner, name)
	return scanRepo(row)
}

// MarkAdopted records the waterline established by adoption. Everything at or
// before waterline is invisible forever after.
func (s *Store) MarkAdopted(ctx context.Context, repoID int64, waterline, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE repos SET adopted_at = ?, waterline_at = ?, last_polled_at = ? WHERE id = ?`,
		now.Unix(), waterline.Unix(), now.Unix(), repoID)
	return err
}

// RecordPoll stores the results of one tick. The waterline only ever moves
// forward, so a repository that returns an out-of-order page cannot rewind it
// and replay issues that were already seen.
func (s *Store) RecordPoll(ctx context.Context, repoID int64, etag string, polledAt, waterline time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE repos
		   SET etag = ?,
		       last_polled_at = ?,
		       waterline_at = MAX(waterline_at, ?)
		 WHERE id = ?`,
		nullIfEmpty(etag), polledAt.Unix(), waterline.Unix(), repoID)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *Store) RepoByID(ctx context.Context, id int64) (Repo, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+repoColumns+` FROM repos WHERE id = ?`, id)
	return scanRepo(row)
}
