// Package workdir owns the git state the MCP server keeps on disk: one base
// clone, and one git worktree per open change.
//
// A worktree rather than the GitHub data API, because build() needs a real
// tree to run hugo in. Branches live in a namespace of their own,
// media/<username>/<slug>, and nothing here will touch a branch outside it,
// force-push, or merge. Publishing is a maintainer's review on GitHub.
//
// Every git invocation goes through os/exec with explicit arguments. No shell,
// so no value that came from a client is ever interpolated into one.
package workdir

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prodeko/prodeko-org/proxy/internal/fence"
	"github.com/prodeko/prodeko-org/proxy/internal/ghauth"
	"github.com/prodeko/prodeko-org/proxy/internal/lint"
)

// BranchPrefix is the namespace every change lives in. A branch name that does
// not start with it is not ours and is never written.
const BranchPrefix = "media/"

// Timeouts. The build one is the wall clock around hugo and check-trees.sh
// together; the git one bounds a push to a remote that has stopped answering.
const (
	BuildTimeout = 30 * time.Second
	GitTimeout   = 60 * time.Second
)

// PRLabel is put on every pull request this server opens, so a maintainer can
// tell a media change from a Decap change at a glance and so review rules can
// key on it.
const PRLabel = "media"

// PreviewURLFormat renders the preview host for a pull request number. The
// preview pipeline is reused unchanged; this only predicts the URL it will
// publish in about a minute.
const PreviewURLFormat = "https://pr-%d.preview.prodeko.org/"

// Author is a git identity. The author of every commit is the verified
// Keycloak identity of the person who asked for the change and never anything
// the client sent; the committer is the bot this process runs as.
type Author struct {
	Name  string
	Email string
}

// String is the "Name <email>" form git --author expects.
func (a Author) String() string { return a.Name + " <" + a.Email + ">" }

type Config struct {
	// RepoPath is MCP_REPO_PATH: an existing clone whose origin is the site
	// repository. Worktrees are added to it and never checked out in it.
	RepoPath string

	// StateDir is MCP_STATE_DIR. Worktrees live at <StateDir>/wt/<user>/<slug>.
	// Everything under it is reconstructible from git and needs no backup.
	StateDir string

	// Committer is GIT_COMMITTER_NAME and GIT_COMMITTER_EMAIL.
	Committer Author

	// A credential and GitHubRepo ("owner/repo") turn submit into a real push
	// and a draft pull request. With either absent, submit is a dry run that
	// pushes nothing to GitHub and says so. The credential is GitHubCredential
	// when set, and GitHubToken as a fixed token otherwise.
	GitHubCredential ghauth.Source
	GitHubToken      string
	GitHubRepo       string

	// GitBin and HugoBin default to "git" and "hugo" on PATH.
	GitBin  string
	HugoBin string

	// APIRoot is the GitHub REST root. Empty means DefaultAPIRoot.
	APIRoot string

	HTTPClient *http.Client // nil means a client with GitTimeout
	Logger     *slog.Logger // nil means slog.Default
	Now        func() time.Time
}

// DefaultAPIRoot is where the draft pull request is opened.
const DefaultAPIRoot = "https://api.github.com"

// Manager owns the clone and hands out changes. It is safe for concurrent use;
// work on one change is serialised, because a worktree has one index.
type Manager struct {
	cfg Config
	log *slog.Logger

	mu       sync.Mutex
	changes  map[string]*Change // BranchFor(user, slug) -> the one Change for it
	base     *Change            // the shared read-only view, made once
	archived []func(*Change)    // told when a change stops existing
}

// Change is one editing session on disk: a worktree on its own branch, based
// on the origin default branch as of the moment it was opened. There is no
// auto-rebase; a stale change fails loudly at submit and is started again.
type Change struct {
	User   string // Keycloak preferred_username; the branch namespace
	Slug   string // short, lowercase, from the first thing asked for
	Branch string // BranchPrefix + User + "/" + Slug
	Dir    string // absolute path to the worktree

	fence *fence.Fence
	mgr   *Manager

	// base is the remote-tracking ref this change was cut from ("origin/main")
	// and baseBranch is the branch name a pull request opens against ("main").
	base       string
	baseBranch string

	// mu serialises work on this change: one worktree has one index, so two
	// concurrent writes or a write racing a commit would corrupt it. Reads hold
	// it shared, because the shared base view is one tree and one build root for
	// everybody: a refresh or a build there runs underneath somebody else's
	// read_file or screenshot unless the two are held apart.
	mu sync.RWMutex
}

// View runs fn with the tree and the last build held still: nothing refreshes,
// rebuilds or commits underneath it. It is what the tools that read the build
// output through another package hold, the ones that read files holding it for
// themselves.
func (c *Change) View(fn func() error) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return fn()
}

// Fence is the allowlist this change is confined to.
func (c *Change) Fence() *fence.Fence { return c.fence }

// Info is what list_my_changes reports about one branch.
type Info struct {
	Slug       string
	Branch     string
	UpdatedAt  time.Time
	Files      []string // paths changed against the base branch
	Dirty      bool     // has edits that were never submitted
	PRNumber   int      // 0 when nothing was pushed
	PRURL      string
	PreviewURL string
	CIState    string // GitHub's combined state, "" when unknown
}

