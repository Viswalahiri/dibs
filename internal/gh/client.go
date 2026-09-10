// Package gh talks to the GitHub REST API. Every request is a GET; dibs holds
// a read-only token and any write path here would be a specification
// violation.
package gh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultBaseURL = "https://api.github.com"
	userAgent      = "dibs/1.0"
	apiVersion     = "2022-11-28"
	defaultAccept  = "application/vnd.github+json"

	// coreResource is the only bucket the poller budgets against. GitHub
	// meters each resource separately and names the one a response was billed
	// to in X-RateLimit-Resource.
	coreResource = "core"

	// timelineAccept is the preview media type the timeline endpoint was
	// introduced under. The endpoint is generally available now and works
	// without it, but GitHub still honours it and sending it costs nothing.
	timelineAccept = "application/vnd.github.mockingbird-preview+json"

	// maxBackoffWait bounds any sleep derived from a server header. GitHub
	// resets hourly, so anything longer means a skewed clock or a bad header
	// and is not worth blocking a poller over.
	maxBackoffWait = time.Hour + time.Minute
)

// Response is the outcome of one conditional GET. On NotModified the body is
// nil and no rate-limit quota was consumed, which is what makes 45-second
// polling affordable.
type Response struct {
	Body        []byte
	ETag        string
	Status      int
	NotModified bool
	// Link is the raw Link header, used for pagination by the backfill
	// subcommand. The poller never paginates.
	Link string
}

// StatusError is an HTTP status dibs cannot act on, such as 404 for a renamed
// repository.
type StatusError struct {
	Status int
	Path   string
	Body   string
}

func (e *StatusError) Error() string {
	body := strings.TrimSpace(e.Body)
	if len(body) > 200 {
		body = body[:200] + "..."
	}
	return fmt.Sprintf("GET %s: HTTP %d: %s", e.Path, e.Status, body)
}

// NotFound reports whether the failure was a 404, which for a repository means
// it was renamed, deleted, or made private.
func (e *StatusError) NotFound() bool { return e.Status == http.StatusNotFound }

type Client struct {
	http    *http.Client
	token   string
	baseURL string

	// sem caps requests in flight. GitHub's secondary rate limit punishes
	// concurrency far harder than volume, so this is a concurrency gate and
	// not a requests-per-second limiter.
	sem chan struct{}

	// sleep is injectable so the retry paths can be tested without real waits.
	sleep func(context.Context, time.Duration) error

	mu        sync.RWMutex
	remaining int
	limit     int
	resetAt   time.Time
}

type Option func(*Client)

// WithBaseURL points the client at a test server.
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = strings.TrimSuffix(u, "/") } }

// WithSleep replaces the wait used by the retry and rate-limit paths.
func WithSleep(f func(context.Context, time.Duration) error) Option {
	return func(c *Client) { c.sleep = f }
}

