package store

import (
	"context"
	"fmt"
	"time"
)

// StaleIssues returns issues sitting in state since before the cutoff, oldest
// first. The reaper reads it to age out issues the push worker never got to.
func (s *Store) StaleIssues(ctx context.Context, state State, seenBefore time.Time) ([]Issue, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+issueColumns+` FROM issues
		  WHERE state = ? AND first_seen_at < ?
		  ORDER BY first_seen_at`, string(state), seenBefore.Unix())
	if err != nil {
		return nil, fmt.Errorf("read stale %s issues: %w", state, err)
	}
	defer rows.Close()

	var out []Issue
	for rows.Next() {
		iss, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, iss)
	}
	return out, rows.Err()
}

// RejectionCounts reports how many issues a repository produced in [from, to)
// and how many of them dibs threw away. Baseline rows are excluded: they were
// never candidates. A repository rejecting nearly everything has usually
// acquired a label bot, not gone quiet.
func (s *Store) RejectionCounts(ctx context.Context, repoID int64, from, to time.Time) (seen, rejected int, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(state = ?), 0) FROM issues
		 WHERE repo_id = ? AND state != ?
		   AND first_seen_at >= ? AND first_seen_at < ?`,
		string(StateRejected), repoID, string(StateBaseline), from.Unix(), to.Unix())
	if err := row.Scan(&seen, &rejected); err != nil {
		return 0, 0, fmt.Errorf("count rejections for repo %d: %w", repoID, err)
	}
	return seen, rejected, nil
}

// IssuesSeenSince counts the issues a repository has produced since t,
// baseline rows excluded. It is what the cadence recompute reads.
func (s *Store) IssuesSeenSince(ctx context.Context, repoID int64, t time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM issues
		 WHERE repo_id = ? AND state != ? AND first_seen_at >= ?`,
		repoID, string(StateBaseline), t.Unix()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count issues for repo %d: %w", repoID, err)
	}
	return n, nil
}

// SetCadence records a repository's observed volume and the polling interval
// derived from it. The poller re-reads the interval each round, so the change
// takes effect without a restart.
func (s *Store) SetCadence(ctx context.Context, repoID int64, issuesLast30d, intervalSec int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE repos SET issues_last_30d = ?, poll_interval_sec = ? WHERE id = ?`,
		issuesLast30d, intervalSec, repoID)
	if err != nil {
		return fmt.Errorf("set cadence for repo %d: %w", repoID, err)
	}
	return nil
}
