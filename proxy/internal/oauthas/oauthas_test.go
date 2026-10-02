package oauthas

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prodeko/prodeko-org/proxy/internal/session"
)

// fakeKeycloak stands in for the upstream login. Everything this package owns
// is on this side of it: the real Keycloak leg is exercised in keycloak_test.go
// against a fake IdP.
type fakeKeycloak struct {
	mu  sync.Mutex
	id  session.Identity
	err error

	// unavailable makes AuthCodeURL return "", which is the only way the
	// interface can report that discovery failed.
	unavailable bool

	lastState    string
	lastNonce    string
	lastVerifier string
	lastCode     string
}

func (f *fakeKeycloak) AuthCodeURL(state, nonce, verifier string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unavailable {
		return ""
	}
	f.lastState, f.lastNonce, f.lastVerifier = state, nonce, verifier
	return "https://id.prodeko.org/authorize?state=" + url.QueryEscape(state) +
		"&nonce=" + url.QueryEscape(nonce)
}

func (f *fakeKeycloak) Exchange(ctx context.Context, code, nonce, verifier string) (session.Identity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastCode = code
	if f.err != nil {
		return session.Identity{}, f.err
	}
	if nonce != f.lastNonce || verifier != f.lastVerifier {
		return session.Identity{}, errors.New("the callback carried a different sign-in's nonce or verifier")
	}
	return f.id, nil
}

func mediaIdentity() session.Identity {
	return session.Identity{
		Subject:  "3f1c",
		Username: "maija",
		Name:     "Maija Meikäläinen",
		Email:    "maija@prodeko.org",
		Roles:    []string{"membership", "prodeko-org-media"},
	}
}

func testStore(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.NewStore(session.Options{
		Secret: []byte("0123456789abcdef0123456789abcdef"),
		TTL:    DefaultTokenTTL,
	})
	if err != nil {
		t.Fatalf("session.NewStore: %v", err)
	}
	return store
}

// quietLogger keeps the flow's own logging out of the test output; what the
// handlers log is not what is under test.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testServer(t *testing.T) (*Server, *session.Store) {
	t.Helper()
	store := testStore(t)
	s, err := New(Config{
		PublicURL:     "https://edit.prodeko.org",
		Keycloak:      &fakeKeycloak{id: mediaIdentity()},
		Sessions:      store,
		Tokens:        store,
		RequiredRoles: []string{"prodeko-org-media", "membership"},
		Logger:        quietLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, store
}

func TestNewValidatesTheConfiguration(t *testing.T) {
	store := testStore(t)
	base := Config{
		PublicURL:     "https://edit.prodeko.org",
		Keycloak:      &fakeKeycloak{},
		Sessions:      store,
		Tokens:        store,
		RequiredRoles: []string{"prodeko-org-media", "membership"},
	}
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"public URL with a path", func(c *Config) { c.PublicURL = "https://edit.prodeko.org/mcp" }},
		{"no public URL", func(c *Config) { c.PublicURL = "" }},
		{"no keycloak", func(c *Config) { c.Keycloak = nil }},
		{"no sessions", func(c *Config) { c.Sessions = nil }},
		{"no roles", func(c *Config) { c.RequiredRoles = nil }},
		{"blank roles", func(c *Config) { c.RequiredRoles = []string{"", "  "} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.edit(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatal("New accepted an unusable configuration")
			}
		})
	}
}

