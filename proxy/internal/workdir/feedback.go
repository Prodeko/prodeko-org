package workdir

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The review side of a change's pull request, and the way out of a change
// that review turned down. Everything GitHub said travels verbatim: a
// maintainer's sentence is the instruction the editor asked to hear, and a
// paraphrase of it would be this server pretending to understand the review.

// maxFeedbackItems bounds each list read back from GitHub. A media change
// draws a handful of comments; hundreds would be a conversation to have on
// GitHub itself, not through a tool result.
const maxFeedbackItems = 50

// Feedback is what the maintainers have said about a change so far.
type Feedback struct {
	PRNumber   int
	PRURL      string
	PreviewURL string
	State      string // "open", "merged" or "closed"
	CIState    string // GitHub's combined state, "" when unknown
	Reviews    []Review
	Comments   []Comment
}

// Review is one submitted review: a verdict and the sentence that came with it.
type Review struct {
	Author string
	State  string // APPROVED, CHANGES_REQUESTED, COMMENTED, ...
	Body   string
	At     time.Time
}

// Comment is one comment, either on the conversation or on a line of a file.
// Path and Line are empty for a conversation comment.
type Comment struct {
	Author string
	Body   string
	Path   string
	Line   int
	At     time.Time
}

// ErrNeverSubmitted is feedback asked of a change that has no pull request.
var ErrNeverSubmitted = errors.New("workdir: this change has no pull request")

// Feedback reads the change's pull request back from GitHub: its state, the
// reviews and every comment, maintainers' and bots' alike. The pull request
// is looked up in any state, because "it was merged" and "it was closed
// without merging" are exactly the answers the editor is asking for.
func (m *Manager) Feedback(ctx context.Context, c *Change) (Feedback, error) {
	if c == nil {
		return Feedback{}, ErrNoChange
	}
	if c.IsBase() {
		return Feedback{}, ErrBaseView
	}
	if m.DryRun() {
		return Feedback{}, errors.New("workdir: with no GitHub credential there is no pull request to read feedback from")
	}

	pr, ok, err := m.pullRequestForBranchState(ctx, c.Branch, "all")
	if err != nil {
		return Feedback{}, err
	}
	if !ok {
		return Feedback{}, ErrNeverSubmitted
	}

	fb := Feedback{
		PRNumber:   pr.Number,
		PRURL:      pr.HTMLURL,
		PreviewURL: PreviewURL(pr.Number),
		State:      pr.State,
	}
	if pr.MergedAt != "" {
		fb.State = "merged"
	}
	if state, err := m.combinedStatus(ctx, pr.Head.SHA); err == nil {
		fb.CIState = state
	}

	if fb.Reviews, err = m.listReviews(ctx, pr.Number); err != nil {
		return Feedback{}, err
	}
	comments, err := m.listIssueComments(ctx, pr.Number)
	if err != nil {
		return Feedback{}, err
	}
	inline, err := m.listReviewComments(ctx, pr.Number)
	if err != nil {
		return Feedback{}, err
	}
	fb.Comments = append(comments, inline...)
	return fb, nil
}

// AbandonResult says what abandoning did, so the answer can name what is gone.
type AbandonResult struct {
	Slug     string
	Branch   string
	PRNumber int    // 0 when there was none to close
	Note     string // what could not be done, when something could not
}