// Result of a build. Output is hugo's and check-trees.sh's combined output,
// verbatim: a Hugo template error is exactly what the model needs to read, and
// paraphrasing it would be the one place this server pretends to understand
// the site.
type Result struct {
	OK       bool
	Output   string
	Duration time.Duration

	// HTML and CSS are what the server's own checks noticed about a build that
	// ran. They do not decide OK: hugo failing means the site does not build,
	// while an unclosed element on a page nobody touched is a thing to report to
	// whoever is looking. An HTML finding names a built page and not the
	// template behind it, so nothing here can be attributed to the change with
	// enough confidence to refuse it — the person reading is what judges.
	HTML []lint.Finding
	CSS  []lint.Finding
}

// SubmitResult describes what submit did. With a GitHub token it is a draft
// pull request and a preview URL; without one it is a local push and a
// diffstat, and Note says so rather than letting a dry run read as a success.
type SubmitResult struct {
	Branch     string
	Commit     string
	Diffstat   string
	Files      []string
	DryRun     bool
	NothingNew bool // the branch already carried the change; no commit was made
	Note       string
	PRNumber   int
	PRURL      string
	PreviewURL string
}

var (
	ErrNoChange     = errors.New("workdir: no such change")
	ErrOutOfDate    = errors.New("workdir: this change is out of date, start a new one")
	ErrNothingToDo  = errors.New("workdir: the change has no edits to submit")
	ErrBadSlug      = errors.New("workdir: not a usable change slug")
	ErrBadUser      = errors.New("workdir: not a usable username")
	ErrTooManyOpen  = errors.New("workdir: too many open changes")
	ErrBuildTimeout = errors.New("workdir: the build did not finish in time")
)

// MaxOpenChanges is how many changes one person may have open at once.
const MaxOpenChanges = 3

// New validates the configuration and applies defaults. It does not touch the
// clone: a missing or broken MCP_REPO_PATH surfaces on the first change, where
// it can be reported to the caller, rather than keeping the process from
// starting at all.
func New(cfg Config) (*Manager, error) {
	if strings.TrimSpace(cfg.RepoPath) == "" {
		return nil, errors.New("workdir: MCP_REPO_PATH must be set")
	}
	if strings.TrimSpace(cfg.StateDir) == "" {
		return nil, errors.New("workdir: MCP_STATE_DIR must be set")
	}
	if cfg.Committer.Name == "" || cfg.Committer.Email == "" {
		return nil, errors.New("workdir: GIT_COMMITTER_NAME and GIT_COMMITTER_EMAIL must be set")
	}
	if cfg.GitBin == "" {
		cfg.GitBin = "git"
	}
	if cfg.HugoBin == "" {
		cfg.HugoBin = "hugo"
	}
	if cfg.APIRoot == "" {
		cfg.APIRoot = DefaultAPIRoot
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: GitTimeout}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.GitHubCredential == nil && cfg.GitHubToken != "" {
		cfg.GitHubCredential = ghauth.Static(cfg.GitHubToken)
	}
	return &Manager{cfg: cfg, log: cfg.Logger, changes: make(map[string]*Change)}, nil
}

// DryRun reports whether submit will stop short of GitHub. It is true whenever
// either the GitHub credential or GITHUB_REPO is absent.
func (m *Manager) DryRun() bool {
	return m.cfg.GitHubCredential == nil || m.cfg.GitHubRepo == ""
}

// userPattern and slugPattern are what may appear in a branch name. They are
// deliberately narrower than git's own rules: everything here ends up in a
// branch, a directory under the state volume and a refspec, and a name that is
// harmless in one of those is not automatically harmless in the others.
var (
	userPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	slugPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

// Change opens the change (user, slug), creating the worktree and the branch
// on first use and reattaching to them afterwards. The branch is
// media/<user>/<slug>, based on the origin default branch at creation.
//
// A full cap is not the last word on the matter: a change whose pull request
// was merged or closed is finished, and sweeping those away is what keeps the
// limit counting work that is still going on. The sweep runs out here rather
// than inside the attempt: it opens changes of its own, and the attempt holds
// the manager's lock from end to end.
func (m *Manager) Change(user, slug string) (*Change, error) {
	c, err := m.change(user, slug)
	if !errors.Is(err, ErrTooManyOpen) {
		return c, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), GitTimeout)
	defer cancel()
	if _, sweepErr := m.ArchiveFinished(ctx, user); sweepErr != nil {
		m.log.Warn("workdir: sweeping finished changes", "user", user, "err", sweepErr)
	}
	return m.change(user, slug)
}

// Fresh opens a change nobody has started yet, named after what was asked for.
// A name already taken takes a number: "main" for the stylesheet is the same
// name today as it was yesterday, and a conversation's first edit landing in
// yesterday's worktree — dirty files and all — is the thing this lifecycle
// exists to stop. Change is for continuing work somebody named; this is for
// starting some.
func (m *Manager) Fresh(user, hint string, now time.Time) (*Change, error) {
	name := Slug(hint, now)
	if !m.taken(user, name) {
		return m.Change(user, name)
	}
	for n := 2; n <= 9; n++ {
		numbered := name + "-" + strconv.Itoa(n)
		if !m.taken(user, numbered) {
			return m.Change(user, numbered)
		}
	}
	// Nine of one name in a namespace that holds three changes means the
	// leftovers are old rather than numerous; the timestamp always terminates.
	return m.Change(user, Slug("", now))
}

