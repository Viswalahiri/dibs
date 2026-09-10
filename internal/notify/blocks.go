// Package notify owns Slack. Every outbound message goes through the outbox
// first, so a disconnect or a crash between deciding to notify and notifying
// loses nothing.
package notify

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/Viswalahiri/dibs/internal/store"
)

// Alert builds the push message. It has to be decidable without opening
// GitHub: that is the entire speed argument, and a message that sends the
// reader to the browser to understand it has already lost the race.
//
// Every field comes off the issue row. There are no buttons, because dibs has
// nothing to offer that the browser does not, and no interaction means the
// Slack app needs no inbound connection at all.
func Alert(iss store.Issue, repo store.Repo, now time.Time) []slack.Block {
	header := fmt.Sprintf("%s · #%d · %s",
		repo.Slug(), iss.Number, AgoShort(now.Sub(iss.CreatedAt)))

	title := fmt.Sprintf("<%s|%s>", iss.HTMLURL, esc(iss.Title))

	blocks := []slack.Block{
		slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, header, false, false)),
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, title, false, false), nil, nil),
	}
	if meta := metaLine(iss, repo); meta != "" {
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject(slack.MarkdownType, meta, false, false)))
	}
	return blocks
}

// metaLine is the at-a-glance row: what it is filed as, who filed it, how much
// conversation is already on it, and how busy the repository is.
//
// Assignees are shown rather than acted on. An assigned issue is usually still
// open in practice, so the alert reports the assignment and leaves the call to
// the reader.
func metaLine(iss store.Issue, repo store.Repo) string {
	var parts []string
	if len(iss.Labels) > 0 {
		parts = append(parts, esc(strings.Join(iss.Labels, ", ")))
	}
	if iss.Author != "" {
		parts = append(parts, "by "+esc(iss.Author))
	}
	if iss.CommentCount > 0 {
		parts = append(parts, plural(iss.CommentCount, "comment"))
	}
	if len(iss.Assignees) > 0 {
		parts = append(parts, "assigned to "+esc(strings.Join(iss.Assignees, ", ")))
	}
	if repo.IssuesLast30d > 0 {
		parts = append(parts, fmt.Sprintf("~%d issues/30d", repo.IssuesLast30d))
	}
	return strings.Join(parts, " · ")
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// Text is a plain one-line message, used for the operational warnings.
func Text(s string) []slack.Block {
	return []slack.Block{slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, s, false, false), nil, nil)}
}

// AgoShort is the compact form for an alert read on a phone.
func AgoShort(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// esc escapes the three characters Slack's mrkdwn treats specially. Issue
// titles regularly contain "<" and "&", and an unescaped one silently mangles
// the message.
func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// WarningPayload encodes an operational warning for the outbox.
func WarningPayload(message string) (string, error) {
	b, err := json.Marshal(Message{
		Text:   message,
		Blocks: slack.Blocks{BlockSet: Text(message)},
	})
	if err != nil {
		return "", fmt.Errorf("encode warning: %w", err)
	}
	return string(b), nil
}