// Abandon is the way out of a change that went wrong: it closes the pull
// request, deletes the remote branch, and removes the worktree and the local
// branch. The edits are gone afterwards; that is the point, and the caller's
// wording has to say so before this is called.
//
// Remote failures are reported in the result rather than aborting: a pull
// request that could not be closed is a nuisance, a worktree left behind
// holds one of the person's three change slots.
func (m *Manager) Abandon(ctx context.Context, c *Change) (AbandonResult, error) {
	if c == nil {
		return AbandonResult{}, ErrNoChange
	}
	if c.IsBase() {
		return AbandonResult{}, ErrBaseView
	}
	if err := assertNamespace(c.User, c.Branch); err != nil {
		return AbandonResult{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	res := AbandonResult{Slug: c.Slug, Branch: c.Branch}
	var notes []string

	if !m.DryRun() {
		if pr, ok, err := m.pullRequestForBranch(ctx, c.Branch); err != nil {
			notes = append(notes, "the pull request could not be looked up: "+err.Error())
		} else if ok {
			if err := m.closePullRequest(ctx, pr.Number); err != nil {
				notes = append(notes, fmt.Sprintf("pull request #%d could not be closed: %s", pr.Number, err.Error()))
			} else {
				res.PRNumber = pr.Number
			}
		}
		if err := m.deleteBranchRef(ctx, c.Branch); err != nil {
			// A branch that was never pushed is already absent, which GitHub
			// reports as a failed deletion; absent is what was asked for.
			if !strings.Contains(err.Error(), "422") {
				notes = append(notes, "the branch on GitHub could not be deleted: "+err.Error())
			}
		}
	}

	ctx, cancel := context.WithTimeout(ctx, GitTimeout)
	defer cancel()
	if _, err := m.git(ctx, m.cfg.RepoPath, "worktree", "remove", "--force", c.Dir); err != nil {
		// Whatever git could not remove, os can: the tree is disposable by
		// definition here, and a slot held by a broken worktree helps nobody.
		if rmErr := os.RemoveAll(c.Dir); rmErr != nil {
			return AbandonResult{}, fmt.Errorf("workdir: removing the worktree: %w", err)
		}
		if _, err := m.git(ctx, m.cfg.RepoPath, "worktree", "prune"); err != nil {
			m.log.Warn("workdir: pruning after an abandon", "branch", c.Branch, "err", err)
		}
	}
	// The user directory is kept only while it has changes in it.
	_ = os.Remove(filepath.Dir(c.Dir))
	if _, err := m.git(ctx, m.cfg.RepoPath, "branch", "-D", c.Branch); err != nil {
		m.log.Warn("workdir: deleting the local branch", "branch", c.Branch, "err", err)
	}

	m.mu.Lock()
	delete(m.changes, c.Branch)
	m.mu.Unlock()

	res.Note = strings.Join(notes, "; ")
	m.log.Info("workdir: change abandoned", "user", c.User, "branch", c.Branch, "pr", res.PRNumber)
	return res, nil
}

// Existing is the change (user, slug) only if it is already open on disk.
// Abandoning goes through here rather than Change, which would create a
// worktree for the pleasure of deleting it.
func (m *Manager) Existing(user, slug string) (*Change, error) {
	slugs, err := m.openSlugs(user)
	if err != nil {
		return nil, err
	}
	for _, s := range slugs {
		if s == slug {
			return m.Change(user, slug)
		}
	}
	if len(slugs) == 0 {
		return nil, fmt.Errorf("%w: you have no open changes", ErrNoChange)
	}
	return nil, fmt.Errorf("%w: no change named %q; open are %s", ErrNoChange, slug, strings.Join(slugs, ", "))
}

// pullRequestForBranchState is pullRequestForBranch for any state. GitHub
// lists newest first, so the first match is the pull request the branch's
// latest submit belongs to.
func (m *Manager) pullRequestForBranchState(ctx context.Context, branch, state string) (PullRequest, bool, error) {
	owner, _, ok := strings.Cut(m.cfg.GitHubRepo, "/")
	if !ok {
		return PullRequest{}, false, fmt.Errorf("workdir: GITHUB_REPO %q is not owner/repo", m.cfg.GitHubRepo)
	}
	q := url.Values{
		"head":     {owner + ":" + branch},
		"state":    {state},
		"per_page": {"1"},
	}
	var prs []PullRequest
	if err := m.api(ctx, http.MethodGet, "/repos/"+m.cfg.GitHubRepo+"/pulls?"+q.Encode(), nil, &prs); err != nil {
		return PullRequest{}, false, err
	}
	for _, pr := range prs {
		if pr.Head.Ref == branch {
			return pr, true, nil
		}
	}
	return PullRequest{}, false, nil
}

// listReviews is the submitted reviews of a pull request, oldest first, as
// GitHub returns them.
func (m *Manager) listReviews(ctx context.Context, number int) ([]Review, error) {
	var raw []struct {
		User        struct{ Login string }
		State       string
		Body        string
		SubmittedAt time.Time `json:"submitted_at"`
	}
	path := fmt.Sprintf("/repos/%s/pulls/%d/reviews?per_page=%d", m.cfg.GitHubRepo, number, maxFeedbackItems)
	if err := m.api(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Review, 0, len(raw))
	for _, r := range raw {
		// A review with no verdict and no words is GitHub's artefact of an
		// inline comment, which listReviewComments already carries.
		if r.Body == "" && r.State == "COMMENTED" {
			continue
		}
		out = append(out, Review{Author: r.User.Login, State: r.State, Body: r.Body, At: r.SubmittedAt})
	}
	return out, nil
}

// listIssueComments is the conversation under the pull request, the preview
// bot's comment included.
func (m *Manager) listIssueComments(ctx context.Context, number int) ([]Comment, error) {
	var raw []struct {
		User      struct{ Login string }
		Body      string
		CreatedAt time.Time `json:"created_at"`
	}
	path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=%d", m.cfg.GitHubRepo, number, maxFeedbackItems)
	if err := m.api(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Comment, 0, len(raw))
	for _, r := range raw {
		out = append(out, Comment{Author: r.User.Login, Body: r.Body, At: r.CreatedAt})
	}
	return out, nil
}

// listReviewComments is the comments left on lines of the diff.
func (m *Manager) listReviewComments(ctx context.Context, number int) ([]Comment, error) {
	var raw []struct {
		User      struct{ Login string }
		Body      string
		Path      string
		Line      int
		CreatedAt time.Time `json:"created_at"`
	}
	path := fmt.Sprintf("/repos/%s/pulls/%d/comments?per_page=%d", m.cfg.GitHubRepo, number, maxFeedbackItems)
	if err := m.api(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Comment, 0, len(raw))
	for _, r := range raw {
		out = append(out, Comment{Author: r.User.Login, Body: r.Body, Path: r.Path, Line: r.Line, At: r.CreatedAt})
	}
	return out, nil
}

// closePullRequest closes without merging. Nothing here can merge: publishing
// stays a maintainer's decision.
func (m *Manager) closePullRequest(ctx context.Context, number int) error {
	req := struct {
		State string `json:"state"`
	}{State: "closed"}
	path := fmt.Sprintf("/repos/%s/pulls/%d", m.cfg.GitHubRepo, number)
	return m.api(ctx, http.MethodPatch, path, req, nil)
}

// deleteBranchRef deletes the change's branch on GitHub. The namespace was
// asserted by the caller; this is spelled as a full ref so nothing shorter
// than an exact branch name can be deleted.
func (m *Manager) deleteBranchRef(ctx context.Context, branch string) error {
	return m.api(ctx, http.MethodDelete, "/repos/"+m.cfg.GitHubRepo+"/git/refs/heads/"+branch, nil, nil)
}