// taken reports whether a slug is already somebody's change. The worktree and
// the branch are asked separately: a worktree lost to a restart leaves the
// branch, and reattaching to that branch would inherit its commits.
func (m *Manager) taken(user, slug string) bool {
	if dirExists(filepath.Join(m.cfg.StateDir, "wt", user, slug)) {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), GitTimeout)
	defer cancel()
	return m.branchExists(ctx, BranchFor(user, slug))
}

// change is one attempt at opening a change, refusing rather than making room.
func (m *Manager) change(user, slug string) (*Change, error) {
	if !userPattern.MatchString(user) || strings.Contains(user, "..") {
		return nil, fmt.Errorf("%w: %q", ErrBadUser, user)
	}
	if !slugPattern.MatchString(slug) || len(slug) > 64 {
		return nil, fmt.Errorf("%w: %q; use lowercase words joined by hyphens", ErrBadSlug, slug)
	}

	branch := BranchFor(user, slug)
	if err := assertNamespace(user, branch); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.changes[branch]; ok {
		return c, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), GitTimeout)
	defer cancel()
	if err := m.checkClone(ctx); err != nil {
		return nil, err
	}

	dir := filepath.Join(m.cfg.StateDir, "wt", user, slug)
	base, baseBranch, err := m.baseRef(ctx)
	if err != nil {
		return nil, err
	}

	if !isWorktree(dir) {
		if err := m.createWorktree(ctx, user, slug, branch, dir, base); err != nil {
			return nil, err
		}
	}

	f, err := fence.NewFence(dir)
	if err != nil {
		return nil, fmt.Errorf("workdir: fencing %s: %w", dir, err)
	}
	c := &Change{
		User: user, Slug: slug, Branch: branch, Dir: dir,
		fence: f, mgr: m, base: base, baseBranch: baseBranch,
	}
	m.changes[branch] = c
	return c, nil
}

// createWorktree adds the worktree for a change that has none yet, after
// checking the per-person limit and refreshing the base branch. A branch that
// already exists is reattached to rather than reset, so a worktree lost to a
// disk wipe does not lose the commits that were pushed from it.
func (m *Manager) createWorktree(ctx context.Context, user, slug, branch, dir, base string) error {
	open, err := m.openSlugs(user)
	if err != nil {
		return err
	}
	if len(open) >= MaxOpenChanges {
		return fmt.Errorf("%w: %s already has %d open (%s); submit or abandon one first",
			ErrTooManyOpen, user, len(open), strings.Join(open, ", "))
	}

	// A worktree whose directory was removed leaves a registration behind, and
	// git refuses to reuse the branch until it is pruned.
	if _, err := m.git(ctx, m.cfg.RepoPath, "worktree", "prune"); err != nil {
		m.log.Warn("workdir: pruning worktrees", "err", err)
	}
	// The base is refreshed on the way in and never again: no auto-rebase, so a
	// change that goes stale fails loudly at submit instead of quietly moving.
	if _, err := m.git(ctx, m.cfg.RepoPath, "fetch", "--quiet", "origin"); err != nil {
		m.log.Warn("workdir: fetching origin", "err", err)
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("workdir: making the worktree directory: %w", err)
	}

	args := []string{"worktree", "add", "--quiet"}
	if m.branchExists(ctx, branch) {
		args = append(args, dir, branch)
	} else {
		args = append(args, "-b", branch, dir, base)
	}
	if _, err := m.git(ctx, m.cfg.RepoPath, args...); err != nil {
		return fmt.Errorf("workdir: opening a worktree for %s: %w", branch, err)
	}
	m.log.Info("workdir: change opened", "user", user, "slug", slug, "branch", branch, "base", base)
	return nil
}

