package store

import (
	"context"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Deduplication is enforced by the unique index, not by application code, which
// is the whole reason a paused poller cannot flood the channel. The partial
// index needs its WHERE clause repeated in the conflict target, and getting
// that wrong fails every enqueue rather than just the duplicate ones.
func TestEnqueueDeduplicates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()

	first, err := s.Enqueue(ctx, Outgoing{
		Kind: "warning", DedupeKey: "rate_limit:1700", Payload: `{"text":"paused"}`,
	}, now)
	if err != nil || !first {
		t.Fatalf("first enqueue: queued=%v err=%v", first, err)
	}

	second, err := s.Enqueue(ctx, Outgoing{
		Kind: "warning", DedupeKey: "rate_limit:1700", Payload: `{"text":"paused again"}`,
	}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second enqueue: %v", err)
	}
	if second {
		t.Fatal("the same dedupe key was queued twice")
	}

	pending, err := s.Pending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending messages, want 1", len(pending))
	}
}

// Messages without a dedupe key are all distinct. Every alert is one of these,
// so a NULL key must not collide with another NULL key.
func TestEnqueueWithoutADedupeKey(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()

	for i := 0; i < 3; i++ {
		queued, err := s.Enqueue(ctx, Outgoing{Kind: "alert", Payload: `{"text":"hi"}`}, now)
		if err != nil || !queued {
			t.Fatalf("enqueue %d: queued=%v err=%v", i, queued, err)
		}
	}
	pending, err := s.Pending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 3 {
		t.Fatalf("got %d pending messages, want 3", len(pending))
	}
}

// Marking a message sent twice is a no-op, which is what a retried flush after
// a crash between the send and the acknowledgement looks like.
func TestMarkSentIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0).UTC()

	if _, err := s.Enqueue(ctx, Outgoing{Kind: "alert", Payload: "{}"}, now); err != nil {
		t.Fatal(err)
	}
	pending, err := s.Pending(ctx, 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%d err=%v", len(pending), err)
	}
	for i := 0; i < 2; i++ {
		if err := s.MarkSent(ctx, pending[0].ID, now); err != nil {
			t.Fatalf("mark sent %d: %v", i, err)
		}
	}
	left, err := s.Pending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("%d messages still pending after being sent", len(left))
	}
}
