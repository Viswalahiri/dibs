package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/slack-go/slack"

	"github.com/Viswalahiri/dibs/internal/store"
)

// Transport delivers one message. Slack is the real one; Console exists so the
// pipeline can be run end to end with only a GitHub token, before a Slack app
// is set up.
type Transport interface {
	Post(ctx context.Context, m Message) error
}

const senderBatch = 20

// Sender drains the outbox. It is the only thing that sends, and it marks a row
// sent only after the transport acknowledges, so a crash mid-send resends
// rather than loses.
type Sender struct {
	transport Transport
	store     *store.Store
	log       *slog.Logger
	now       func() time.Time
}

func NewSender(t Transport, s *store.Store, log *slog.Logger) *Sender {
	return &Sender{
		transport: t, store: s, log: log,
		now: func() time.Time { return time.Now().UTC() },
	}
}

func (s *Sender) Run(ctx context.Context) error {
	const idle = 2 * time.Second
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
		}
		n, err := s.Flush(ctx)
		if err != nil && ctx.Err() == nil {
			s.log.Error("flush outbox", "err", err)
		}
		if ctx.Err() != nil {
			return nil
		}
		if n > 0 {
			timer.Reset(0)
		} else {
			timer.Reset(idle)
		}
	}
}

// Flush sends everything pending and reports how many went out.
func (s *Sender) Flush(ctx context.Context) (int, error) {
	pending, err := s.store.Pending(ctx, senderBatch)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, row := range pending {
		var m Message
		if err := json.Unmarshal([]byte(row.Payload), &m); err != nil {
			// A payload dibs cannot read will never become readable. Marking it
			// sent stops it blocking the queue forever.
			s.log.Error("undecodable outbox row, dropping", "id", row.ID, "err", err)
			if err := s.store.MarkSent(ctx, row.ID, s.now()); err != nil {
				return sent, err
			}
			continue
		}
		if err := s.transport.Post(ctx, m); err != nil {
			// Leave it pending. The next tick, or the flush on reconnect,
			// picks it up again.
			return sent, fmt.Errorf("post outbox %d: %w", row.ID, err)
		}
		if err := s.store.MarkSent(ctx, row.ID, s.now()); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}

// Console prints what would have been sent. It makes the whole pipeline
// runnable with a GitHub token and an Anthropic key, before a Slack app exists.
type Console struct{}

func (Console) Post(_ context.Context, m Message) error {
	fmt.Fprintln(os.Stdout, "---")
	fmt.Fprintln(os.Stdout, m.Text)
	for _, b := range m.Blocks.BlockSet {
		if section, ok := b.(*slack.SectionBlock); ok && section.Text != nil {
			fmt.Fprintln(os.Stdout, "  "+section.Text.Text)
		}
		if ctxBlock, ok := b.(*slack.ContextBlock); ok {
			for _, el := range ctxBlock.ContextElements.Elements {
				if t, ok := el.(*slack.TextBlockObject); ok {
					fmt.Fprintln(os.Stdout, "  "+t.Text)
				}
			}
		}
	}
	return nil
}
