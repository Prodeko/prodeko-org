package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prodeko/prodeko-org/proxy/internal/mcpserver"
	"github.com/prodeko/prodeko-org/proxy/internal/workdir"
)

// pekka is the second editor: what one person's change does must be invisible
// to everybody else's conversation.
var pekka = mcpserver.Identity{Username: "pekka", Name: "Pekka Prodeko", Email: "pekka@prodeko.org"}

// A conversation is a session here, and a session is a clock and a binding.
// Which tree a tool call lands in is as much git's answer as this package's, so
// the manager and its worktrees are real; only the clock is the test's.

// fixtureSite is the smallest thing hugo will build, with one page per
// language so a read has something to answer and a pair to find.
var fixtureSite = map[string]string{
	"hugo.toml":                    "baseURL = \"https://example.org/\"\ntitle = \"Fixture\"\n",
	"content/fi/tapahtumat.md":     "---\ntitle: Tapahtumat\ntranslationKey: events\n---\n\nTapahtumia tulossa.\n",
	"content/en/events.md":         "---\ntitle: Events\ntranslationKey: events\n---\n\nEvents coming up.\n",
	"layouts/_default/single.html": "<html><body><h1 class=\"otsikko\">{{ .Title }}</h1>{{ .Content }}</body></html>\n",
}

// fixture is a tool set over a real clone of that site, with a clock the test
// moves. A machine without git skips rather than pretends.
type fixture struct {
	ts    *Toolset
	mgr   *workdir.Manager
	root  string
	state string
	now   time.Time
}

// newFixture builds that tool set. Pull requests given here put a fake GitHub
// behind it, which is what takes the manager out of dry run: a change's state
// and a pull request number mean nothing without one.
func newFixture(t *testing.T, prs ...fakePR) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}

	root := t.TempDir()
	f := &fixture{
		root:  root,
		state: filepath.Join(root, "state"),
		now:   time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC),
	}
	origin, seed, repo := filepath.Join(root, "origin.git"), filepath.Join(root, "seed"), filepath.Join(root, "repo")
	for rel, body := range fixtureSite {
		writeFixtureFile(t, filepath.Join(seed, "site", filepath.FromSlash(rel)), body)
	}

	f.git(t, root, "init", "--bare", "--initial-branch=main", origin)
	f.git(t, seed, "init", "--initial-branch=main", ".")
	f.git(t, seed, "add", "-A")
	f.git(t, seed, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.org",
		"commit", "--quiet", "--message", "Seed the site")
	f.git(t, seed, "remote", "add", "origin", origin)
	f.git(t, seed, "push", "--quiet", "origin", "main")
	f.git(t, origin, "symbolic-ref", "HEAD", "refs/heads/main")
	f.git(t, root, "clone", "--quiet", origin, repo)

	quiet := slog.New(slog.DiscardHandler)
	cfg := workdir.Config{
		RepoPath:  repo,
		StateDir:  f.state,
		Committer: workdir.Author{Name: "Prodeko media bot", Email: "media-bot@prodeko.org"},
		Logger:    quiet,
	}
	if len(prs) > 0 {
		cfg.GitHubToken = "ghp_test"
		cfg.GitHubRepo = fixtureRepo
		cfg.APIRoot = serveGitHub(t, prs)
	}
	mgr, err := workdir.New(cfg)
	if err != nil {
		t.Fatalf("workdir.New: %v", err)
	}
	ts, err := New(Config{Workdir: mgr, Logger: quiet, Now: f.clock})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.mgr, f.ts = mgr, ts
	return f
}

// clock is what the tool set reads the time from, so a test can put an idle
// three quarters of an hour between two calls without waiting for it.
func (f *fixture) clock() time.Time { return f.now }

func (f *fixture) advance(d time.Duration) { f.now = f.now.Add(d) }

// worktree is where a change's tree would be, whether or not one was opened.
func (f *fixture) worktree(user, slug string) string {
	return filepath.Join(f.state, "wt", user, slug)
}

