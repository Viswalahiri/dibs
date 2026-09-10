package triage

import (
	_ "embed"
	"fmt"
	"strings"
	"time"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/gh"
	"github.com/Viswalahiri/dibs/internal/store"
)

//go:embed prompt.txt
var systemPromptTemplate string

// SystemPrompt fills the one placeholder in the embedded prompt. Keeping the
// prompt in its own file rather than a Go string means a prompt change shows up
// in a diff as prose.
func SystemPrompt(cfg *config.Config) string {
	return strings.ReplaceAll(systemPromptTemplate, "{{EFFORT_CEILING}}",
		trimFloat(cfg.Profile.EffortCeilingHours))
}

// RenderUserMessage builds the exact text sent to the model. It is stored
// verbatim on the issue before the call, because `dibs replay` re-scores from
// stored responses and needs to show what produced them.
//
// author_assoc is always included. OWNER, MEMBER, and COLLABORATOR authors are
// far more likely to be filing self-tracking tickets, and it is the single most
// predictive field in the payload.
func RenderUserMessage(iss store.Issue, ctx gh.Context, repo store.Repo, cfg *config.Config, now time.Time) string {
	var b strings.Builder

	fmt.Fprintf(&b, "REPOSITORY: %s\n", repo.Slug())
	fmt.Fprintf(&b, "RECEPTIVITY: %s\n", repo.Receptivity)
	fmt.Fprintf(&b, "CONTRIBUTOR STACKS: %s\n\n", strings.Join(effectiveStacks(repo, cfg), ", "))

	fmt.Fprintf(&b, "ISSUE #%d: %s\n", iss.Number, iss.Title)
	fmt.Fprintf(&b, "AUTHOR: %s (association: %s)\n", orNone(iss.Author), orNone(iss.AuthorAssoc))
	fmt.Fprintf(&b, "LABELS: %s\n", orNone(strings.Join(iss.Labels, ", ")))
	fmt.Fprintf(&b, "OPENED: %s\n\n", Ago(now.Sub(iss.CreatedAt)))

	fmt.Fprintf(&b, "--- BODY ---\n%s\n\n", gh.Truncate(strings.TrimSpace(iss.Body), cfg.Triage.MaxBodyChars))

	fmt.Fprintf(&b, "--- COMMENTS (%d) ---\n%s\n\n", len(ctx.Comments),
		renderComments(ctx.Comments, cfg.Triage.MaxThreadChars, now))

	fmt.Fprintf(&b, "--- AUTHOR HISTORY IN THIS REPO ---\n")
	fmt.Fprintf(&b, "Opened %d issues and %d pull requests.\n\n",
		ctx.Author.IssuesOpened, ctx.Author.PRsOpened)

	fmt.Fprintf(&b, "--- CONTRIBUTING.md EXCERPT ---\n%s\n", orNotAvailable(ctx.Doc))

	return b.String()
}

// renderComments keeps the oldest comments, because a claim lands early and the
// tail of a long thread is usually people saying "+1". The whole block is
// capped at limit.
func renderComments(comments []gh.Comment, limit int, now time.Time) string {
	if len(comments) == 0 {
		return "none"
	}
	var b strings.Builder
	for _, c := range comments {
		fmt.Fprintf(&b, "%s (%s, %s): %s\n",
			orNone(c.Login), orNone(c.Assoc), Ago(now.Sub(c.CreatedAt)),
			strings.TrimSpace(c.Body))
	}
	return gh.Truncate(strings.TrimSpace(b.String()), limit)
}

// Ago renders a duration the way a person reads it. The alert and the prompt
// both need it, and both are read at a glance.
func Ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour")
	}
	return plural(int(d.Hours()/24), "day")
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s ago", unit)
	}
	return fmt.Sprintf("%d %ss ago", n, unit)
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return s
}

func orNotAvailable(s string) string {
	if strings.TrimSpace(s) == "" {
		return "not available"
	}
	return s
}

// trimFloat prints a whole number without a trailing ".0", so the prompt reads
// "16 hours" rather than "16.0 hours".
func trimFloat(f float64) string {
	return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%.1f", f), "0"), ".")
}