func New(token string, maxConcurrent int, opts ...Option) *Client {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	c := &Client{
		http:      &http.Client{Timeout: 30 * time.Second},
		token:     token,
		baseURL:   defaultBaseURL,
		sem:       make(chan struct{}, maxConcurrent),
		sleep:     sleepCtx,
		remaining: -1, // unknown until the first response
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Budget reports the last observed core rate-limit state. remaining is -1
// before the first response.
func (c *Client) Budget() (remaining int, resetAt time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.remaining, c.resetAt
}

// Get performs a conditional GET, retrying transient failures. Pass a non-empty
// etag to send If-None-Match.
func (c *Client) Get(ctx context.Context, path, etag string) (Response, error) {
	return c.get(ctx, path, etag, defaultAccept)
}

func (c *Client) get(ctx context.Context, path, etag, accept string) (Response, error) {
	const maxServerErrorAttempts = 3
	serverErrors := 0
	retriedAfterHeader := false

	for {
		resp, retry, err := c.attempt(ctx, path, etag, accept)
		if err != nil {
			return Response{}, err
		}
		if retry == nil {
			return resp, nil
		}

		wait := retry.wait
		switch retry.kind {
		case retryAfterHeader:
			if retriedAfterHeader {
				return Response{}, retry.err
			}
			retriedAfterHeader = true
		case retryServerError:
			// The wait escalates across attempts, so it is computed here where
			// the attempt count lives rather than inside a single attempt.
			wait = backoff(serverErrors)
			serverErrors++
			if serverErrors >= maxServerErrorAttempts {
				return Response{}, retry.err
			}
		case retryRateLimited:
			// Waiting for the reset is not an attempt against a budget; the
			// alternative is failing a poll we know would be rejected.
		}
		if err := c.sleep(ctx, wait); err != nil {
			return Response{}, err
		}
	}
}

type retryKind int

const (
	retryAfterHeader retryKind = iota
	retryServerError
	retryRateLimited
)

// retryDecision asks the caller to wait and try again. wait is set only when
// the delay comes from a server header; for server errors the caller derives
// it from how many attempts have already failed.
type retryDecision struct {
	kind retryKind
	wait time.Duration
	err  error
}

// attempt performs a single request. It returns a non-nil retryDecision when
// the caller should wait and try again.
func (c *Client) attempt(ctx context.Context, path, etag, accept string) (Response, *retryDecision, error) {
	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return Response{}, nil, ctx.Err()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return Response{}, nil, fmt.Errorf("build request for %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	httpResp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, nil, ctx.Err()
		}
		return Response{}, &retryDecision{
			kind: retryServerError,
			err:  fmt.Errorf("GET %s: %w", path, err),
		}, nil
	}
	defer httpResp.Body.Close()

	c.recordBudget(httpResp.Header)

	// A 304 costs no quota. It is the reason this cadence is affordable.
	if httpResp.StatusCode == http.StatusNotModified {
		io.Copy(io.Discard, httpResp.Body)
		return Response{Status: httpResp.StatusCode, NotModified: true, ETag: etag}, nil, nil
	}

	body, readErr := io.ReadAll(httpResp.Body)
	if readErr != nil {
		return Response{}, &retryDecision{
			kind: retryServerError,
			err:  fmt.Errorf("GET %s: read body: %w", path, readErr),
		}, nil
	}

	switch {
	case httpResp.StatusCode >= 200 && httpResp.StatusCode < 300:
		return Response{
			Body:   body,
			ETag:   httpResp.Header.Get("ETag"),
			Status: httpResp.StatusCode,
			Link:   httpResp.Header.Get("Link"),
		}, nil, nil

	case httpResp.StatusCode >= 500:
		return Response{}, &retryDecision{
			kind: retryServerError,
			err:  &StatusError{Status: httpResp.StatusCode, Path: path, Body: string(body)},
		}, nil

	case httpResp.StatusCode == http.StatusForbidden || httpResp.StatusCode == http.StatusTooManyRequests:
		statusErr := &StatusError{Status: httpResp.StatusCode, Path: path, Body: string(body)}

		if d, ok := retryAfter(httpResp.Header); ok {
			return Response{}, &retryDecision{kind: retryAfterHeader, wait: d, err: statusErr}, nil
		}
		// A primary rate-limit exhaustion arrives as 403 with the remaining
		// budget at zero. Wait for the reset rather than hammering. The reset
		// comes from this response, because an exhausted search bucket opens
		// within the minute while the tracked core reset can be an hour out.
		if remaining, ok := headerInt(httpResp.Header, "X-RateLimit-Remaining"); ok && remaining == 0 {
			reset := c.resetTime()
			if secs, ok := headerInt(httpResp.Header, "X-RateLimit-Reset"); ok {
				reset = time.Unix(int64(secs), 0).UTC()
			}
			wait := time.Until(reset) + 5*time.Second + jitter(5*time.Second)
			return Response{}, &retryDecision{kind: retryRateLimited, wait: clampWait(wait), err: statusErr}, nil
		}
		return Response{}, nil, statusErr

	default:
		return Response{}, nil, &StatusError{Status: httpResp.StatusCode, Path: path, Body: string(body)}
	}
}

// GetJSON performs a conditional GET and decodes the body into v. On 304 it
// leaves v untouched and reports notModified.
func (c *Client) GetJSON(ctx context.Context, path, etag string, v any) (resp Response, notModified bool, err error) {
	return c.getJSON(ctx, path, etag, defaultAccept, v)
}

func (c *Client) getJSON(ctx context.Context, path, etag, accept string, v any) (resp Response, notModified bool, err error) {
	resp, err = c.get(ctx, path, etag, accept)
	if err != nil {
		return Response{}, false, err
	}
	if resp.NotModified {
		return resp, true, nil
	}
	if err := json.Unmarshal(resp.Body, v); err != nil {
		return Response{}, false, fmt.Errorf("GET %s: decode response: %w", path, err)
	}
	return resp, false, nil
}

// User returns the authenticated login, so startup can confirm which account
// the token belongs to.
func (c *Client) User(ctx context.Context) (AuthenticatedUser, error) {
	var u AuthenticatedUser
	if _, _, err := c.GetJSON(ctx, "/user", "", &u); err != nil {
		return AuthenticatedUser{}, err
	}
	return u, nil
}

// RateLimit reads the current budget without spending any of it.
func (c *Client) RateLimit(ctx context.Context) (RateLimit, error) {
	var rl RateLimit
	if _, _, err := c.GetJSON(ctx, "/rate_limit", "", &rl); err != nil {
		return RateLimit{}, err
	}
	return rl, nil
}

func (c *Client) recordBudget(h http.Header) {
	// Search allows thirty a minute where core allows five thousand an hour,
	// so a search reading recorded here reads as a nearly drained core budget
	// and pauses the poller on a full tank.
	if res := h.Get("X-RateLimit-Resource"); res != "" && res != coreResource {
		return
	}
	remaining, okRemaining := headerInt(h, "X-RateLimit-Remaining")
	limit, okLimit := headerInt(h, "X-RateLimit-Limit")
	reset, okReset := headerInt(h, "X-RateLimit-Reset")
	if !okRemaining && !okReset {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if okRemaining {
		c.remaining = remaining
	}
	if okLimit {
		c.limit = limit
	}
	if okReset {
		c.resetAt = time.Unix(int64(reset), 0).UTC()
	}
}

func (c *Client) resetTime() time.Time {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.resetAt
}

func headerInt(h http.Header, key string) (int, bool) {
	raw := h.Get(key)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false
	}
	return n, true
}

// retryAfter reads the Retry-After header, which GitHub sends as a number of
// seconds on secondary rate limits.
func retryAfter(h http.Header) (time.Duration, bool) {
	raw := h.Get("Retry-After")
	if raw == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(raw); err == nil {
		return clampWait(time.Duration(secs) * time.Second), true
	}
	if t, err := http.ParseTime(raw); err == nil {
		return clampWait(time.Until(t)), true
	}
	return 0, false
}

func clampWait(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > maxBackoffWait {
		return maxBackoffWait
	}
	return d
}

// backoff returns 1s, 2s, or 4s with full jitter.
func backoff(attempt int) time.Duration {
	base := time.Second << attempt
	if base > 4*time.Second {
		base = 4 * time.Second
	}
	return jitter(base)
}

func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(d)) + 1)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