// baseView is the shared read-only checkout every read serves from until a
// change is bound.
func (f *fixture) baseView() string { return filepath.Join(f.state, "wt", ".base") }

// slugs is the changes one person has open on disk, which is the only record
// of them there is.
func (f *fixture) slugs(t *testing.T, user string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(f.state, "wt", user))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the open changes: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func (f *fixture) git(t *testing.T, dir string, args ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + f.root,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeFixtureFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// fixtureRepo is the repository the fake GitHub answers for, as owner/repo.
const fixtureRepo = "prodeko/prodeko-org"

// fakePR is one pull request the fake serves, keyed by the branch it heads.
type fakePR struct {
	number int
	branch string
	state  string // GitHub's own word: "open" or "closed"
	merged bool   // closed by merging, which is what merged_at says
}

func (p fakePR) json() string {
	var mergedAt string
	if p.merged {
		mergedAt = "2026-09-22T10:00:00Z"
	}
	return fmt.Sprintf(`{"number":%d,"html_url":"https://github.com/%s/pull/%d",`+
		`"state":%q,"merged_at":%q,"head":{"ref":%q,"sha":"sha%d"}}`,
		p.number, fixtureRepo, p.number, p.state, mergedAt, p.branch, p.number)
}

// serveGitHub answers the lookups a resume makes: the pull request for a
// branch, the pull request for a number, and the empty review conversation the
// state lookup walks past on its way to the state.
func serveGitHub(t *testing.T, prs []fakePR) string {
	t.Helper()
	const repo = "/repos/" + fixtureRepo
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch path := r.URL.Path; {
		case path == repo+"/pulls":
			head := strings.TrimPrefix(r.URL.Query().Get("head"), "prodeko:")
			state := r.URL.Query().Get("state")
			for _, p := range prs {
				if p.branch == head && (state == "all" || state == p.state) {
					w.Write([]byte("[" + p.json() + "]"))
					return
				}
			}
			w.Write([]byte(`[]`))
		case strings.HasSuffix(path, "/status"):
			w.Write([]byte(`{"state":"success"}`))
		case strings.HasSuffix(path, "/reviews"), strings.HasSuffix(path, "/comments"):
			w.Write([]byte(`[]`))
		case strings.HasPrefix(path, repo+"/pulls/"):
			number := strings.TrimPrefix(path, repo+"/pulls/")
			for _, p := range prs {
				if number == strconv.Itoa(p.number) {
					w.Write([]byte(p.json()))
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Not Found"}`))
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// A conversation that only looks at the site is the common one, and it must
// cost nothing: the read comes out of the shared checkout and leaves no change
// behind to count against the person's three.
func TestANewSessionStartsOnTheBase(t *testing.T) {
	f := newFixture(t)

	out, err := call(t, f.ts, ToolReadFile, maija, `{"path":"site/content/fi/tapahtumat.md"}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(out, "Tapahtumia tulossa.") {
		t.Fatalf("read_file answered:\n%s", out)
	}
	if slugs := f.slugs(t, "maija"); len(slugs) != 0 {
		t.Fatalf("a read opened %v", slugs)
	}
	if _, err := os.Stat(filepath.Join(f.baseView(), "site", "hugo.toml")); err != nil {
		t.Fatalf("the read did not come from the base view: %v", err)
	}
}

// The first write is what opens a change, and the second one belongs to the
// same piece of work: a person editing two files is not making two changes.
func TestTheFirstWriteOpensAFreshChange(t *testing.T) {
	f := newFixture(t)

	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/uutinen.md","content":"# Uutinen\n"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/toinen.md","content":"# Toinen\n"}`); err != nil {
		t.Fatalf("the second write_file: %v", err)
	}

	if got := f.slugs(t, "maija"); len(got) != 1 || got[0] != "uutinen" {
		t.Fatalf("open changes = %v, want the one named after the first edit", got)
	}
	out, err := call(t, f.ts, ToolListMyChanges, maija, `{}`)
	if err != nil {
		t.Fatalf("list_my_changes: %v", err)
	}
	for _, want := range []string{"uutinen", "site/content/fi/uutinen.md", "site/content/fi/toinen.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("list_my_changes omits %q:\n%s", want, out)
		}
	}
}

// A change is named after the file the first edit touched, and file names
// repeat: the stylesheet is "main" every day of the week. A fresh conversation
// editing the same file as the last one must still get a change of its own, or
// tomorrow's one-line fix is submitted together with today's eight unfinished
// files.
func TestAFreshSessionDoesNotJoinYesterdaysChangeOfTheSameName(t *testing.T) {
	f := newFixture(t)

	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/assets/css/main.css","content":"body { color: red }\n"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	f.advance(SessionIdle + time.Minute)

	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/assets/css/main.css","content":"body { color: blue }\n"}`); err != nil {
		t.Fatalf("the write after the idle window: %v", err)
	}

	got := f.slugs(t, "maija")
	slices.Sort(got)
	if want := []string{"main", "main-2"}; !slices.Equal(got, want) {
		t.Fatalf("open changes = %v, want %v: the second conversation opened one of its own", got, want)
	}
	// Yesterday's work is where it was left, and today's is not on top of it.
	for slug, want := range map[string]string{"main": "color: red", "main-2": "color: blue"} {
		body, err := os.ReadFile(filepath.Join(f.worktree("maija", slug), "site", "assets", "css", "main.css"))
		if err != nil {
			t.Fatalf("reading %s: %v", slug, err)
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("%s holds %q, want %q", slug, body, want)
		}
	}
}

// A turn can call two tools at once, and the transport gives each its own
// goroutine. Both would find no change and open one, and the pull request would
// carry half the errand.
func TestTwoFirstWritesOpenOneChange(t *testing.T) {
	f := newFixture(t)

	var write func(context.Context, mcpserver.Identity, json.RawMessage) (mcpserver.Result, error)
	for _, tool := range f.ts.Tools() {
		if tool.Name == ToolWriteFile {
			write = tool.Call
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, args := range []string{
		`{"path":"site/content/fi/uutinen.md","content":"# Uutinen\n"}`,
		`{"path":"site/content/fi/toinen.md","content":"# Toinen\n"}`,
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = write(context.Background(), maija, json.RawMessage(args))
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("write_file: %v", err)
		}
	}

	if got := f.slugs(t, "maija"); len(got) != 1 {
		t.Fatalf("open changes = %v, want the one both writes landed in", got)
	}
	// Both edits are in it, which is what submit will publish.
	dir := f.worktree("maija", f.slugs(t, "maija")[0])
	for _, rel := range []string{"uutinen.md", "toinen.md"} {
		if _, err := os.Stat(filepath.Join(dir, "site", "content", "fi", rel)); err != nil {
			t.Errorf("%s is not in the conversation's change: %v", rel, err)
		}
	}
}

// Silence is the only conversation boundary this server can see. An edit after
// the idle window is a new piece of work and gets a change of its own, rather
// than landing in yesterday's half-finished one.
func TestAnIdleSessionExpires(t *testing.T) {
	f := newFixture(t)

	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/uutinen.md","content":"# Uutinen\n"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}

	f.advance(SessionIdle + time.Minute)
	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/toinen.md","content":"# Toinen\n"}`); err != nil {
		t.Fatalf("the write after the idle window: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.worktree("maija", "toinen"), "site", "content", "fi", "toinen.md")); err != nil {
		t.Fatalf("the edit did not land in a fresh change: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.worktree("maija", "uutinen"), "site", "content", "fi", "toinen.md")); err == nil {
		t.Fatal("the edit landed in the expired session's change")
	}

	// The fresh session's clock started at the write that opened it, so the one
	// after it is still the same conversation.
	f.advance(time.Minute)
	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/kolmas.md","content":"# Kolmas\n"}`); err != nil {
		t.Fatalf("the third write_file: %v", err)
	}
	if got := f.slugs(t, "maija"); len(got) != 2 {
		t.Fatalf("open changes = %v, want two", got)
	}
	if _, err := os.Stat(filepath.Join(f.worktree("maija", "toinen"), "site", "content", "fi", "kolmas.md")); err != nil {
		t.Fatalf("the third edit did not continue the second change: %v", err)
	}
}

// The window is idleness and not age: an afternoon of steady work is one
// conversation however long it runs.
func TestActivityKeepsASessionAlive(t *testing.T) {
	f := newFixture(t)

	for i, args := range []string{
		`{"path":"site/content/fi/uutinen.md","content":"# Uutinen\n"}`,
		`{"path":"site/content/fi/toinen.md","content":"# Toinen\n"}`,
		`{"path":"site/content/fi/kolmas.md","content":"# Kolmas\n"}`,
	} {
		if i > 0 {
			f.advance(30 * time.Minute)
		}
		if _, err := call(t, f.ts, ToolWriteFile, maija, args); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	if got := f.slugs(t, "maija"); len(got) != 1 || got[0] != "uutinen" {
		t.Fatalf("open changes = %v, want the one the session opened", got)
	}
}

// Once a change is bound, everything serves from it: a read of a file that
// exists only in the change has to find it, or the model would be shown the
// published site as though its own edit had not happened.
func TestReadsFollowTheBoundChange(t *testing.T) {
	f := newFixture(t)

	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/uutinen.md","content":"# Uutinen\n"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	out, err := call(t, f.ts, ToolReadFile, maija, `{"path":"site/content/fi/uutinen.md"}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(out, "# Uutinen") {
		t.Fatalf("read_file answered:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(f.baseView(), "site", "content", "fi", "uutinen.md")); err == nil {
		t.Fatal("the edit reached the shared base view")
	}

	// Another person's conversation is untouched by it: they are still reading
	// the published site.
	if _, err := call(t, f.ts, ToolReadFile, pekka, `{"path":"site/content/fi/uutinen.md"}`); err == nil {
		t.Fatal("a second person's read found the first person's unsubmitted edit")
	}
}

// Nothing is opened implicitly any more, so a tool that needs a change and has
// none answers in words. A conversation that has not edited anything is the
// ordinary state, not a failure of the transport.
func TestToolsThatNeedAChangeSayWhenThereIsNone(t *testing.T) {
	f := newFixture(t)

	for _, tc := range []struct{ tool, args string }{
		{ToolSubmit, `{"title":"Otsikko"}`},
		{ToolGetFeedback, `{}`},
	} {
		out, err := call(t, f.ts, tc.tool, maija, tc.args)
		if err != nil {
			t.Fatalf("%s with no change open: %v", tc.tool, err)
		}
		if !strings.Contains(strings.ToLower(out), "no change is open") {
			t.Errorf("%s answered:\n%s", tc.tool, out)
		}
		if !strings.Contains(out, "resume_change") {
			t.Errorf("%s does not say how to continue an existing change:\n%s", tc.tool, out)
		}
	}
	if got := f.slugs(t, "maija"); len(got) != 0 {
		t.Fatalf("a refusal opened %v", got)
	}
}

// A maintainer can merge while the conversation is still going, and the next
// listing sweeps the merged change away. The conversation has to let go of it
// with it: a binding pointing at a removed worktree answers every later call
// with a path the person never mentioned, for the rest of the idle window.
func TestASweepDropsTheConversationsChange(t *testing.T) {
	f := newFixture(t, fakePR{number: 12, branch: workdir.BranchFor("maija", "uutinen"), state: "closed", merged: true})

	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/uutinen.md","content":"# Uutinen\n"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	// Submitted and merged: the worktree holds nothing the site does not, which
	// is the state the sweep archives.
	wt := f.worktree("maija", "uutinen")
	f.git(t, wt, "add", "-A")
	f.git(t, wt, "-c", "user.name=Maija", "-c", "user.email=maija@prodeko.org",
		"commit", "--quiet", "--message", "Uutinen")

	if _, err := call(t, f.ts, ToolListMyChanges, maija, `{}`); err != nil {
		t.Fatalf("list_my_changes: %v", err)
	}
	if got := f.slugs(t, "maija"); len(got) != 0 {
		t.Fatalf("the merged change survived the listing: %v", got)
	}

	// The conversation carries on, on something new rather than on a worktree
	// that is gone.
	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/toinen.md","content":"# Toinen\n"}`); err != nil {
		t.Fatalf("the write after the sweep: %v", err)
	}
	if got := f.slugs(t, "maija"); len(got) != 1 || got[0] != "toinen" {
		t.Fatalf("open changes = %v, want a fresh one", got)
	}
}

// -------------------------------------------------------- resuming a change --

// Yesterday's work is picked up by naming it, and from then on the
// conversation is on that change: this is the whole of what replaced inheriting
// it silently.
func TestResumeBySlugRebinds(t *testing.T) {
	f := newFixture(t)

	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/uutinen.md","content":"# Uutinen\n"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	f.advance(SessionIdle + time.Minute)

	out, err := call(t, f.ts, ToolResumeChange, maija, `{"slug":"uutinen"}`)
	if err != nil {
		t.Fatalf("resume_change: %v", err)
	}
	for _, want := range []string{"uutinen", workdir.BranchFor("maija", "uutinen"), "site/content/fi/uutinen.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("resume_change omits %q:\n%s", want, out)
		}
	}

	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/toinen.md","content":"# Toinen\n"}`); err != nil {
		t.Fatalf("the write after the resume: %v", err)
	}
	if got := f.slugs(t, "maija"); len(got) != 1 || got[0] != "uutinen" {
		t.Fatalf("open changes = %v, want the resumed one alone", got)
	}
	if _, err := os.Stat(filepath.Join(f.worktree("maija", "uutinen"), "site", "content", "fi", "toinen.md")); err != nil {
		t.Fatalf("the edit did not land in the resumed change: %v", err)
	}
}

// A restart empties the session map, and picking yesterday's work back up is
// the first thing a person asks for after one. The resume has to bind on its
// own: a "Resumed" answer with the edits landing elsewhere is exactly the
// silently wrong workspace this package exists to end.
func TestResumeAsTheVeryFirstCallBinds(t *testing.T) {
	f := newFixture(t)

	c, err := f.mgr.Change("maija", "uutinen")
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	if err := c.WriteFile("site/content/fi/uutinen.md", []byte("# Uutinen\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := call(t, f.ts, ToolResumeChange, maija, `{"slug":"uutinen"}`); err != nil {
		t.Fatalf("resume_change: %v", err)
	}
	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/toinen.md","content":"# Toinen\n"}`); err != nil {
		t.Fatalf("the write after the resume: %v", err)
	}
	if got := f.slugs(t, "maija"); len(got) != 1 || got[0] != "uutinen" {
		t.Fatalf("open changes = %v, want the resumed one alone", got)
	}
	if _, err := os.Stat(filepath.Join(f.worktree("maija", "uutinen"), "site", "content", "fi", "toinen.md")); err != nil {
		t.Fatalf("the edit did not land in the resumed change: %v", err)
	}
}

// A person answering review feedback has the pull request number in front of
// them and not the slug this server made up, so the number is a way in.
func TestResumeByPRNumber(t *testing.T) {
	f := newFixture(t, fakePR{number: 7, branch: workdir.BranchFor("maija", "uutinen"), state: "open"})

	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/uutinen.md","content":"# Uutinen\n"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	f.advance(SessionIdle + time.Minute)

	out, err := call(t, f.ts, ToolResumeChange, maija, `{"pr":7}`)
	if err != nil {
		t.Fatalf("resume_change: %v", err)
	}
	for _, want := range []string{"uutinen", "#7"} {
		if !strings.Contains(out, want) {
			t.Errorf("resume_change omits %q:\n%s", want, out)
		}
	}

	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/toinen.md","content":"# Toinen\n"}`); err != nil {
		t.Fatalf("the write after the resume: %v", err)
	}
	if got := f.slugs(t, "maija"); len(got) != 1 || got[0] != "uutinen" {
		t.Fatalf("open changes = %v, want the resumed one alone", got)
	}
}

// A number is somebody else's branch as easily as your own, and the namespace
// is the whole of this server's authorisation: a refusal here is the difference
// between editing your own work and editing theirs.
func TestResumeRefusesAnotherUsersPR(t *testing.T) {
	f := newFixture(t, fakePR{number: 8, branch: "media/joku-muu/juttu", state: "open"})

	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/uutinen.md","content":"# Uutinen\n"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}

	out, err := call(t, f.ts, ToolResumeChange, maija, `{"pr":8}`)
	if err == nil {
		t.Fatalf("resume_change accepted another person's pull request:\n%s", out)
	}
	if !strings.Contains(err.Error(), "media/joku-muu/juttu") {
		t.Errorf("the refusal does not name the branch it refused: %v", err)
	}

	// The conversation is still on its own change, untouched by the refusal.
	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/toinen.md","content":"# Toinen\n"}`); err != nil {
		t.Fatalf("the write after the refusal: %v", err)
	}
	if got := f.slugs(t, "maija"); len(got) != 1 || got[0] != "uutinen" {
		t.Fatalf("open changes = %v, want the conversation's own change alone", got)
	}
}

