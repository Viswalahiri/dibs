package gh

import "testing"

func TestNextPage(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"no header", "", ""},
		{
			"next and last",
			`<https://api.github.com/repositories/1/issues?page=2>; rel="next", ` +
				`<https://api.github.com/repositories/1/issues?page=9>; rel="last"`,
			"/repositories/1/issues?page=2",
		},
		{
			"last page names only prev and first",
			`<https://api.github.com/repositories/1/issues?page=8>; rel="prev", ` +
				`<https://api.github.com/repositories/1/issues?page=1>; rel="first"`,
			"",
		},
		{
			"a stub on another host is still followed by path",
			`<http://127.0.0.1:8791/repos/acme/widget/issues?page=3&per_page=100>; rel="next"`,
			"/repos/acme/widget/issues?page=3&per_page=100",
		},
		{"malformed", `https://api.github.com/x; rel="next"`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NextPage(tc.header); got != tc.want {
				t.Errorf("NextPage = %q, want %q", got, tc.want)
			}
		})
	}
}
