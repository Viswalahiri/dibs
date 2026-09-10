package notify

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/slack-go/slack"
)

// Slack is the real transport. It only ever posts. Dibs holds no inbound
// connection and handles no interactions, so there is no tunnel, no static IP,
// and no firewall rule to open.
type Slack struct {
	api *slack.Client
	log *slog.Logger

	// deliverTo is "dm" or a channel ID from config. A DM has to be opened
	// before it can be posted to, so the resolved ID is cached here.
	deliverTo string

	mu        sync.Mutex
	channelID string
}

func NewSlack(botToken, deliverTo string, log *slog.Logger) *Slack {
	return &Slack{
		api:       slack.New(botToken),
		log:       log,
		deliverTo: deliverTo,
	}
}

// Post sends one message. slack-go retries rate limits internally, so a
// returned error means the message genuinely did not land and the outbox row
// stays pending.
func (s *Slack) Post(ctx context.Context, m Message) error {
	channel, err := s.channel(ctx)
	if err != nil {
		return err
	}
	_, _, err = s.api.PostMessageContext(ctx, channel,
		slack.MsgOptionText(m.Text, false),
		slack.MsgOptionBlocks(m.Blocks.BlockSet...))
	if err != nil {
		return fmt.Errorf("post to slack: %w", err)
	}
	return nil
}

// channel resolves and caches the destination. "dm" means a direct message to
// the account the bot token belongs to, which needs a conversation opened
// first.
func (s *Slack) channel(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.channelID != "" {
		return s.channelID, nil
	}
	if s.deliverTo != "dm" {
		s.channelID = s.deliverTo
		return s.channelID, nil
	}

	auth, err := s.api.AuthTestContext(ctx)
	if err != nil {
		return "", fmt.Errorf("slack auth test: %w", err)
	}
	conv, _, _, err := s.api.OpenConversationContext(ctx, &slack.OpenConversationParameters{
		Users: []string{auth.UserID},
	})
	if err != nil {
		return "", fmt.Errorf("open slack dm: %w", err)
	}
	s.channelID = conv.ID
	return s.channelID, nil
}