// openSlugs lists the slugs one person has worktrees for.
func (m *Manager) openSlugs(user string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(m.cfg.StateDir, "wt", user))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workdir: reading open changes: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// List reports the user's own changes: their worktrees, their branches, and
// what GitHub knows about them when a token is configured. It never reports
// another person's change.
func (m *Manager) List(ctx context.Context, user string) ([]Info, error) {
	if !userPattern.MatchString(user) {
		return nil, fmt.Errorf("%w: %q", ErrBadUser, user)
	}
	// Merged and closed work is not a listing of what somebody is working on,
	// and GitHub is being asked about every change here anyway.
	if _, err := m.ArchiveFinished(ctx, user); err != nil {
		m.log.Warn("workdir: sweeping finished changes", "user", user, "err", err)
	}
	slugs, err := m.openSlugs(user)
	if err != nil {
		return nil, err
	}

	out := make([]Info, 0, len(slugs))
	for _, slug := range slugs {
		c, err := m.Change(user, slug)
		if err != nil {
			m.log.Warn("workdir: skipping an unreadable change", "user", user, "slug", slug, "err", err)
			continue
		}
		info := Info{Slug: slug, Branch: c.Branch, UpdatedAt: c.updatedAt()}

		files, err := c.Touched()
		if err != nil {
			m.log.Warn("workdir: reading touched files", "branch", c.Branch, "err", err)
		}
		info.Files = files
		dirty, err := c.dirtyPaths(ctx)
		if err != nil {
			m.log.Warn("workdir: reading the worktree status", "branch", c.Branch, "err", err)
		}
		info.Dirty = len(dirty) > 0

		// GitHub is advisory here: a listing that cannot reach the API is still
		// a useful listing of what is on disk.
		if !m.DryRun() {
			if pr, ok, err := m.pullRequestForBranch(ctx, c.Branch); err != nil {
				m.log.Warn("workdir: looking up the pull request", "branch", c.Branch, "err", err)
			} else if ok {
				info.PRNumber = pr.Number
				info.PRURL = pr.HTMLURL
				info.PreviewURL = PreviewURL(pr.Number)
				if state, err := m.combinedStatus(ctx, pr.Head.SHA); err != nil {
					m.log.Warn("workdir: reading the CI state", "branch", c.Branch, "err", err)
				} else {
					info.CIState = state
				}
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// updatedAt is the later of the worktree's own mtime and its last commit, so a
// change that was only edited and a change that was only committed both read
// as recent.
func (c *Change) updatedAt() time.Time {
	var t time.Time
	if info, err := os.Stat(c.Dir); err == nil {
		t = info.ModTime()
	}
	ctx, cancel := context.WithTimeout(context.Background(), GitTimeout)
	defer cancel()
	if out, err := c.mgr.git(ctx, c.Dir, "log", "-1", "--format=%ct"); err == nil {
		if secs, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64); err == nil {
			if committed := time.Unix(secs, 0); committed.After(t) {
				t = committed
			}
		}
	}
	return t
}

// Build runs hugo into the change's own output directory, then check-trees.sh if
// the site carries one, under one BuildTimeout for both, and finally the
// server's own checks on the markup and the stylesheets. The command line is
// fixed: this is the only process this server will ever run on behalf of a
// client, which is what keeps it a content editor rather than remote code
// execution with extra steps.
//
// The output is kept rather than thrown away, because render and screenshot read
// it: what the model looks at is the site this build produced, and building a
// second copy to look at would be a second answer to the same question.
func (m *Manager) Build(ctx context.Context, c *Change) (Result, error) {
	if c == nil {
		return Result{}, ErrNoChange
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	site := filepath.Join(c.Dir, "site")
	if _, err := os.Stat(filepath.Join(site, "hugo.toml")); err != nil {
		return Result{}, fmt.Errorf("workdir: no hugo site at %s: %w", site, err)
	}

	// Emptied first, every time. A page deleted from the content tree has to
	// disappear from the output with it, or render would answer out of a file
	// the site no longer has; and check-trees.sh is staged in here, which is a
	// copy that cannot be made on top of last build's.
	root := c.BuildRoot()
	if err := os.RemoveAll(root); err != nil {
		return Result{}, fmt.Errorf("workdir: clearing the last build: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Result{}, fmt.Errorf("workdir: making the build directory: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, BuildTimeout)
	defer cancel()

	started := m.cfg.Now()
	var buf bytes.Buffer
	finish := func(ok bool, err error) (Result, error) {
		text := relativise(buf.String(), c.Dir, root)
		res := Result{OK: ok, Output: strings.TrimRight(text, "\n")}
		// The stylesheets are checked whether the site built or not, because a
		// stylesheet that does not parse is exactly the mistake hugo builds
		// happily. The pages are only checked when there are pages: a failed
		// build leaves the output of the one before it, or none at all.
		res.CSS = lint.CSS(c.Dir)
		if ok {
			res.HTML = lint.HTML(c.Output())
		}
		// The duration is what the tool took, checks included: it is read as
		// "how long will this take me next time", and the checks are part of the
		// answer.
		res.Duration = m.cfg.Now().Sub(started)
		return res, err
	}

	// --logLevel error rather than --quiet: quiet mode discards hugo's own
	// error text along with everything else, and that text is the entire point
	// of the tool. This keeps the errors and drops the build statistics.
	out, err := m.run(ctx, site, m.cfg.HugoBin, "--logLevel", "error", "--destination", filepath.Join(root, "public"))
	buf.WriteString(out)
	if err != nil {
		if ctx.Err() != nil {
			return finish(false, ErrBuildTimeout)
		}
		return finish(false, nil)
	}

	// check-trees.sh asserts the public/member split. It reads its sections
	// from content-members/ and its trees from public/ and public-members/
	// beside itself, so it runs from a copy in the build directory with the
	// member tree built next to it: the worktree keeps no build output, and the
	// script on the deny list is run, never edited.
	script := filepath.Join(site, "check-trees.sh")
	members := filepath.Join(site, "content-members")
	if fileExists(script) && dirExists(members) {
		out, err = m.run(ctx, site, m.cfg.HugoBin, "--logLevel", "error", "--environment", "members",
			"--destination", filepath.Join(root, "public-members"))
		buf.WriteString(out)
		if err != nil {
			if ctx.Err() != nil {
				return finish(false, ErrBuildTimeout)
			}
			return finish(false, nil)
		}
		copied := filepath.Join(root, "check-trees.sh")
		if err := copyFile(script, copied, 0o755); err != nil {
			return finish(false, fmt.Errorf("workdir: staging check-trees.sh: %w", err))
		}
		if err := os.Symlink(members, filepath.Join(root, "content-members")); err != nil {
			return finish(false, fmt.Errorf("workdir: staging the member tree: %w", err))
		}
		out, err = m.run(ctx, root, copied)
		buf.WriteString(out)
		if err != nil {
			if ctx.Err() != nil {
				return finish(false, ErrBuildTimeout)
			}
			return finish(false, nil)
		}
	}
	// The stamp is what submit compares against the last edit. A stamp that
	// could not be written is not a build that did not happen, so the result
	// stands; the gate will ask for another build, which is the safe way round.
	if err := c.markBuild(m.cfg.Now()); err != nil {
		m.log.Warn("workdir: recording the build", "branch", c.Branch, "err", err)
	}
	return finish(true, nil)
}

// Submit stages the change's allowlisted paths, commits them authored by the
// signed-in editor, and pushes.
//
// With a GitHub credential and GITHUB_REPO set it pushes the branch to origin and
// opens a draft pull request labelled media, returning its number and the
// preview URL. Without them it pushes to the local origin only and returns the
// branch and a diffstat, marked as a dry run.
//
// It never force-pushes and never writes a branch outside media/<user>/.
func (m *Manager) Submit(ctx context.Context, c *Change, author Author, title, description string) (SubmitResult, error) {
	if c == nil {
		return SubmitResult{}, ErrNoChange
	}
	if c.IsBase() {
		return SubmitResult{}, ErrBaseView
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return SubmitResult{}, errors.New("workdir: a submission needs a title")
	}
	if err := checkAuthor(author); err != nil {
		return SubmitResult{}, err
	}
	if err := assertNamespace(c.User, c.Branch); err != nil {
		return SubmitResult{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, GitTimeout)
	defer cancel()

	if err := c.stage(ctx); err != nil {
		return SubmitResult{}, err
	}

	staged, err := c.hasStagedChanges(ctx)
	if err != nil {
		return SubmitResult{}, err
	}
	var nothingNew bool
	if staged {
		msg := []string{"--message", title}
		if d := strings.TrimSpace(description); d != "" {
			msg = append(msg, "--message", d)
		}
		args := append([]string{
			"-c", "user.name=" + m.cfg.Committer.Name,
			"-c", "user.email=" + m.cfg.Committer.Email,
			"commit", "--quiet", "--author", author.String(),
		}, msg...)
		if _, err := m.git(ctx, c.Dir, args...); err != nil {
			return SubmitResult{}, fmt.Errorf("workdir: committing: %w", err)
		}
	} else if ahead, err := c.commitsAhead(ctx); err != nil {
		return SubmitResult{}, err
	} else if ahead == 0 {
		return SubmitResult{}, ErrNothingToDo
	} else {
		// Nothing new to commit, but the branch already carries the change: a
		// second submit republishes it rather than minting an empty commit.
		nothingNew = true
	}

	head, err := m.git(ctx, c.Dir, "rev-parse", "HEAD")
	if err != nil {
		return SubmitResult{}, fmt.Errorf("workdir: reading HEAD: %w", err)
	}
	res := SubmitResult{Branch: c.Branch, Commit: strings.TrimSpace(head), DryRun: m.DryRun(), NothingNew: nothingNew}

	if stat, err := m.git(ctx, c.Dir, "diff", "--stat", c.base+"...HEAD"); err == nil {
		res.Diffstat = strings.TrimRight(stat, "\n")
	}
	if files, err := c.Touched(); err == nil {
		res.Files = files
	}

	if pushErr := m.push(ctx, c); pushErr != nil {
		if !res.DryRun || errors.Is(pushErr, ErrOutOfDate) {
			return SubmitResult{}, pushErr
		}
		// A dry run has no credential and may well have no reachable origin.
		// The commit is real and worth reporting; the failure travels with it
		// rather than being swallowed.
		res.Note = "Dry run: with no GitHub credential the change is committed on " + c.Branch +
			" but could not be pushed to origin: " + pushErr.Error()
		return res, nil
	}

	if res.DryRun {
		res.Note = "Dry run: with no GitHub credential the branch was pushed to this server's origin only, and no pull request was opened."
		return res, nil
	}

	pr, err := m.ensurePullRequest(ctx, c, author, title, description, res.Files)
	if err != nil {
		return SubmitResult{}, err
	}
	res.PRNumber = pr.Number
	res.PRURL = pr.HTMLURL
	res.PreviewURL = PreviewURL(pr.Number)
	if err := m.addLabel(ctx, pr.Number, PRLabel); err != nil {
		// A missing label is a review inconvenience, not a failed submission.
		m.log.Warn("workdir: labelling the pull request", "pr", pr.Number, "err", err)
		res.Note = "the pull request is open but could not be labelled " + PRLabel
	}
	res.Note = strings.TrimSpace(res.Note + " The preview is ready in about a minute.")
	m.log.Info("workdir: change submitted", "user", c.User, "branch", c.Branch, "pr", pr.Number)
	return res, nil
}

// ensurePullRequest opens the draft pull request, or finds the one already
// open for the branch when a change is submitted a second time.
func (m *Manager) ensurePullRequest(ctx context.Context, c *Change, author Author, title, description string, files []string) (PullRequest, error) {
	if pr, ok, err := m.pullRequestForBranch(ctx, c.Branch); err == nil && ok {
		return pr, nil
	}
	pr, err := m.openPullRequest(ctx, c.Branch, c.baseBranch, title, prBody(author, description, files))
	if err == nil {
		return pr, nil
	}
	// GitHub refuses a second pull request for the same head with 422; that
	// refusal means the first one is still the right answer.
	if pr, ok, lookupErr := m.pullRequestForBranch(ctx, c.Branch); lookupErr == nil && ok {
		return pr, nil
	}
	return PullRequest{}, err
}

// prBody names the editor and the files, which is what a maintainer reads
// first and what makes an unreviewed media change obvious.
func prBody(author Author, description string, files []string) string {
	var b strings.Builder
	if d := strings.TrimSpace(description); d != "" {
		b.WriteString(d)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "Submitted through the Prodeko content editor by %s.\n", author)
	if len(files) > 0 {
		b.WriteString("\nFiles:\n")
		for _, f := range files {
			b.WriteString("- `" + f + "`\n")
		}
	}
	return b.String()
}

// stage puts the change's allowlisted paths in the index and nothing else. The
// index is emptied first, so a path staged by something other than this
// function cannot ride along into the commit.
func (c *Change) stage(ctx context.Context) error {
	if _, err := c.mgr.git(ctx, c.Dir, "reset", "--quiet"); err != nil {
		return fmt.Errorf("workdir: clearing the index: %w", err)
	}
	paths, err := c.dirtyPaths(ctx)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil
	}

	// The fence is asked about every path a second time, on the filesystem this
	// time: dirtyPaths has already dropped what may not be committed, so this
	// loop is the assertion that nothing else can reach `git add`.
	var total int64
	for _, p := range paths {
		size, err := c.fence.CheckStage(p)
		if err != nil {
			return err
		}
		total += size
	}
	if err := c.fence.CheckChange(paths, total); err != nil {
		return err
	}

	args := append([]string{"add", "--"}, paths...)
	if _, err := c.mgr.git(ctx, c.Dir, args...); err != nil {
		return fmt.Errorf("workdir: staging: %w", err)
	}
	return nil
}

// dirtyPaths is the worktree's uncommitted, allowlisted changes. Anything the
// fence would not have let a tool write — build output, a stray file, a symlink
// — is dropped rather than committed, so the commit can only contain paths a
// tool could have produced.
//
// The check is the fence's own, filesystem included, and not the lexical rule
// alone: a symlink is exactly what the tool layer cannot make and what a
// path-pattern match cannot see.
func (c *Change) dirtyPaths(ctx context.Context) ([]string, error) {
	out, err := c.mgr.git(ctx, c.Dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil, fmt.Errorf("workdir: reading the worktree status: %w", err)
	}
	var paths []string
	for _, p := range parsePorcelainZ(out) {
		if _, err := c.fence.CheckStage(p); err != nil {
			c.mgr.log.Warn("workdir: not committing a path the fence refuses", "branch", c.Branch, "path", p, "err", err)
			continue
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}

// parsePorcelainZ reads git status --porcelain=v1 -z. A rename carries its
// origin as a second NUL-terminated field, which has to be consumed or it
// reads as a path of its own.
func parsePorcelainZ(out string) []string {
	fields := strings.Split(out, "\x00")
	var paths []string
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 4 {
			continue
		}
		x, y, path := entry[0], entry[1], entry[3:]
		if x == 'R' || y == 'R' || x == 'C' || y == 'C' {
			if i+1 < len(fields) {
				// The origin of the rename is a change too: it was deleted.
				if from := fields[i+1]; from != "" {
					paths = append(paths, from)
				}
				i++
			}
		}
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func (c *Change) hasStagedChanges(ctx context.Context) (bool, error) {
	_, err := c.mgr.git(ctx, c.Dir, "diff", "--cached", "--quiet")
	if err == nil {
		return false, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return true, nil
	}
	return false, fmt.Errorf("workdir: inspecting the index: %w", err)
}

func (c *Change) commitsAhead(ctx context.Context) (int, error) {
	out, err := c.mgr.git(ctx, c.Dir, "rev-list", "--count", c.base+"..HEAD")
	if err != nil {
		return 0, fmt.Errorf("workdir: counting commits: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, fmt.Errorf("workdir: counting commits: %w", err)
	}
	return n, nil
}

// push sends the branch, never forced and never under another name. The
// refspec is spelled out on both sides so no remote configuration can redirect
// it, and the namespace is asserted immediately before the command runs.
func (m *Manager) push(ctx context.Context, c *Change) error {
	if err := assertNamespace(c.User, c.Branch); err != nil {
		return err
	}
	refspec := "refs/heads/" + c.Branch + ":refs/heads/" + c.Branch

	remote := "origin"
	url, ok, err := m.credentialedRemote(ctx, c)
	if err != nil {
		return err
	}
	if ok {
		remote = url
	}
	out, err := m.git(ctx, c.Dir, "push", remote, refspec)
	if err != nil {
		text := m.redact(out + " " + err.Error())
		if rejected(text) {
			return fmt.Errorf("%w (git said: %s)", ErrOutOfDate, firstLine(text))
		}
		return fmt.Errorf("workdir: pushing %s: %s", c.Branch, firstLine(text))
	}
	return nil
}

// credentialedRemote is the https URL to push to when the configured token is
// the credential that will be accepted there. An origin that is not the
// configured github.com repository — a local fixture, an ssh remote — already
// carries its own credential and is pushed to by name.
//
// The token travels in argv rather than in a config file: this process is
// alone on its host and its state volume holds nothing secret, and a token
// written to .git/config outlives the push.
func (m *Manager) credentialedRemote(ctx context.Context, c *Change) (string, bool, error) {
	if m.DryRun() {
		return "", false, nil
	}
	out, err := m.git(ctx, c.Dir, "remote", "get-url", "origin")
	if err != nil {
		return "", false, nil
	}
	origin := strings.TrimSpace(out)
	if !strings.HasPrefix(origin, "https://github.com/") {
		return "", false, nil
	}
	token, err := m.cfg.GitHubCredential.Token(ctx)
	if err != nil {
		return "", false, fmt.Errorf("workdir: no GitHub credential to push with: %w", err)
	}
	return "https://x-access-token:" + token + "@github.com/" + m.cfg.GitHubRepo + ".git", true, nil
}

// rejected recognises the push failures that mean the branch moved underneath
// this change. There is no auto-rebase: a media person is never asked to
// resolve a conflict, so the answer is to start again.
func rejected(out string) bool {
	for _, s := range []string{"[rejected]", "non-fast-forward", "fetch first", "Updates were rejected"} {
		if strings.Contains(out, s) {
			return true
		}
	}
	return false
}

// credentialPattern matches every form a GitHub token takes, and the userinfo
// of a credentialed push URL. An App's installation token changes every hour,
// so there is no single value to search for.
var credentialPattern = regexp.MustCompile(`x-access-token:[^@\s]*@|\bgh[pousr]_[A-Za-z0-9_]+|\bgithub_pat_[A-Za-z0-9_]+`)

// redact keeps the push credential out of anything returned or logged.
func (m *Manager) redact(s string) string {
	if m.cfg.GitHubToken != "" {
		s = strings.ReplaceAll(s, m.cfg.GitHubToken, "[redacted]")
	}
	return credentialPattern.ReplaceAllStringFunc(s, func(match string) string {
		if strings.HasPrefix(match, "x-access-token:") {
			return "x-access-token:[redacted]@"
		}
		return "[redacted]"
	})
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i]) + " ..."
	}
	return s
}

// assertNamespace is the one invariant this package exists to hold: nothing
// outside media/<user>/ is ever written. It runs before the worktree is made
// and again before every push.
func assertNamespace(user, branch string) error {
	want := BranchPrefix + user + "/"
	if !strings.HasPrefix(branch, want) || len(branch) == len(want) {
		return fmt.Errorf("workdir: refusing to touch %q, which is outside %s", branch, want)
	}
	if strings.Contains(branch, "..") || strings.HasSuffix(branch, ".lock") || strings.Contains(branch, " ") {
		return fmt.Errorf("workdir: refusing to touch %q, which is not a legal branch name", branch)
	}
	return nil
}

// checkAuthor refuses an identity that would break the commit header. It comes
// from Keycloak rather than from the client, so this is a guard against a
// surprising realm rather than against an attacker.
func checkAuthor(a Author) error {
	if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Email) == "" {
		return errors.New("workdir: the commit author needs a name and an email")
	}
	for _, s := range []string{a.Name, a.Email} {
		if strings.ContainsAny(s, "\n\r<>") {
			return fmt.Errorf("workdir: %q cannot appear in a commit author", s)
		}
	}
	return nil
}

// checkClone reports whether MCP_REPO_PATH is what it claims to be, in words a
// deployer can act on. It is checked on every first change rather than at
// startup, because a state volume that failed to mount should not keep the
// health check and the OAuth endpoints from answering.
func (m *Manager) checkClone(ctx context.Context) error {
	info, err := os.Stat(m.cfg.RepoPath)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workdir: MCP_REPO_PATH %s does not exist; it must be an existing clone of the site repository", m.cfg.RepoPath)
	}
	if err != nil {
		return fmt.Errorf("workdir: MCP_REPO_PATH %s cannot be read: %w", m.cfg.RepoPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workdir: MCP_REPO_PATH %s is not a directory", m.cfg.RepoPath)
	}
	if _, err := m.git(ctx, m.cfg.RepoPath, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("workdir: MCP_REPO_PATH %s is not a git clone: %w", m.cfg.RepoPath, err)
	}
	return nil
}

// baseRef is the origin default branch, as a ref to cut from and as the branch
// name a pull request opens against.
func (m *Manager) baseRef(ctx context.Context) (string, string, error) {
	if out, err := m.git(ctx, m.cfg.RepoPath, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if ref := strings.TrimSpace(out); ref != "" {
			return ref, strings.TrimPrefix(ref, "origin/"), nil
		}
	}
	for _, name := range []string{"main", "master"} {
		if _, err := m.git(ctx, m.cfg.RepoPath, "rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+name); err == nil {
			return "origin/" + name, name, nil
		}
	}
	// A clone with no remote-tracking branches is a local fixture; its own
	// checked-out branch is the only base there is.
	if out, err := m.git(ctx, m.cfg.RepoPath, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		if b := strings.TrimSpace(out); b != "" && b != "HEAD" {
			return b, b, nil
		}
	}
	return "", "", fmt.Errorf("workdir: the clone at %s has no default branch to base a change on", m.cfg.RepoPath)
}

func (m *Manager) branchExists(ctx context.Context, branch string) bool {
	_, err := m.git(ctx, m.cfg.RepoPath, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// git runs one git command in dir and returns its combined output. Arguments
// are explicit; there is no shell, so nothing a client sent is ever parsed as
// one.
func (m *Manager) git(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := m.run(ctx, dir, m.cfg.GitBin, args...)
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", args[0], err, firstLine(m.redact(out)))
	}
	return out, nil
}

// run executes one fixed command line and returns stdout and stderr
// interleaved, which is the form a build error wants to be read in.
//
// The environment is built rather than inherited: the process's own HOME would
// bring a developer's git configuration into a commit, and a hook or a
// credential helper configured there would run under the editor's hands.
func (m *Manager) run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + m.cfg.StateDir,
		"TMPDIR=" + os.TempDir(),
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_ASKPASS=",
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	// Killing hugo does not kill anything hugo started, and a grandchild
	// holding the output pipe open would keep the timeout from being a timeout.
	// Whatever was written before the kill is enough to report.
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	return buf.String(), err
}

// relativise turns the absolute paths hugo prints into the repository-relative
// ones every tool speaks. A template error names the worktree root several
// times on one line, and that root discloses the state directory, the username
// and the branch slug to whoever is connected while saying nothing the model
// can act on.
//
// The build directory is a temporary name that will not exist by the time the
// output is read, so it is named for what it is instead.
func relativise(out, worktree, build string) string {
	for _, name := range variants(build) {
		out = replacePrefix(out, name, "<build>")
	}
	for _, name := range variants(worktree) {
		out = replacePrefix(out, name, "")
	}
	return out
}

// variants is a directory under both the name it was given and the name it has
// once symlinks are resolved: hugo reports whichever it opened.
func variants(dir string) []string {
	if dir == "" {
		return nil
	}
	out := []string{dir}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != dir {
		out = append(out, resolved)
	}
	return out
}

func replacePrefix(out, dir, label string) string {
	sep := string(filepath.Separator)
	if label == "" {
		out = strings.ReplaceAll(out, dir+sep, "")
		// The root on its own is still a path and still has to say something.
		return strings.ReplaceAll(out, dir, ".")
	}
	out = strings.ReplaceAll(out, dir+sep, label+sep)
	return strings.ReplaceAll(out, dir, label)
}

// PreviewURL is where the preview pipeline publishes pull request n.
func PreviewURL(n int) string { return fmt.Sprintf(PreviewURLFormat, n) }

// BranchFor names the branch of a change. It is the only place the namespace
// is constructed, so the rule that nothing outside media/<user>/ is written
// can be checked in one place.
func BranchFor(user, slug string) string {
	return BranchPrefix + user + "/" + slug
}

// transliterations carry the letters a Finnish or Swedish title is most likely
// to open with into the ASCII a branch name is limited to. Dropping them
// instead would turn "Tapahtumat ja ähkyt" into a slug that reads wrong.
var transliterations = map[rune]string{
	'ä': "a", 'å': "a", 'á': "a", 'à': "a", 'â': "a", 'ã': "a",
	'ö': "o", 'ø': "o", 'ó': "o", 'ò': "o", 'ô': "o", 'õ': "o",
	'é': "e", 'è': "e", 'ê': "e", 'ë': "e",
	'í': "i", 'ì': "i", 'î': "i", 'ï': "i",
	'ú': "u", 'ù': "u", 'û': "u", 'ü': "u",
	'ý': "y", 'ñ': "n", 'ç': "c", 'š': "s", 'ž': "z",
	'æ': "ae", 'ß': "ss", 'þ': "th", 'ð': "d",
}

// maxSlugLen keeps a branch name readable in a pull request list. Titles are
// sentences; the first few words are the part that identifies the change.
const maxSlugLen = 48

// Slug turns a title into the branch's slug component: lowercase, ASCII,
// hyphen-separated, short, and never empty. A title with nothing ASCII left in
// it falls back to the timestamp, which is why now is a parameter.
func Slug(title string, now time.Time) string {
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			if sub, ok := transliterations[r]; ok {
				b.WriteString(sub)
			} else {
				b.WriteByte('-')
			}
		}
	}
	words := strings.FieldsFunc(b.String(), func(r rune) bool { return r == '-' })

	var slug string
	for _, w := range words {
		if slug == "" {
			slug = w
			continue
		}
		if len(slug)+1+len(w) > maxSlugLen {
			break
		}
		slug += "-" + w
	}
	if len(slug) > maxSlugLen {
		slug = slug[:maxSlugLen]
	}
	slug = strings.Trim(slug, "-")
	if slug == "" {
		if now.IsZero() {
			now = time.Now()
		}
		return "change-" + now.UTC().Format("20060102-150405")
	}
	return slug
}

// isWorktree reports whether dir is already a checked-out worktree. A linked
// worktree carries a .git file rather than a directory, but either form means
// the tree is there and is reattached to instead of being made again.
func isWorktree(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