func TestMetadataDocument(t *testing.T) {
	s, _ := testServer(t)
	mux := http.NewServeMux()
	s.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, MetadataPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var doc ASMetadata
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	if doc.Issuer != "https://edit.prodeko.org" {
		t.Errorf("issuer = %q", doc.Issuer)
	}
	if doc.AuthorizationEndpoint != "https://edit.prodeko.org/authorize" {
		t.Errorf("authorization_endpoint = %q", doc.AuthorizationEndpoint)
	}
	if doc.TokenEndpoint != "https://edit.prodeko.org/token" {
		t.Errorf("token_endpoint = %q", doc.TokenEndpoint)
	}
	if doc.RegistrationEndpoint != "https://edit.prodeko.org/register" {
		t.Errorf("registration_endpoint = %q", doc.RegistrationEndpoint)
	}
	// S256 only: plain would bind a code issued to an open-registration public
	// client to nothing at all.
	if strings.Join(doc.CodeChallengeMethodsSupported, ",") != "S256" {
		t.Errorf("code_challenge_methods_supported = %v, want [S256]", doc.CodeChallengeMethodsSupported)
	}
	if strings.Join(doc.GrantTypesSupported, ",") != "authorization_code" {
		t.Errorf("grant_types_supported = %v, want [authorization_code]", doc.GrantTypesSupported)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
}

func TestResourceMetadataDocument(t *testing.T) {
	s, _ := testServer(t)
	mux := http.NewServeMux()
	s.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, ResourceMetadataPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var doc ResourceMetadataDocument
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("metadata is not JSON: %v", err)
	}
	if doc.Resource != "https://edit.prodeko.org/mcp" {
		t.Errorf("resource = %q", doc.Resource)
	}
	if strings.Join(doc.AuthorizationServers, ",") != "https://edit.prodeko.org" {
		t.Errorf("authorization_servers = %v", doc.AuthorizationServers)
	}
	if s.ResourceMetadataURL() != "https://edit.prodeko.org"+ResourceMetadataPath {
		t.Errorf("ResourceMetadataURL = %q", s.ResourceMetadataURL())
	}
	if s.RedirectURI() != "https://edit.prodeko.org/oauth/callback" {
		t.Errorf("RedirectURI = %q", s.RedirectURI())
	}
}

// The role conjunction is checked again when a token is presented, not only at
// sign-in. All the roles, never any of them.
func TestAuthenticateRequiresEveryRole(t *testing.T) {
	s, store := testServer(t)

	full, _, err := store.Issue(mediaIdentity())
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	id, err := s.Authenticate(full)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if id.Username != "maija" {
		t.Errorf("username = %q", id.Username)
	}

	partial, _, err := store.Issue(session.Identity{
		Subject:  "def",
		Username: "matti",
		Roles:    []string{"membership"},
	})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := s.Authenticate(partial); err == nil {
		t.Error("Authenticate accepted an account missing prodeko-org-media")
	}

	if _, err := s.Authenticate("not-a-token"); err == nil {
		t.Error("Authenticate accepted a token it never issued")
	}
}

func TestNewKeycloakValidatesItsConfiguration(t *testing.T) {
	ctx := context.Background()
	base := KeycloakConfig{
		Issuer:      "https://id.prodeko.org/realms/membership-registry",
		ClientID:    "prodeko-mcp",
		RedirectURI: "https://edit.prodeko.org/oauth/callback",
	}
	if _, err := NewKeycloak(ctx, base); err != nil {
		t.Fatalf("NewKeycloak: %v", err)
	}
	for _, tc := range []struct {
		name string
		edit func(*KeycloakConfig)
	}{
		{"no issuer", func(c *KeycloakConfig) { c.Issuer = "" }},
		{"no client id", func(c *KeycloakConfig) { c.ClientID = "" }},
		{"no redirect uri", func(c *KeycloakConfig) { c.RedirectURI = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.edit(&cfg)
			if _, err := NewKeycloak(ctx, cfg); err == nil {
				t.Fatal("NewKeycloak accepted an incomplete configuration")
			}
		})
	}
}

func TestDefaultTokenTTLIsAWorkingDay(t *testing.T) {
	if DefaultTokenTTL != 8*time.Hour {
		t.Fatalf("DefaultTokenTTL = %v, want 8h", DefaultTokenTTL)
	}
}

// --- the flow -------------------------------------------------------------

const testRedirectURI = "https://claude.ai/api/mcp/auth_callback"

// A code_verifier is 43-128 characters of the unreserved set; this one is 48.
const testVerifier = "prodeko-media-test-verifier.0123456789_abcdef~ABC"

func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// flow drives one connector through registration, authorization and the
// Keycloak callback.
type flow struct {
	t   *testing.T
	s   *Server
	mux *http.ServeMux
	kc  *fakeKeycloak
}

