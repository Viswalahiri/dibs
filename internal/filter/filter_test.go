package filter

import (
	"strings"
	"testing"

	"github.com/Viswalahiri/dibs/internal/config"
	"github.com/Viswalahiri/dibs/internal/gh"
	"github.com/Viswalahiri/dibs/internal/store"
)

const self = "test-user"

func cfg() *config.Config {
	return &config.Config{Profile: config.Profile{GitHubLogin: self}}
}

// solidBody is long enough to survive the thin check, so a test that is about
// something else is never rejected for the wrong reason.
const solidBody = "Calling Drain twice concurrently panics with a nil map write. " +
	"Reproduced on v1.4.2 with GOMAXPROCS=8. Expected the second call to be a no-op."

func issue(mut ...func(*store.Issue)) store.Issue {
	iss := store.Issue{Title: "Fix the drain race", Body: solidBody}
	for _, f := range mut {
		f(&iss)
	}
	return iss
}

func comment(login, body string) gh.Comment {
	return gh.Comment{Login: login, Assoc: "NONE", Body: body}
}

func TestApplyPasses(t *testing.T) {
	got := Apply(issue(), gh.Context{}, cfg())
	if got.Rejected {
		t.Fatalf("a clean issue was rejected as %s", got.Reason)
	}
}

func TestApplyRejects(t *testing.T) {
	tests := []struct {
		name string
		iss  store.Issue
		ctx  gh.Context
		want store.RejectReason
	}{
		{
			name: "assigned to someone",
			iss:  issue(func(i *store.Issue) { i.Assignees = []string{"maintainer"} }),
			want: store.ReasonAlreadyAssigned,
		},
		{
			name: "an open pull request is linked",
			ctx:  gh.Context{HasLinkedPR: true},
			iss:  issue(),
			want: store.ReasonLinkedPRExists,
		},
		{
			name: "a killfile label is present",
			iss:  issue(func(i *store.Issue) { i.Labels = []string{"stale"} }),
			want: store.ReasonKillfileLabel,
		},
		{
			name: "someone else claimed it in the thread",
			iss:  issue(),
			ctx:  gh.Context{Comments: []gh.Comment{comment("rando", "taking this")}},
			want: store.ReasonClaimedInThread,
		},
		{
			name: "the body says nothing",
			iss:  issue(func(i *store.Issue) { i.Body = "doesn't work" }),
			want: store.ReasonTooThin,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Apply(tt.iss, tt.ctx, cfg())
			if !got.Rejected || got.Reason != tt.want {
				t.Fatalf("got %+v, want rejection %s", got, tt.want)
			}
		})
	}
}

// The order matters: the reason recorded should be the most specific fact
// known, and assignment is more specific than a thin body.
func TestApplyReportsTheFirstReason(t *testing.T) {
	iss := issue(func(i *store.Issue) {
		i.Assignees = []string{"maintainer"}
		i.Labels = []string{"wontfix"}
		i.Body = "hm"
	})
	got := Apply(iss, gh.Context{HasLinkedPR: true}, cfg())
	if got.Reason != store.ReasonAlreadyAssigned {
		t.Fatalf("got %s, want %s", got.Reason, store.ReasonAlreadyAssigned)
	}
}

func TestKillfileLabels(t *testing.T) {
	// Every entry in the killfile, plus the casing and prefixing repositories
	// actually use.
	rejected := []string{
		"wontfix", "duplicate", "invalid", "stale",
		"WontFix", "Duplicate", "INVALID", "Stale",
		"status: wontfix", "resolution/duplicate", "stale-bot",
	}
	for _, label := range rejected {
		t.Run(label, func(t *testing.T) {
			got := Apply(issue(func(i *store.Issue) { i.Labels = []string{label} }), gh.Context{}, cfg())
			if got.Reason != store.ReasonKillfileLabel {
				t.Fatalf("label %q was not killed, got %+v", label, got)
			}
		})
	}

	// Labels that describe an issue nobody has touched yet. These are the ones
	// worth seeing, and killing them would defeat the whole system.
	kept := []string{
		"needs-triage", "blocked", "bug", "good first issue",
		"question", "discussion", "rfc", "help wanted", "enhancement",
	}
	for _, label := range kept {
		t.Run("keeps "+label, func(t *testing.T) {
			got := Apply(issue(func(i *store.Issue) { i.Labels = []string{label} }), gh.Context{}, cfg())
			if got.Rejected {
				t.Fatalf("label %q was killed as %s", label, got.Reason)
			}
		})
	}
}

