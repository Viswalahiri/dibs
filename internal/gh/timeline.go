package gh

import (
	"context"
	"fmt"
	"net/url"
)

// LinkedPR is a pull request GitHub reports as linked to an issue. The
// enricher reads Open to decide whether the work is already under way; the
// reaper reads Author and HTMLURL to recognise the operator's own pull request.
type LinkedPR struct {
	HTMLURL string
	Author  string
	Open    bool
}

// timelineEvent is the subset of the timeline payload that reveals a linked
// pull request. GitHub reports the link from the issue's side as a
// cross-referenced or connected event whose source is a pull request.
type timelineEvent struct {
	Event  string `json:"event"`
	Source *struct {
		Issue *struct {
			State       string          `json:"state"`
			HTMLURL     string          `json:"html_url"`
			User        *User           `json:"user"`
			PullRequest *PullRequestRef `json:"pull_request"`
		} `json:"issue"`
	} `json:"source"`
}

// LinkedPRs returns the pull requests cross-referenced from an issue, in
// timeline order. A deleted or transferred issue yields no events rather than
// an error: there is nothing to retry.
func (c *Client) LinkedPRs(ctx context.Context, owner, name string, number int) ([]LinkedPR, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/timeline?per_page=100",
		url.PathEscape(owner), url.PathEscape(name), number)

	var events []timelineEvent
	if _, _, err := c.getJSON(ctx, path, "", timelineAccept, &events); err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []LinkedPR
	for _, ev := range events {
		if ev.Event != "cross-referenced" && ev.Event != "connected" {
			continue
		}
		src := ev.Source
		if src == nil || src.Issue == nil || src.Issue.PullRequest == nil {
			continue
		}
		pr := LinkedPR{HTMLURL: src.Issue.HTMLURL, Open: src.Issue.State == "open"}
		if src.Issue.User != nil {
			pr.Author = src.Issue.User.Login
		}
		out = append(out, pr)
	}
	return out, nil
}