func newFlow(t *testing.T) *flow {
	t.Helper()
	s, _ := testServer(t)
	mux := http.NewServeMux()
	s.Register(mux)
	return &flow{t: t, s: s, mux: mux, kc: s.cfg.Keycloak.(*fakeKeycloak)}
}

func (f *flow) do(req *http.Request) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func (f *flow) register(redirectURIs ...string) string {
	f.t.Helper()
	body, err := json.Marshal(RegistrationRequest{
		ClientName:   "Claude",
		RedirectURIs: redirectURIs,
		GrantTypes:   []string{"authorization_code", "refresh_token"},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	rec := f.do(httptest.NewRequest(http.MethodPost, RegisterPath, strings.NewReader(string(body))))
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("POST /register: status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp RegistrationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		f.t.Fatalf("registration response is not JSON: %v", err)
	}
	if resp.ClientID == "" {
		f.t.Fatal("registration returned no client_id")
	}
	return resp.ClientID
}

// authorize returns the /authorize response.
func (f *flow) authorize(params url.Values) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(httptest.NewRequest(http.MethodGet, AuthorizePath+"?"+params.Encode(), nil))
}

func authParams(clientID, redirectURI, state string) url.Values {
	return url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"code_challenge":        {challengeFor(testVerifier)},
		"code_challenge_method": {"S256"},
		"scope":                 {Scope},
		"resource":              {"https://edit.prodeko.org/mcp"},
	}
}

// keycloakState reads the state this server sent Keycloak out of the redirect.
func (f *flow) keycloakState(rec *httptest.ResponseRecorder) string {
	f.t.Helper()
	if rec.Code != http.StatusFound {
		f.t.Fatalf("GET /authorize: status = %d, body = %s", rec.Code, rec.Body)
	}
	u, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		f.t.Fatalf("Location is not a URL: %v", err)
	}
	if u.Host != "id.prodeko.org" {
		f.t.Fatalf("/authorize redirected to %q, not to Keycloak", u)
	}
	state := u.Query().Get("state")
	if state == "" {
		f.t.Fatal("the Keycloak redirect carries no state")
	}
	return state
}

func (f *flow) callback(state string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.do(httptest.NewRequest(http.MethodGet,
		CallbackPath+"?state="+url.QueryEscape(state)+"&code=keycloak-code", nil))
}

// redirectParams pulls the query off a 302 back to the client.
func (f *flow) redirectParams(rec *httptest.ResponseRecorder, wantPrefix string) url.Values {
	f.t.Helper()
	if rec.Code != http.StatusFound {
		f.t.Fatalf("status = %d, want 302; body = %s", rec.Code, rec.Body)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, wantPrefix) {
		f.t.Fatalf("redirected to %q, want a %q", loc, wantPrefix)
	}
	u, err := url.Parse(loc)
	if err != nil {
		f.t.Fatalf("Location is not a URL: %v", err)
	}
	return u.Query()
}

func (f *flow) token(params url.Values) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(http.MethodPost, TokenPath, strings.NewReader(params.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return f.do(req)
}

func tokenParams(clientID, code string) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {testVerifier},
	}
}

// The whole point, end to end: a connector that knows nothing but the resource
// URL registers, signs in, and ends up holding a token the MCP endpoint
// accepts.
func TestAuthorizationCodeFlow(t *testing.T) {
	f := newFlow(t)
	clientID := f.register(testRedirectURI)

	rec := f.authorize(authParams(clientID, testRedirectURI, "client-state-1"))
	kcState := f.keycloakState(rec)

	back := f.redirectParams(f.callback(kcState), testRedirectURI)
	if back.Get("state") != "client-state-1" {
		t.Errorf("state = %q, want the client's own state back untouched", back.Get("state"))
	}
	code := back.Get("code")
	if code == "" {
		t.Fatalf("the callback returned no code: %v", back)
	}
	if back.Get("error") != "" {
		t.Fatalf("the callback returned an error: %v", back)
	}

	rec = f.token(tokenParams(clientID, code))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /token: status = %d, body = %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	var tok tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &tok); err != nil {
		t.Fatalf("token response is not JSON: %v", err)
	}
	if tok.TokenType != "Bearer" {
		t.Errorf("token_type = %q", tok.TokenType)
	}
	if tok.ExpiresIn != int64(DefaultTokenTTL/time.Second) {
		t.Errorf("expires_in = %d, want %d", tok.ExpiresIn, int64(DefaultTokenTTL/time.Second))
	}
	if tok.Scope != Scope {
		t.Errorf("scope = %q", tok.Scope)
	}

	// The token is what the MCP endpoint authenticates with, and it carries the
	// identity Keycloak verified.
	id, err := f.s.Authenticate(tok.AccessToken)
	if err != nil {
		t.Fatalf("Authenticate the freshly issued token: %v", err)
	}
	if id.Username != "maija" || id.Email != "maija@prodeko.org" {
		t.Errorf("identity = %+v, want the Keycloak one", id)
	}
}

