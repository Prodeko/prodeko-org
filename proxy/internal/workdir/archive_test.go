package workdir

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// ----------------------------------------------------------- fake GitHub --

// pr is one pull request the fake serves, keyed by the branch it heads.
type pr struct {
	number int
	branch string
	state  string // GitHub's own word: "open" or "closed"
	merged bool   // closed by merging, which is what merged_at says
}

func (p pr) json() string {
	var mergedAt string
	if p.merged {
		mergedAt = "2026-09-22T10:00:00Z"
	}
	return fmt.Sprintf(`{"number":%d,"html_url":"https://github.com/prodeko/prodeko-org/pull/%d",`+
		`"state":%q,"merged_at":%q,"head":{"ref":%q,"sha":"sha%d"}}`,
		p.number, p.number, p.state, mergedAt, p.branch, p.number)
}

// githubFake answers the pull request lookups a sweep makes and records the
// methods it was asked with, because archiving must read GitHub and never
// write to it.
type githubFake struct {
	prs []pr

	mu      sync.Mutex
	methods []string
}

func (g *githubFake) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.methods = append(g.methods, r.Method)
		g.mu.Unlock()

		const list = "/repos/prodeko/prodeko-org/pulls"
		switch {
		case r.URL.Path == list:
			head := strings.TrimPrefix(r.URL.Query().Get("head"), "prodeko:")
			state := r.URL.Query().Get("state")
			for _, p := range g.prs {
				if p.branch == head && (state == "all" || state == p.state) {
					w.Write([]byte("[" + p.json() + "]"))
					return
				}
			}
			w.Write([]byte(`[]`))
		case strings.HasPrefix(r.URL.Path, list+"/"):
			number := strings.TrimPrefix(r.URL.Path, list+"/")
			for _, p := range g.prs {
				if number == fmt.Sprint(p.number) {
					w.Write([]byte(p.json()))
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Not Found"}`))
		case strings.HasSuffix(r.URL.Path, "/status"):
			w.Write([]byte(`{"state":"success"}`))
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// readOnly asserts that nothing the fake was asked to do changed anything:
// archiving ends a change locally and leaves GitHub in the state review put it
// in, which is the whole difference between archiving and abandoning.
func (g *githubFake) readOnly(t *testing.T) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, method := range g.methods {
		if method != http.MethodGet {
			t.Errorf("the sweep made a %s request; archiving may only read GitHub", method)
		}
	}
}

// githubManager is a manager pointed at a fake GitHub, which is what takes it
// out of dry run: without a token there is no pull request state to archive on.
func (f *fixture) githubManager(t *testing.T, apiRoot string) *Manager {
	t.Helper()
	cfg := f.config(t)
	cfg.GitHubToken = "ghp_test"
	cfg.GitHubRepo = "prodeko/prodeko-org"
	cfg.APIRoot = apiRoot
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func openSlugsFor(t *testing.T, m *Manager, user string, slugs ...string) map[string]string {
	t.Helper()
	dirs := make(map[string]string, len(slugs))
	for _, slug := range slugs {
		c, err := m.Change(user, slug)
		if err != nil {
			t.Fatalf("Change(%q, %q): %v", user, slug, err)
		}
		dirs[slug] = c.Dir
	}
	return dirs
}

// ------------------------------------------------------------- archiving --

func TestArchiveFinishedRemovesMergedChanges(t *testing.T) {
	f := newFixture(t)
	gh := &githubFake{prs: []pr{
		{number: 1, branch: BranchFor("maija", "merged-one"), state: "closed", merged: true},
		{number: 2, branch: BranchFor("maija", "still-open"), state: "open"},
	}}
	m := f.githubManager(t, gh.start(t))
	dirs := openSlugsFor(t, m, "maija", "merged-one", "still-open", "never-pushed")

	archived, err := m.ArchiveFinished(t.Context(), "maija")
	if err != nil {
		t.Fatalf("ArchiveFinished: %v", err)
	}
	if want := []string{"merged-one"}; !slices.Equal(archived, want) {
		t.Fatalf("archived = %q, want %q", archived, want)
	}

	if _, err := os.Stat(dirs["merged-one"]); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the merged change still has a worktree at %s", dirs["merged-one"])
	}
	if m.branchExists(t.Context(), BranchFor("maija", "merged-one")) {
		t.Error("the merged change still has a local branch")
	}
	open, err := m.openSlugs("maija")
	if err != nil {
		t.Fatalf("openSlugs: %v", err)
	}
	if want := []string{"never-pushed", "still-open"}; !slices.Equal(open, want) {
		t.Errorf("open = %q, want %q: only finished work is swept", open, want)
	}
	gh.readOnly(t)
}

func TestArchiveFinishedIsNilInDryRun(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t)
	c := openChange(t, m)

	archived, err := m.ArchiveFinished(t.Context(), "maija")
	if err != nil {
		t.Fatalf("ArchiveFinished: %v", err)
	}
	if archived != nil {
		t.Errorf("archived = %q; with no token nothing is known about pull requests", archived)
	}
	if _, err := os.Stat(c.Dir); err != nil {
		t.Errorf("a dry run archived %s: %v", c.Dir, err)
	}
}

// The sweep runs behind the person's back, inside a listing they asked for
// something else from. What it takes away has to be work GitHub already has:
// an edit made after the merge exists nowhere else, and losing it to a listing
// is not a thing anybody could have seen coming.
func TestTheSweepSparesUncommittedWork(t *testing.T) {
	f := newFixture(t)
	gh := &githubFake{prs: []pr{
		{number: 5, branch: BranchFor("maija", "jatkettu"), state: "closed", merged: true},
	}}
	m := f.githubManager(t, gh.start(t))

	c, err := m.Change("maija", "jatkettu")
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	if err := c.WriteFile("site/content/fi/tapahtumat.md", []byte("---\ntitle: Tapahtumat\n---\n\nUusi teksti.\n")); err != nil {
		t.Fatalf("writing after the merge: %v", err)
	}

	archived, err := m.ArchiveFinished(t.Context(), "maija")
	if err != nil {
		t.Fatalf("ArchiveFinished: %v", err)
	}
	if len(archived) != 0 {
		t.Errorf("archived = %q, want nothing: the change carries edits nobody has seen", archived)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "site", "content", "fi", "tapahtumat.md")); err != nil {
		t.Fatalf("the sweep took the uncommitted edit with it: %v", err)
	}
}

