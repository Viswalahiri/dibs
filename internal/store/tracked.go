package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Outcome is what became of an issue the operator took. It stays pending until
// the issue closes, which is the only moment dibs can tell the difference.
type Outcome string

const (
	OutcomePending Outcome = "pending"

	// OutcomeLanded means the issue closed with the operator's own pull
	// request linked to it. Dibs never checks whether that PR merged: a
	// maintainer closing the issue alongside it is the same signal for a
	// fraction of the requests.
	OutcomeLanded Outcome = "landed"

	// OutcomeLost means the issue closed without one. Somebody else got there,
	// or it was closed unfixed.
	OutcomeLost Outcome = "lost"
)

// Tracked is one issue the operator took, plus what dibs has since observed
// about it. Observation is read-only: dibs never assigns, comments, or opens
// anything on the operator's behalf.
type Tracked struct {
	IssueID     int64
	TrackedAt   time.Time
	AssignedAt  time.Time // zero until the operator holds the issue on GitHub
	PRURL       string    // the operator's own pull request, empty until one appears
	ClosedAt    time.Time
	Outcome     Outcome
	LastChecked time.Time
}

const trackedColumns = `issue_id, tracked_at, assigned_at, COALESCE(pr_url, ''),
	closed_at, outcome, last_checked`

func scanTracked(sc interface{ Scan(...any) error }) (Tracked, error) {
	var (
		t           Tracked
		trackedAt   int64
		assignedAt  sql.NullInt64
		closedAt    sql.NullInt64
		lastChecked sql.NullInt64
	)
	err := sc.Scan(&t.IssueID, &trackedAt, &assignedAt, &t.PRURL,
		&closedAt, &t.Outcome, &lastChecked)
	if err != nil {
		return Tracked{}, err
	}
	t.TrackedAt = time.Unix(trackedAt, 0).UTC()
	t.AssignedAt = timeOrZero(assignedAt)
	t.ClosedAt = timeOrZero(closedAt)
	t.LastChecked = timeOrZero(lastChecked)
	return t, nil
}

// TrackedDue returns pending issues that have not been looked at since
// checkedBefore, oldest first. Each one costs two requests to check, so the
// interval is what keeps outcome tracking free at this volume.
func (s *Store) TrackedDue(ctx context.Context, checkedBefore time.Time, limit int) ([]Tracked, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+trackedColumns+` FROM tracked
		 WHERE outcome = ?
		   AND (last_checked IS NULL OR last_checked < ?)
		 ORDER BY tracked_at LIMIT ?`,
		string(OutcomePending), checkedBefore.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("read tracked issues: %w", err)
	}
	defer rows.Close()
	return collectTracked(rows)
}

// NudgeDue returns pending issues taken before trackedBefore with no pull
// request of the operator's own on them yet.
func (s *Store) NudgeDue(ctx context.Context, trackedBefore time.Time) ([]Tracked, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+trackedColumns+` FROM tracked
		 WHERE outcome = ?
		   AND (pr_url IS NULL OR pr_url = '')
		   AND tracked_at < ?
		 ORDER BY tracked_at`,
		string(OutcomePending), trackedBefore.Unix())
	if err != nil {
		return nil, fmt.Errorf("read nudgeable issues: %w", err)
	}
	defer rows.Close()
	return collectTracked(rows)
}

func collectTracked(rows *sql.Rows) ([]Tracked, error) {
	var out []Tracked
	for rows.Next() {
		t, err := scanTracked(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveOutcome writes back what one check observed. Every field is set from the
// observation rather than merged, so running the check twice on the same issue
// leaves the same row.
func (s *Store) SaveOutcome(ctx context.Context, t Tracked, checkedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE tracked
		   SET assigned_at = ?, pr_url = ?, closed_at = ?, outcome = ?, last_checked = ?
		 WHERE issue_id = ?`,
		unixOrNil(t.AssignedAt), nullIfEmpty(t.PRURL), unixOrNil(t.ClosedAt),
		string(t.Outcome), checkedAt.Unix(), t.IssueID)
	if err != nil {
		return fmt.Errorf("save outcome for issue %d: %w", t.IssueID, err)
	}
	return nil
}
