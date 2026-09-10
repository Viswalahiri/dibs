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

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/store"
	"github.com/Viswalahiri/dibs/internal/triage"
)

// Action IDs. The value on every button is the issue's primary key.
const (
	ActionOpen   = "dibs_open"
	ActionTrack  = "dibs_track"
	ActionSkip   = "dibs_skip"
	ActionSnooze = "dibs_snooze"
	ActionWhy    = "dibs_why"
)

// goodScore is where the badge turns green. It is a display threshold only and
// has nothing to do with the junk floor.
const goodScore = 70

// Alert builds the push message. It has to be decidable without opening
// GitHub: that is the entire speed argument, and a message that sends the
// reader to the browser to understand it has already lost the race.
func Alert(iss store.Issue, repo store.Repo, r triage.Response, cfg *config.Config, now time.Time) []slack.Block {
	score := int(iss.Score.Int64)

	header := fmt.Sprintf("%s *%d* · %s · #%d · %s",
		badge(score), score, repo.Slug(), iss.Number, AgoShort(now.Sub(iss.CreatedAt)))

	title := fmt.Sprintf("<%s|%s>", iss.HTMLURL, esc(iss.Title))

	blocks := []slack.Block{
		slack.NewContextBlock("", slack.NewTextBlockObject(slack.MarkdownType, header, false, false)),
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, title, false, false), nil, nil),
	}

	if meta := metaLine(iss, repo, r, cfg); meta != "" {
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject(slack.MarkdownType, meta, false, false)))
	}
	if summary := summaryLines(r); summary != "" {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject(slack.MarkdownType, summary, false, false), nil, nil))
	}

	id := fmt.Sprint(iss.ID)
	blocks = append(blocks, slack.NewActionBlock("",
		linkButton("Open issue", iss.HTMLURL, id),
		button(ActionTrack, "Track", id, slack.StylePrimary),
		button(ActionSkip, "Skip", id, slack.StyleDefault),
		button(ActionSnooze, "Snooze 1h", id, slack.StyleDefault),
		button(ActionWhy, "Why?", id, slack.StyleDefault),
	))
	return blocks
}

// metaLine is the at-a-glance row: how long it takes, what it touches, and how
// busy the repository is.
func metaLine(iss store.Issue, repo store.Repo, r triage.Response, cfg *config.Config) string {
	var parts []string
	if effort := effortRange(iss, cfg); effort != "" {
		parts = append(parts, effort)
	}
	if len(r.Stack) > 0 {
		parts = append(parts, strings.Join(r.Stack, ", "))
	}
	if repo.IssuesLast30d > 0 {
		parts = append(parts, fmt.Sprintf("~%d issues/30d", repo.IssuesLast30d))
	}
	return strings.Join(parts, " · ")
}

// effortRange appends an hourglass when the estimate runs past the ceiling.
// That is informational: a long job is still worth seeing, it just is not a
// lunch break.
func effortRange(iss store.Issue, cfg *config.Config) string {
	if iss.EffortHighH <= 0 {
		return ""
	}
	out := fmt.Sprintf("%s-%sh", trimHours(iss.EffortLowH), trimHours(iss.EffortHighH))
	if iss.EffortHighH > cfg.Profile.EffortCeilingHours {
		out += " ⏳"
	}
	return out
}

func summaryLines(r triage.Response) string {
	var lines []string
	for _, p := range r.Positives {
		if p = strings.TrimSpace(p); p != "" {
			lines = append(lines, "✓ "+esc(p))
		}
	}
	if risk := strings.TrimSpace(r.TopRisk); risk != "" {
		lines = append(lines, "⚠ "+esc(risk))
	}
	return strings.Join(lines, "\n")
}

// Why is the threaded breakdown behind the score. It reads stored JSON and
// never calls the model again.
func Why(r triage.Response) []slack.Block {
	var b strings.Builder
	b.WriteString("*Dimensions*\n")
	for _, d := range r.Dimensions.Each() {
		fmt.Fprintf(&b, "`%d` %s · %s\n", d.Dimension.Score, d.Name, orUnstated(d.Dimension.Why))
	}
	b.WriteString("\n*Vetoes*\n")
	for _, v := range r.Vetoes.Each() {
		fmt.Fprintf(&b, "`%.2f` %s · %s\n", v.Veto.Confidence, v.Name, orUnstated(v.Veto.Evidence))
	}
	if r.Effort.Confidence != "" {
		fmt.Fprintf(&b, "\n_Effort confidence: %s_", r.Effort.Confidence)
	}
	return []slack.Block{slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, b.String(), false, false), nil, nil)}
}

// Decided replaces the alert once a button has been pressed, so the channel
// reads as a list of what is left rather than a history of what was.
func Decided(iss store.Issue, repo store.Repo, state store.State, now time.Time) []slack.Block {
	var mark string
	switch state {
	case store.StateTracked:
		mark = "📌 tracking"
	case store.StateSkipped:
		mark = "· skipped"
	case store.StateSnoozed:
		mark = "💤 snoozed until " + now.Add(time.Hour).Format("15:04")
	default:
		mark = string(state)
	}
	line := fmt.Sprintf("%s · <%s|%s #%d> %s",
		mark, iss.HTMLURL, repo.Slug(), iss.Number, esc(iss.Title))
	return []slack.Block{slack.NewContextBlock("",
		slack.NewTextBlockObject(slack.MarkdownType, line, false, false))}
}

// Taken replaces the alert when Track is pressed on an issue that was claimed
// while the operator was deciding. Nothing is tracked.
func Taken(iss store.Issue, repo store.Repo) []slack.Block {
	line := fmt.Sprintf("⚠ someone took <%s|%s #%d> while you were deciding",
		iss.HTMLURL, repo.Slug(), iss.Number)
	return []slack.Block{slack.NewContextBlock("",
		slack.NewTextBlockObject(slack.MarkdownType, line, false, false))}
}

// Text is a plain one-line message, used for the operational warnings.
func Text(s string) []slack.Block {
	return []slack.Block{slack.NewSectionBlock(
		slack.NewTextBlockObject(slack.MarkdownType, s, false, false), nil, nil)}
}

func badge(score int) string {
	if score >= goodScore {
		return "🟢"
	}
	return "🟡"
}

func button(actionID, label, value string, style slack.Style) *slack.ButtonBlockElement {
	b := slack.NewButtonBlockElement(actionID, value,
		slack.NewTextBlockObject(slack.PlainTextType, label, true, false))
	if style != slack.StyleDefault {
		b.Style = style
	}
	return b
}

// linkButton opens GitHub directly. It records nothing, because that is where
// claiming happens, in the browser, by hand.
func linkButton(label, url, value string) *slack.ButtonBlockElement {
	b := slack.NewButtonBlockElement(ActionOpen, value,
		slack.NewTextBlockObject(slack.PlainTextType, label+" ↗", true, false))
	b.URL = url
	return b
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

func trimHours(f float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.1f", f), "0"), ".")
}

// esc escapes the three characters Slack's mrkdwn treats specially. Issue
// titles regularly contain "<" and "&", and an unescaped one silently mangles
// the message.
func esc(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func orUnstated(s string) string {
	if strings.TrimSpace(s) == "" {
		return "_not stated_"
	}
	return esc(s)
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
