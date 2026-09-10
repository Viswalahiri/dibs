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
	Score        sql.NullInt64
	EffortLowH   float64
	EffortHighH  float64
	SurfacedAt   time.Time
	DecidedAt    time.Time

	// EnrichmentJSON is the encoded gh.Context the enricher fetched, and
	// TriageJSON is the model's raw response. store treats both as opaque;
	// gh and triage own their shapes. Each is empty until its stage has run.
	EnrichmentJSON string
	TriageJSON     string
}

// ErrNotClaimable is returned when a transition is attempted against a row
// that has already moved on, which is what a double-tapped Slack button looks
// like.
var ErrNotClaimable = errors.New("issue is no longer in the expected state")

const issueColumns = `id, repo_id, number, node_id, title, body, html_url, author,
	author_assoc, labels, assignees, comment_count, created_at, first_seen_at, state,
	COALESCE(reject_reason, ''), score,
	COALESCE(effort_low_h, 0), COALESCE(effort_high_h, 0),
	surfaced_at, decided_at,
	COALESCE(enrichment_json, ''), COALESCE(triage_json, '')`

func scanIssue(sc interface{ Scan(...any) error }) (Issue, error) {
	var (
		iss           Issue
		labelsJSON    string
		assigneesJSON string
		created       int64
		firstSeen     int64
		surfaced      sql.NullInt64
		decided       sql.NullInt64
	)
	err := sc.Scan(&iss.ID, &iss.RepoID, &iss.Number, &iss.NodeID, &iss.Title, &iss.Body,
		&iss.HTMLURL, &iss.Author, &iss.AuthorAssoc, &labelsJSON, &assigneesJSON,
		&iss.CommentCount, &created, &firstSeen, &iss.State, &iss.RejectReason,
		&iss.Score, &iss.EffortLowH, &iss.EffortHighH,
		&surfaced, &decided, &iss.EnrichmentJSON, &iss.TriageJSON)
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
	iss.DecidedAt = timeOrZero(decided)
	return iss, nil
}

// Insert records a newly seen issue. It reports inserted=false when the
// (repo, number) pair is already known, which is how a repeated page or an
// overlapping backfill stays a no-op instead of emitting the issue twice.
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