// Every alternative in claimRe, written the way a person would actually type
// it. A phrasing that stops matching here is a phrasing dibs starts pushing
// issues for after someone already took them.
func TestClaimRegexAlternatives(t *testing.T) {
	claims := []string{
		"I'll take this",
		"ill take it",
		"I’ll take this",
		"I am taking this one",
		"taking this",
		"working on this",
		"Working on it now",
		"I'm on it",
		"im on this",
		"/assign",
		"please /assign me",
		".take",
		"PR incoming",
		"pr is incoming",
		"I have a patch",
		"i have a fix ready",
		"opened a PR",
		"submitted a pr for this",
		"will submit shortly",
		// A disqualifier after the phrase must not cancel it.
		"I'm working on this, don't duplicate the effort",
		// A question followed by its own answer. Splitting per sentence is what
		// keeps the question's words from disqualifying the answer.
		"Is anyone on this? I'll take it.",
	}
	for _, body := range claims {
		t.Run(body, func(t *testing.T) {
			ctx := gh.Context{Comments: []gh.Comment{comment("rando", body)}}
			if got := Apply(issue(), ctx, cfg()); got.Reason != store.ReasonClaimedInThread {
				t.Fatalf("%q was not read as a claim, got %+v", body, got)
			}
		})
	}
}

func TestClaimRegexNegatives(t *testing.T) {
	// Ordinary thread traffic. Every one of these would cost a real issue.
	notClaims := []string{
		"Is anyone working on this?",
		"Are you taking this or should I?",
		"Nobody is working on it as far as I know",
		"This is broken for me too",
		"I can reproduce it",
		"Would a PR be welcome here?",
		"Which file has the drain logic?",
		"See the retake helper in pool.go",
		"The mistaken assumption is in line 40",
		"I don't think anyone is working on this",
		"Not working on it myself, just reporting",
		"Wondering whether anyone is taking this",
	}
	for _, body := range notClaims {
		t.Run(body, func(t *testing.T) {
			ctx := gh.Context{Comments: []gh.Comment{comment("rando", body)}}
			if got := Apply(issue(), ctx, cfg()); got.Rejected {
				t.Fatalf("%q was read as a claim: %s", body, got.Reason)
			}
		})
	}
}

func TestOperatorsOwnClaimIsNotARejection(t *testing.T) {
	ctx := gh.Context{Comments: []gh.Comment{comment(self, "taking this")}}
	if got := Apply(issue(), ctx, cfg()); got.Rejected {
		t.Fatalf("the operator's own claim rejected the issue as %s", got.Reason)
	}
	// GitHub logins are case-insensitive, and the config file is typed by hand.
	ctx = gh.Context{Comments: []gh.Comment{comment(strings.ToUpper(self), "taking this")}}
	if got := Apply(issue(), ctx, cfg()); got.Rejected {
		t.Fatalf("a case-different spelling of the operator rejected the issue as %s", got.Reason)
	}
}

func TestQuotedClaimsAreIgnored(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"fenced with backticks", "The regex is:\n```\nworking on this\n```\nDoes it match?"},
		{"fenced with tildes", "~~~\ntaking this\n~~~\nthat is the fixture"},
		{"blockquoted", "> working on this\n\nThat was last year and nothing landed."},
		{"quoted then real question", "> I'll take this\n\nDid anything come of it?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := gh.Context{Comments: []gh.Comment{comment("rando", tt.body)}}
			if got := Apply(issue(), ctx, cfg()); got.Rejected {
				t.Fatalf("a quoted claim rejected the issue as %s", got.Reason)
			}
		})
	}
}

func TestUnfencedClaimAfterAQuoteStillCounts(t *testing.T) {
	body := "> is anyone on this?\n\nYes, I'm on it."
	ctx := gh.Context{Comments: []gh.Comment{comment("rando", body)}}
	if got := Apply(issue(), ctx, cfg()); got.Reason != store.ReasonClaimedInThread {
		t.Fatalf("a real claim below a quote was missed, got %+v", got)
	}
}

func TestTooThin(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"empty", "", true},
		{"whitespace only", "   \n\t ", true},
		{"a shrug", "doesn't work", true},
		{"just under the line", strings.Repeat("a", minBodyChars-1), true},
		{"exactly at the line", strings.Repeat("a", minBodyChars), false},
		{"short but has a stack trace", "panics:\n```\nnil map write\n```", false},
		{"short with a tilde fence", "~~~\nsegfault\n~~~", false},
		{"long prose", solidBody, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Apply(issue(func(i *store.Issue) { i.Body = tt.body }), gh.Context{}, cfg())
			if rejected := got.Reason == store.ReasonTooThin; rejected != tt.want {
				t.Fatalf("body %q: rejected as thin = %v, want %v", tt.body, rejected, tt.want)
			}
		})
	}
}

func TestStripQuoted(t *testing.T) {
	body := "before\n```go\nfenced\n```\n> quoted\nafter\n"
	got := StripQuoted(body)
	for _, gone := range []string{"fenced", "quoted", "```"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q survived stripping: %q", gone, got)
		}
	}
	for _, kept := range []string{"before", "after"} {
		if !strings.Contains(got, kept) {
			t.Errorf("%q was stripped: %q", kept, got)
		}
	}
}

// An unterminated fence swallows the rest of the comment. That is the safe
// direction: it can only cause dibs to miss a claim and push an issue, never
// to kill one.
func TestUnterminatedFenceSwallowsTheRest(t *testing.T) {
	got := StripQuoted("intro\n```\ntaking this")
	if strings.Contains(got, "taking this") {
		t.Fatalf("text after an unterminated fence survived: %q", got)
	}
}
