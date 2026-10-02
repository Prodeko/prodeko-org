package workdir

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// ----------------------------------------------------------------- feedback --

func TestFeedbackReadsTheReviewsAndComments(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/prodeko/prodeko-org/pulls":
			if got := r.URL.Query().Get("state"); got != "all" {
				t.Errorf("state = %q, want all: a merged pull request is an answer too", got)
			}
			w.Write([]byte(`[{"number":47,"html_url":"https://github.com/prodeko/prodeko-org/pull/47","state":"open","head":{"ref":"media/maija/sininen-otsikko","sha":"deadbeef"}}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/prodeko/prodeko-org/pulls/47/reviews":
			w.Write([]byte(`[
				{"user":{"login":"maintainer"},"state":"CHANGES_REQUESTED","body":"Otsikko on nyt liian tumma.","submitted_at":"2026-09-20T10:00:00Z"},
				{"user":{"login":"maintainer"},"state":"COMMENTED","body":"","submitted_at":"2026-09-20T10:01:00Z"}
			]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/prodeko/prodeko-org/issues/47/comments":
			w.Write([]byte(`[{"user":{"login":"github-actions"},"body":"Esikatselu: https://pr-47.preview.prodeko.org/","created_at":"2026-09-20T09:00:00Z"}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/prodeko/prodeko-org/pulls/47/comments":
			w.Write([]byte(`[{"user":{"login":"maintainer"},"body":"Tämä rivi kovakoodaa värin.","path":"site/assets/css/main.css","line":12,"created_at":"2026-09-20T10:02:00Z"}]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/status"):
			w.Write([]byte(`{"state":"success"}`))
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

	fb, err := m.Feedback(t.Context(), c)
	if err != nil {
		t.Fatalf("Feedback: %v", err)
	}
	if fb.PRNumber != 47 || fb.State != "open" || fb.CIState != "success" {
		t.Errorf("Feedback = #%d %s ci=%s", fb.PRNumber, fb.State, fb.CIState)
	}
	if len(fb.Reviews) != 1 {
		t.Fatalf("reviews = %d, want the empty COMMENTED shell dropped and one kept", len(fb.Reviews))
	}
	if fb.Reviews[0].State != "CHANGES_REQUESTED" || fb.Reviews[0].Body != "Otsikko on nyt liian tumma." {
		t.Errorf("review = %+v", fb.Reviews[0])
	}
	if len(fb.Comments) != 2 {
		t.Fatalf("comments = %d, want the conversation and the inline one", len(fb.Comments))
	}
	inline := fb.Comments[1]
	if inline.Path != "site/assets/css/main.css" || inline.Line != 12 {
		t.Errorf("inline comment = %+v", inline)
	}
}

func TestFeedbackMarksAMergedPullRequestMerged(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/prodeko/prodeko-org/pulls" {
			w.Write([]byte(`[{"number":47,"html_url":"u","state":"closed","merged_at":"2026-09-20T11:00:00Z","head":{"ref":"media/maija/sininen-otsikko","sha":"deadbeef"}}]`))
			return
		}
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	cfg := f.config(t)
	cfg.GitHubToken = "ghp_test"
	cfg.GitHubRepo = "prodeko/prodeko-org"
	cfg.APIRoot = srv.URL
	m, _ := New(cfg)
	fb, err := m.Feedback(t.Context(), openChange(t, m))
	if err != nil {
		t.Fatalf("Feedback: %v", err)
	}
	if fb.State != "merged" {
		t.Errorf("State = %q; GitHub says closed, merged_at is what says merged", fb.State)
	}
}

func TestFeedbackOfAnUnsubmittedChange(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	cfg := f.config(t)
	cfg.GitHubToken = "ghp_test"
	cfg.GitHubRepo = "prodeko/prodeko-org"
	cfg.APIRoot = srv.URL
	m, _ := New(cfg)
	_, err := m.Feedback(t.Context(), openChange(t, m))
	if !errors.Is(err, ErrNeverSubmitted) {
		t.Fatalf("Feedback = %v, want %v", err, ErrNeverSubmitted)
	}
}

// ------------------------------------------------------------------ abandon --

func TestAbandonClosesThePullRequestAndRemovesEverything(t *testing.T) {
	f := newFixture(t)
	var closed, deletedRef bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/prodeko/prodeko-org/pulls":
			w.Write([]byte(`[{"number":47,"html_url":"u","state":"open","head":{"ref":"media/maija/sininen-otsikko","sha":"deadbeef"}}]`))
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/prodeko/prodeko-org/pulls/47":
			body := make([]byte, 64)
			n, _ := r.Body.Read(body)
			if !strings.Contains(string(body[:n]), `"closed"`) {
				t.Errorf("patch body = %s", body[:n])
			}
			closed = true
			w.Write([]byte(`{}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/repos/prodeko/prodeko-org/git/refs/heads/media/maija/sininen-otsikko":
			deletedRef = true
			w.WriteHeader(http.StatusNoContent)
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
	dir := c.Dir

	res, err := m.Abandon(t.Context(), c)
	if err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	if !closed || !deletedRef {
		t.Errorf("closed=%v deletedRef=%v, want both", closed, deletedRef)
	}
	if res.PRNumber != 47 || res.Note != "" {
		t.Errorf("result = %+v", res)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the worktree is still at %s", dir)
	}
	if slugs, _ := m.openSlugs("maija"); len(slugs) != 0 {
		t.Errorf("open changes = %v, want none", slugs)
	}
	// The slot is free: the same slug opens again as a fresh change.
	if _, err := m.Change("maija", "sininen-otsikko"); err != nil {
		t.Fatalf("reopening after an abandon: %v", err)
	}
}

func TestAbandonWithoutGitHubStillFreesTheSlot(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t)
	c := openChange(t, m)
	dir := c.Dir

	res, err := m.Abandon(t.Context(), c)
	if err != nil {
		t.Fatalf("Abandon: %v", err)
	}
	if res.PRNumber != 0 {
		t.Errorf("PRNumber = %d in a dry run", res.PRNumber)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the worktree is still at %s", dir)
	}
}

// ----------------------------------------------------------------- existing --

func TestExistingRefusesToCreate(t *testing.T) {
	f := newFixture(t)
	m := f.manager(t)

	if _, err := m.Existing("maija", "sininen-otsikko"); !errors.Is(err, ErrNoChange) {
		t.Fatalf("Existing before any change = %v, want %v", err, ErrNoChange)
	}
	openChange(t, m)
	if _, err := m.Existing("maija", "sininen-otsikko"); err != nil {
		t.Fatalf("Existing of an open change: %v", err)
	}
	_, err := m.Existing("maija", "toinen")
	if !errors.Is(err, ErrNoChange) {
		t.Fatalf("Existing of an unknown slug = %v, want %v", err, ErrNoChange)
	}
	if !strings.Contains(err.Error(), "sininen-otsikko") {
		t.Errorf("the refusal does not name what is open: %v", err)
	}
}