func TestListArchivesFinishedChanges(t *testing.T) {
	f := newFixture(t)
	gh := &githubFake{prs: []pr{
		{number: 3, branch: BranchFor("maija", "suljettu"), state: "closed"},
		{number: 4, branch: BranchFor("maija", "avoin"), state: "open"},
	}}
	m := f.githubManager(t, gh.start(t))
	dirs := openSlugsFor(t, m, "maija", "avoin", "suljettu")

	infos, err := m.List(t.Context(), "maija")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 1 || infos[0].Slug != "avoin" {
		t.Fatalf("List = %+v, want the open change alone", infos)
	}
	if infos[0].PRNumber != 4 {
		t.Errorf("PRNumber = %d, want 4", infos[0].PRNumber)
	}
	if _, err := os.Stat(dirs["suljettu"]); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a change closed without merging still has a worktree at %s", dirs["suljettu"])
	}
	gh.readOnly(t)
}

// ----------------------------------------------------------------- the cap --

func TestTheCapSelfHeals(t *testing.T) {
	f := newFixture(t)
	gh := &githubFake{}
	slugs := make([]string, 0, MaxOpenChanges)
	for i := range MaxOpenChanges {
		slug := "muutos-" + string(rune('a'+i))
		slugs = append(slugs, slug)
		gh.prs = append(gh.prs, pr{number: i + 1, branch: BranchFor("maija", slug), state: "closed", merged: true})
	}
	m := f.githubManager(t, gh.start(t))
	openSlugsFor(t, m, "maija", slugs...)

	if _, err := m.Change("maija", "tuore"); err != nil {
		t.Fatalf("Change with the cap full of merged work: %v", err)
	}
	open, err := m.openSlugs("maija")
	if err != nil {
		t.Fatalf("openSlugs: %v", err)
	}
	if want := []string{"tuore"}; !slices.Equal(open, want) {
		t.Errorf("open = %q, want %q", open, want)
	}
	gh.readOnly(t)
}

func TestTheCapHoldsWhileTheChangesAreOpen(t *testing.T) {
	f := newFixture(t)
	gh := &githubFake{}
	slugs := make([]string, 0, MaxOpenChanges)
	for i := range MaxOpenChanges {
		slug := "muutos-" + string(rune('a'+i))
		slugs = append(slugs, slug)
		gh.prs = append(gh.prs, pr{number: i + 1, branch: BranchFor("maija", slug), state: "open"})
	}
	m := f.githubManager(t, gh.start(t))
	openSlugsFor(t, m, "maija", slugs...)

	_, err := m.Change("maija", "yksi-liikaa")
	if !errors.Is(err, ErrTooManyOpen) {
		t.Fatalf("Change = %v, want %v: work under review still holds its slot", err, ErrTooManyOpen)
	}
	if _, err := os.Stat(filepath.Join(f.state, "wt", "maija", slugs[0])); err != nil {
		t.Errorf("the refused change swept away an open one: %v", err)
	}
}

// ------------------------------------------------------- pull request lookup --

func TestPullRequestByNumber(t *testing.T) {
	f := newFixture(t)
	gh := &githubFake{prs: []pr{{number: 7, branch: BranchFor("maija", "jotain"), state: "open"}}}
	m := f.githubManager(t, gh.start(t))

	got, err := m.PullRequestByNumber(t.Context(), 7)
	if err != nil {
		t.Fatalf("PullRequestByNumber: %v", err)
	}
	if got.Number != 7 || got.Head.Ref != BranchFor("maija", "jotain") {
		t.Fatalf("pull request = %+v", got)
	}
	if Finished(got) {
		t.Error("an open pull request reads as finished")
	}
	if !Finished(PullRequest{State: "closed"}) {
		t.Error("a pull request closed without merging does not read as finished")
	}
	if !Finished(PullRequest{State: "closed", MergedAt: "2026-09-22T10:00:00Z"}) {
		t.Error("a merged pull request does not read as finished")
	}
}
