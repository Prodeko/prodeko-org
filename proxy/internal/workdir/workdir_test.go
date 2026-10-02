package workdir

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prodeko/prodeko-org/proxy/internal/fence"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	return Config{
		RepoPath:  t.TempDir(),
		StateDir:  t.TempDir(),
		Committer: Author{Name: "Prodeko media bot", Email: "media-bot@prodeko.org"},
	}
}

func TestNewRequiresPathsAndCommitter(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
		want string
	}{
		{"no repo", func(c *Config) { c.RepoPath = "" }, "MCP_REPO_PATH"},
		{"no state dir", func(c *Config) { c.StateDir = "" }, "MCP_STATE_DIR"},
		{"no committer name", func(c *Config) { c.Committer.Name = "" }, "GIT_COMMITTER_NAME"},
		{"no committer email", func(c *Config) { c.Committer.Email = "" }, "GIT_COMMITTER_EMAIL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t)
			tc.edit(&cfg)
			_, err := New(cfg)
			if err == nil {
				t.Fatal("New accepted an incomplete configuration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("New error = %v, want it to name %s", err, tc.want)
			}
		})
	}
}

// Without both GitHub variables submit must stop at the local origin, and the
// manager has to know that before it is asked to push.
func TestDryRunWithoutBothGitHubVariables(t *testing.T) {
	cfg := testConfig(t)
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !m.DryRun() {
		t.Error("DryRun = false with neither GITHUB_TOKEN nor GITHUB_REPO")
	}

	cfg.GitHubToken = "ghp_example"
	m, err = New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !m.DryRun() {
		t.Error("DryRun = false with GITHUB_REPO missing")
	}

	cfg.GitHubRepo = "prodeko/prodeko-org"
	m, err = New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.DryRun() {
		t.Error("DryRun = true with both variables set")
	}
}

// The branch namespace is the boundary this package exists to hold.
func TestBranchForStaysInTheMediaNamespace(t *testing.T) {
	got := BranchFor("maija", "sininen-otsikko")
	if want := "media/maija/sininen-otsikko"; got != want {
		t.Fatalf("BranchFor = %q, want %q", got, want)
	}
	if !strings.HasPrefix(got, BranchPrefix) {
		t.Fatalf("BranchFor = %q, which is outside %s", got, BranchPrefix)
	}
}