// A code is single use, and it is spent by being presented: a wrong verifier
// does not leave it lying around to be guessed at again.
func TestAuthorizationCodeIsSingleUse(t *testing.T) {
	f := newFlow(t)
	clientID := f.register(testRedirectURI)
	kcState := f.keycloakState(f.authorize(authParams(clientID, testRedirectURI, "s")))
	code := f.redirectParams(f.callback(kcState), testRedirectURI).Get("code")

	if rec := f.token(tokenParams(clientID, code)); rec.Code != http.StatusOK {
		t.Fatalf("first redemption: status = %d, body = %s", rec.Code, rec.Body)
	}
	rec := f.token(tokenParams(clientID, code))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("second redemption: status = %d, want 400", rec.Code)
	}
	var e errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error != "invalid_grant" {
		t.Errorf("error = %q, want invalid_grant (%v)", e.Error, err)
	}
}

func TestTokenRefusesABadVerifierAndBurnsTheCode(t *testing.T) {
	f := newFlow(t)
	clientID := f.register(testRedirectURI)
	kcState := f.keycloakState(f.authorize(authParams(clientID, testRedirectURI, "s")))
	code := f.redirectParams(f.callback(kcState), testRedirectURI).Get("code")

	bad := tokenParams(clientID, code)
	bad.Set("code_verifier", strings.Repeat("x", 50))
	rec := f.token(bad)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	// The right verifier no longer helps: the code was spent by the attempt.
	if rec := f.token(tokenParams(clientID, code)); rec.Code != http.StatusBadRequest {
		t.Fatalf("a code survived a failed redemption: status = %d", rec.Code)
	}
}

