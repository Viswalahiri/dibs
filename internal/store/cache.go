package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// cacheTTL is how long a cached document or author-stats row stays usable.
// Both back an endpoint dibs must not hammer: contents is cheap but pointless
// to re-read, and the search endpoint has its own 30-per-minute limit.
const cacheTTL = 7 * 24 * time.Hour

// Doc is a cached repository document, currently only CONTRIBUTING.md.
type Doc struct {
	Path      string
	ETag      string
	Content   string
	FetchedAt time.Time
}

// Fresh reports whether the cached copy is recent enough to use without
// revalidating.
func (d Doc) Fresh(now time.Time) bool {
	return !d.FetchedAt.IsZero() && now.Sub(d.FetchedAt) < cacheTTL
}

// Doc returns the cached copy, or a zero Doc when nothing is cached. A miss is
// not an error; the caller fetches.
func (s *Store) Doc(ctx context.Context, repoID int64, path string) (Doc, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT path, COALESCE(etag, ''), content, fetched_at
		   FROM doc_cache WHERE repo_id = ? AND path = ?`, repoID, path)
	var (
		d       Doc
		fetched int64
	)
	err := row.Scan(&d.Path, &d.ETag, &d.Content, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return Doc{}, nil
	}
	if err != nil {
		return Doc{}, fmt.Errorf("read doc cache %s: %w", path, err)
	}
	d.FetchedAt = time.Unix(fetched, 0).UTC()
	return d, nil
}

func (s *Store) PutDoc(ctx context.Context, repoID int64, d Doc) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO doc_cache (repo_id, path, etag, content, fetched_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(repo_id, path) DO UPDATE SET
			etag = excluded.etag,
			content = excluded.content,
			fetched_at = excluded.fetched_at`,
		repoID, d.Path, nullIfEmpty(d.ETag), d.Content, d.FetchedAt.Unix())
	if err != nil {
		return fmt.Errorf("write doc cache %s: %w", d.Path, err)
	}
	return nil
}

// AuthorStats is how much one person files and fixes in one repository. An
// author who opens issues and also opens pull requests here is the profile
// most likely to be filing a self-tracking ticket rather than an invitation,
// which is exactly what the self_fixing veto is looking for.
type AuthorStats struct {
	Login        string
	IssuesOpened int
	PRsOpened    int
	ComputedAt   time.Time
}

func (a AuthorStats) Fresh(now time.Time) bool {
	return !a.ComputedAt.IsZero() && now.Sub(a.ComputedAt) < cacheTTL
}

// AuthorStats returns the cached row, or a zero value on a miss. Zeroed stats
// are a legitimate answer: the search endpoint is rate-limited separately and
// degrading to zeros beats erroring the issue out of the pipeline.
func (s *Store) AuthorStats(ctx context.Context, repoID int64, login string) (AuthorStats, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT login, issues_opened, prs_opened, computed_at
		   FROM author_stats WHERE repo_id = ? AND login = ?`, repoID, login)
	var (
		a        AuthorStats
		computed int64
	)
	err := row.Scan(&a.Login, &a.IssuesOpened, &a.PRsOpened, &computed)
	if errors.Is(err, sql.ErrNoRows) {
		return AuthorStats{Login: login}, nil
	}
	if err != nil {
		return AuthorStats{}, fmt.Errorf("read author stats for %s: %w", login, err)
	}
	a.ComputedAt = time.Unix(computed, 0).UTC()
	return a, nil
}

func (s *Store) PutAuthorStats(ctx context.Context, repoID int64, a AuthorStats) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO author_stats (repo_id, login, issues_opened, prs_opened, computed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(repo_id, login) DO UPDATE SET
			issues_opened = excluded.issues_opened,
			prs_opened = excluded.prs_opened,
			computed_at = excluded.computed_at`,
		repoID, a.Login, a.IssuesOpened, a.PRsOpened, a.ComputedAt.Unix())
	if err != nil {
		return fmt.Errorf("write author stats for %s: %w", a.Login, err)
	}
	return nil
}
