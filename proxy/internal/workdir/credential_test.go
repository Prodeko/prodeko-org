package workdir

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prodeko/prodeko-org/proxy/internal/ghauth"
)

type sourceFunc func(context.Context) (string, error)

func (f sourceFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

func credentialManager(t *testing.T, apiRoot string, cred ghauth.Source) *Manager {
	t.Helper()
	m, err := New(Config{
		RepoPath:         t.TempDir(),
		StateDir:         t.TempDir(),
		Committer:        Author{Name: "Prodeko media bot", Email: "media-bot@prodeko.org"},
		GitHubCredential: cred,
		GitHubRepo:       "prodeko/prodeko-org",
		APIRoot:          apiRoot,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// An installation token expires within the hour, so every call asks for one.
func TestAPICallsAskTheCredentialEachTime(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)

	n := 0
	m := credentialManager(t, srv.URL, sourceFunc(func(context.Context) (string, error) {
		n++
		return "ghs_minted" + strings.Repeat("x", n), nil
	}))
	for range 2 {
		if err := m.api(t.Context(), http.MethodGet, "/repos/prodeko/prodeko-org", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 2 || seen[0] != "Bearer ghs_mintedx" || seen[1] != "Bearer ghs_mintedxx" {
		t.Fatalf("Authorization headers = %q, want a fresh token per call", seen)
	}
}

func TestAPICallWithoutACredentialFails(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	t.Cleanup(srv.Close)

	m := credentialManager(t, srv.URL, sourceFunc(func(context.Context) (string, error) {
		return "", errors.New("ghauth: the App is not installed")
	}))
	err := m.api(t.Context(), http.MethodGet, "/repos/prodeko/prodeko-org", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err = %v, want the credential's reason", err)
	}
	if called {
		t.Error("the call went to GitHub without a credential")
	}
}

func TestCredentialMakesItReal(t *testing.T) {
	m := credentialManager(t, "", ghauth.Static("ghp_x"))
	if m.DryRun() {
		t.Error("a credential and a repository must not be a dry run")
	}
	m = credentialManager(t, "", nil)
	if !m.DryRun() {
		t.Error("no credential must be a dry run")
	}
}

// No single token value is known in advance with an App, so redaction goes by
// shape.
func TestRedactByShape(t *testing.T) {
	m := credentialManager(t, "", nil)
	for _, in := range []string{
		"fatal: unable to access 'https://x-access-token:ghs_abc123DEF@github.com/prodeko/prodeko-org.git/'",
		"remote: token ghs_abc123DEF rejected",
		"remote: token ghp_abc123DEF rejected",
		"remote: token github_pat_11ABC_def rejected",
	} {
		out := m.redact(in)
		for _, secret := range []string{"ghs_abc123DEF", "ghp_abc123DEF", "github_pat_11ABC_def"} {
			if strings.Contains(out, secret) {
				t.Errorf("redact(%q) = %q, still carries %s", in, out, secret)
			}
		}
		if !strings.Contains(out, "[redacted]") {
			t.Errorf("redact(%q) = %q, marks nothing", in, out)
		}
	}
	if got := m.redact("rejected non-fast-forward"); got != "rejected non-fast-forward" {
		t.Errorf("redact changed ordinary output: %q", got)
	}
}
