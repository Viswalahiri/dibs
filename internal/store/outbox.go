package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Outgoing is one message queued for Slack. Every send is written here first
// and marked sent on acknowledgement, so a disconnect or a crash between
// deciding to notify and notifying loses nothing.
type Outgoing struct {
	ID        int64
	Kind      string
	DedupeKey string
	Payload   string
	CreatedAt time.Time
}

// Enqueue adds a message and reports whether it was new. A row carrying a
// dedupe key that already exists is dropped, which is how the rate-limit and
// gap warnings stay at one per window without any application-side bookkeeping.
func (s *Store) Enqueue(ctx context.Context, o Outgoing, now time.Time) (bool, error) {
	row := s.db.QueryRowContext(ctx, `
		INSERT INTO outbox (kind, dedupe_key, payload, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(dedupe_key) WHERE dedupe_key IS NOT NULL DO NOTHING
		RETURNING id`,
		o.Kind, nullIfEmpty(o.DedupeKey), o.Payload, now.Unix())

	var id int64
	err := row.Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("enqueue %s: %w", o.Kind, err)
	}
	return true, nil
}

// Pending returns unsent messages, oldest first. Flushing on startup and on
// every reconnect is what makes a sleep and wake cycle lose no queued message.
func (s *Store) Pending(ctx context.Context, limit int) ([]Outgoing, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, COALESCE(dedupe_key, ''), payload, created_at
		  FROM outbox WHERE sent_at IS NULL ORDER BY created_at, id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("read outbox: %w", err)
	}
	defer rows.Close()

	var out []Outgoing
	for rows.Next() {
		var (
			o       Outgoing
			created int64
		)
		if err := rows.Scan(&o.ID, &o.Kind, &o.DedupeKey, &o.Payload, &created); err != nil {
			return nil, err
		}
		o.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, o)
	}
	return out, rows.Err()
}

// MarkSent records the acknowledgement. It is safe to call twice: the second
// call simply finds nothing left unsent.
func (s *Store) MarkSent(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE outbox SET sent_at = ? WHERE id = ? AND sent_at IS NULL`, at.Unix(), id)
	if err != nil {
		return fmt.Errorf("mark outbox %d sent: %w", id, err)
	}
	return nil
}
