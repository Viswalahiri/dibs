package gh

import "time"

// Issue is one item from the issues list endpoint. Only the fields dibs
// actually reads are declared; GitHub sends far more.
type Issue struct {
	Number    int       `json:"number"`
	NodeID    string    `json:"node_id"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	HTMLURL   string    `json:"html_url"`
	State     string    `json:"state"`
	Comments  int       `json:"comments"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// ClosedAt is nil while the issue is open. The reaper reads it to date an
	// outcome, so an issue closed during a suspend is recorded at the time it
	// actually closed rather than the time dibs noticed.
	ClosedAt          *time.Time `json:"closed_at"`
	AuthorAssociation string     `json:"author_association"`
	User              *User      `json:"user"`
	Assignee          *User      `json:"assignee"`
	Assignees         []User     `json:"assignees"`
	Labels            []Label    `json:"labels"`

	// PullRequest is non-nil when this item is actually a pull request. The
	// issues endpoint returns both, and forgetting to check this is the most
	// common bug in this pattern.
	PullRequest *PullRequestRef `json:"pull_request"`
}

type User struct {
	Login string `json:"login"`
}

type Label struct {
	Name string `json:"name"`
}

type PullRequestRef struct {
	URL string `json:"url"`
}

// IsPullRequest reports whether this item is a pull request rather than an
// issue. Always check it before treating a list item as an issue.
func (i Issue) IsPullRequest() bool { return i.PullRequest != nil }

// AuthorLogin is the issue author, or "" when GitHub reports a deleted account.
func (i Issue) AuthorLogin() string {
	if i.User == nil {
		return ""
	}
	return i.User.Login
}

// LabelNames flattens the label objects to their names.
func (i Issue) LabelNames() []string {
	out := make([]string, 0, len(i.Labels))
	for _, l := range i.Labels {
		out = append(out, l.Name)
	}
	return out
}

// AssigneeLogins flattens both assignee fields to a deduplicated list of
// logins. GitHub populates the singular and plural fields inconsistently, so
// both are read.
func (i Issue) AssigneeLogins() []string {
	out := make([]string, 0, len(i.Assignees)+1)
	seen := map[string]bool{}
	add := func(u *User) {
		if u == nil || u.Login == "" || seen[u.Login] {
			return
		}
		seen[u.Login] = true
		out = append(out, u.Login)
	}
	add(i.Assignee)
	for j := range i.Assignees {
		add(&i.Assignees[j])
	}
	return out
}

// IsAssigned reports whether anyone holds the issue. GitHub populates both
// assignee and assignees, but not always consistently on older payloads, so
// check both.
func (i Issue) IsAssigned() bool {
	return i.Assignee != nil || len(i.Assignees) > 0
}

// AuthenticatedUser is the subset of GET /user that startup validation needs.
type AuthenticatedUser struct {
	Login string `json:"login"`
}

// RateLimit is the subset of GET /rate_limit used by the startup assertion.
type RateLimit struct {
	Resources struct {
		Core struct {
			Limit     int   `json:"limit"`
			Remaining int   `json:"remaining"`
			Reset     int64 `json:"reset"`
		} `json:"core"`
		Search struct {
			Limit     int   `json:"limit"`
			Remaining int   `json:"remaining"`
			Reset     int64 `json:"reset"`
		} `json:"search"`
	} `json:"resources"`
}
