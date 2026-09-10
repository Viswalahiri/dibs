// Package filter is the deterministic rejection stage. It runs on enriched
// issues, before any model call, and every issue it rejects costs nothing.
//
// Everything here is a pure function of the issue and its fetched context, so
// the whole stage is testable without a network or a database. Expect it to
// remove 40 to 60% of volume. If it is removing much less than that, tighten
// it before touching anything downstream, because it is the only lever whose
// savings are free.
//
// Do not add checks beyond the ones below. The strainer rule is load-bearing:
// a bad issue reaching Slack costs one click, a good issue killed here costs
// the issue.
package filter

import (
	"regexp"
	"strings"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/gh"
	"github.com/Viswalahiri/dibs/internal/store"
)

// Result is the verdict on one issue. A surviving issue carries no reason.
type Result struct {
	Rejected bool
	Reason   store.RejectReason
}

var pass = Result{}

func reject(r store.RejectReason) Result { return Result{Rejected: true, Reason: r} }

// killfile labels are matched as case-insensitive substrings, so "status:
// wontfix" and "Wontfix" both hit.
//
// needs-triage and blocked are deliberately absent. Both mean nobody has acted
// yet, which describes exactly the fresh unclaimed issue this system exists to
// find, and many repositories apply needs-triage to everything automatically.
//
// question, discussion, and rfc are absent too. They cost a score penalty in
// the triage stage instead of a kill here.
var killfile = []string{"wontfix", "duplicate", "invalid", "stale"}

// minBodyChars is the length below which a body with no code fence is treated
// as too thin to act on. A short issue is not automatically bad, which is why
// a code fence exempts it: "this panics, here is the trace" is short and
// perfectly actionable.
const minBodyChars = 80

// claimRe matches someone saying they have taken the issue. The slash and dot
// commands are separated out because a word boundary cannot precede a "/", so
// folding them into the main alternation would silently never match the way
// people actually write them, at the start of a line or after a space.
var claimRe = regexp.MustCompile(`(?i)(` +
	`\b(i['’]?ll take (this|it)|i am taking|taking this|` +
	`working on (this|it)|i['’]?m on (this|it)|pr (is )?incoming|` +
	`i have a (patch|fix)|opened a pr|submitted a pr|will submit)\b` +
	`|(^|\s)(/assign|\.take)\b)`)

// disqualifierRe marks a sentence as something other than a claim when it
// appears ahead of the claim phrase. "Nobody is working on this" contains the
// exact words "working on this" and means the opposite.
//
// Position is the whole point: a word after the phrase does not disqualify it,
// so "I'm on it, don't duplicate" still reads as a claim.
var disqualifierRe = regexp.MustCompile(
	`(?i)(\b(anyone|anybody|nobody|no one|who|whether|are you|if you|not)\b|n['’]t\b)`)

// IsClaim reports whether anything in body is a person saying they have taken
// this issue. The push worker calls it again on freshly fetched comments, which
// is what makes a ping mean the issue was unclaimed seconds ago.
//
// The check is per sentence rather than per comment, because a comment often
// asks a question and then answers it, and running the pattern over the whole
// body would let the question's words disqualify the answer's claim.
func IsClaim(body string) bool {
	for _, sentence := range sentences(StripQuoted(body)) {
		loc := claimRe.FindStringIndex(sentence)
		if loc == nil {
			continue
		}
		// A question about the issue is not a claim on it. This is the single
		// most common false positive, and every one of them costs a real issue.
		if strings.HasSuffix(strings.TrimSpace(sentence), "?") {
			continue
		}
		if disqualifierRe.MatchString(sentence[:loc[0]]) {
			continue
		}
		return true
	}
	return false
}

// sentences splits text on sentence terminators and line breaks, keeping the
// terminating punctuation so a question can be told from a statement.
func sentences(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if !endsSentence(s, i) {
			continue
		}
		out = append(out, s[start:i+1])
		start = i + 1
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// endsSentence reports whether the byte at i closes a sentence. A period glued
// to the next character does not: that is ".take", "v1.4.2", or "pool.go".
func endsSentence(s string, i int) bool {
	switch s[i] {
	case '\n', '!', '?':
		return true
	case '.':
		return i+1 >= len(s) || s[i+1] == ' ' || s[i+1] == '\n' || s[i+1] == '\t'
	}
	return false
}

// Apply returns the verdict for one enriched issue. Checks run in the order
// below, so the reason recorded is the most specific fact known about why the
// issue is unavailable.
func Apply(iss store.Issue, ctx gh.Context, cfg *config.Config) Result {
	if len(iss.Assignees) > 0 {
		return reject(store.ReasonAlreadyAssigned)
	}
	if ctx.HasLinkedPR {
		return reject(store.ReasonLinkedPRExists)
	}
	if hasKillfileLabel(iss.Labels) {
		return reject(store.ReasonKillfileLabel)
	}
	if claimedByOther(ctx.Comments, cfg.Profile.GitHubLogin) {
		return reject(store.ReasonClaimedInThread)
	}
	if tooThin(iss.Body) {
		return reject(store.ReasonTooThin)
	}
	return pass
}

func hasKillfileLabel(labels []string) bool {
	for _, label := range labels {
		lower := strings.ToLower(label)
		for _, kill := range killfile {
			if strings.Contains(lower, kill) {
				return true
			}
		}
	}
	return false
}

// claimedByOther reports whether anyone but the operator has said they are
// taking the issue. The operator's own claim is not a rejection: it is the
// outcome this whole system exists to produce.
func claimedByOther(comments []gh.Comment, self string) bool {
	for _, c := range comments {
		if strings.EqualFold(c.Login, self) {
			continue
		}
		if IsClaim(c.Body) {
			return true
		}
	}
	return false
}

// tooThin rejects a body with nothing to work from. A code fence is proof
// there is something concrete in there regardless of length.
func tooThin(body string) bool {
	trimmed := strings.TrimSpace(body)
	return len(trimmed) < minBodyChars && !hasCodeFence(trimmed)
}

func hasCodeFence(body string) bool {
	return strings.Contains(body, "```") || strings.Contains(body, "~~~")
}

// StripQuoted removes fenced code blocks and blockquoted lines. Without it, an
// issue that quotes someone else saying "working on this" would be rejected
// for a claim nobody made in this thread.
func StripQuoted(body string) string {
	var out strings.Builder
	fence := ""
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case fence != "":
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			continue
		case strings.HasPrefix(trimmed, "```"):
			fence = "```"
			continue
		case strings.HasPrefix(trimmed, "~~~"):
			fence = "~~~"
			continue
		case strings.HasPrefix(trimmed, ">"):
			continue
		}
		out.WriteString(line)
		out.WriteString("\n")
	}
	return out.String()
}
