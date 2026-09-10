package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Issue mirrors a row of the issues table. Fields past State are filled in by
// later pipeline stages and are zero until then.
type Issue struct {
	ID           int64
	RepoID       int64
	Number       int
	NodeID       string
	Title        string
	Body         string
	HTMLURL      string
	Author       string
	AuthorAssoc  string
	Labels       []string
	Assignees    []string
	CommentCount int
	CreatedAt    time.Time
	FirstSeenAt  time.Time
	State        State
	RejectReason RejectReason
	SurfacedAt   time.Time
}

// ErrNotClaimable is returned when a transition is attempted against a row
// that has already moved on, which is what a retried lease looks like.
var ErrNotClaimable = errors.New("issue is no longer in the expected state")

const issueColumns = `id, repo_id, number, node_id, title, body, html_url, author,
	author_assoc, labels, assignees, comment_count, created_at, first_seen_at, state,
	COALESCE(reject_reason, ''), surfaced_at`

func scanIssue(sc interface{ Scan(...any) error }) (Issue, error) {
	var (
		iss           Issue
		labelsJSON    string
		assigneesJSON string
		created       int64
		firstSeen     int64
		surfaced      sql.NullInt64
	)
	err := sc.Scan(&iss.ID, &iss.RepoID, &iss.Number, &iss.NodeID, &iss.Title, &iss.Body,
		&iss.HTMLURL, &iss.Author, &iss.AuthorAssoc, &labelsJSON, &assigneesJSON,
		&iss.CommentCount, &created, &firstSeen, &iss.State, &iss.RejectReason,
		&surfaced)
	if err != nil {
		return Issue{}, err
	}
	if err := json.Unmarshal([]byte(labelsJSON), &iss.Labels); err != nil {
		return Issue{}, fmt.Errorf("decode labels for issue %d: %w", iss.ID, err)
	}
	if err := json.Unmarshal([]byte(assigneesJSON), &iss.Assignees); err != nil {
		return Issue{}, fmt.Errorf("decode assignees for issue %d: %w", iss.ID, err)
	}
	iss.CreatedAt = time.Unix(created, 0).UTC()
	iss.FirstSeenAt = time.Unix(firstSeen, 0).UTC()
	iss.SurfacedAt = timeOrZero(surfaced)
	return iss, nil
}

