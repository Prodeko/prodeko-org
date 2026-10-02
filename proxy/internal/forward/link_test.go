package forward

import (
	"net/url"
	"testing"
)

func testHandler(t *testing.T, publicBase string) *Handler {
	t.Helper()
	cfg := Config{
		Owner:       "prodeko",
		Repo:        "prodeko-org",
		Branch:      testBranch,
		Token:       "ghp_secret",
		EditorRoles: testRoles,
		Committer:   testCommitter,
	}
	if publicBase != "" {
		parsed, err := url.Parse(publicBase)
		if err != nil {
			t.Fatal(err)
		}
		cfg.PublicBase = parsed
	}
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Relaying GitHub's Link header verbatim sends the session token to
// api.github.com: Decap follows rel=next itself and reuses the same
// Authorization header.
func TestRewriteLink(t *testing.T) {
	h := testHandler(t, "https://cms.prodeko.org")

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			// The form GitHub actually emits. A rewriter matching only
			// /repos/{owner}/{repo} would leave this pointing at GitHub.
			name: "numeric repository form",
			in:   `<https://api.github.com/repositories/29514104/pulls?state=all&per_page=2&page=2>; rel="next", <https://api.github.com/repositories/29514104/pulls?state=all&per_page=2&page=2204>; rel="last"`,
			want: `<https://cms.prodeko.org/github/repos/prodeko/prodeko-org/pulls?state=all&per_page=2&page=2>; rel="next", <https://cms.prodeko.org/github/repos/prodeko/prodeko-org/pulls?state=all&per_page=2&page=2204>; rel="last"`,
		},
		{
			name: "named repository form",
			in:   `<https://api.github.com/repos/prodeko/prodeko-org/pulls?page=2>; rel="next"`,
			want: `<https://cms.prodeko.org/github/repos/prodeko/prodeko-org/pulls?page=2>; rel="next"`,
		},
		{
			name: "a link somewhere else is dropped",
			in:   `<https://evil.example.com/steal?page=2>; rel="next"`,
			want: ``,
		},
		{
			name: "a link outside any repository is dropped",
			in:   `<https://api.github.com/user/repos?page=2>; rel="next"`,
			want: ``,
		},
		{
			name: "the survivors of a mixed header",
			in:   `<https://evil.example.com/a>; rel="prev", <https://api.github.com/repositories/1/pulls?page=2>; rel="next"`,
			want: `<https://cms.prodeko.org/github/repos/prodeko/prodeko-org/pulls?page=2>; rel="next"`,
		},
		{
			name: "an encoded segment survives unchanged",
			in:   `<https://api.github.com/repos/prodeko/prodeko-org/git/refs/heads/cms%2Fpages%2Fslug?page=2>; rel="next"`,
			want: `<https://cms.prodeko.org/github/repos/prodeko/prodeko-org/git/refs/heads/cms%2Fpages%2Fslug?page=2>; rel="next"`,
		},
		{
			name: "a comma inside a query value does not split the entry",
			in:   `<https://api.github.com/repositories/1/issues?labels=a,b&page=2>; rel="next"`,
			want: `<https://cms.prodeko.org/github/repos/prodeko/prodeko-org/issues?labels=a,b&page=2>; rel="next"`,
		},
		{"empty", ``, ``},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var values []string
			if tc.in != "" {
				values = []string{tc.in}
			}
			if got := h.rewriteLink(values); got != tc.want {
				t.Fatalf("rewriteLink(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// With no PublicBase there is nowhere safe to point, so the header goes away
// and Decap stops after one page.
func TestRewriteLinkDropsEverythingWithoutAPublicBase(t *testing.T) {
	h := testHandler(t, "")
	in := []string{`<https://api.github.com/repositories/1/pulls?page=2>; rel="next"`}
	if got := h.rewriteLink(in); got != "" {
		t.Fatalf("rewriteLink = %q, want the header to be dropped", got)
	}
}

func TestSplitLinkEntries(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"one", `<https://a/x>; rel="next"`, 1},
		{"two", `<https://a/x>; rel="next", <https://a/y>; rel="last"`, 2},
		{"comma inside the url", `<https://a/x?l=a,b>; rel="next"`, 1},
		{"comma inside the url, two entries", `<https://a/x?l=a,b>; rel="next", <https://a/y>; rel="last"`, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := splitLinkEntries(tc.in); len(got) != tc.want {
				t.Fatalf("splitLinkEntries(%q) = %d entries %q, want %d", tc.in, len(got), got, tc.want)
			}
		})
	}
}
