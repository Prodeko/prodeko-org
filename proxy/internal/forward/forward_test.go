package forward

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/prodeko/prodeko-org/proxy/internal/session"
)

const (
	testRole  = "website-editor"
	testToken = "github_pat_secret"
)

// testRoles is the required set: an editor has to hold every one of them.
var testRoles = []string{"membership", testRole}

var editorIdentity = session.Identity{
	Subject:  "b2c3-uuid",
	Name:     "Aino Editor",
	Email:    "aino@prodeko.org",
	Username: "aino",
	Roles:    testRoles,
}

// capture records what the fake GitHub saw, so tests can assert on the
// outbound request rather than only on the response.
type capture struct {
	method        string
	escapedPath   string
	rawQuery      string
	authorization string
	body          string
}

// newProxy stands up the handler in front of a fake api.github.com.
func newProxy(t *testing.T, upstream http.HandlerFunc) (*Handler, *capture, *httptest.Server) {
	t.Helper()
	seen := &capture{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		seen.method = r.Method
		seen.escapedPath = r.URL.EscapedPath()
		seen.rawQuery = r.URL.RawQuery
		seen.authorization = r.Header.Get("Authorization")
		seen.body = string(raw)
		if upstream != nil {
			upstream(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)

	apiRoot, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	publicBase, err := url.Parse("https://cms.prodeko.org")
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{
		Owner:       "prodeko",
		Repo:        "prodeko-org",
		Branch:      testBranch,
		Token:       testToken,
		EditorRoles: testRoles,
		Committer:   testCommitter,
		APIRoot:     apiRoot,
		PublicBase:  publicBase,
		Client:      server.Client(),
		Logger:      slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return h, seen, server
}

func request(t *testing.T, h *Handler, id *session.Identity, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, reader)
	if id != nil {
		r = r.WithContext(session.NewContext(r.Context(), *id))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Rule 1: no session, or a session short of any one required role, no access.
// A session holding some of the roles is the interesting case: it is what a
// lapsed membership leaves behind.
func TestRequiresASessionAndEveryEditorRole(t *testing.T) {
	h, _, _ := newProxy(t, nil)

	tests := []struct {
		name     string
		identity *session.Identity
		want     int
	}{
		{"no session at all", nil, http.StatusUnauthorized},
		{"signed in with both roles", &editorIdentity, http.StatusOK},
		{
			"both roles plus unrelated ones",
			&session.Identity{Subject: "x", Name: "Ossi", Email: "ossi@prodeko.org",
				Roles: []string{"admin", testRole, "offline_access", "membership"}},
			http.StatusOK,
		},
		{
			"membership but not the editor role",
			&session.Identity{Subject: "x", Name: "Ossi", Email: "ossi@prodeko.org", Roles: []string{"membership"}},
			http.StatusForbidden,
		},
		{
			"the editor role but membership lapsed",
			&session.Identity{Subject: "x", Name: "Ossi", Email: "ossi@prodeko.org", Roles: []string{testRole}},
			http.StatusForbidden,
		},
		{
			"no roles at all",
			&session.Identity{Subject: "x", Name: "Ossi", Email: "ossi@prodeko.org"},
			http.StatusForbidden,
		},
		{
			"a role that merely looks similar",
			&session.Identity{Subject: "x", Roles: []string{"membership", "website-editors"}},
			http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := request(t, h, tc.identity, "GET", "/github/repos/prodeko/prodeko-org/branches/main", "")
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// The roles are re-checked on writes too, not just on the first read.
func TestRolesAreCheckedOnWrites(t *testing.T) {
	h, seen, _ := newProxy(t, nil)
	id := session.Identity{Subject: "x", Name: "Ossi", Email: "ossi@prodeko.org", Roles: []string{"membership"}}
	w := request(t, h, &id, "POST", "/github/repos/prodeko/prodeko-org/git/commits",
		`{"message":"m","tree":"t","parents":[]}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if seen.method != "" {
		t.Fatal("the request reached GitHub despite the missing role")
	}
}

// Rule 2: the author is the Keycloak identity, whatever the browser sent.
func TestCommitAuthorIsTheKeycloakIdentity(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"what Decap sends", `{"message":"Update index.md","tree":"t1","parents":["p1"]}`},
		{
			"a forged author",
			`{"message":"m","tree":"t1","parents":["p1"],"author":{"name":"Chair","email":"chair@prodeko.org"}}`,
		},
		{
			"a rebase replaying someone else's commit",
			`{"message":"m","tree":"t1","parents":["p1"],` +
				`"author":{"name":"Other","email":"other@prodeko.org","date":"2020-01-01T00:00:00Z"},` +
				`"committer":{"name":"Other","email":"other@prodeko.org","date":"2020-01-01T00:00:00Z"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, seen, _ := newProxy(t, nil)
			w := request(t, h, &editorIdentity,
				"POST", "/github/repos/prodeko/prodeko-org/git/commits", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
			}

			var sent struct {
				Message   string   `json:"message"`
				Tree      string   `json:"tree"`
				Parents   []string `json:"parents"`
				Author    Author   `json:"author"`
				Committer Author   `json:"committer"`
			}
			if err := json.Unmarshal([]byte(seen.body), &sent); err != nil {
				t.Fatalf("forwarded body is not JSON: %v (%s)", err, seen.body)
			}
			if sent.Author != (Author{Name: "Aino Editor", Email: "aino@prodeko.org"}) {
				t.Errorf("author = %+v, want the session identity", sent.Author)
			}
			if sent.Committer != testCommitter {
				t.Errorf("committer = %+v, want the bot", sent.Committer)
			}
			if sent.Tree != "t1" || sent.Message == "" || len(sent.Parents) != 1 {
				t.Errorf("the rest of the commit was not preserved: %s", seen.body)
			}
			if strings.Contains(seen.body, "2020-01-01") {
				t.Errorf("a replayed commit date survived: %s", seen.body)
			}
		})
	}
}

// An identity with no email cannot be written into a commit, and inventing one
// would defeat the point of rule 2, so the write is refused.
func TestCommitWithoutAnEmailIsRefused(t *testing.T) {
	h, seen, _ := newProxy(t, nil)
	id := session.Identity{Subject: "x", Name: "Aino", Username: "aino", Roles: testRoles}
	w := request(t, h, &id, "POST", "/github/repos/prodeko/prodeko-org/git/commits",
		`{"message":"m","tree":"t","parents":[]}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", w.Code, w.Body.String())
	}
	if seen.method != "" {
		t.Fatal("an unattributable commit reached GitHub")
	}
}

// Rule 3: owner and repo come from config. Whatever the browser asks for is
// discarded, so a modified client cannot write somewhere else.
func TestRepositoryIsPinned(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		want    int
		wantOut string
	}{
		{
			"the configured repository",
			"/github/repos/prodeko/prodeko-org/git/blobs",
			http.StatusOK,
			"/repos/prodeko/prodeko-org/git/blobs",
		},
		{
			"somebody else's repository is rewritten to ours",
			"/github/repos/attacker/evil/git/blobs",
			http.StatusOK,
			"/repos/prodeko/prodeko-org/git/blobs",
		},
		{
			"a different case for the same repository",
			"/github/repos/ProDeko/Prodeko-Org/git/blobs",
			http.StatusOK,
			"/repos/prodeko/prodeko-org/git/blobs",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, seen, _ := newProxy(t, nil)
			w := request(t, h, &editorIdentity, "POST", tc.target, `{}`)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
			if seen.escapedPath != tc.wantOut {
				t.Fatalf("forwarded path = %q, want %q", seen.escapedPath, tc.wantOut)
			}
		})
	}
}

func TestPathsOutsideTheRepositoryAreRefused(t *testing.T) {
	tests := []struct {
		name   string
		target string
		want   int
	}{
		{"the user list", "/github/users/some-bot", http.StatusForbidden},
		{"issue search", "/github/search/issues?q=x", http.StatusForbidden},
		{"graphql", "/github/graphql", http.StatusForbidden},
		{"the org", "/github/orgs/prodeko", http.StatusForbidden},
		{"a bare repos path", "/github/repos", http.StatusForbidden},
		{"repos with only an owner", "/github/repos/prodeko", http.StatusForbidden},
		{"outside the prefix", "/elsewhere/repos/prodeko/prodeko-org", http.StatusNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, seen, _ := newProxy(t, nil)
			w := request(t, h, &editorIdentity, "GET", tc.target, "")
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
			if seen.method != "" {
				t.Fatal("the request reached GitHub")
			}
		})
	}
}

// Rule 4: the bot credential goes on here and the caller's token never does.
func TestCredentialHandling(t *testing.T) {
	h, seen, _ := newProxy(t, nil)
	r := httptest.NewRequest("GET", "/github/repos/prodeko/prodeko-org/branches/main", nil)
	r.Header.Set("Authorization", "Bearer our-session-token")
	r.Header.Set("Cookie", "session=abc")
	r = r.WithContext(session.NewContext(r.Context(), editorIdentity))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if seen.authorization != "Bearer "+testToken {
		t.Fatalf("upstream Authorization = %q, want the bot token", seen.authorization)
	}
	if strings.Contains(w.Body.String(), testToken) {
		t.Fatal("the GitHub token appeared in the response to the browser")
	}
}

// Decap sends cms%2Fpages%2Fslug as one segment. Forwarding the decoded form
// happens to work against GitHub today for some endpoints, which is exactly
// why it needs a test rather than a coincidence.
func TestEncodedPathIsForwardedEncoded(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		target  string
		body    string
		wantOut string
	}{
		{
			"an encoded ref name",
			"PATCH",
			"/github/repos/prodeko/prodeko-org/git/refs/heads/cms%2Fpages%2Fslug",
			`{"sha":"abc","force":true}`,
			"/repos/prodeko/prodeko-org/git/refs/heads/cms%2Fpages%2Fslug",
		},
		{
			"an encoded tree directory",
			"GET",
			"/github/repos/prodeko/prodeko-org/git/trees/main:site%2Fcontent",
			"",
			"/repos/prodeko/prodeko-org/git/trees/main:site%2Fcontent",
		},
		{
			"an unencoded tree directory stays unencoded",
			"GET",
			"/github/repos/prodeko/prodeko-org/git/trees/main:site/content",
			"",
			"/repos/prodeko/prodeko-org/git/trees/main:site/content",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, seen, _ := newProxy(t, nil)
			w := request(t, h, &editorIdentity, tc.method, tc.target, tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
			}
			if seen.escapedPath != tc.wantOut {
				t.Fatalf("forwarded path = %q, want %q", seen.escapedPath, tc.wantOut)
			}
		})
	}
}

func TestQueryIsForwarded(t *testing.T) {
	h, seen, _ := newProxy(t, nil)
	request(t, h, &editorIdentity, "GET",
		"/github/repos/prodeko/prodeko-org/pulls?state=open&head=prodeko%3Acms%2Fpages%2Fslug", "")
	if seen.rawQuery != "state=open&head=prodeko%3Acms%2Fpages%2Fslug" {
		t.Fatalf("forwarded query = %q", seen.rawQuery)
	}
}

// Decap gates its entire UI on permissions.push, and whether a fine-grained
// token reports it honestly is not something we can rely on. Write authority
// comes from the role check and the allowlist, not from this field.
func TestRepositoryResponseReportsPushAccess(t *testing.T) {
	h, _, _ := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `W/"abc123"`)
		_, _ = w.Write([]byte(`{"id":1,"name":"prodeko-org",` +
			`"owner":{"login":"ProDeko","id":42},` +
			`"default_branch":"main","permissions":{"admin":false,"push":false,"pull":true}}`))
	})

	w := request(t, h, &editorIdentity, "GET", "/github/repos/prodeko/prodeko-org", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var got struct {
		Name        string `json:"name"`
		Permissions struct {
			Push bool `json:"push"`
			Pull bool `json:"pull"`
		} `json:"permissions"`
		Owner struct {
			Login string `json:"login"`
		} `json:"owner"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, w.Body.String())
	}
	if !got.Permissions.Push || !got.Permissions.Pull {
		t.Errorf("permissions = %+v, want push and pull", got.Permissions)
	}
	// Decap reads owner.login back and builds pull request heads from it, so
	// GitHub's capitalisation has to survive untouched.
	if got.Owner.Login != "ProDeko" {
		t.Errorf("owner.login = %q, want it preserved", got.Owner.Login)
	}
	if got.Name != "prodeko-org" || got.DefaultBranch != "main" {
		t.Errorf("other fields were lost: %s", w.Body.String())
	}
	// A cached 304 would skip the rewrite entirely.
	if etag := w.Header().Get("ETag"); etag != "" {
		t.Errorf("ETag = %q, want it stripped from the rewritten response", etag)
	}
}

func TestRepositoryRequestDoesNotRevalidate(t *testing.T) {
	var sawConditional bool
	h, _, _ := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
		sawConditional = r.Header.Get("If-None-Match") != ""
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"owner":{"login":"prodeko"}}`))
	})
	r := httptest.NewRequest("GET", "/github/repos/prodeko/prodeko-org", nil)
	r.Header.Set("If-None-Match", `W/"abc"`)
	r = r.WithContext(session.NewContext(r.Context(), editorIdentity))
	h.ServeHTTP(httptest.NewRecorder(), r)

	if sawConditional {
		t.Fatal("If-None-Match was forwarded on the repository root, so GitHub could answer 304 with no body to rewrite")
	}
}

// Decap's client calls .match() on json.message for every 403. A body without
// that field throws inside the client and sends it into five rounds of
// exponential backoff; a body containing the rate-limit phrase stalls it
// outright.
func TestDenialsAreJSONWithASafeMessage(t *testing.T) {
	h, _, _ := newProxy(t, nil)

	tests := []struct {
		name   string
		method string
		target string
		id     *session.Identity
	}{
		{"no session", "GET", "/github/repos/prodeko/prodeko-org", nil},
		{"no role", "GET", "/github/repos/prodeko/prodeko-org",
			&session.Identity{Subject: "x", Roles: []string{"membership"}}},
		{"denied path", "GET", "/github/repos/prodeko/prodeko-org/hooks", &editorIdentity},
		{"denied path with the rate limit phrase in it", "GET",
			"/github/repos/prodeko/prodeko-org/API%20rate%20limit%20exceeded", &editorIdentity},
		{"denied ref write", "DELETE",
			"/github/repos/prodeko/prodeko-org/git/refs/heads/main", &editorIdentity},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := request(t, h, tc.id, tc.method, tc.target, "")
			if w.Code < 400 {
				t.Fatalf("status = %d, want a refusal", w.Code)
			}
			if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "json") {
				t.Fatalf("Content-Type = %q, want JSON so the client can parse it", ct)
			}
			var body struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("body is not JSON: %v (%s)", err, w.Body.String())
			}
			if body.Message == "" {
				t.Fatal("body has no message field; the client throws on this")
			}
			if strings.Contains(w.Body.String(), "API rate limit exceeded") {
				t.Fatalf("the rate limit phrase reached the client: %s", w.Body.String())
			}
		})
	}
}

func TestScrubRateLimit(t *testing.T) {
	got := scrubRateLimit("GET /API rate limit exceeded is not on the allowlist")
	if strings.Contains(got, "API rate limit exceeded") {
		t.Fatalf("scrubRateLimit left the phrase in place: %q", got)
	}
}

// The bot's real GitHub profile must not leak, so this never reaches GitHub.
func TestUserIsSynthesisedFromTheSession(t *testing.T) {
	h, seen, _ := newProxy(t, nil)
	w := request(t, h, &editorIdentity, "GET", "/github/user", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if seen.method != "" {
		t.Fatal("GET /user was forwarded to GitHub")
	}

	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"login": "aino",
		"name":  "Aino Editor",
		"email": "aino@prodeko.org",
	} {
		if got[key] != want {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
	// Decap renders a placeholder icon when avatar_url is falsy, which beats a
	// broken image pointing at a GitHub profile that is not the editor's.
	if _, ok := got["avatar_url"]; ok {
		t.Error("avatar_url should be omitted")
	}
}

func TestUserRequiresASession(t *testing.T) {
	h, _, _ := newProxy(t, nil)
	if w := request(t, h, nil, "GET", "/github/user", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestLoginFor(t *testing.T) {
	tests := []struct {
		name string
		id   session.Identity
		want string
	}{
		{"preferred username", session.Identity{Username: "aino", Email: "a@b.fi", Subject: "s"}, "aino"},
		{"falls back to the email local part", session.Identity{Email: "aino.e@prodeko.org", Subject: "s"}, "aino.e"},
		{"falls back to the subject", session.Identity{Subject: "b2c3-uuid"}, "b2c3-uuid"},
		{"nothing at all", session.Identity{}, "editor"},
		{"blank username is ignored", session.Identity{Username: "  ", Subject: "s"}, "s"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := loginFor(tc.id); got != tc.want {
				t.Fatalf("loginFor = %q, want %q", got, tc.want)
			}
		})
	}
}

// The end-to-end shape of a force push over the default branch: the allowlist
// lets the path through because the branch is configured, and the body check
// is what stops it.
func TestForcePushOverTheDefaultBranchIsRefused(t *testing.T) {
	h, seen, _ := newProxy(t, nil)
	w := request(t, h, &editorIdentity, "PATCH",
		"/github/repos/prodeko/prodeko-org/git/refs/heads/main", `{"sha":"deadbeef","force":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (%s)", w.Code, w.Body.String())
	}
	if seen.method != "" {
		t.Fatal("the force push reached GitHub")
	}
}

func TestFastForwardOfTheDefaultBranchIsAllowed(t *testing.T) {
	h, seen, _ := newProxy(t, nil)
	w := request(t, h, &editorIdentity, "PATCH",
		"/github/repos/prodeko/prodeko-org/git/refs/heads/main", `{"sha":"deadbeef","force":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if seen.body != `{"sha":"deadbeef","force":false}` {
		t.Fatalf("body was altered: %s", seen.body)
	}
}

// Media uploads are the large bodies and must not be buffered or rewritten.
func TestBlobBodyIsForwardedUntouched(t *testing.T) {
	h, seen, _ := newProxy(t, nil)
	body := `{"content":"aGVsbG8=","encoding":"base64"}`
	w := request(t, h, &editorIdentity, "POST", "/github/repos/prodeko/prodeko-org/git/blobs", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if seen.body != body {
		t.Fatalf("body = %s, want it forwarded unchanged", seen.body)
	}
}

func TestOversizedBodyIsRefused(t *testing.T) {
	h, seen, _ := newProxy(t, nil)
	h.maxBody = 16
	w := request(t, h, &editorIdentity, "POST", "/github/repos/prodeko/prodeko-org/git/commits",
		`{"message":"`+strings.Repeat("x", 200)+`","tree":"t","parents":[]}`)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", w.Code)
	}
	if seen.method != "" {
		t.Fatal("an oversized body reached GitHub")
	}
}

// Everything GitHub sends that is not on the response allowlist is dropped,
// so nothing about the bot account or the upstream request leaks.
func TestResponseHeadersAreFiltered(t *testing.T) {
	h, _, _ := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `W/"keep"`)
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.Header().Set("X-OAuth-Scopes", "repo, admin:org")
		w.Header().Set("X-GitHub-Request-Id", "ABCD:1234")
		w.Header().Set("Location", "https://api.github.com/repos/prodeko/prodeko-org/git/refs/heads/x")
		w.Header().Set("Set-Cookie", "logged_in=no")
		w.Header().Set("Link", `<`+r.Host+`/x>; rel="next"`)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	w := request(t, h, &editorIdentity, "GET", "/github/repos/prodeko/prodeko-org/pulls", "")

	for _, name := range []string{"ETag", "X-RateLimit-Remaining", "Content-Type"} {
		if w.Header().Get(name) == "" {
			t.Errorf("%s was dropped but is needed", name)
		}
	}
	for _, name := range []string{"X-OAuth-Scopes", "X-GitHub-Request-Id", "Location", "Set-Cookie"} {
		if v := w.Header().Get(name); v != "" {
			t.Errorf("%s = %q, want it dropped", name, v)
		}
	}
}

// A Link header pointing at api.github.com would make Decap send our session
// token there.
func TestLinkHeaderNeverPointsAtGitHub(t *testing.T) {
	h, _, _ := newProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Link", `<https://api.github.com/repositories/29514104/pulls?page=2>; rel="next"`)
		_, _ = w.Write([]byte(`[]`))
	})
	w := request(t, h, &editorIdentity, "GET", "/github/repos/prodeko/prodeko-org/pulls", "")
	if link := w.Header().Get("Link"); strings.Contains(link, "api.github.com") {
		t.Fatalf("Link = %q, want it rewritten or dropped", link)
	}
}

func TestPreflightIsNotRefused(t *testing.T) {
	h, _, _ := newProxy(t, nil)
	if w := request(t, h, nil, "OPTIONS", "/github/repos/prodeko/prodeko-org/git/commits", ""); w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; a 401 here breaks every request", w.Code)
	}
}

func TestUpstreamFailureIsABadGateway(t *testing.T) {
	h, _, server := newProxy(t, nil)
	server.Close()
	w := request(t, h, &editorIdentity, "GET", "/github/repos/prodeko/prodeko-org/pulls", "")
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
	if strings.Contains(w.Body.String(), testToken) {
		t.Fatal("the token leaked in an error message")
	}
}

func TestNewValidatesConfig(t *testing.T) {
	valid := Config{
		Owner: "prodeko", Repo: "prodeko-org", Branch: "main",
		Token: "t", EditorRoles: testRoles, Committer: testCommitter,
	}
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{"valid", func(*Config) {}, false},
		{"no owner", func(c *Config) { c.Owner = "" }, true},
		{"no repo", func(c *Config) { c.Repo = "" }, true},
		{"no branch", func(c *Config) { c.Branch = "" }, true},
		{"no token", func(c *Config) { c.Token = "" }, true},
		{"no editor roles", func(c *Config) { c.EditorRoles = nil }, true},
		{"an empty list of editor roles", func(c *Config) { c.EditorRoles = []string{} }, true},
		{"blank editor roles", func(c *Config) { c.EditorRoles = []string{"", "  "} }, true},
		{"no committer", func(c *Config) { c.Committer = Author{} }, true},
		{"committer without an email", func(c *Config) { c.Committer = Author{Name: "Bot"} }, true},
		{"a slash in the owner", func(c *Config) { c.Owner = "a/b" }, true},
		{"a query in the repo", func(c *Config) { c.Repo = "a?b" }, true},
		{"api root without a scheme", func(c *Config) { c.APIRoot = &url.URL{Host: "api.github.com"} }, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := valid
			tc.mutate(&cfg)
			_, err := New(cfg)
			if (err != nil) != tc.wantErr {
				t.Fatalf("New() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestPrefixDefaultsAndNormalises(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", "/github"},
		{"/github", "/github"},
		{"github", "/github"},
		{"/github/", "/github"},
		{"/api/github", "/api/github"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			h, err := New(Config{
				Owner: "prodeko", Repo: "r", Branch: "main", Token: "t",
				EditorRoles: testRoles, Committer: testCommitter, Prefix: tc.in,
			})
			if err != nil {
				t.Fatal(err)
			}
			if h.prefix != tc.want {
				t.Fatalf("prefix = %q, want %q", h.prefix, tc.want)
			}
		})
	}
}
