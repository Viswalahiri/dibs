package notify

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
)

// Slack is the real transport. It connects over Socket Mode, which is an
// outbound WebSocket, so there is no tunnel, no static IP, and no inbound
// firewall rule to open on a laptop.
type Slack struct {
	api    *slack.Client
	socket *socketmode.Client
	log    *slog.Logger

	// deliverTo is "dm" or a channel ID from config. A DM has to be opened
	// before it can be posted to, so the resolved ID is cached here.
	deliverTo string

	mu        sync.Mutex
	channelID string
}

func NewSlack(botToken, appToken, deliverTo string, log *slog.Logger) *Slack {
	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	return &Slack{
		api:       api,
		socket:    socketmode.New(api),
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

// Run keeps the Socket Mode connection up and hands every button press to the
// router. It returns when ctx is cancelled.
func (s *Slack) Run(ctx context.Context, router *Router) error {
	go func() {
		for evt := range s.socket.Events {
			switch evt.Type {
			case socketmode.EventTypeConnected:
				s.log.Info("slack connected")
			case socketmode.EventTypeConnectionError:
				s.log.Warn("slack connection error, reconnecting")
			case socketmode.EventTypeInteractive:
				callback, ok := evt.Data.(slack.InteractionCallback)
				if !ok {
					continue
				}
				// Acknowledge first. Slack shows the user an error if the
				// acknowledgement takes more than three seconds, and the work
				// below can involve two GitHub requests.
				s.socket.Ack(*evt.Request)
				if err := router.Handle(ctx, callback); err != nil {
					s.log.Error("handle slack action", "err", err)
				}
			}
		}
	}()
	return s.socket.RunContext(ctx)
}

// Update replaces a posted message in place, which is how a decided issue
// collapses to one line instead of leaving a stale set of buttons in the
// channel.
func (s *Slack) Update(ctx context.Context, channel, timestamp, text string, blocks []slack.Block) error {
	_, _, _, err := s.api.UpdateMessageContext(ctx, channel, timestamp,
		slack.MsgOptionText(text, false),
		slack.MsgOptionBlocks(blocks...))
	if err != nil {
		return fmt.Errorf("update slack message: %w", err)
	}
	return nil
}

// Reply posts into a message's thread, used for the score breakdown.
func (s *Slack) Reply(ctx context.Context, channel, timestamp string, blocks []slack.Block) error {
	_, _, err := s.api.PostMessageContext(ctx, channel,
		slack.MsgOptionTS(timestamp),
		slack.MsgOptionBlocks(blocks...))
	if err != nil {
		return fmt.Errorf("reply in slack thread: %w", err)
	}
	return nil
}
