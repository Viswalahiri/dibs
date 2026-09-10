package gh

import (
	"context"
	"fmt"
	"net/url"
)

// timelineEvent is the subset of the timeline payload that reveals a linked
// pull request. GitHub reports the link from the issue's side as a
// cross-referenced or connected event whose source is a pull request.
type timelineEvent struct {
	Event  string `json:"event"`
	Source *struct {
		Issue *struct {
			State       string          `json:"state"`
			PullRequest *PullRequestRef `json:"pull_request"`
		} `json:"issue"`
	} `json:"source"`
}

// HasOpenLinkedPR reports whether an open pull request already points at the
// issue. Only an open one means the work is genuinely under way; a closed one
// is usually an abandoned attempt, which leaves the issue available.
//
// A deleted or transferred issue reports false rather than an error: there is
// nothing to retry.
func (c *Client) HasOpenLinkedPR(ctx context.Context, owner, name string, number int) (bool, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/timeline?per_page=100",
		url.PathEscape(owner), url.PathEscape(name), number)

	var events []timelineEvent
	if _, _, err := c.getJSON(ctx, path, "", timelineAccept, &events); err != nil {
		if notFound(err) {
			return false, nil
		}
		return false, err
	}

	for _, ev := range events {
		if ev.Event != "cross-referenced" && ev.Event != "connected" {
			continue
		}
		src := ev.Source
		if src == nil || src.Issue == nil || src.Issue.PullRequest == nil {
			continue
		}
		if src.Issue.State == "open" {
			return true, nil
		}
	}
	return false, nil
}
