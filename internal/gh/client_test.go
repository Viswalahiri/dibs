package gh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recordedSleeps replaces the real wait so retry paths run instantly and the
// delays themselves can be asserted.
type recordedSleeps struct {
	mu   sync.Mutex
	list []time.Duration
}

func (r *recordedSleeps) sleep(ctx context.Context, d time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, d)
	return nil
}

func (r *recordedSleeps) all() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.list...)
}

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *recordedSleeps) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	sleeps := &recordedSleeps{}
	return New("test-token", 4, WithBaseURL(srv.URL), WithSleep(sleeps.sleep)), sleeps
}

func TestGetSendsRequiredHeaders(t *testing.T) {
	var got http.Header
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Write([]byte(`{}`))
	})
	if _, err := c.get(context.Background(), "/x", `W/"etag"`, defaultAccept); err != nil {
		t.Fatalf("Get: %v", err)
	}
	for header, want := range map[string]string{
		"Authorization":        "Bearer test-token",
		"Accept":               "application/vnd.github+json",
		"X-Github-Api-Version": apiVersion,
		"User-Agent":           userAgent,
		"If-None-Match":        `W/"etag"`,
	} {
		if got.Get(header) != want {
			t.Errorf("%s = %q, want %q", header, got.Get(header), want)
		}
	}
}

func TestGetOmitsIfNoneMatchWhenEtagIsEmpty(t *testing.T) {
	var had bool
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, had = r.Header["If-None-Match"]
		w.Write([]byte(`{}`))
	})
	if _, err := c.get(context.Background(), "/x", "", defaultAccept); err != nil {
		t.Fatal(err)
	}
	if had {
		t.Error("If-None-Match was sent with an empty etag")
	}
}

func TestNotModifiedReturnsNoBodyAndKeepsTheEtag(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	resp, err := c.get(context.Background(), "/x", `W/"abc"`, defaultAccept)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !resp.NotModified {
		t.Error("NotModified = false, want true")
	}
	if resp.Body != nil {
		t.Errorf("body = %q, want nil on 304", resp.Body)
	}
	if resp.ETag != `W/"abc"` {
		t.Errorf("etag = %q, want the request etag carried through", resp.ETag)
	}
}

func TestBudgetTracksRateLimitHeaders(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute).Unix()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4321")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		w.Write([]byte(`{}`))
	})
	if remaining, _ := c.Budget(); remaining != -1 {
		t.Errorf("remaining before any request = %d, want -1 for unknown", remaining)
	}
	if _, err := c.get(context.Background(), "/x", "", defaultAccept); err != nil {
		t.Fatal(err)
	}
	remaining, resetAt := c.Budget()
	if remaining != 4321 {
		t.Errorf("remaining = %d, want 4321", remaining)
	}
	if resetAt.Unix() != reset {
		t.Errorf("resetAt = %v, want %v", resetAt.Unix(), reset)
	}
}