// Published work has nothing left to continue. Saying so and tidying it away in
// the same breath is what keeps a merged change from being edited on into a
// branch nobody will ever look at again.
func TestResumeRefusesAndArchivesAFinishedChange(t *testing.T) {
	f := newFixture(t, fakePR{number: 9, branch: workdir.BranchFor("maija", "uutinen"), state: "closed", merged: true})

	// The change was made in an earlier conversation and merged since, which is
	// why the tool set knows nothing about it: a leftover worktree is all that
	// is left of it here.
	c, err := f.mgr.Change("maija", "uutinen")
	if err != nil {
		t.Fatalf("opening the change an earlier conversation left: %v", err)
	}
	if err := c.WriteFile("site/content/fi/uutinen.md", []byte("# Uutinen\n")); err != nil {
		t.Fatalf("writing into it: %v", err)
	}

	out, err := call(t, f.ts, ToolResumeChange, maija, `{"slug":"uutinen"}`)
	if err != nil {
		t.Fatalf("resume_change on finished work: %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "finished") {
		t.Errorf("resume_change does not say the work is over:\n%s", out)
	}
	if !strings.Contains(out, "#9") {
		t.Errorf("resume_change does not name the pull request:\n%s", out)
	}
	if got := f.slugs(t, "maija"); len(got) != 0 {
		t.Fatalf("the finished change was left behind: %v", got)
	}

	// The next edit starts something new, which is what the refusal promised.
	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/toinen.md","content":"# Toinen\n"}`); err != nil {
		t.Fatalf("the write after the refusal: %v", err)
	}
	if got := f.slugs(t, "maija"); len(got) != 1 || got[0] != "toinen" {
		t.Fatalf("open changes = %v, want a fresh one", got)
	}
}

