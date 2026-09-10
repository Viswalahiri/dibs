package gh

import (
	"net/url"
	"strings"
)

// NextPage returns the path and query of the next page named by a Link header,
// or "" when the header names no next page.
//
// Only `dibs backfill` paginates. The poller never does: anything past the
// newest page in a forty-five second window is either already known or already
// stale. The host is dropped rather than followed, so the client keeps talking
// to the base URL it was configured with.
func NextPage(linkHeader string) string {
	for _, part := range strings.Split(linkHeader, ",") {
		segments := strings.Split(strings.TrimSpace(part), ";")
		if len(segments) < 2 {
			continue
		}
		isNext := false
		for _, attr := range segments[1:] {
			if strings.EqualFold(strings.TrimSpace(attr), `rel="next"`) {
				isNext = true
			}
		}
		if !isNext {
			continue
		}
		raw := strings.TrimSpace(segments[0])
		if !strings.HasPrefix(raw, "<") || !strings.HasSuffix(raw, ">") {
			continue
		}
		u, err := url.Parse(raw[1 : len(raw)-1])
		if err != nil {
			continue
		}
		if u.RawQuery == "" {
			return u.Path
		}
		return u.Path + "?" + u.RawQuery
	}
	return ""
}