func TestAssertNamespaceRefusesEverythingElse(t *testing.T) {
	for _, tc := range []struct {
		user, branch string
		ok           bool
	}{
		{"maija", "media/maija/sininen-otsikko", true},
		{"maija", "main", false},
		{"maija", "media/maija/", false},
		{"maija", "media/other/slug", false},
		{"maija", "media/maija/../../main", false},
		{"maija", "media/maija/x.lock", false},
		{"maija", "media/maija/two words", false},
	} {
		err := assertNamespace(tc.user, tc.branch)
		if tc.ok && err != nil {
			t.Errorf("assertNamespace(%q, %q) = %v, want nil", tc.user, tc.branch, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("assertNamespace(%q, %q) accepted a branch outside the namespace", tc.user, tc.branch)
		}
	}
}

func TestPreviewURL(t *testing.T) {
	if got, want := PreviewURL(47), "https://pr-47.preview.prodeko.org/"; got != want {
		t.Fatalf("PreviewURL = %q, want %q", got, want)
	}
}

func TestAuthorString(t *testing.T) {
	a := Author{Name: "Maija Meikäläinen", Email: "maija@prodeko.org"}
	if got, want := a.String(), "Maija Meikäläinen <maija@prodeko.org>"; got != want {
		t.Fatalf("Author.String = %q, want %q", got, want)
	}
}

func TestSlug(t *testing.T) {
	now := time.Date(2026, 9, 19, 14, 5, 6, 0, time.UTC)
	for _, tc := range []struct{ title, want string }{
		{"Sininen otsikko tapahtumasivulle", "sininen-otsikko-tapahtumasivulle"},
		{"Vähän vaaleampi ÖÖ", "vahan-vaaleampi-oo"},
		{"  Trailing / slashes!! ", "trailing-slashes"},
		{"A very long title that keeps going well past the branch name budget", "a-very-long-title-that-keeps-going-well-past-the"},
		{"日本語", "change-20260919-140506"},
		{"", "change-20260919-140506"},
	} {
		if got := Slug(tc.title, now); got != tc.want {
			t.Errorf("Slug(%q) = %q, want %q", tc.title, got, tc.want)
		}
	}
	for _, title := range []string{"Sininen otsikko", "Vähän!", "日本語"} {
		s := Slug(title, now)
		if !slugPattern.MatchString(s) {
			t.Errorf("Slug(%q) = %q, which Change would refuse", title, s)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	for _, tc := range []struct {
		pattern, rel string
		want         bool
	}{
		{"", "site/content/fi/tapahtumat.md", true},
		{"*.css", "site/assets/css/main.css", true},
		{"*.css", "site/content/fi/tapahtumat.md", false},
		{"site/content/fi/**", "site/content/fi/deep/page.md", true},
		{"site/content/fi/**", "site/content/en/page.md", false},
		{"site/assets/css/*.css", "site/assets/css/main.css", true},
		{"site/assets/css/*.css", "site/assets/css/tokens/colour.css", false},
		{"**/tapahtumat.md", "site/content/fi/tapahtumat.md", true},
	} {
		if got := globMatch(tc.pattern, tc.rel); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.rel, got, tc.want)
		}
	}
}

func TestParsePorcelainZ(t *testing.T) {
	out := " M site/content/fi/tapahtumat.md\x00?? site/assets/css/new.css\x00R  site/data/new.yaml\x00site/data/old.yaml\x00"
	got := parsePorcelainZ(out)
	want := []string{"site/content/fi/tapahtumat.md", "site/assets/css/new.css", "site/data/old.yaml", "site/data/new.yaml"}
	if len(got) != len(want) {
		t.Fatalf("parsePorcelainZ = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("parsePorcelainZ = %q, want %q", got, want)
		}
	}
}

// ---------------------------------------------------------------- fixtures --

// fixture is a bare origin holding a tiny but real Hugo site, and a clone of it
// to serve as MCP_REPO_PATH. Everything the manager does is done against these
// rather than against a mock, because the behaviour under test is git's.
type fixture struct {
	root   string
	origin string
	repo   string
	state  string
	home   string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	root := t.TempDir()
	f := &fixture{
		root:   root,
		origin: filepath.Join(root, "origin.git"),
		repo:   filepath.Join(root, "repo"),
		state:  filepath.Join(root, "state"),
		home:   filepath.Join(root, "home"),
	}
	mustMkdir(t, f.state)
	mustMkdir(t, f.home)

	f.git(t, root, "init", "--bare", "--initial-branch=main", f.origin)

	seed := filepath.Join(root, "seed")
	mustMkdir(t, seed)
	f.git(t, seed, "init", "--initial-branch=main", ".")
	writeSite(t, seed)
	f.git(t, seed, "add", "-A")
	f.commit(t, seed, "Seed the site")
	f.git(t, seed, "remote", "add", "origin", f.origin)
	f.git(t, seed, "push", "origin", "main")
	f.git(t, f.origin, "symbolic-ref", "HEAD", "refs/heads/main")

	f.git(t, root, "clone", "--quiet", f.origin, f.repo)
	return f
}

func (f *fixture) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + f.home,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"LC_ALL=C",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (f *fixture) commit(t *testing.T, dir, message string) {
	t.Helper()
	f.git(t, dir, "-c", "user.name=Fixture", "-c", "user.email=fixture@example.org",
		"commit", "--quiet", "--message", message)
}

// pushNewFile moves the origin's default branch on, the way somebody else's
// merged change does. It works through a clone of its own, so nothing the
// manager holds is touched behind its back.
func (f *fixture) pushNewFile(t *testing.T, rel, content string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pusher")
	f.git(t, f.root, "clone", "--quiet", f.origin, dir)
	mustWrite(t, filepath.Join(dir, rel), content)
	f.git(t, dir, "add", "-A")
	f.commit(t, dir, "Add "+rel)
	f.git(t, dir, "push", "--quiet", "origin", "HEAD:refs/heads/main")
}

func (f *fixture) config(t *testing.T) Config {
	t.Helper()
	return Config{
		RepoPath:  f.repo,
		StateDir:  f.state,
		Committer: Author{Name: "Prodeko media bot", Email: "media-bot@prodeko.org"},
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func (f *fixture) manager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(f.config(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeSite lays down the smallest thing hugo will build: a configuration, two
// pages, a stylesheet and the layouts that render them.
func writeSite(t *testing.T, root string) {
	t.Helper()
	site := filepath.Join(root, "site")
	mustWrite(t, filepath.Join(site, "hugo.toml"), "baseURL = \"https://example.org/\"\ntitle = \"Fixture\"\n")
	mustWrite(t, filepath.Join(site, "content", "_index.md"), "---\ntitle: Etusivu\n---\n\nHei.\n")
	mustWrite(t, filepath.Join(site, "content", "fi", "tapahtumat.md"),
		"---\ntitle: Tapahtumat\ntranslationKey: events\n---\n\nTapahtumia tulossa.\n")
	mustWrite(t, filepath.Join(site, "content", "en", "events.md"),
		"---\ntitle: Events\ntranslationKey: events\n---\n\nEvents coming up.\n")
	mustWrite(t, filepath.Join(site, "assets", "css", "main.css"),
		".events-header { color: var(--color-brand); }\n.footer { color: var(--color-ink); }\n")
	mustWrite(t, filepath.Join(site, "layouts", "index.html"), "<html><body>{{ .Content }}</body></html>\n")
	mustWrite(t, filepath.Join(site, "layouts", "page.html"),
		"<html><body class=\"events-header\">{{ .Content }}</body></html>\n")
	mustWrite(t, filepath.Join(site, "layouts", "_default", "single.html"),
		"<html><body class=\"events-header\">{{ .Content }}</body></html>\n")
	// Outside the fence on purpose: nothing the manager does may commit it.
	mustWrite(t, filepath.Join(root, "README.md"), "Fixture repository.\n")
}

func openChange(t *testing.T, m *Manager) *Change {
	t.Helper()
	c, err := m.Change("maija", "sininen-otsikko")
	if err != nil {
		t.Fatalf("Change: %v", err)
	}
	return c
}

// ------------------------------------------------------------------ change --

func TestChangeReportsAMissingClonePlainly(t *testing.T) {
	cfg := testConfig(t)
	cfg.RepoPath = filepath.Join(t.TempDir(), "absent")
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = m.Change("maija", "sininen-otsikko")
	if err == nil {
		t.Fatal("Change succeeded against a path that does not exist")
	}
	if !strings.Contains(err.Error(), "MCP_REPO_PATH") {
		t.Fatalf("Change error = %v, want it to name MCP_REPO_PATH", err)
	}
}

func TestChangeReportsADirectoryThatIsNotAClone(t *testing.T) {
	m, err := New(testConfig(t))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = m.Change("maija", "sininen-otsikko")
	if err == nil {
		t.Fatal("Change succeeded against a directory that is not a clone")
	}
	if !strings.Contains(err.Error(), "not a git clone") {
		t.Fatalf("Change error = %v, want it to say the path is not a git clone", err)
	}
}

func TestChangeRefusesUnusableNames(t *testing.T) {
	m := newFixture(t).manager(t)
	for _, tc := range []struct {
		name, user, slug string
		want             error
	}{
		{"path in the user", "../../etc", "slug", ErrBadUser},
		{"slash in the user", "a/b", "slug", ErrBadUser},
		{"empty user", "", "slug", ErrBadUser},
		{"uppercase slug", "maija", "Sininen", ErrBadSlug},
		{"slash in the slug", "maija", "a/b", ErrBadSlug},
		{"empty slug", "maija", "", ErrBadSlug},
		{"dots in the slug", "maija", "..", ErrBadSlug},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := m.Change(tc.user, tc.slug); !errors.Is(err, tc.want) {
				t.Fatalf("Change(%q, %q) = %v, want %v", tc.user, tc.slug, err, tc.want)
			}
		})
	}
}

func TestChangeCreatesAWorktreeOnItsOwnBranch(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t)

	c := openChange(t, m)
	if c.Branch != "media/maija/sininen-otsikko" {
		t.Fatalf("Branch = %q", c.Branch)
	}
	if want := filepath.Join(f.state, "wt", "maija", "sininen-otsikko"); c.Dir != want {
		t.Fatalf("Dir = %q, want %q", c.Dir, want)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "site", "hugo.toml")); err != nil {
		t.Fatalf("the worktree has no site: %v", err)
	}
	head := strings.TrimSpace(f.git(t, c.Dir, "rev-parse", "--abbrev-ref", "HEAD"))
	if head != c.Branch {
		t.Fatalf("the worktree is on %q, want %q", head, c.Branch)
	}

	// Reopening the same change reattaches rather than starting over, and hands
	// back the same object so its lock serialises everything on that worktree.
	again, err := m.Change("maija", "sininen-otsikko")
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	if again != c {
		t.Fatal("reopening a change produced a second object for one worktree")
	}
}

// A name derived from a file name repeats itself: the stylesheet is called
// "main" today and was called "main" yesterday. Fresh is what keeps a new
// conversation's first edit out of the worktree the last one left behind.
func TestFreshNeverJoinsAnExistingChange(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t)
	now := time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)

	first, err := m.Fresh("maija", "main", now)
	if err != nil {
		t.Fatalf("Fresh: %v", err)
	}
	if first.Slug != "main" {
		t.Fatalf("slug = %q, want the name it was asked for", first.Slug)
	}
	if err := first.WriteFile("site/assets/css/main.css", []byte("body { color: red }\n")); err != nil {
		t.Fatalf("writing into the first change: %v", err)
	}

	second, err := m.Fresh("maija", "main", now)
	if err != nil {
		t.Fatalf("the second Fresh: %v", err)
	}
	if second.Slug == first.Slug {
		t.Fatalf("the second change is the first one: %q", second.Slug)
	}
	if second.Slug != "main-2" {
		t.Errorf("slug = %q, want the asked-for name and a number", second.Slug)
	}
	// Nothing of the first change's work came along.
	got, err := second.ReadFile("site/assets/css/main.css", 0, 0)
	if err != nil {
		t.Fatalf("reading the stylesheet in the fresh change: %v", err)
	}
	if strings.Contains(got, "color: red") {
		t.Error("the fresh change inherited the other one's edit")
	}

	// A branch outlives its worktree, and reattaching to one would inherit the
	// commits on it.
	taken := BranchFor("maija", "main-3")
	f.git(t, f.repo, "branch", taken, "main")
	third, err := m.Fresh("maija", "main", now)
	if err != nil {
		t.Fatalf("the third Fresh: %v", err)
	}
	if third.Branch == taken {
		t.Errorf("Fresh reattached to the existing branch %s", taken)
	}
}