// Two ways to name a change is one too many to answer at once, and neither is
// a way of saying "whichever".
func TestResumeArgsAreExactlyOne(t *testing.T) {
	f := newFixture(t)

	for _, args := range []string{`{}`, `{"slug":"uutinen","pr":1}`} {
		out, err := call(t, f.ts, ToolResumeChange, maija, args)
		if err == nil {
			t.Fatalf("resume_change%s was accepted:\n%s", args, out)
		}
		if !strings.Contains(err.Error(), "exactly one") {
			t.Errorf("resume_change%s does not say the rule: %v", args, err)
		}
	}
}

// With no GitHub there is nothing to turn a number into a branch, so the tool
// says which half of itself still works rather than failing obscurely.
func TestResumeByPRInDryRun(t *testing.T) {
	f := newFixture(t)

	out, err := call(t, f.ts, ToolResumeChange, maija, `{"pr":7}`)
	if err == nil {
		t.Fatalf("resume_change by number was accepted in a dry run:\n%s", out)
	}
	if !strings.Contains(err.Error(), ToolListMyChanges) {
		t.Errorf("the refusal does not say where a slug comes from: %v", err)
	}
}

// ------------------------------------------------------ the proactive note --

// noteMark is the phrase that tells the note apart from whatever answer it is
// riding on. A test asserting on it is asserting that the note is there at all.
const noteMark = "open changes from before this conversation"

