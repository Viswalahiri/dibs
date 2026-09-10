package gh

import (
	"context"
	"fmt"
	"net/url"

	"github.com/Viswalahiri/dibs/internal/filter"
)

// commentPageSize is how far back a claim is looked for. A thread longer than
// this has enough conversation on it that dibs is not the thing deciding
// whether the issue is still free.
const commentPageSize = 20

type commentPayload struct {
	Body string `json:"body"`
	User *User  `json:"user"`
}

// Comments returns an issue's thread in the shape the filter reads. Both the
// enricher and the push worker call it, the second time immediately before
// sending, which is what makes a ping mean the issue was unclaimed seconds ago.
//
// A deleted or transferred issue returns no comments rather than an error:
// there is nothing to retry.
func (c *Client) Comments(ctx context.Context, owner, name string, number int) ([]filter.Comment, error) {
	path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=%d",
		url.PathEscape(owner), url.PathEscape(name), number, commentPageSize)

	var payload []commentPayload
	if _, _, err := c.GetJSON(ctx, path, "", &payload); err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]filter.Comment, 0, len(payload))
	for _, cm := range payload {
		login := ""
		if cm.User != nil {
			login = cm.User.Login
		}
		out = append(out, filter.Comment{Login: login, Body: cm.Body})
	}
	return out, nil
}