func TestChangeEnforcesTheOpenChangeLimit(t *testing.T) {
	m := newFixture(t).manager(t)
	for i := range MaxOpenChanges {
		if _, err := m.Change("maija", "muutos-"+string(rune('a'+i))); err != nil {
			t.Fatalf("Change %d: %v", i, err)
		}
	}
	_, err := m.Change("maija", "yksi-liikaa")
	if !errors.Is(err, ErrTooManyOpen) {
		t.Fatalf("Change = %v, want %v", err, ErrTooManyOpen)
	}
	// The limit is per person, not global.
	if _, err := m.Change("pekka", "oma-muutos"); err != nil {
		t.Fatalf("Change for a second person: %v", err)
	}
}

// ------------------------------------------------------------------- files --

func TestListFilesShowsOnlyTheAllowlistedTree(t *testing.T) {
	c := openChange(t, newFixture(t).manager(t))
	files, err := c.ListFiles("")
	if err != nil {
		t.Fatalf("ListFiles: %v", err)
	}
	joined := strings.Join(files, "\n")
	for _, want := range []string{
		"site/content/fi/tapahtumat.md",
		"site/assets/css/main.css",
		"site/layouts/index.html",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("ListFiles omitted %s; got:\n%s", want, joined)
		}
	}
	for _, unwanted := range []string{"README.md", "site/hugo.toml"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("ListFiles showed %s, which is outside the fence", unwanted)
		}
	}

	css, err := c.ListFiles("*.css")
	if err != nil {
		t.Fatalf("ListFiles(*.css): %v", err)
	}
	if len(css) != 1 || css[0] != "site/assets/css/main.css" {
		t.Fatalf("ListFiles(*.css) = %q", css)
	}
}

