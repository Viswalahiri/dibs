package triage

import (
	"strings"
	"testing"
	"time"

	"github.com/Viswalahiri/dibs/internal/gh"
	"github.com/Viswalahiri/dibs/internal/store"
)

// The already_taken veto asks the model whether an issue is assigned, so the
// payload has to carry the answer. It did not until assignment stopped being a
// kill in the filter, which left the model guessing at the one fact the veto
// names.
func TestRenderCarriesAssignees(t *testing.T) {
	now := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	iss := store.Issue{
		Number:    7,
		Title:     "Drain deadlocks",
		Body:      "Calling Drain twice concurrently deadlocks on the second call.",
		CreatedAt: now.Add(-10 * time.Minute),
		Assignees: []string{"maintainer", "some-bot"},
	}

	got := RenderUserMessage(iss, gh.Context{}, repo(), baseConfig(), now)
	if !strings.Contains(got, "ASSIGNEES: maintainer, some-bot") {
		t.Errorf("rendered message does not list the assignees:\n%s", got)
	}

	iss.Assignees = nil
	if got := RenderUserMessage(iss, gh.Context{}, repo(), baseConfig(), now); !strings.Contains(got, "ASSIGNEES: none") {
		t.Errorf("an unassigned issue must say so explicitly:\n%s", got)
	}
}