func TestTokenChecksClientAndRedirectURI(t *testing.T) {
	f := newFlow(t)
	clientID := f.register(testRedirectURI)
	other := f.register("https://evil.example/callback")

	mint := func() string {
		t.Helper()
		kcState := f.keycloakState(f.authorize(authParams(clientID, testRedirectURI, "s")))
		return f.redirectParams(f.callback(kcState), testRedirectURI).Get("code")
	}

	for _, tc := range []struct {
		name string
		edit func(url.Values)
	}{
		{"another client's id", func(p url.Values) { p.Set("client_id", other) }},
		{"no client id", func(p url.Values) { p.Del("client_id") }},
		{"another redirect uri", func(p url.Values) { p.Set("redirect_uri", "https://evil.example/callback") }},
		{"no redirect uri, though one was authorized", func(p url.Values) { p.Del("redirect_uri") }},
		{"no verifier", func(p url.Values) { p.Del("code_verifier") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := tokenParams(clientID, mint())
			tc.edit(params)
			if rec := f.token(params); rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestTokenRefusesEveryOtherGrant(t *testing.T) {
	f := newFlow(t)
	for _, grant := range []string{"", "refresh_token", "client_credentials", "urn:ietf:params:oauth:grant-type:token-exchange"} {
		rec := f.token(url.Values{"grant_type": {grant}, "code": {"whatever"}})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("grant_type=%q: status = %d, want 400", grant, rec.Code)
		}
		var e errorResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error != "unsupported_grant_type" {
			t.Errorf("grant_type=%q: error = %q (%v)", grant, e.Error, err)
		}
	}
}

// An authorization code expires with the rest of the half-finished sign-in.
func TestAuthorizationCodeExpires(t *testing.T) {
	store := testStore(t)
	now := time.Now()
	kc := &fakeKeycloak{id: mediaIdentity()}
	s, err := New(Config{
		PublicURL:     "https://edit.prodeko.org",
		Keycloak:      kc,
		Sessions:      store,
		Tokens:        store,
		RequiredRoles: []string{"prodeko-org-media", "membership"},
		Logger:        quietLogger(),
		Now:           func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	f := &flow{t: t, s: s, mux: mux, kc: kc}

	clientID := f.register(testRedirectURI)
	kcState := f.keycloakState(f.authorize(authParams(clientID, testRedirectURI, "s")))
	code := f.redirectParams(f.callback(kcState), testRedirectURI).Get("code")

	now = now.Add(DefaultAuthTTL + time.Second)
	if rec := f.token(tokenParams(clientID, code)); rec.Code != http.StatusBadRequest {
		t.Fatalf("an expired code was redeemed: status = %d", rec.Code)
	}
}

// An unknown client or an unregistered redirect_uri must not be reported by
// redirecting: that would make this an open redirector and hand an error, and
// later a code, to whoever asked.
func TestAuthorizeRefusesToRedirectWhatItCannotTrust(t *testing.T) {
	f := newFlow(t)
	clientID := f.register(testRedirectURI)

	for _, tc := range []struct {
		name string
		edit func(url.Values)
	}{
		{"unknown client", func(p url.Values) { p.Set("client_id", "never-registered") }},
		{"no client", func(p url.Values) { p.Del("client_id") }},
		{"unregistered redirect uri", func(p url.Values) { p.Set("redirect_uri", "https://evil.example/steal") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := authParams(clientID, testRedirectURI, "s")
			tc.edit(params)
			rec := f.authorize(params)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if loc := rec.Header().Get("Location"); loc != "" {
				t.Fatalf("redirected to %q instead of refusing in place", loc)
			}
		})
	}
}

// Everything found after the client is known is reported at its redirect URI,
// with its state, which is how a connector shows the user what went wrong.
func TestAuthorizeReportsBadRequestsAtTheRedirectURI(t *testing.T) {
	f := newFlow(t)
	clientID := f.register(testRedirectURI)

	for _, tc := range []struct {
		name      string
		edit      func(url.Values)
		wantError string
	}{
		{"no PKCE at all", func(p url.Values) {
			p.Del("code_challenge")
			p.Del("code_challenge_method")
		}, "invalid_request"},
		{"plain PKCE", func(p url.Values) { p.Set("code_challenge_method", "plain") }, "invalid_request"},
		{"a challenge that is not one", func(p url.Values) { p.Set("code_challenge", "short") }, "invalid_request"},
		{"an implicit flow", func(p url.Values) { p.Set("response_type", "token") }, "unsupported_response_type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := authParams(clientID, testRedirectURI, "state-42")
			tc.edit(params)
			back := f.redirectParams(f.authorize(params), testRedirectURI)
			if back.Get("error") != tc.wantError {
				t.Errorf("error = %q, want %q", back.Get("error"), tc.wantError)
			}
			if back.Get("state") != "state-42" {
				t.Errorf("state = %q, want it echoed", back.Get("state"))
			}
			if back.Get("code") != "" {
				t.Error("a failed authorization returned a code")
			}
		})
	}
}

// Keycloak being unreachable is a failure of ours, reported as such rather than
// as a refusal of the client.
func TestAuthorizeReportsAnUnavailableKeycloak(t *testing.T) {
	f := newFlow(t)
	clientID := f.register(testRedirectURI)
	f.kc.mu.Lock()
	f.kc.unavailable = true
	f.kc.mu.Unlock()

	back := f.redirectParams(f.authorize(authParams(clientID, testRedirectURI, "s")), testRedirectURI)
	if back.Get("error") != "temporarily_unavailable" {
		t.Errorf("error = %q, want temporarily_unavailable", back.Get("error"))
	}
}

// A client that registered exactly one redirect URI may omit it; one that
// registered several may not, because guessing is guessing where to send a code.
func TestAuthorizeDefaultsToTheOnlyRegisteredRedirectURI(t *testing.T) {
	f := newFlow(t)

	single := f.register(testRedirectURI)
	params := authParams(single, "", "s")
	params.Del("redirect_uri")
	f.keycloakState(f.authorize(params))

	many := f.register(testRedirectURI, "http://127.0.0.1:33418/callback")
	params = authParams(many, "", "s")
	params.Del("redirect_uri")
	if rec := f.authorize(params); rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 when the client registered two URIs", rec.Code)
	}
}

// The role conjunction at sign-in: the account logs into Keycloak successfully
// and still gets no code.
func TestCallbackRequiresEveryRole(t *testing.T) {
	f := newFlow(t)
	clientID := f.register(testRedirectURI)

	f.kc.mu.Lock()
	f.kc.id = session.Identity{
		Subject: "abc", Username: "matti", Name: "Matti", Email: "matti@prodeko.org",
		Roles: []string{"membership"},
	}
	f.kc.mu.Unlock()

	kcState := f.keycloakState(f.authorize(authParams(clientID, testRedirectURI, "s")))
	back := f.redirectParams(f.callback(kcState), testRedirectURI)
	if back.Get("error") != "access_denied" {
		t.Errorf("error = %q, want access_denied", back.Get("error"))
	}
	if back.Get("code") != "" {
		t.Fatal("an account missing prodeko-org-media was given an authorization code")
	}
	// The description names what is required, never what this account is.
	if d := back.Get("error_description"); !strings.Contains(d, "prodeko-org-media") {
		t.Errorf("error_description = %q, want the required roles named", d)
	}
}

func TestCallbackStateIsSingleUse(t *testing.T) {
	f := newFlow(t)
	clientID := f.register(testRedirectURI)
	kcState := f.keycloakState(f.authorize(authParams(clientID, testRedirectURI, "s")))

	if code := f.redirectParams(f.callback(kcState), testRedirectURI).Get("code"); code == "" {
		t.Fatal("the first callback returned no code")
	}
	rec := f.callback(kcState)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a replayed callback: status = %d, want 400", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("a replayed callback redirected to %q", loc)
	}
}

func TestCallbackReportsKeycloakFailures(t *testing.T) {
	f := newFlow(t)
	clientID := f.register(testRedirectURI)

	t.Run("Keycloak refused", func(t *testing.T) {
		kcState := f.keycloakState(f.authorize(authParams(clientID, testRedirectURI, "s")))
		rec := f.do(httptest.NewRequest(http.MethodGet,
			CallbackPath+"?state="+url.QueryEscape(kcState)+"&error=access_denied", nil))
		back := f.redirectParams(rec, testRedirectURI)
		if back.Get("error") != "access_denied" || back.Get("code") != "" {
			t.Errorf("params = %v", back)
		}
	})

	t.Run("the exchange failed", func(t *testing.T) {
		f.kc.mu.Lock()
		f.kc.err = errors.New("the ID token did not verify")
		f.kc.mu.Unlock()
		defer func() {
			f.kc.mu.Lock()
			f.kc.err = nil
			f.kc.mu.Unlock()
		}()

		kcState := f.keycloakState(f.authorize(authParams(clientID, testRedirectURI, "s")))
		back := f.redirectParams(f.callback(kcState), testRedirectURI)
		if back.Get("error") != "server_error" || back.Get("code") != "" {
			t.Errorf("params = %v", back)
		}
		// Whatever Keycloak said stays in the log, not in a redirect.
		if strings.Contains(back.Get("error_description"), "ID token") {
			t.Errorf("error_description leaks the upstream failure: %q", back.Get("error_description"))
		}
	})
}

// Registration is permissive about what a connector asks for and exact about
// where a code may be sent.
func TestRegistrationIsPermissiveButBoundsRedirectURIs(t *testing.T) {
	f := newFlow(t)

	body, err := json.Marshal(map[string]any{
		"client_name":                "Claude",
		"redirect_uris":              []string{testRedirectURI, "http://localhost:33418/callback", "claudeai://callback"},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_method": "client_secret_post",
		"software_statement":         "ignored",
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := f.do(httptest.NewRequest(http.MethodPost, RegisterPath, strings.NewReader(string(body))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	var resp RegistrationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	// What is returned is what was granted, not what was asked for.
	if strings.Join(resp.GrantTypes, ",") != "authorization_code" {
		t.Errorf("grant_types = %v", resp.GrantTypes)
	}
	if resp.TokenEndpointAuthMethod != "none" {
		t.Errorf("token_endpoint_auth_method = %q", resp.TokenEndpointAuthMethod)
	}
	if len(resp.RedirectURIs) != 3 {
		t.Errorf("redirect_uris = %v", resp.RedirectURIs)
	}
	if resp.ClientIDIssuedAt == 0 {
		t.Error("client_id_issued_at is missing")
	}

	for _, tc := range []struct{ name, uri string }{
		{"no redirect uri at all", ""},
		{"relative", "/callback"},
		{"plain http off the loopback", "http://evil.example/callback"},
		{"javascript", "javascript:alert(1)"},
		{"with a fragment", "https://claude.ai/callback#x"},
		{"with a newline", "https://claude.ai/call\nback"},
		{"far too long", "https://claude.ai/" + strings.Repeat("a", MaxRedirectURILen)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var uris []string
			if tc.uri != "" {
				uris = []string{tc.uri}
			}
			body, err := json.Marshal(RegistrationRequest{ClientName: "Claude", RedirectURIs: uris})
			if err != nil {
				t.Fatal(err)
			}
			rec := f.do(httptest.NewRequest(http.MethodPost, RegisterPath, strings.NewReader(string(body))))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for %q", rec.Code, tc.uri)
			}
		})
	}

	rec = f.do(httptest.NewRequest(http.MethodPost, RegisterPath, strings.NewReader("{not json")))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a body that is not JSON: status = %d, want 400", rec.Code)
	}
}

// Two registrations must not collide, and a client may only be answered at its
// own URIs.
func TestRegistrationsAreSeparate(t *testing.T) {
	f := newFlow(t)
	a := f.register(testRedirectURI)
	b := f.register("http://localhost:1234/callback")
	if a == b {
		t.Fatal("two registrations got the same client_id")
	}

	params := authParams(a, "http://localhost:1234/callback", "s")
	if rec := f.authorize(params); rec.Code != http.StatusBadRequest {
		t.Fatalf("a client was allowed another client's redirect_uri: status = %d", rec.Code)
	}
}

func TestVerifyPKCE(t *testing.T) {
	challenge := challengeFor(testVerifier)
	if err := verifyPKCE(challenge, testVerifier); err != nil {
		t.Fatalf("verifyPKCE with the right verifier: %v", err)
	}
	for _, tc := range []struct{ name, challenge, verifier string }{
		{"wrong verifier", challenge, strings.Repeat("y", 48)},
		{"too short", challengeFor("short"), "short"},
		{"too long", challengeFor(strings.Repeat("z", 129)), strings.Repeat("z", 129)},
		{"not the unreserved set", challengeFor("a b" + strings.Repeat("c", 45)), "a b" + strings.Repeat("c", 45)},
		{"no challenge recorded", "", testVerifier},
		{"no verifier presented", challenge, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verifyPKCE(tc.challenge, tc.verifier); !errors.Is(err, ErrBadVerifier) {
				t.Fatalf("err = %v, want ErrBadVerifier", err)
			}
		})
	}
}

// The redirect URI may already carry a query of its own; the code is added to
// it rather than replacing it.
func TestRedirectPreservesTheClientsOwnQuery(t *testing.T) {
	f := newFlow(t)
	const uri = "https://claude.ai/callback?tenant=prodeko"
	clientID := f.register(uri)

	kcState := f.keycloakState(f.authorize(authParams(clientID, uri, "s")))
	back := f.redirectParams(f.callback(kcState), "https://claude.ai/callback?")
	if back.Get("tenant") != "prodeko" {
		t.Errorf("the client's own query was dropped: %v", back)
	}
	if back.Get("code") == "" {
		t.Errorf("no code: %v", back)
	}
}