func TestReadFileRangesAndRefusals(t *testing.T) {
	c := openChange(t, newFixture(t).manager(t))

	whole, err := c.ReadFile("site/assets/css/main.css", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(whole, ".footer") {
		t.Fatalf("ReadFile returned %q", whole)
	}

	first, err := c.ReadFile("site/assets/css/main.css", 1, 1)
	if err != nil {
		t.Fatalf("ReadFile range: %v", err)
	}
	if want := ".events-header { color: var(--color-brand); }\n"; first != want {
		t.Fatalf("ReadFile(1,1) = %q, want %q", first, want)
	}

	if _, err := c.ReadFile("site/assets/css/main.css", 3, 2); !errors.Is(err, ErrBadRange) {
		t.Fatalf("ReadFile with an inverted range = %v, want %v", err, ErrBadRange)
	}
	if _, err := c.ReadFile("site/assets/css/absent.css", 0, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ReadFile of a missing file = %v, want %v", err, ErrNotFound)
	}
	if _, err := c.ReadFile("site/hugo.toml", 0, 0); !errors.Is(err, fence.ErrOutside) {
		t.Fatalf("ReadFile of site/hugo.toml = %v, want %v", err, fence.ErrOutside)
	}
}

func TestSearchFindsLinesAndHonoursTheCap(t *testing.T) {
	c := openChange(t, newFixture(t).manager(t))

	hits, err := c.Search("color: var", "*.css", 0)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("Search found %d hits, want 2: %+v", len(hits), hits)
	}
	if hits[0].Path != "site/assets/css/main.css" || hits[0].Line != 1 {
		t.Fatalf("first hit = %+v", hits[0])
	}

	capped, err := c.Search("color: var", "", 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(capped) != 1 {
		t.Fatalf("Search ignored max: %d hits", len(capped))
	}

	if _, err := c.Search("(unclosed", "", 0); err == nil {
		t.Fatal("Search accepted a broken regular expression")
	}
}

func TestWriteAndEditStayInsideTheFence(t *testing.T) {
	c := openChange(t, newFixture(t).manager(t))

	if err := c.WriteFile("site/content/fi/uusi.md", []byte("---\ntitle: Uusi\n---\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "site", "content", "fi", "uusi.md")); err != nil {
		t.Fatalf("the file was not written: %v", err)
	}

	if err := c.EditFile("site/assets/css/main.css", "var(--color-brand)", "var(--color-sky)"); err != nil {
		t.Fatalf("EditFile: %v", err)
	}
	after, err := c.ReadFile("site/assets/css/main.css", 0, 0)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(after, "var(--color-sky)") {
		t.Fatalf("EditFile did not take: %q", after)
	}

	if err := c.EditFile("site/assets/css/main.css", "color:", "colour:"); !errors.Is(err, ErrManyMatches) {
		t.Fatalf("EditFile with two matches = %v, want %v", err, ErrManyMatches)
	}
	if err := c.EditFile("site/assets/css/main.css", "nowhere", "x"); !errors.Is(err, ErrNoMatch) {
		t.Fatalf("EditFile with no match = %v, want %v", err, ErrNoMatch)
	}

	if err := c.WriteFile("site/layouts/index.html", []byte("owned")); !errors.Is(err, fence.ErrReadOnly) {
		t.Fatalf("WriteFile into site/layouts = %v, want %v", err, fence.ErrReadOnly)
	}
	if err := c.WriteFile(".github/workflows/deploy.yml", []byte("owned")); !errors.Is(err, fence.ErrOutside) {
		t.Fatalf("WriteFile into .github = %v, want %v", err, fence.ErrOutside)
	}
	if err := c.WriteFile("site/content/../hugo.toml", []byte("owned")); !errors.Is(err, fence.ErrBadPath) {
		t.Fatalf("WriteFile through a traversal = %v, want %v", err, fence.ErrBadPath)
	}
}

func TestTouchedListsOnlyTheChange(t *testing.T) {
	c := openChange(t, newFixture(t).manager(t))
	if touched, err := c.Touched(); err != nil {
		t.Fatalf("Touched: %v", err)
	} else if len(touched) != 0 {
		t.Fatalf("a fresh change touches %q", touched)
	}

	if err := c.EditFile("site/assets/css/main.css", "--color-brand", "--color-sky"); err != nil {
		t.Fatalf("EditFile: %v", err)
	}
	// Written by something other than a tool, and outside the fence.
	mustWrite(t, filepath.Join(c.Dir, "README.md"), "tampered\n")

	touched, err := c.Touched()
	if err != nil {
		t.Fatalf("Touched: %v", err)
	}
	if len(touched) != 1 || touched[0] != "site/assets/css/main.css" {
		t.Fatalf("Touched = %q, want just the stylesheet", touched)
	}
}

// ------------------------------------------------------------------- build --

func TestBuildRunsHugoAndReturnsItsOutput(t *testing.T) {
	if _, err := exec.LookPath("hugo"); err != nil {
		t.Skip("hugo is not on PATH")
	}
	f := newFixture(t)
	m := f.manager(t)
	c := openChange(t, m)

	res, err := m.Build(t.Context(), c)
	if err != nil {
		t.Fatalf("Build: %v (output %q)", err, res.Output)
	}
	if !res.OK {
		t.Fatalf("Build failed on a clean fixture: %s", res.Output)
	}
	if res.Duration <= 0 {
		t.Error("Build reported no duration")
	}
	// The build leaves nothing behind in the worktree for submit to pick up.
	if _, err := os.Stat(filepath.Join(c.Dir, "site", "public")); err == nil {
		t.Error("Build wrote its output into the worktree")
	}
}

func TestBuildReturnsHugoErrorsVerbatim(t *testing.T) {
	if _, err := exec.LookPath("hugo"); err != nil {
		t.Skip("hugo is not on PATH")
	}
	f := newFixture(t)
	m := f.manager(t)
	c := openChange(t, m)

	if err := c.WriteFile("site/content/fi/rikki.md",
		[]byte("---\ntitle: Rikki\n---\n\n{{< nosuchshortcode >}}\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	res, err := m.Build(t.Context(), c)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.OK {
		t.Fatal("Build passed a site with an undefined shortcode")
	}
	if !strings.Contains(res.Output, "nosuchshortcode") {
		t.Fatalf("Build output does not name the problem: %q", res.Output)
	}
	// The error names the file the way every other tool does. The worktree root
	// is the state directory, the username and the branch slug spelled out, and
	// it is in a template error several times per line.
	if strings.Contains(res.Output, c.Dir) {
		t.Errorf("Build output discloses the worktree path: %q", res.Output)
	}
	if !strings.Contains(res.Output, "site/content/fi/rikki.md") {
		t.Errorf("Build output does not name the file relative to the repository: %q", res.Output)
	}
}

// The per-change text limit is enforced on the write that crosses it, not only
// at submit: fifty maximum-sized writes to an unsubmitted change would
// otherwise sit on the state volume.
func TestWritesStopAtThePerChangeTextLimit(t *testing.T) {
	c := openChange(t, newFixture(t).manager(t))

	big := strings.Repeat("x", fence.MaxTextBytes/2+1)
	if err := c.WriteFile("site/content/fi/yksi.md", []byte(big)); err != nil {
		t.Fatalf("the first write: %v", err)
	}
	err := c.WriteFile("site/content/fi/kaksi.md", []byte(big))
	if !errors.Is(err, fence.ErrTooLarge) {
		t.Fatalf("the second write = %v, want %v", err, fence.ErrTooLarge)
	}
	if _, statErr := os.Stat(filepath.Join(c.Dir, "site", "content", "fi", "kaksi.md")); statErr == nil {
		t.Error("the refused write landed on disk anyway")
	}
}

// check-trees.sh reads its trees from beside itself, so the build has to stage
// it next to the two output directories for it to be checking anything.
func TestBuildRunsCheckTreesAgainstTheBuiltTrees(t *testing.T) {
	if _, err := exec.LookPath("hugo"); err != nil {
		t.Skip("hugo is not on PATH")
	}
	f := newFixture(t)
	site := filepath.Join(f.repo, "site")
	mustWrite(t, filepath.Join(site, "content-members", "jasenille", "_index.md"),
		"---\ntitle: Jäsenille\n---\n\nVain jäsenille.\n")
	script := filepath.Join(site, "check-trees.sh")
	mustWrite(t, script, `#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
for d in public public-members content-members; do
  [ -d "$d" ] || { echo "check-trees: $d is missing" >&2; exit 1; }
done
echo "check-trees: both trees present"
`)
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	f.git(t, f.repo, "add", "-A")
	f.commit(t, f.repo, "Add the tree check")
	f.git(t, f.repo, "push", "--quiet", "origin", "main")

	m := f.manager(t)
	c := openChange(t, m)
	res, err := m.Build(t.Context(), c)
	if err != nil {
		t.Fatalf("Build: %v (output %q)", err, res.Output)
	}
	if !res.OK {
		t.Fatalf("Build failed: %s", res.Output)
	}
	if !strings.Contains(res.Output, "both trees present") {
		t.Fatalf("check-trees.sh did not run: %q", res.Output)
	}
}

// Hugo answers one question and the checks answer two more: a page whose markup
// does not close and a stylesheet that does not parse both build perfectly well.
// A finding is reported without changing what hugo said about the build.
func TestBuildReportsTheCheckFindings(t *testing.T) {
	if _, err := exec.LookPath("hugo"); err != nil {
		t.Skip("hugo is not on PATH")
	}
	m := newFixture(t).manager(t)
	c := openChange(t, m)

	// A page layout is writable, and this is the mistake it is writable for.
	if err := c.WriteFile("site/layouts/page.html",
		[]byte("<html><body><div class=\"card\">{{ .Content }}</body></html>\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := c.WriteFile("site/assets/css/main.css", []byte(".a { color: red }\n}\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	res, err := m.Build(t.Context(), c)
	if err != nil {
		t.Fatalf("Build: %v (output %q)", err, res.Output)
	}
	if !res.OK {
		t.Fatalf("hugo refused a site it has no quarrel with: %s", res.Output)
	}
	if len(res.CSS) != 1 {
		t.Fatalf("the stylesheet check found %d things, want 1: %v", len(res.CSS), res.CSS)
	}
	if res.CSS[0].Where != "site/assets/css/main.css" || res.CSS[0].Line != 2 {
		t.Errorf("the finding is at %s:%d, want site/assets/css/main.css:2", res.CSS[0].Where, res.CSS[0].Line)
	}
	if len(res.HTML) == 0 {
		t.Fatal("the page check found nothing on a page with an unclosed div")
	}
	if !strings.Contains(res.HTML[0].Text, "<div>") {
		t.Errorf("the finding does not name the element: %s", res.HTML[0])
	}
	// The finding names a built page, which is the thing that can be looked at.
	if !strings.HasSuffix(res.HTML[0].Where, ".html") {
		t.Errorf("the finding names %q, want a built page", res.HTML[0].Where)
	}
}

// The stamps submit reads: a write records an edit, a build that passed records
// itself, and a build that failed records nothing. Their order is the whole
// content of the gate, so the order is what is asserted.
func TestWritesAndBuildsStampTheChange(t *testing.T) {
	if _, err := exec.LookPath("hugo"); err != nil {
		t.Skip("hugo is not on PATH")
	}
	f := newFixture(t)
	m := f.manager(t)
	c := openChange(t, m)

	if st := c.Stamps(); !st.EditedAt.IsZero() || !st.BuiltAt.IsZero() {
		t.Fatalf("a change nobody has touched reports %+v", st)
	}
	if err := c.WriteFile("site/content/fi/uusi.md", []byte("---\ntitle: Uusi\n---\n\nHei.\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	edited := c.Stamps()
	if edited.EditedAt.IsZero() {
		t.Fatal("a write recorded no edit, so submit would let it through unbuilt")
	}
	if !edited.BuiltAt.IsZero() {
		t.Fatalf("a write recorded a build at %s", edited.BuiltAt)
	}

	if res, err := m.Build(t.Context(), c); err != nil || !res.OK {
		t.Fatalf("Build = %+v, %v", res, err)
	}
	built := c.Stamps()
	if !built.BuiltAt.After(built.EditedAt) {
		t.Fatalf("the build at %s does not come after the edit at %s", built.BuiltAt, built.EditedAt)
	}

	// A second edit puts the change back behind its build, which is what makes
	// submit ask for another one.
	if err := c.WriteFile("site/content/fi/uusi.md", []byte("---\ntitle: Uusi\n---\n\nHei taas.\n")); err != nil {
		t.Fatalf("the second WriteFile: %v", err)
	}
	if again := c.Stamps(); !again.EditedAt.After(again.BuiltAt) {
		t.Fatalf("the second edit at %s does not come after the build at %s", again.EditedAt, again.BuiltAt)
	}

	// A build that failed is not a build: the stamp it would have written is the
	// one thing standing between a broken change and a pull request.
	if err := c.WriteFile("site/content/fi/rikki.md",
		[]byte("---\ntitle: Rikki\n---\n\n{{< nosuchshortcode >}}\n")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before := c.Stamps().BuiltAt
	res, err := m.Build(t.Context(), c)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if res.OK {
		t.Fatal("the fixture built with an undefined shortcode")
	}
	if got := c.Stamps().BuiltAt; !got.Equal(before) {
		t.Fatalf("a failed build moved the stamp from %s to %s", before, got)
	}
}

// The layout fence applies to the write itself and not only to the path: a
// template that builds an asset is read-only whatever it is called, and the
// refusal has to land before anything reaches the disk.
func TestWritesApplyTheLayoutFence(t *testing.T) {
	c := openChange(t, newFixture(t).manager(t))

	const partial = "site/layouts/partials/nosto.html"
	if err := c.WriteFile(partial, []byte("<div class=\"nosto\">Hei</div>\n")); err != nil {
		t.Fatalf("writing a partial: %v", err)
	}
	if err := c.WriteFile(partial, []byte("{{ partialCached \"nosto.html\" . }}\n")); !errors.Is(err, fence.ErrReadOnly) {
		t.Fatalf("writing partialCached into a partial = %v, want %v", err, fence.ErrReadOnly)
	}
	// The refused write must not have landed, or the fence would be a warning.
	data, err := os.ReadFile(filepath.Join(c.Dir, filepath.FromSlash(partial)))
	if err != nil {
		t.Fatalf("reading the partial back: %v", err)
	}
	if strings.Contains(string(data), "partialCached") {
		t.Fatalf("the refused write landed anyway: %q", data)
	}
	if err := c.WriteFile("site/layouts/baseof.html", []byte("<html></html>\n")); !errors.Is(err, fence.ErrReadOnly) {
		t.Fatalf("writing the page skeleton = %v, want %v", err, fence.ErrReadOnly)
	}
}

// ------------------------------------------------------------------ submit --

func TestSubmitDryRunCommitsAuthoredByTheEditorAndPushesToOrigin(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t)
	c := openChange(t, m)

	if err := c.EditFile("site/assets/css/main.css", "var(--color-brand)", "var(--color-sky)"); err != nil {
		t.Fatalf("EditFile: %v", err)
	}
	mustWrite(t, filepath.Join(c.Dir, "README.md"), "tampered\n")

	author := Author{Name: "Maija Meikäläinen", Email: "maija@prodeko.org"}
	res, err := m.Submit(t.Context(), c, author, "Sininen otsikko tapahtumasivulle", "Vaihdoin värin.")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !res.DryRun {
		t.Error("Submit did not report a dry run without a GitHub token")
	}
	if !strings.Contains(res.Note, "no GITHUB_TOKEN") {
		t.Errorf("Note = %q, want it to say the run was dry", res.Note)
	}
	if res.PRNumber != 0 || res.PRURL != "" {
		t.Errorf("a dry run reported a pull request: %+v", res)
	}
	if len(res.Files) != 1 || res.Files[0] != "site/assets/css/main.css" {
		t.Errorf("Files = %q, want just the stylesheet", res.Files)
	}
	if !strings.Contains(res.Diffstat, "main.css") {
		t.Errorf("Diffstat = %q", res.Diffstat)
	}

	// The commit is the editor's; the committer is the bot.
	line := strings.TrimSpace(f.git(t, c.Dir, "log", "-1", "--format=%an <%ae>|%cn <%ce>|%s"))
	want := "Maija Meikäläinen <maija@prodeko.org>|Prodeko media bot <media-bot@prodeko.org>|Sininen otsikko tapahtumasivulle"
	if line != want {
		t.Errorf("commit = %q, want %q", line, want)
	}
	if body := f.git(t, c.Dir, "log", "-1", "--format=%b"); !strings.Contains(body, "Vaihdoin värin.") {
		t.Errorf("commit body = %q", body)
	}

	// The file outside the fence never entered the commit.
	names := f.git(t, c.Dir, "show", "--name-only", "--format=", "HEAD")
	if strings.Contains(names, "README.md") {
		t.Errorf("the commit carries a path outside the fence: %q", names)
	}

	// The branch reached the origin, under its own name and nothing else.
	refs := f.git(t, f.origin, "for-each-ref", "--format=%(refname)")
	if !strings.Contains(refs, "refs/heads/media/maija/sininen-otsikko") {
		t.Errorf("origin refs = %q, want the media branch", refs)
	}
	before := strings.TrimSpace(f.git(t, f.origin, "rev-parse", "main"))
	seeded := strings.TrimSpace(f.git(t, f.repo, "rev-parse", "origin/main"))
	if before != seeded {
		t.Errorf("main moved: %q != %q", before, seeded)
	}
}

// git stores a symlink as a blob holding its target, so a link inside the
// allowlist would carry an out-of-tree path into a pull request and be followed
// by whatever checks the branch out. The tools cannot make one; a submit must
// not commit one that arrived some other way.
func TestSubmitNeverCommitsASymlink(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t)
	c := openChange(t, m)

	if err := c.EditFile("site/assets/css/main.css", "var(--color-brand)", "var(--color-sky)"); err != nil {
		t.Fatalf("EditFile: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "secret.txt")
	mustWrite(t, outside, "token\n")
	links := map[string]string{
		"site/content/passwd.md": outside,
		"site/content/out":       filepath.Dir(outside),
		"site/data/root":         string(filepath.Separator),
		"site/content/alias.md":  filepath.Join(c.Dir, "site", "content", "fi", "tapahtumat.md"),
	}
	for rel, target := range links {
		path := filepath.Join(c.Dir, filepath.FromSlash(rel))
		mustMkdir(t, filepath.Dir(path))
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}

	author := Author{Name: "Maija", Email: "maija@prodeko.org"}
	res, err := m.Submit(t.Context(), c, author, "Sininen otsikko", "")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	for rel := range links {
		if slices.Contains(res.Files, rel) {
			t.Errorf("Submit reported %s among the files: %q", rel, res.Files)
		}
	}
	tree := f.git(t, c.Dir, "ls-tree", "-r", "HEAD")
	if strings.Contains(tree, "120000") {
		t.Errorf("the commit carries a symlink:\n%s", tree)
	}
	for rel := range links {
		if strings.Contains(tree, rel) {
			t.Errorf("the commit carries %s:\n%s", rel, tree)
		}
	}
	if !strings.Contains(tree, "site/assets/css/main.css") {
		t.Errorf("the real edit is missing from the commit:\n%s", tree)
	}
}

// A symlink is all that is dirty: there is nothing to commit, and the answer is
// the same refusal an untouched change gets rather than an empty commit.
func TestSubmitWithOnlySymlinksDirtyHasNothingToDo(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t)
	c := openChange(t, m)

	link := filepath.Join(c.Dir, "site", "content", "passwd.md")
	if err := os.Symlink(filepath.Join(t.TempDir(), "secret.txt"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	author := Author{Name: "Maija", Email: "maija@prodeko.org"}
	if _, err := m.Submit(t.Context(), c, author, "Ei mitään", ""); !errors.Is(err, ErrNothingToDo) {
		t.Fatalf("Submit = %v, want %v", err, ErrNothingToDo)
	}
}

// Submitting twice with nothing in between republishes the branch; it does not
// mint a second, empty commit for a presenter who clicked twice.
func TestSubmitTwiceMakesOneCommit(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t)
	c := openChange(t, m)

	if err := c.EditFile("site/assets/css/main.css", "var(--color-brand)", "var(--color-sky)"); err != nil {
		t.Fatalf("EditFile: %v", err)
	}
	author := Author{Name: "Maija", Email: "maija@prodeko.org"}
	first, err := m.Submit(t.Context(), c, author, "Sininen otsikko", "")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	second, err := m.Submit(t.Context(), c, author, "Sininen otsikko", "")
	if err != nil {
		t.Fatalf("Submit again: %v", err)
	}
	if second.Commit != first.Commit {
		t.Errorf("the second submit moved HEAD from %s to %s", first.Commit, second.Commit)
	}
	if !second.NothingNew {
		t.Error("the second submit did not report that nothing had changed")
	}
	if n := strings.Count(f.git(t, c.Dir, "rev-list", c.base+"..HEAD"), "\n"); n != 1 {
		t.Errorf("the branch carries %d commits, want 1", n)
	}
}

func TestSubmitRefusesAnEmptyChange(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t)
	c := openChange(t, m)

	author := Author{Name: "Maija", Email: "maija@prodeko.org"}
	if _, err := m.Submit(t.Context(), c, author, "Ei mitään", ""); !errors.Is(err, ErrNothingToDo) {
		t.Fatalf("Submit = %v, want %v", err, ErrNothingToDo)
	}
}

func TestSubmitRefusesAnEmptyTitleOrAuthor(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t)
	c := openChange(t, m)

	if _, err := m.Submit(t.Context(), c, Author{Name: "Maija", Email: "m@prodeko.org"}, "  ", ""); err == nil {
		t.Error("Submit accepted an empty title")
	}
	if _, err := m.Submit(t.Context(), c, Author{}, "Otsikko", ""); err == nil {
		t.Error("Submit accepted an empty author")
	}
	bad := Author{Name: "Maija <evil@example.org>", Email: "m@prodeko.org"}
	if _, err := m.Submit(t.Context(), c, bad, "Otsikko", ""); err == nil {
		t.Error("Submit accepted an author that would forge a commit header")
	}
}

// A branch somebody else moved is not rebased onto; it is reported as stale in
// the words the design chose, because a media person is never asked to resolve
// a conflict.
func TestSubmitReportsAStaleBranch(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t)
	c := openChange(t, m)

	if err := c.EditFile("site/assets/css/main.css", "var(--color-brand)", "var(--color-sky)"); err != nil {
		t.Fatalf("EditFile: %v", err)
	}
	author := Author{Name: "Maija", Email: "maija@prodeko.org"}
	if _, err := m.Submit(t.Context(), c, author, "Ensimmäinen", ""); err != nil {
		t.Fatalf("first Submit: %v", err)
	}

	// Somebody else moves the branch on the origin underneath this change, so
	// the worktree's next commit can no longer fast-forward it.
	other := filepath.Join(f.root, "other")
	f.git(t, f.root, "clone", "--quiet", "--branch", c.Branch, f.origin, other)
	mustWrite(t, filepath.Join(other, "site", "content", "fi", "toinen.md"), "---\ntitle: Toinen\n---\n")
	f.git(t, other, "add", "-A")
	f.commit(t, other, "Another commit on the same branch")
	f.git(t, other, "push", "--quiet", "origin", "HEAD:refs/heads/"+c.Branch)

	if err := c.EditFile("site/assets/css/main.css", "var(--color-ink)", "var(--color-slate)"); err != nil {
		t.Fatalf("EditFile: %v", err)
	}
	_, err := m.Submit(t.Context(), c, author, "Toinen", "")
	if !errors.Is(err, ErrOutOfDate) {
		t.Fatalf("Submit = %v, want %v", err, ErrOutOfDate)
	}
	if !strings.Contains(err.Error(), "out of date") {
		t.Fatalf("Submit error = %v, want it to say the change is out of date", err)
	}
}

func TestSubmitOpensADraftPullRequestAndLabelsIt(t *testing.T) {
	f := newFixture(t)

	var opened map[string]any
	var labelled []string
	var listCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer ghp_test" {
			t.Errorf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/prodeko/prodeko-org/pulls":
			listCalls++
			if got := r.URL.Query().Get("head"); got != "prodeko:media/maija/sininen-otsikko" {
				t.Errorf("head = %q", got)
			}
			w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/prodeko/prodeko-org/pulls":
			if err := json.NewDecoder(r.Body).Decode(&opened); err != nil {
				t.Errorf("decoding the pull request: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"number":47,"html_url":"https://github.com/prodeko/prodeko-org/pull/47","draft":true,"state":"open","head":{"ref":"media/maija/sininen-otsikko","sha":"deadbeef"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/prodeko/prodeko-org/issues/47/labels":
			var body struct {
				Labels []string `json:"labels"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			labelled = body.Labels
			w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := f.config(t)
	cfg.GitHubToken = "ghp_test"
	cfg.GitHubRepo = "prodeko/prodeko-org"
	cfg.APIRoot = srv.URL
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := openChange(t, m)
	if err := c.EditFile("site/assets/css/main.css", "var(--color-brand)", "var(--color-sky)"); err != nil {
		t.Fatalf("EditFile: %v", err)
	}

	author := Author{Name: "Maija Meikäläinen", Email: "maija@prodeko.org"}
	res, err := m.Submit(t.Context(), c, author, "Sininen otsikko", "Vaihdoin värin.")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.DryRun {
		t.Error("Submit reported a dry run with both GitHub variables set")
	}
	if res.PRNumber != 47 || res.PRURL != "https://github.com/prodeko/prodeko-org/pull/47" {
		t.Errorf("pull request = %d %q", res.PRNumber, res.PRURL)
	}
	if res.PreviewURL != "https://pr-47.preview.prodeko.org/" {
		t.Errorf("PreviewURL = %q", res.PreviewURL)
	}
	if listCalls == 0 {
		t.Error("Submit opened a pull request without looking for an existing one")
	}
	if opened["draft"] != true {
		t.Errorf("the pull request was not opened as a draft: %+v", opened)
	}
	if opened["head"] != "media/maija/sininen-otsikko" || opened["base"] != "main" {
		t.Errorf("head/base = %v/%v", opened["head"], opened["base"])
	}
	body, _ := opened["body"].(string)
	for _, want := range []string{"Vaihdoin värin.", "Maija Meikäläinen <maija@prodeko.org>", "site/assets/css/main.css"} {
		if !strings.Contains(body, want) {
			t.Errorf("the pull request body omits %q: %q", want, body)
		}
	}
	if len(labelled) != 1 || labelled[0] != PRLabel {
		t.Errorf("labels = %q, want [%s]", labelled, PRLabel)
	}
}

// A second submit onto the same branch reuses the open pull request; the
// design's loop is "vähän vaaleampi" onto the change already in review.
func TestSubmitReusesTheOpenPullRequest(t *testing.T) {
	f := newFixture(t)
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/prodeko/prodeko-org/pulls":
			w.Write([]byte(`[{"number":47,"html_url":"https://github.com/prodeko/prodeko-org/pull/47","draft":true,"state":"open","head":{"ref":"media/maija/sininen-otsikko","sha":"deadbeef"}}]`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			posts++
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"A pull request already exists."}]}`))
		default:
			w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	cfg := f.config(t)
	cfg.GitHubToken = "ghp_test"
	cfg.GitHubRepo = "prodeko/prodeko-org"
	cfg.APIRoot = srv.URL
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := openChange(t, m)
	if err := c.EditFile("site/assets/css/main.css", "var(--color-brand)", "var(--color-sky)"); err != nil {
		t.Fatalf("EditFile: %v", err)
	}
	res, err := m.Submit(t.Context(), c, Author{Name: "Maija", Email: "maija@prodeko.org"}, "Sininen otsikko", "")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if res.PRNumber != 47 {
		t.Fatalf("PRNumber = %d, want the pull request already open", res.PRNumber)
	}
	if posts != 0 {
		t.Errorf("Submit tried to open a second pull request %d times", posts)
	}
}

// -------------------------------------------------------------------- list --

func TestListReportsOnlyTheCallersChanges(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t)

	c := openChange(t, m)
	if err := c.EditFile("site/assets/css/main.css", "var(--color-brand)", "var(--color-sky)"); err != nil {
		t.Fatalf("EditFile: %v", err)
	}
	if _, err := m.Change("pekka", "toisen-muutos"); err != nil {
		t.Fatalf("Change: %v", err)
	}

	infos, err := m.List(t.Context(), "maija")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("List = %+v, want one change", infos)
	}
	got := infos[0]
	if got.Slug != "sininen-otsikko" || got.Branch != c.Branch {
		t.Errorf("List = %+v", got)
	}
	if !got.Dirty {
		t.Error("List reported a change with unsubmitted edits as clean")
	}
	if len(got.Files) != 1 || got.Files[0] != "site/assets/css/main.css" {
		t.Errorf("Files = %q", got.Files)
	}
	if got.UpdatedAt.IsZero() {
		t.Error("List reported no update time")
	}
	if got.PRNumber != 0 {
		t.Error("List reported a pull request in a dry run")
	}

	if infos, err := m.List(t.Context(), "pekka"); err != nil {
		t.Fatalf("List: %v", err)
	} else if len(infos) != 1 || infos[0].Slug != "toisen-muutos" {
		t.Fatalf("List for the second person = %+v", infos)
	}
}

func TestBuildTimeoutSurfacesAsItsOwnError(t *testing.T) {
	f := newFixture(t)
	// A "hugo" that never finishes is the only honest way to reach the clock.
	slow := filepath.Join(f.root, "slow-hugo")
	mustWrite(t, slow, "#!/bin/sh\nexec sleep 30\n")
	if err := os.Chmod(slow, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	cfg := f.config(t)
	cfg.HugoBin = slow
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := openChange(t, m)

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	res, err := m.Build(ctx, c)
	if !errors.Is(err, ErrBuildTimeout) {
		t.Fatalf("Build = %v, want %v", err, ErrBuildTimeout)
	}
	if res.OK {
		t.Error("a timed-out build reported success")
	}
}
