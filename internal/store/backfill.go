package store

import (
	"context"
	"fmt"
)

// BackfillPending returns backfilled rows that have no score yet. A backfill
// killed partway through leaves rows like these, and the next run finishes
// them instead of starting over.
func (s *Store) BackfillPending(ctx context.Context, repoID int64) ([]Issue, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+issueColumns+` FROM issues
		  WHERE repo_id = ? AND state = ? AND score IS NULL
		  ORDER BY created_at DESC`, repoID, string(StateBackfilled))
	if err != nil {
		return nil, fmt.Errorf("read pending backfill: %w", err)
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

// SaveBackfill writes what one backfilled issue scored. The row never changes
// state: it was inserted terminal so no pipeline worker could claim it and
// push it, which is the whole contract of the subcommand. Writing only when
// the score is still null makes a repeated run a no-op.
func (s *Store) SaveBackfill(ctx context.Context, id int64, enrichmentJSON string, r TriageResult) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE issues
		   SET enrichment_json = ?, score = ?, effort_low_h = ?, effort_high_h = ?,
		       triage_input = ?, triage_json = ?, reject_reason = ?
		 WHERE id = ? AND state = ? AND score IS NULL`,
		enrichmentJSON, r.Score, r.EffortLowH, r.EffortHighH, r.Input, r.JSON,
		nullIfEmpty(string(r.RejectReason)), id, string(StateBackfilled))
	if err != nil {
		return fmt.Errorf("save backfill for issue %d: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotClaimable
	}
	return nil
}