// Work left over from an earlier conversation has to be mentioned by the server:
// the model has no other way to learn that it exists, and the moment to offer to
// continue it is the first thing said, not the third.
func TestTheNoteAppearsOnceASession(t *testing.T) {
	f := newFixture(t, fakePR{number: 12, branch: workdir.BranchFor("maija", "uutinen"), state: "open"})

	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/uutinen.md","content":"# Uutinen\n"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	f.advance(SessionIdle + time.Minute)

	out, err := call(t, f.ts, ToolReadFile, maija, `{"path":"site/content/fi/tapahtumat.md"}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	// The answer to what was asked, and then what to do about the change under
	// review: its number is what the person will recognise it by.
	for _, want := range []string{"Tapahtumia tulossa.", noteMark, "uutinen", "#12", ToolResumeChange} {
		if !strings.Contains(out, want) {
			t.Errorf("the first answer of the session omits %q:\n%s", want, out)
		}
	}

	// Said once: a note on every answer would be a standing instruction rather
	// than a thing to act on.
	again, err := call(t, f.ts, ToolReadFile, maija, `{"path":"site/content/fi/tapahtumat.md"}`)
	if err != nil {
		t.Fatalf("the second read_file: %v", err)
	}
	if strings.Contains(again, noteMark) {
		t.Errorf("the note was repeated:\n%s", again)
	}
}

// Nothing to say is said in no words at all. Most conversations are the first
// one of the day and a postscript about nothing would train the model to skip
// the postscript.
func TestNoNoteWithoutOpenChanges(t *testing.T) {
	f := newFixture(t)

	out, err := call(t, f.ts, ToolReadFile, maija, `{"path":"site/content/fi/tapahtumat.md"}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if strings.Contains(out, noteMark) || strings.Contains(out, ToolResumeChange) {
		t.Errorf("a person with no open changes was told about them:\n%s", out)
	}
}

// The change this conversation just opened is not news to it. The note is what
// was left behind before, and naming the change in hand would be an invitation
// to resume the thing already being edited.
func TestTheNoteLeavesOutTheConversationsOwnChange(t *testing.T) {
	f := newFixture(t)

	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/vanha.md","content":"# Vanha\n"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	f.advance(SessionIdle + time.Minute)

	out, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/tiedote.md","content":"# Tiedote\n"}`)
	if err != nil {
		t.Fatalf("the write that opens this session's change: %v", err)
	}
	_, note, ok := strings.Cut(out, noteMark)
	if !ok {
		t.Fatalf("the first answer of the session carries no note:\n%s", out)
	}
	if !strings.Contains(note, "vanha") {
		t.Errorf("the note omits the change left behind:\n%s", note)
	}
	if strings.Contains(note, "tiedote") {
		t.Errorf("the note names the change this conversation is on:\n%s", note)
	}
}

// list_my_changes answers the note's question in full, so appending it would
// print the same list twice. It still spends the note: the person has been told.
func TestListMyChangesSwallowsTheNote(t *testing.T) {
	f := newFixture(t)

	if _, err := call(t, f.ts, ToolWriteFile, maija, `{"path":"site/content/fi/uutinen.md","content":"# Uutinen\n"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}
	f.advance(SessionIdle + time.Minute)

	out, err := call(t, f.ts, ToolListMyChanges, maija, `{}`)
	if err != nil {
		t.Fatalf("list_my_changes: %v", err)
	}
	if !strings.Contains(out, "uutinen") {
		t.Fatalf("list_my_changes omits the open change:\n%s", out)
	}
	if strings.Contains(out, noteMark) {
		t.Errorf("list_my_changes carries the note as well as the listing:\n%s", out)
	}

	after, err := call(t, f.ts, ToolReadFile, maija, `{"path":"site/content/fi/tapahtumat.md"}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if strings.Contains(after, noteMark) {
		t.Errorf("the note outlived the listing that answered it:\n%s", after)
	}
}
