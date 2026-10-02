package workdir

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// The GitHub REST calls submit makes, over net/http against api.github.com.
// Four endpoints, no client library: opening a draft pull request, labelling
// it, and reading back what list_my_changes reports.
//
// The credential is a fine-grained token on a bot account separate from the
// Decap proxy's, so media access can be revoked without signing out every
// editor. Attribution does not depend on it: the author of the commit is the
// editor's Keycloak identity.

const (
	githubAPIVersion = "2022-11-28"
	githubUserAgent  = "prodeko-content-editor-mcp"

	// maxAPIResponseBytes bounds what is read back from a response. These are
	// small documents; a large one is a wrong answer, not a big one.
	maxAPIResponseBytes = 1 << 20
)

// PullRequest is the part of GitHub's pull request object this server uses.
type PullRequest struct {
	Number   int    `json:"number"`
	HTMLURL  string `json:"html_url"`
	State    string `json:"state"`
	Draft    bool   `json:"draft"`
	Title    string `json:"title"`
	MergedAt string `json:"merged_at"` // "" until merged; GitHub closes a merged PR
	Head     struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
}

// openPullRequest opens a draft pull request from branch onto base.
// Draft-on-open is what fires the preview build.
func (m *Manager) openPullRequest(ctx context.Context, branch, base, title, body string) (PullRequest, error) {
	req := struct {
		Title string `json:"title"`
		Head  string `json:"head"`
		Base  string `json:"base"`
		Body  string `json:"body"`
		Draft bool   `json:"draft"`
	}{Title: title, Head: branch, Base: base, Body: body, Draft: true}

	var pr PullRequest
	if err := m.api(ctx, http.MethodPost, "/repos/"+m.cfg.GitHubRepo+"/pulls", req, &pr); err != nil {
		return PullRequest{}, err
	}
	return pr, nil
}

// addLabel puts PRLabel on a pull request.
func (m *Manager) addLabel(ctx context.Context, number int, label string) error {
	req := struct {
		Labels []string `json:"labels"`
	}{Labels: []string{label}}
	path := fmt.Sprintf("/repos/%s/issues/%d/labels", m.cfg.GitHubRepo, number)
	return m.api(ctx, http.MethodPost, path, req, nil)
}

// pullRequestForBranch finds the open pull request whose head is branch, if
// there is one. A change that was never submitted has none.
func (m *Manager) pullRequestForBranch(ctx context.Context, branch string) (PullRequest, bool, error) {
	owner, _, ok := strings.Cut(m.cfg.GitHubRepo, "/")
	if !ok {
		return PullRequest{}, false, fmt.Errorf("workdir: GITHUB_REPO %q is not owner/repo", m.cfg.GitHubRepo)
	}
	q := url.Values{
		"head":     {owner + ":" + branch},
		"state":    {"open"},
		"per_page": {"1"},
	}
	var prs []PullRequest
	if err := m.api(ctx, http.MethodGet, "/repos/"+m.cfg.GitHubRepo+"/pulls?"+q.Encode(), nil, &prs); err != nil {
		return PullRequest{}, false, err
	}
	for _, pr := range prs {
		// GitHub filters by head itself; this only refuses to report a pull
		// request for a branch other than the one asked about.
		if pr.Head.Ref == branch {
			return pr, true, nil
		}
	}
	return PullRequest{}, false, nil
}

// PullRequestByNumber is one pull request, in whatever state it is in. It is
// how resume_change turns a number somebody read on GitHub back into a branch.
func (m *Manager) PullRequestByNumber(ctx context.Context, number int) (PullRequest, error) {
	var pr PullRequest
	path := fmt.Sprintf("/repos/%s/pulls/%d", m.cfg.GitHubRepo, number)
	err := m.api(ctx, http.MethodGet, path, nil, &pr)
	return pr, err
}

// Finished reports work review has ended: merged into the site, or closed
// without merging. Either way the change under it has nothing left to do, and
// GitHub closes a merged pull request, so merged_at is what tells the two
// apart.
func Finished(pr PullRequest) bool {
	return pr.MergedAt != "" || pr.State == "closed"
}

// combinedStatus is the CI state of a commit, as list_my_changes reports it.
func (m *Manager) combinedStatus(ctx context.Context, sha string) (string, error) {
	if sha == "" {
		return "", nil
	}
	var res struct {
		State string `json:"state"`
	}
	if err := m.api(ctx, http.MethodGet, "/repos/"+m.cfg.GitHubRepo+"/commits/"+url.PathEscape(sha)+"/status", nil, &res); err != nil {
		return "", err
	}
	return res.State, nil
}

// api makes one REST call. The token is attached here and travels nowhere
// else; a non-2xx answer carries GitHub's own message, because "422" alone
// never told anyone what to do next.
func (m *Manager) api(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("workdir: encoding the %s %s request: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, m.cfg.APIRoot+path, reader)
	if err != nil {
		return fmt.Errorf("workdir: building the %s %s request: %w", method, path, err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", githubUserAgent)
	if m.cfg.GitHubCredential != nil {
		token, err := m.cfg.GitHubCredential.Token(ctx)
		if err != nil {
			return fmt.Errorf("workdir: no GitHub credential for %s %s: %w", method, path, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := m.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("workdir: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseBytes))
	if err != nil {
		return fmt.Errorf("workdir: reading the %s %s response: %w", method, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("workdir: %s %s: GitHub answered %d: %s", method, path, resp.StatusCode, apiMessage(data))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("workdir: decoding the %s %s response: %w", method, path, err)
	}
	return nil
}

// apiMessage pulls the human part out of a GitHub error document, falling back
// to the raw body when it is not one.
func apiMessage(data []byte) string {
	var doc struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &doc); err == nil && doc.Message != "" {
		parts := []string{doc.Message}
		for _, e := range doc.Errors {
			if e.Message != "" {
				parts = append(parts, e.Message)
			}
		}
		return strings.Join(parts, "; ")
	}
	return firstLine(string(data))
}