// Insert records a newly seen issue. It reports inserted=false when the
// (repo, number) pair is already known, which is how a repeated page or an
// re-poll of the same page stays a no-op instead of emitting it twice.
func (s *Store) Insert(ctx context.Context, iss Issue) (id int64, inserted bool, err error) {
	if !iss.State.Valid() {
		return 0, false, fmt.Errorf("insert issue %d: unknown state %q", iss.Number, iss.State)
	}
	labels, err := json.Marshal(nonNil(iss.Labels))
	if err != nil {
		return 0, false, fmt.Errorf("encode labels: %w", err)
	}
	assignees, err := json.Marshal(nonNil(iss.Assignees))
	if err != nil {
		return 0, false, fmt.Errorf("encode assignees: %w", err)
	}
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO issues (repo_id, number, node_id, title, body, html_url, author,
		                    author_assoc, labels, assignees, comment_count, created_at,
		                    first_seen_at, state, reject_reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(repo_id, number) DO NOTHING
		RETURNING id`,
		iss.RepoID, iss.Number, iss.NodeID, iss.Title, iss.Body, iss.HTMLURL, iss.Author,
		iss.AuthorAssoc, string(labels), string(assignees), iss.CommentCount,
		iss.CreatedAt.Unix(), iss.FirstSeenAt.Unix(), string(iss.State),
		nullIfEmpty(string(iss.RejectReason)))

	err = row.Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("insert issue %d: %w", iss.Number, err)
	}
	return id, true, nil
}

// Claim leases up to limit issues sitting in state, oldest first, and returns
// them. The lease and the read happen in one statement, so two workers can
// never take the same row. A worker that dies mid-flight leaves a lease that
// expires and the row is picked up again, which is why every stage has to be
// safe to run twice.
func (s *Store) Claim(ctx context.Context, state State, limit int, owner string, ttl time.Duration, now time.Time) ([]Issue, error) {
	if !state.Claimable() {
		return nil, fmt.Errorf("claim: %s is not a claimable state", state)
	}
	rows, err := s.db.QueryContext(ctx, `
		UPDATE issues
		   SET lease_owner = ?, lease_expires_at = ?
		 WHERE id IN (
		   SELECT id FROM issues
		    WHERE state = ?
		      AND (lease_expires_at IS NULL OR lease_expires_at < ?)
		    ORDER BY first_seen_at
		    LIMIT ?
		 )
		RETURNING `+issueColumns,
		owner, now.Add(ttl).Unix(), string(state), now.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("claim %s: %w", state, err)
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

// Advance moves a leased issue to its next state and clears the lease in the
// same statement. It refuses illegal transitions outright, and returns
// ErrNotClaimable when the row has already left `from`, which makes a repeated
// call a no-op rather than a double-process.
func (s *Store) Advance(ctx context.Context, id int64, from, to State, reason RejectReason) error {
	if err := checkTransition(from, to); err != nil {
		return fmt.Errorf("advance issue %d: %w", id, err)
	}
	if to == StateRejected && reason == "" {
		return fmt.Errorf("advance issue %d: a rejection needs a reason", id)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE issues
		   SET state = ?, reject_reason = ?, lease_owner = NULL, lease_expires_at = NULL
		 WHERE id = ? AND state = ?`,
		string(to), nullIfEmpty(string(reason)), id, string(from))
	if err != nil {
		return fmt.Errorf("advance issue %d: %w", id, err)
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

// ReleaseExpiredLeases returns abandoned rows to their worker's queue. Run it
// on boot and on the reaper's tick.
func (s *Store) ReleaseExpiredLeases(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE issues
		   SET lease_owner = NULL, lease_expires_at = NULL
		 WHERE lease_expires_at IS NOT NULL AND lease_expires_at < ?`, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("release expired leases: %w", err)
	}
	return res.RowsAffected()
}

func (s *Store) IssueByID(ctx context.Context, id int64) (Issue, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+issueColumns+` FROM issues WHERE id = ?`, id)
	return scanIssue(row)
}

// CountByState is used by the status subcommand and by the tests that assert
// adoption spent nothing.
func (s *Store) CountByState(ctx context.Context, repoID int64) (map[State]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT state, COUNT(*) FROM issues WHERE repo_id = ? GROUP BY state`, repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[State]int{}
	for rows.Next() {
		var st State
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// SaveVerdict commits the filter's decision and moves the issue out of `new`
// in one statement, so a crash can never leave a row marked ready without
// having been screened. It returns ErrNotClaimable when the row has already
// moved on, which is what a retried lease looks like.
func (s *Store) SaveVerdict(ctx context.Context, id int64, to State, reason RejectReason) error {
	if err := checkTransition(StateNew, to); err != nil {
		return fmt.Errorf("save verdict for issue %d: %w", id, err)
	}
	if to == StateRejected && reason == "" {
		return fmt.Errorf("save verdict for issue %d: a rejection needs a reason", id)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE issues
		   SET state = ?, reject_reason = ?,
		       lease_owner = NULL, lease_expires_at = NULL
		 WHERE id = ? AND state = ?`,
		string(to), nullIfEmpty(string(reason)), id, string(StateNew))
	if err != nil {
		return fmt.Errorf("save verdict for issue %d: %w", id, err)
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

// Surface records that the Slack alert went out. ready -> pushed, and the end
// of the issue's life in dibs.
func (s *Store) Surface(ctx context.Context, id int64, at time.Time) error {
	return s.move(ctx, id, StateReady, StatePushed, "", "surfaced_at", at)
}

// ClaimedBeforePush records that the re-check immediately before sending found
// the issue taken. No notification is sent.
func (s *Store) ClaimedBeforePush(ctx context.Context, id int64) error {
	return s.move(ctx, id, StateReady, StateClaimedBeforePush, "", "", time.Time{})
}

// Expire ages a ready issue out of the queue. It only fires when the push
// worker has been stuck long enough that surfacing the issue is pointless.
func (s *Store) Expire(ctx context.Context, id int64) error {
	return s.move(ctx, id, StateReady, StateExpired, "", "", time.Time{})
}

// move is the one conditional state change every transition above goes
// through. Checking the legal-transition table here means an illegal move is a
// programming error caught at the call, not a silently wrong row.
func (s *Store) move(ctx context.Context, id int64, from, to State, reason RejectReason,
	stampColumn string, stamp time.Time) error {

	if err := checkTransition(from, to); err != nil {
		return fmt.Errorf("issue %d: %w", id, err)
	}
	query := `UPDATE issues SET state = ?, reject_reason = ?,
	                 lease_owner = NULL, lease_expires_at = NULL`
	args := []any{string(to), nullIfEmpty(string(reason))}
	if stampColumn != "" {
		query += ", " + stampColumn + " = ?"
		args = append(args, stamp.Unix())
	}
	query += ` WHERE id = ? AND state = ?`
	args = append(args, id, string(from))

	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("issue %d: %s -> %s: %w", id, from, to, err)
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

// PushedSince returns issues notified at or after t, newest first. The status
// subcommand reads it.
func (s *Store) PushedSince(ctx context.Context, t time.Time) ([]Issue, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+issueColumns+` FROM issues
		  WHERE surfaced_at >= ? ORDER BY surfaced_at DESC`, t.Unix())
	if err != nil {
		return nil, fmt.Errorf("read pushed issues: %w", err)
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