// SaveEnrichment stores the fetched context and moves the issue from new to
// enriched in one statement, so a crash can never leave a row marked enriched
// with nothing to show for it. It returns ErrNotClaimable when the row has
// already moved on, which is what a retried lease looks like.
func (s *Store) SaveEnrichment(ctx context.Context, id int64, enrichmentJSON string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE issues
		   SET enrichment_json = ?, state = ?, lease_owner = NULL, lease_expires_at = NULL
		 WHERE id = ? AND state = ?`,
		enrichmentJSON, string(StateEnriched), id, string(StateNew))
	if err != nil {
		return fmt.Errorf("save enrichment for issue %d: %w", id, err)
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

// TriageRun is one attempt at scoring an issue, successful or not. Failures
// are recorded too: a run of them is what the outage and spend warnings read.
type TriageRun struct {
	IssueID   int64
	Model     string
	InputTok  int
	OutputTok int
	LatencyMS int
	OK        bool
	Err       string
}

func (s *Store) RecordTriageRun(ctx context.Context, run TriageRun, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO triage_runs (issue_id, model, input_tok, output_tok, latency_ms, ok, error, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		run.IssueID, run.Model, run.InputTok, run.OutputTok, run.LatencyMS,
		boolToInt(run.OK), nullIfEmpty(run.Err), at.Unix())
	if err != nil {
		return fmt.Errorf("record triage run for issue %d: %w", run.IssueID, err)
	}
	return nil
}

// TriageResult is everything one scoring produces. It is written with the state
// change in a single statement, so an issue is never left marked scored with no
// score on it.
type TriageResult struct {
	Score        int
	EffortLowH   float64
	EffortHighH  float64
	Input        string
	JSON         string
	State        State
	RejectReason RejectReason
}

// SaveTriage commits the score and moves the issue out of enriched.
func (s *Store) SaveTriage(ctx context.Context, id int64, r TriageResult) error {
	if err := checkTransition(StateEnriched, r.State); err != nil {
		return fmt.Errorf("save triage for issue %d: %w", id, err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE issues
		   SET score = ?, effort_low_h = ?, effort_high_h = ?,
		       triage_input = ?, triage_json = ?,
		       state = ?, reject_reason = ?,
		       lease_owner = NULL, lease_expires_at = NULL
		 WHERE id = ? AND state = ?`,
		r.Score, r.EffortLowH, r.EffortHighH, r.Input, r.JSON,
		string(r.State), nullIfEmpty(string(r.RejectReason)), id, string(StateEnriched))
	if err != nil {
		return fmt.Errorf("save triage for issue %d: %w", id, err)
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

// CallsSince counts triage attempts made since t. The daily cap is enforced
// against this rather than an in-memory counter, so a restart cannot reset it.
func (s *Store) CallsSince(ctx context.Context, t time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM triage_runs WHERE created_at >= ?`, t.Unix()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count triage runs: %w", err)
	}
	return n, nil
}

// TokensSince sums what has been spent since t, for the monthly budget check.
func (s *Store) TokensSince(ctx context.Context, t time.Time) (input, output int, err error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input_tok), 0), COALESCE(SUM(output_tok), 0)
		  FROM triage_runs WHERE created_at >= ?`, t.Unix())
	if err := row.Scan(&input, &output); err != nil {
		return 0, 0, fmt.Errorf("sum triage tokens: %w", err)
	}
	return input, output, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// Surface records that the Slack alert went out. scored -> pushed.
func (s *Store) Surface(ctx context.Context, id int64, at time.Time) error {
	return s.move(ctx, id, StateScored, StatePushed, "", "surfaced_at", at)
}

// ClaimedBeforePush records that the re-check immediately before sending found
// the issue taken. No notification is sent.
func (s *Store) ClaimedBeforePush(ctx context.Context, id int64) error {
	return s.move(ctx, id, StateScored, StateClaimedBeforePush, "", "", time.Time{})
}

// Decide records the operator's button press. It is idempotent by construction:
// the update is conditional on the row still being in `pushed`, so a
// double-tapped button finds nothing to change and reports ErrNotClaimable.
func (s *Store) Decide(ctx context.Context, id int64, to State, at time.Time) error {
	return s.move(ctx, id, StatePushed, to, "", "decided_at", at)
}

// Resurface returns a snoozed issue to the push queue. The push worker's
// freshness re-check runs again from there, which is why snoozing routes back
// through `scored` rather than straight to a second notification.
func (s *Store) Resurface(ctx context.Context, id int64) error {
	return s.move(ctx, id, StateSnoozed, StateScored, "", "", time.Time{})
}

// Expire ages an issue out of the queue. Legal from scored and snoozed.
func (s *Store) Expire(ctx context.Context, id int64, from State) error {
	return s.move(ctx, id, from, StateExpired, "", "", time.Time{})
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

// Track records that the operator took the issue. Claiming itself happens in
// the browser; this is only dibs' note that it did.
func (s *Store) Track(ctx context.Context, issueID int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tracked (issue_id, tracked_at, outcome)
		VALUES (?, ?, 'pending')
		ON CONFLICT(issue_id) DO NOTHING`, issueID, at.Unix())
	if err != nil {
		return fmt.Errorf("track issue %d: %w", issueID, err)
	}
	return nil
}

// Snoozed returns issues whose snooze has run out. The deadline is decided_at
// plus the snooze window; there is no separate column because a snoozed issue
// has exactly one decision on it.
func (s *Store) Snoozed(ctx context.Context, before time.Time) ([]Issue, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+issueColumns+` FROM issues
		  WHERE state = ? AND decided_at IS NOT NULL AND decided_at < ?
		  ORDER BY decided_at`, string(StateSnoozed), before.Unix())
	if err != nil {
		return nil, fmt.Errorf("read snoozed issues: %w", err)
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

// Triaged returns every issue that has a stored model response, newest first.
// `dibs replay` reads these to re-score under a candidate config without
// spending anything.
func (s *Store) Triaged(ctx context.Context) ([]Issue, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+issueColumns+` FROM issues
		  WHERE triage_json IS NOT NULL AND triage_json != ''
		  ORDER BY first_seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("read triaged issues: %w", err)
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

// ScoredSince returns issues first seen at or after t that carry a score,
// newest first. The status reports read it.
func (s *Store) ScoredSince(ctx context.Context, t time.Time) ([]Issue, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+issueColumns+` FROM issues
		  WHERE score IS NOT NULL AND first_seen_at >= ?
		  ORDER BY first_seen_at DESC`, t.Unix())
	if err != nil {
		return nil, fmt.Errorf("read scored issues: %w", err)
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
