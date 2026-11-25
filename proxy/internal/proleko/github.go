package proleko

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prodeko/prodeko-org/proxy/internal/ghauth"
)

// Author is a git identity.
type Author struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// change is one commit's worth of files: path → content, and paths to remove.
type change struct {
	files   map[string][]byte
	removed []string
}

// published says where a change ended up: merged into the branch, or waiting
// in a pull request for the review that branch protection asks for.
type published struct {
	Merged bool
	PRURL  string
	Commit string
}

// github is the handful of REST calls one Proleko? change needs. The
// repository is pinned in configuration exactly as forward pins it: nothing
// in a request names it.
type github struct {
	api        string
	owner      string
	repo       string
	branch     string
	credential ghauth.Source
	committer  Author
	client     *http.Client
	now        func() time.Time
}

// apiError is a non-2xx answer from GitHub.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("GitHub answered %d: %s", e.Status, e.Message)
}

func (g *github) call(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.api+path, reader)
	if err != nil {
		return err
	}
	token, err := g.credential.Token(ctx)
	if err != nil {
		return fmt.Errorf("GitHub credential: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		return &apiError{Status: resp.StatusCode, Message: e.Message}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (g *github) repoPath(rest string) string {
	return "/repos/" + url.PathEscape(g.owner) + "/" + url.PathEscape(g.repo) + rest
}

// read returns a file on the branch, or nil and no error when it does not
// exist.
func (g *github) read(ctx context.Context, path string) ([]byte, error) {
	var file struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	err := g.call(ctx, http.MethodGet,
		g.repoPath("/contents/"+escapePath(path)+"?ref="+url.QueryEscape(g.branch)), nil, &file)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if file.Encoding != "base64" {
		return nil, fmt.Errorf("%s: unexpected encoding %q", path, file.Encoding)
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
}

// publish commits the change on a branch of its own, opens a pull request and
// merges it. The detour through a pull request is what lets the same code
// work whether or not main is protected: where a review is required, the
// merge is refused and the pull request is left open for a reviewer, which
// is the same step a Decap editor's change waits at.
func (g *github) publish(ctx context.Context, ch change, author Author, branchName, title, body string) (published, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := g.call(ctx, http.MethodGet, g.repoPath("/git/ref/heads/"+escapePath(g.branch)), nil, &ref); err != nil {
		return published{}, fmt.Errorf("reading %s: %w", g.branch, err)
	}
	base := ref.Object.SHA

	var baseCommit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := g.call(ctx, http.MethodGet, g.repoPath("/git/commits/"+base), nil, &baseCommit); err != nil {
		return published{}, fmt.Errorf("reading commit %s: %w", base, err)
	}

	type entry struct {
		Path string  `json:"path"`
		Mode string  `json:"mode"`
		Type string  `json:"type"`
		SHA  *string `json:"sha"`
	}
	var entries []entry
	for path, content := range ch.files {
		var blob struct {
			SHA string `json:"sha"`
		}
		if err := g.call(ctx, http.MethodPost, g.repoPath("/git/blobs"), map[string]string{
			"content":  base64.StdEncoding.EncodeToString(content),
			"encoding": "base64",
		}, &blob); err != nil {
			return published{}, fmt.Errorf("uploading %s: %w", path, err)
		}
		sha := blob.SHA
		entries = append(entries, entry{Path: path, Mode: "100644", Type: "blob", SHA: &sha})
	}
	for _, path := range ch.removed {
		// A null sha deletes the path from the tree.
		entries = append(entries, entry{Path: path, Mode: "100644", Type: "blob", SHA: nil})
	}

	var tree struct {
		SHA string `json:"sha"`
	}
	if err := g.call(ctx, http.MethodPost, g.repoPath("/git/trees"), map[string]any{
		"base_tree": baseCommit.Tree.SHA,
		"tree":      entries,
	}, &tree); err != nil {
		return published{}, fmt.Errorf("writing the tree: %w", err)
	}

	when := g.now().UTC().Format(time.RFC3339)
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := g.call(ctx, http.MethodPost, g.repoPath("/git/commits"), map[string]any{
		"message":   title + "\n\n" + body,
		"tree":      tree.SHA,
		"parents":   []string{base},
		"author":    map[string]string{"name": author.Name, "email": author.Email, "date": when},
		"committer": map[string]string{"name": g.committer.Name, "email": g.committer.Email, "date": when},
	}, &commit); err != nil {
		return published{}, fmt.Errorf("writing the commit: %w", err)
	}

	if err := g.call(ctx, http.MethodPost, g.repoPath("/git/refs"), map[string]string{
		"ref": "refs/heads/" + branchName,
		"sha": commit.SHA,
	}, nil); err != nil {
		return published{}, fmt.Errorf("creating branch %s: %w", branchName, err)
	}

	var pr struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := g.call(ctx, http.MethodPost, g.repoPath("/pulls"), map[string]string{
		"title": title,
		"head":  branchName,
		"base":  g.branch,
		"body":  body,
	}, &pr); err != nil {
		return published{}, fmt.Errorf("opening the pull request: %w", err)
	}

	out := published{PRURL: pr.HTMLURL, Commit: commit.SHA}
	err := g.call(ctx, http.MethodPut, g.repoPath(fmt.Sprintf("/pulls/%d/merge", pr.Number)), map[string]string{
		"merge_method": "merge",
		"sha":          commit.SHA,
	}, nil)
	var ae *apiError
	switch {
	case err == nil:
		out.Merged = true
		// The branch has done its job. Failing to delete it leaves a stray
		// branch, not a broken change, so the error is not the editor's.
		_ = g.call(ctx, http.MethodDelete, g.repoPath("/git/refs/heads/"+escapePath(branchName)), nil, nil)
	case errors.As(err, &ae) && (ae.Status == http.StatusMethodNotAllowed || ae.Status == http.StatusForbidden ||
		ae.Status == http.StatusConflict || ae.Status == http.StatusUnprocessableEntity):
		// Protected branch, a required review or check, or a conflict: the
		// pull request stays open for a person to look at.
	default:
		return out, fmt.Errorf("merging %s: %w", pr.HTMLURL, err)
	}
	return out, nil
}

// escapePath escapes each segment of a slash-separated path.
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}
