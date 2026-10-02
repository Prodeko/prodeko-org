package forward

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/prodeko/prodeko-org/proxy/internal/ghauth"
)

// rotating stands in for a GitHub App: a different token on every call.
type rotating struct {
	calls int
	err   error
}

func (r *rotating) Token(context.Context) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	r.calls++
	return "ghs_installation" + string(rune('0'+r.calls)), nil
}

func newProxyWithCredential(t *testing.T, credential ghauth.Source) (*Handler, *capture) {
	t.Helper()
	seen := &capture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	apiRoot, _ := url.Parse(server.URL)
	h, err := New(Config{
		Owner:       "prodeko",
		Repo:        "prodeko-org",
		Branch:      testBranch,
		Credential:  credential,
		EditorRoles: testRoles,
		Committer:   testCommitter,
		APIRoot:     apiRoot,
		Client:      server.Client(),
		Logger:      slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, seen
}

// An installation token expires, so the credential is asked on every request
// and its answer is never kept.
func TestCredentialIsAskedOnEveryRequest(t *testing.T) {
	cred := &rotating{}
	h, seen := newProxyWithCredential(t, cred)

	for _, want := range []string{"Bearer ghs_installation1", "Bearer ghs_installation2"} {
		w := request(t, h, &editorIdentity, "GET", "/github/repos/prodeko/prodeko-org/branches/main", "")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body)
		}
		if seen.authorization != want {
			t.Errorf("upstream Authorization = %q, want %q", seen.authorization, want)
		}
	}
}

// A credential that cannot be had is GitHub being unreachable from the
// editor's point of view, and the reason stays in the log.
func TestCredentialFailureIsABadGateway(t *testing.T) {
	cred := &rotating{err: errors.New("ghauth: minting an installation token: GitHub answered 401: secret detail")}
	h, seen := newProxyWithCredential(t, cred)

	w := request(t, h, &editorIdentity, "GET", "/github/repos/prodeko/prodeko-org/branches/main", "")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if strings.Contains(w.Body.String(), "secret detail") {
		t.Errorf("the credential error reached the browser: %s", w.Body)
	}
	if seen.authorization != "" {
		t.Error("the request went upstream without a credential")
	}
}

func TestNewRequiresACredential(t *testing.T) {
	apiRoot, _ := url.Parse("https://api.github.com")
	_, err := New(Config{
		Owner: "prodeko", Repo: "prodeko-org", Branch: testBranch,
		EditorRoles: testRoles, Committer: testCommitter, APIRoot: apiRoot,
	})
	if err == nil || !strings.Contains(err.Error(), "Credential") {
		t.Fatalf("err = %v, want one naming the missing credential", err)
	}
}