func TestRetryAfterIsHonouredOnceThenFails(t *testing.T) {
	var calls int32
	c, sleeps := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"message":"secondary rate limit"}`))
	})
	_, err := c.get(context.Background(), "/x", "", defaultAccept)
	if err == nil {
		t.Fatal("want an error after the single retry is exhausted")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("made %d requests, want 2 (original plus one retry)", got)
	}
	if got := sleeps.all(); len(got) != 1 || got[0] != 7*time.Second {
		t.Errorf("sleeps = %v, want one 7s wait from the Retry-After header", got)
	}
}

func TestRetryAfterSucceedsOnTheRetry(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	})
	resp, err := c.get(context.Background(), "/x", "", defaultAccept)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(resp.Body) != `{"ok":true}` {
		t.Errorf("body = %q, want the retried response", resp.Body)
	}
}

func TestPrimaryRateLimitWaitsForReset(t *testing.T) {
	reset := time.Now().Add(10 * time.Minute)
	var calls int32
	c, sleeps := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Write([]byte(`{}`))
	})
	if _, err := c.get(context.Background(), "/x", "", defaultAccept); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got := sleeps.all()
	if len(got) != 1 {
		t.Fatalf("sleeps = %v, want exactly one wait for the reset", got)
	}
	// Roughly ten minutes, plus the 5s pad and jitter.
	if got[0] < 9*time.Minute || got[0] > 11*time.Minute {
		t.Errorf("waited %v, want about 10m until the reset", got[0])
	}
}

func TestServerErrorsBackOffAndGiveUp(t *testing.T) {
	var calls int32
	c, sleeps := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	})
	_, err := c.get(context.Background(), "/x", "", defaultAccept)
	if err == nil {
		t.Fatal("want an error after three failed attempts")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("made %d requests, want 3", got)
	}
	waits := sleeps.all()
	if len(waits) != 2 {
		t.Fatalf("sleeps = %v, want 2 waits between 3 attempts", waits)
	}
	// Full jitter, so assert the ceilings escalate rather than exact values.
	if waits[0] > time.Second || waits[1] > 2*time.Second {
		t.Errorf("waits = %v, want them bounded by 1s then 2s", waits)
	}
	if waits[1] <= 0 || waits[0] <= 0 {
		t.Errorf("waits = %v, want positive delays", waits)
	}
}

func TestServerErrorRecoversWithinTheAttemptBudget(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	})
	resp, err := c.get(context.Background(), "/x", "", defaultAccept)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(resp.Body) != `{"ok":true}` {
		t.Errorf("body = %q", resp.Body)
	}
}

func TestNotFoundIsATerminalStatusError(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	})
	_, err := c.get(context.Background(), "/repos/a/b/issues", "", defaultAccept)
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a *StatusError", err)
	}
	if !se.NotFound() {
		t.Errorf("NotFound() = false for status %d", se.Status)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("made %d requests, want 1; a 404 must not be retried", got)
	}
}

func TestConcurrencyIsCapped(t *testing.T) {
	const limit = 3
	var inFlight, peak int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			old := atomic.LoadInt32(&peak)
			if n <= old || atomic.CompareAndSwapInt32(&peak, old, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New("t", limit, WithBaseURL(srv.URL))
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := c.get(context.Background(), fmt.Sprintf("/x/%d", i), "", defaultAccept); err != nil {
				t.Errorf("Get: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if got := atomic.LoadInt32(&peak); got > limit {
		t.Errorf("peak concurrency = %d, want at most %d", got, limit)
	}
}

func TestContextCancellationStopsRetrying(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.get(ctx, "/x", "", defaultAccept); !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestGetJSONDecodesAndSkipsOn304(t *testing.T) {
	status := http.StatusOK
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if status == http.StatusNotModified {
			w.WriteHeader(status)
			return
		}
		w.Write([]byte(`[{"number":7,"title":"x"}]`))
	})
	var issues []Issue
	if _, notModified, err := c.GetJSON(context.Background(), "/x", "", &issues); err != nil || notModified {
		t.Fatalf("GetJSON: notModified=%v err=%v", notModified, err)
	}
	if len(issues) != 1 || issues[0].Number != 7 {
		t.Fatalf("decoded %+v", issues)
	}

	status = http.StatusNotModified
	before := len(issues)
	_, notModified, err := c.GetJSON(context.Background(), "/x", `W/"e"`, &issues)
	if err != nil {
		t.Fatal(err)
	}
	if !notModified {
		t.Error("notModified = false, want true")
	}
	if len(issues) != before {
		t.Error("304 overwrote the destination")
	}
}

func TestIsPullRequestDistinguishesListItems(t *testing.T) {
	if (Issue{}).IsPullRequest() {
		t.Error("a plain issue reported itself as a pull request")
	}
	if !(Issue{PullRequest: &PullRequestRef{URL: "u"}}).IsPullRequest() {
		t.Error("a pull request was not detected; it would be surfaced as an issue")
	}
}

func TestBudgetIgnoresOtherResourceBuckets(t *testing.T) {
	coreReset := time.Now().Add(30 * time.Minute).Unix()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search/issues" {
			w.Header().Set("X-RateLimit-Resource", "search")
			w.Header().Set("X-RateLimit-Limit", "30")
			w.Header().Set("X-RateLimit-Remaining", "28")
			w.Header().Set("X-RateLimit-Reset",
				strconv.FormatInt(time.Now().Add(time.Minute).Unix(), 10))
		} else {
			w.Header().Set("X-RateLimit-Resource", "core")
			w.Header().Set("X-RateLimit-Limit", "5000")
			w.Header().Set("X-RateLimit-Remaining", "4900")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(coreReset, 10))
		}
		w.Write([]byte(`{}`))
	})
	ctx := context.Background()
	if _, err := c.get(ctx, "/repos/acme/widget/issues", "", defaultAccept); err != nil {
		t.Fatal(err)
	}
	if _, err := c.get(ctx, "/search/issues?q=x", "", defaultAccept); err != nil {
		t.Fatal(err)
	}
	remaining, resetAt := c.Budget()
	if remaining != 4900 {
		t.Errorf("remaining = %d, want the core budget of 4900; search is its own bucket", remaining)
	}
	if resetAt.Unix() != coreReset {
		t.Errorf("resetAt = %d, want the core reset %d", resetAt.Unix(), coreReset)
	}
}

func TestPrimaryRateLimitWaitsForTheRespondingBucketsReset(t *testing.T) {
	searchReset := time.Now().Add(40 * time.Second)
	var calls int32
	c, sleeps := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			w.Header().Set("X-RateLimit-Resource", "core")
			w.Header().Set("X-RateLimit-Remaining", "4900")
			w.Header().Set("X-RateLimit-Reset",
				strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
			w.Write([]byte(`{}`))
		case 2:
			w.Header().Set("X-RateLimit-Resource", "search")
			w.Header().Set("X-RateLimit-Limit", "30")
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(searchReset.Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
		default:
			w.Header().Set("X-RateLimit-Resource", "search")
			w.Header().Set("X-RateLimit-Remaining", "29")
			w.Write([]byte(`{}`))
		}
	})
	ctx := context.Background()
	if _, err := c.get(ctx, "/x", "", defaultAccept); err != nil {
		t.Fatal(err)
	}
	if _, err := c.get(ctx, "/search/issues?q=x", "", defaultAccept); err != nil {
		t.Fatalf("Get: %v", err)
	}
	got := sleeps.all()
	if len(got) != 1 {
		t.Fatalf("sleeps = %v, want exactly one wait for the reset", got)
	}
	if got[0] > 2*time.Minute {
		t.Errorf("waited %v for a search reset 40s out; that is the core window, not search's", got[0])
	}
}
