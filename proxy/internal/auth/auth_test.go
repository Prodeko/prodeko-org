package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/prodeko/prodeko-org/proxy/internal/session"
)

const (
	testClientID   = "prodeko-cms"
	testEditorRole = "cms-editor"
	testPublicURL  = "https://cms.prodeko.org"
)

// testEditorRoles is the required set. Every one of them has to be present:
// membership is maintained automatically and lapses on its own, cms-editor is
// granted by hand, and requiring both is what makes the hand-granted half
// expire with the automatic one.
var testEditorRoles = []string{"membership", testEditorRole}

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

func newTestHandler(t *testing.T, idp *fakeIdP, mutate func(*Config)) (*Handler, *session.Store) {
	t.Helper()
	store, err := session.NewStore(session.Options{Secret: []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{
		Issuer:       idp.issuer(),
		ClientID:     testClientID,
		ClientSecret: "s3cret",
		EditorRoles:  testEditorRoles,
		PublicURL:    testPublicURL,
		CMSOrigins:   []string{"https://prodeko.org"},
		HTTPClient:   idp.srv.Client(),
		Logger:       discardLogger(),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	h, err := New(context.Background(), cfg, store)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h, store
}

// startSignIn runs GET /auth and returns the query of the Keycloak redirect.
func startSignIn(t *testing.T, h *Handler) url.Values {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Start(rec, httptest.NewRequest(http.MethodGet, "/auth?provider=github&site_id=prodeko.org&scope=repo", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("/auth status = %d, want 302", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return loc.Query()
}

func callback(t *testing.T, h *Handler, query url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Callback(rec, httptest.NewRequest(http.MethodGet, "/callback?"+query.Encode(), nil))
	return rec
}

var payloadRE = regexp.MustCompile(`var payload  = (\{.*\});`)

// handshakeOutcome pulls the status and payload the popup will post.
func handshakeOutcome(t *testing.T, body string) (status string, payload map[string]string) {
	t.Helper()
	switch {
	case strings.Contains(body, `var status   = "success";`):
		status = "success"
	case strings.Contains(body, `var status   = "error";`):
		status = "error"
	default:
		t.Fatalf("no handshake status in the page:\n%s", body)
	}
	m := payloadRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no handshake payload in the page:\n%s", body)
	}
	if err := json.Unmarshal([]byte(m[1]), &payload); err != nil {
		t.Fatalf("payload %q is not a JSON object: %v", m[1], err)
	}
	return status, payload
}

// assertHandshakeIsTwoStep is the test that matters most. Decap registers its
// success/error listener only after it receives "authorizing:<provider>", so a
// page that posts the result first is dropped on the floor and the popup closes
// with nothing happening. This must hold on the error path too; CivicDataLab's
// proxy gets that wrong, which is why its "you are not an editor" message never
// reaches the CMS.
func assertHandshakeIsTwoStep(t *testing.T, body string) {
	t.Helper()

	const (
		register  = `window.addEventListener('message', onMessage, false);`
		handshake = `window.opener.postMessage('authorizing:' + provider, '*');`
		result    = `'authorization:' + provider + ':' + status + ':' + JSON.stringify(payload),`
		onMessage = `function onMessage(e) {`
	)
	for _, s := range []string{register, handshake, result, onMessage} {
		if strings.Count(body, s) != 1 {
			t.Fatalf("expected exactly one occurrence of %q, found %d", s, strings.Count(body, s))
		}
	}
	if strings.Index(body, register) > strings.Index(body, handshake) {
		t.Error("the message listener is registered after the handshake is sent; replies would be missed")
	}
	if strings.Index(body, result) < strings.Index(body, onMessage) {
		t.Error("the result is posted outside the message handler, i.e. before Decap is listening")
	}
	// The result must only ever leave in response to the opener's echo.
	if !strings.Contains(body, `if (done || e.data !== 'authorizing:' + provider) { return; }`) {
		t.Error("the handler does not check for the echoed handshake string")
	}
	if !strings.Contains(body, `if (e.source !== window.opener)`) {
		t.Error("the handler does not check that the reply came from the opener")
	}
	if !strings.Contains(body, `e.source.postMessage(`) {
		t.Error("the result is not posted back to the window that answered")
	}
	// targetOrigin must be the opener's origin, never '*'.
	if strings.Contains(body, `JSON.stringify(payload),`+"\n      '*'") {
		t.Error("the result is posted with targetOrigin '*'")
	}
}

func assertPopupHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Cross-Origin-Opener-Policy"); got != "unsafe-none" {
		t.Errorf("COOP = %q; anything stricter severs window.opener and kills the handshake", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	nonce := regexp.MustCompile(`<script nonce="([^"]+)">`).FindStringSubmatch(rec.Body.String())
	if nonce == nil {
		t.Fatal("the inline script carries no nonce")
	}
	if !strings.Contains(csp, "'nonce-"+nonce[1]+"'") {
		t.Errorf("CSP %q does not allow the script nonce %q; the inline handshake would be blocked", csp, nonce[1])
	}
}

// idTokenClaims is the default set of ID token claims for a successful login.
func idTokenClaims(nonce string) map[string]any {
	return map[string]any{
		"aud":                testClientID,
		"azp":                testClientID,
		"sub":                "3f0c-11ee-user",
		"nonce":              nonce,
		"name":               "Aino Esimerkki",
		"email":              "aino@prodeko.org",
		"preferred_username": "aino",
	}
}

func accessTokenClaims(roles []string) map[string]any {
	return map[string]any{
		"aud":                "account",
		"azp":                testClientID,
		"sub":                "3f0c-11ee-user",
		"preferred_username": "aino",
		"realm_access":       map[string]any{"roles": roles},
	}
}

// The realistic Keycloak default: realm roles are in the access token only,
// because the built-in "realm roles" mapper is created with idToken=false.
func TestCallbackSuccessWithRolesInAccessToken(t *testing.T) {
	idp := newFakeIdP(t)
	h, store := newTestHandler(t, idp, nil)

	q := startSignIn(t, h)
	idp.setTokenResponse(func(url.Values) (int, map[string]any) {
		return http.StatusOK, map[string]any{
			"token_type":   "Bearer",
			"expires_in":   300,
			"access_token": idp.sign(accessTokenClaims(testEditorRoles)),
			"id_token":     idp.sign(idTokenClaims(q.Get("nonce"))),
		}
	})

	rec := callback(t, h, url.Values{"state": {q.Get("state")}, "code": {"abc"}})
	body := rec.Body.String()

	assertHandshakeIsTwoStep(t, body)
	assertPopupHeaders(t, rec)

	status, payload := handshakeOutcome(t, body)
	if status != "success" {
		t.Fatalf("status = %s, payload = %v", status, payload)
	}
	if payload["provider"] != "github" {
		t.Errorf("provider = %q, want github", payload["provider"])
	}

	id, err := store.Verify(payload["token"])
	if err != nil {
		t.Fatalf("the issued token does not verify: %v", err)
	}
	if id.Subject != "3f0c-11ee-user" || id.Name != "Aino Esimerkki" ||
		id.Email != "aino@prodeko.org" || id.Username != "aino" {
		t.Errorf("identity = %+v", id)
	}
	if missing := id.MissingRoles(testEditorRoles); len(missing) > 0 {
		t.Errorf("roles = %v, want to include %v", id.Roles, testEditorRoles)
	}
}

// If somebody does flip "Add to ID token" on the mapper, that must work too.
func TestCallbackSuccessWithRolesInIDToken(t *testing.T) {
	idp := newFakeIdP(t)
	h, store := newTestHandler(t, idp, nil)

	q := startSignIn(t, h)
	claims := idTokenClaims(q.Get("nonce"))
	claims["realm_access"] = map[string]any{"roles": testEditorRoles}
	idp.setTokenResponse(func(url.Values) (int, map[string]any) {
		return http.StatusOK, map[string]any{
			"token_type": "Bearer",
			// The access token carries no roles: the ID token is enough.
			"access_token": idp.sign(accessTokenClaims(nil)),
			"id_token":     idp.sign(claims),
		}
	})

	rec := callback(t, h, url.Values{"state": {q.Get("state")}, "code": {"abc"}})
	status, payload := handshakeOutcome(t, rec.Body.String())
	if status != "success" {
		t.Fatalf("status = %s, payload = %v", status, payload)
	}
	if _, err := store.Verify(payload["token"]); err != nil {
		t.Fatalf("issued token does not verify: %v", err)
	}
}

// Every required role has to be present; anything else the realm hands out is
// none of this proxy's business.
func TestCallbackSuccessWithUnrelatedExtraRoles(t *testing.T) {
	idp := newFakeIdP(t)
	h, store := newTestHandler(t, idp, nil)

	q := startSignIn(t, h)
	idp.setTokenResponse(func(url.Values) (int, map[string]any) {
		return http.StatusOK, map[string]any{
			"token_type": "Bearer",
			"access_token": idp.sign(accessTokenClaims([]string{
				"default-roles-membership-registry", testEditorRole,
				"offline_access", "membership", "prodeko-external-member",
			})),
			"id_token": idp.sign(idTokenClaims(q.Get("nonce"))),
		}
	})

	status, payload := handshakeOutcome(t, callback(t, h, url.Values{"state": {q.Get("state")}, "code": {"abc"}}).Body.String())
	if status != "success" {
		t.Fatalf("status = %s, payload = %v", status, payload)
	}
	id, err := store.Verify(payload["token"])
	if err != nil {
		t.Fatalf("the issued token does not verify: %v", err)
	}
	if missing := id.MissingRoles(testEditorRoles); len(missing) > 0 {
		t.Errorf("roles = %v, missing %v", id.Roles, missing)
	}
}

// Rule 1: short of any one editor role, no session. Every one of these must end
// with an error payload that still completes the handshake, and must mint
// nothing.
func TestCallbackRefusals(t *testing.T) {
	tests := []struct {
		name string
		// tokens builds the token endpoint response; nonce is the one /auth issued.
		tokens func(idp *fakeIdP, nonce string) map[string]any
		// query overrides the callback query; nil means the normal state+code.
		query func(state string) url.Values
		// wantMessage is a fragment the error must mention, so the first sign-in
		// attempt diagnoses itself.
		wantMessage string
	}{
		{
			name: "member who was never granted the editor role",
			tokens: func(idp *fakeIdP, nonce string) map[string]any {
				return map[string]any{
					"access_token": idp.sign(accessTokenClaims([]string{"membership"})),
					"id_token":     idp.sign(idTokenClaims(nonce)),
				}
			},
			wantMessage: testEditorRole,
		},
		// The case the conjunction exists for: the hand-granted role is still
		// there, the automatically maintained membership is not.
		{
			name: "editor whose guild membership has lapsed",
			tokens: func(idp *fakeIdP, nonce string) map[string]any {
				return map[string]any{
					"access_token": idp.sign(accessTokenClaims([]string{testEditorRole, "offline_access"})),
					"id_token":     idp.sign(idTokenClaims(nonce)),
				}
			},
			wantMessage: "membership",
		},
		{
			name: "neither role",
			tokens: func(idp *fakeIdP, nonce string) map[string]any {
				return map[string]any{
					"access_token": idp.sign(accessTokenClaims([]string{"prodeko-external-member"})),
					"id_token":     idp.sign(idTokenClaims(nonce)),
				}
			},
			wantMessage: "missing membership, " + testEditorRole,
		},
		{
			name: "no realm_access.roles anywhere",
			tokens: func(idp *fakeIdP, nonce string) map[string]any {
				at := accessTokenClaims(nil)
				delete(at, "realm_access")
				return map[string]any{
					"access_token": idp.sign(at),
					"id_token":     idp.sign(idTokenClaims(nonce)),
				}
			},
			wantMessage: "realm roles",
		},
		{
			name: "access token issued to another client",
			tokens: func(idp *fakeIdP, nonce string) map[string]any {
				at := accessTokenClaims(testEditorRoles)
				at["azp"] = "some-other-client"
				return map[string]any{
					"access_token": idp.sign(at),
					"id_token":     idp.sign(idTokenClaims(nonce)),
				}
			},
			wantMessage: "some-other-client",
		},
		{
			name: "id token for another audience",
			tokens: func(idp *fakeIdP, nonce string) map[string]any {
				c := idTokenClaims(nonce)
				c["aud"] = "a-different-client"
				return map[string]any{
					"access_token": idp.sign(accessTokenClaims(testEditorRoles)),
					"id_token":     idp.sign(c),
				}
			},
			wantMessage: "did not verify",
		},
		{
			name: "id token from another issuer",
			tokens: func(idp *fakeIdP, nonce string) map[string]any {
				c := idTokenClaims(nonce)
				c["iss"] = "https://evil.example"
				return map[string]any{
					"access_token": idp.sign(accessTokenClaims(testEditorRoles)),
					"id_token":     idp.sign(c),
				}
			},
			wantMessage: "did not verify",
		},
		{
			name: "expired id token",
			tokens: func(idp *fakeIdP, nonce string) map[string]any {
				c := idTokenClaims(nonce)
				c["exp"] = time.Now().Add(-time.Hour).Unix()
				return map[string]any{
					"access_token": idp.sign(accessTokenClaims(testEditorRoles)),
					"id_token":     idp.sign(c),
				}
			},
			wantMessage: "did not verify",
		},
		{
			name: "replayed id token from a different sign-in",
			tokens: func(idp *fakeIdP, nonce string) map[string]any {
				return map[string]any{
					"access_token": idp.sign(accessTokenClaims(testEditorRoles)),
					"id_token":     idp.sign(idTokenClaims("a-nonce-from-another-login")),
				}
			},
			wantMessage: "different sign-in",
		},
		{
			name: "no id token at all",
			tokens: func(idp *fakeIdP, nonce string) map[string]any {
				return map[string]any{"access_token": idp.sign(accessTokenClaims(testEditorRoles))}
			},
			wantMessage: "no ID token",
		},
		{
			name: "account with no email",
			tokens: func(idp *fakeIdP, nonce string) map[string]any {
				c := idTokenClaims(nonce)
				delete(c, "email")
				return map[string]any{
					"access_token": idp.sign(accessTokenClaims(testEditorRoles)),
					"id_token":     idp.sign(c),
				}
			},
			wantMessage: "no email address",
		},
		{
			name: "account with no name",
			tokens: func(idp *fakeIdP, nonce string) map[string]any {
				c := idTokenClaims(nonce)
				delete(c, "name")
				delete(c, "preferred_username")
				return map[string]any{
					"access_token": idp.sign(accessTokenClaims(testEditorRoles)),
					"id_token":     idp.sign(c),
				}
			},
			wantMessage: "no name",
		},
		{
			name:        "unknown state",
			query:       func(string) url.Values { return url.Values{"state": {"not-a-state"}, "code": {"abc"}} },
			wantMessage: "not recognised",
		},
		{
			name:        "no state",
			query:       func(string) url.Values { return url.Values{"code": {"abc"}} },
			wantMessage: "not recognised",
		},
		{
			name:        "no code",
			query:       func(state string) url.Values { return url.Values{"state": {state}} },
			wantMessage: "authorization code",
		},
		{
			name: "keycloak refused",
			query: func(state string) url.Values {
				return url.Values{
					"state":             {state},
					"error":             {"access_denied"},
					"error_description": {"User cancelled"},
				}
			},
			wantMessage: "access_denied",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idp := newFakeIdP(t)
			h, store := newTestHandler(t, idp, nil)
			q := startSignIn(t, h)

			if tc.tokens != nil {
				body := tc.tokens(idp, q.Get("nonce"))
				body["token_type"] = "Bearer"
				idp.setTokenResponse(func(url.Values) (int, map[string]any) { return http.StatusOK, body })
			}
			cq := url.Values{"state": {q.Get("state")}, "code": {"abc"}}
			if tc.query != nil {
				cq = tc.query(q.Get("state"))
			}

			rec := callback(t, h, cq)
			body := rec.Body.String()

			assertHandshakeIsTwoStep(t, body)
			assertPopupHeaders(t, rec)

			status, payload := handshakeOutcome(t, body)
			if status != "error" {
				t.Fatalf("status = %s (a session was handed out), payload = %v", status, payload)
			}
			if payload["message"] == "" {
				t.Fatal("the error payload has no message field; Decap renders NetlifyError.toString() as undefined")
			}
			if !strings.Contains(payload["message"], tc.wantMessage) {
				t.Errorf("message %q does not mention %q", payload["message"], tc.wantMessage)
			}
			if strings.Contains(body, "pkd1_") {
				t.Error("a session token leaked into a failure page")
			}
			_ = store
		})
	}
}

// A state is single use, so a callback URL lifted from browser history cannot
// be turned into a second session.
func TestCallbackStateIsSingleUse(t *testing.T) {
	idp := newFakeIdP(t)
	h, _ := newTestHandler(t, idp, nil)
	q := startSignIn(t, h)
	idp.setTokenResponse(func(url.Values) (int, map[string]any) {
		return http.StatusOK, map[string]any{
			"token_type":   "Bearer",
			"access_token": idp.sign(accessTokenClaims(testEditorRoles)),
			"id_token":     idp.sign(idTokenClaims(q.Get("nonce"))),
		}
	})

	cq := url.Values{"state": {q.Get("state")}, "code": {"abc"}}
	if status, _ := handshakeOutcome(t, callback(t, h, cq).Body.String()); status != "success" {
		t.Fatal("the first callback should have succeeded")
	}
	status, payload := handshakeOutcome(t, callback(t, h, cq).Body.String())
	if status != "error" {
		t.Fatal("the same state was accepted twice")
	}
	if !strings.Contains(payload["message"], "not recognised") {
		t.Errorf("message = %q", payload["message"])
	}
}

func TestStartRedirect(t *testing.T) {
	idp := newFakeIdP(t)
	h, _ := newTestHandler(t, idp, nil)

	rec := httptest.NewRecorder()
	h.Start(rec, httptest.NewRequest(http.MethodGet, "/auth?provider=github&site_id=prodeko.org&scope=repo", nil))

	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if want := idp.issuer() + "/protocol/openid-connect/auth"; loc.Scheme+"://"+loc.Host+loc.Path != want {
		t.Errorf("redirect to %q, want %q", loc, want)
	}
	q := loc.Query()
	want := map[string]string{
		"response_type":         "code",
		"client_id":             testClientID,
		"redirect_uri":          testPublicURL + "/callback",
		"code_challenge_method": "S256",
		"scope":                 "openid profile email",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, q.Get(k), v)
		}
	}
	for _, k := range []string{"state", "nonce", "code_challenge"} {
		if len(q.Get(k)) < 20 {
			t.Errorf("%s = %q is too short to be unguessable", k, q.Get(k))
		}
	}
	// Netlify's own parameters are not ours to forward.
	if q.Get("site_id") != "" || q.Get("provider") != "" {
		t.Errorf("Decap's site_id/provider leaked into the Keycloak request: %v", q)
	}
	// Two sign-ins must not share any of it.
	q2 := startSignIn(t, h)
	for _, k := range []string{"state", "nonce", "code_challenge"} {
		if q.Get(k) == q2.Get(k) {
			t.Errorf("%s is reused between sign-ins", k)
		}
	}
}

// PKCE is sent on every sign-in, so that a leaked authorization code is not on
// its own enough to obtain tokens.
func TestPKCEVerifierMatchesChallenge(t *testing.T) {
	idp := newFakeIdP(t)
	h, _ := newTestHandler(t, idp, nil)
	q := startSignIn(t, h)
	idp.setTokenResponse(func(url.Values) (int, map[string]any) {
		return http.StatusOK, map[string]any{
			"token_type":   "Bearer",
			"access_token": idp.sign(accessTokenClaims(testEditorRoles)),
			"id_token":     idp.sign(idTokenClaims(q.Get("nonce"))),
		}
	})
	callback(t, h, url.Values{"state": {q.Get("state")}, "code": {"abc"}})

	form := idp.tokenForm()
	verifier := form.Get("code_verifier")
	if verifier == "" {
		t.Fatal("no code_verifier was sent to the token endpoint")
	}
	sum := sha256.Sum256([]byte(verifier))
	if got := base64.RawURLEncoding.EncodeToString(sum[:]); got != q.Get("code_challenge") {
		t.Errorf("S256(code_verifier) = %q, but the challenge was %q", got, q.Get("code_challenge"))
	}
	if form.Get("grant_type") != "authorization_code" {
		t.Errorf("grant_type = %q", form.Get("grant_type"))
	}
	if form.Get("redirect_uri") != testPublicURL+"/callback" {
		t.Errorf("redirect_uri = %q", form.Get("redirect_uri"))
	}
	// Rule 4, at the Keycloak end: the secret never leaves the server.
	if form.Get("client_secret") != "s3cret" {
		t.Errorf("client_secret = %q, want the configured secret", form.Get("client_secret"))
	}
}

// A public client (no secret in the environment) still has to work, because
// whether Prodeko's realm registers a confidential client is not settled.
func TestPublicClientSendsNoSecret(t *testing.T) {
	idp := newFakeIdP(t)
	h, _ := newTestHandler(t, idp, func(c *Config) { c.ClientSecret = "" })
	q := startSignIn(t, h)
	idp.setTokenResponse(func(url.Values) (int, map[string]any) {
		return http.StatusOK, map[string]any{
			"token_type":   "Bearer",
			"access_token": idp.sign(accessTokenClaims(testEditorRoles)),
			"id_token":     idp.sign(idTokenClaims(q.Get("nonce"))),
		}
	})
	if status, p := handshakeOutcome(t, callback(t, h, url.Values{"state": {q.Get("state")}, "code": {"abc"}}).Body.String()); status != "success" {
		t.Fatalf("public client sign-in failed: %v", p)
	}
	if got := idp.tokenForm().Get("client_secret"); got != "" {
		t.Errorf("client_secret = %q, want empty for a public client", got)
	}
	if got := idp.tokenForm().Get("code_verifier"); got == "" {
		t.Error("a public client must send PKCE")
	}
}

// The allowlist is what stops another window in the same browsing context group
// from answering the handshake and walking off with the session token.
func TestCMSOriginsReachTheScript(t *testing.T) {
	idp := newFakeIdP(t)
	h, _ := newTestHandler(t, idp, func(c *Config) {
		c.CMSOrigins = []string{"https://prodeko.org/", "HTTPS://WWW.Prodeko.org", "https://prodeko.org"}
	})
	rec := callback(t, h, url.Values{"state": {"nope"}})
	body := rec.Body.String()
	if !strings.Contains(body, `var origins  = ["https://prodeko.org","https://www.prodeko.org"];`) {
		t.Errorf("origins were not normalised and deduplicated into the script:\n%s", body)
	}
}

// In the dev compose stack the browser reaches Keycloak at localhost and the
// proxy reaches it at keycloak:8180, so discovery happens at one URL while iss
// stays the other. Tokens must still be validated against the issuer.
func TestSplitHorizonDiscovery(t *testing.T) {
	idp := newFakeIdP(t)
	const frontChannel = "https://id.prodeko.example/realms/membership-registry"
	idp.setDiscoveredIssuer(frontChannel)

	h, store := newTestHandler(t, idp, func(c *Config) {
		c.Issuer = frontChannel
		c.DiscoveryURL = idp.issuer()
	})

	q := startSignIn(t, h)
	idp.setTokenResponse(func(url.Values) (int, map[string]any) {
		at := accessTokenClaims(testEditorRoles)
		at["iss"] = frontChannel
		id := idTokenClaims(q.Get("nonce"))
		id["iss"] = frontChannel
		return http.StatusOK, map[string]any{
			"token_type":   "Bearer",
			"access_token": idp.sign(at),
			"id_token":     idp.sign(id),
		}
	})

	status, payload := handshakeOutcome(t, callback(t, h, url.Values{"state": {q.Get("state")}, "code": {"abc"}}).Body.String())
	if status != "success" {
		t.Fatalf("split-horizon sign-in failed: %v", payload)
	}
	if _, err := store.Verify(payload["token"]); err != nil {
		t.Fatal(err)
	}

	// A token carrying the back-channel issuer must still be refused.
	q = startSignIn(t, h)
	idp.setTokenResponse(func(url.Values) (int, map[string]any) {
		at := accessTokenClaims(testEditorRoles)
		at["iss"] = frontChannel
		return http.StatusOK, map[string]any{
			"token_type":   "Bearer",
			"access_token": idp.sign(at),
			"id_token":     idp.sign(idTokenClaims(q.Get("nonce"))), // iss = back channel
		}
	})
	status, payload = handshakeOutcome(t, callback(t, h, url.Values{"state": {q.Get("state")}, "code": {"abc"}}).Body.String())
	if status != "error" || !strings.Contains(payload["message"], "did not verify") {
		t.Fatalf("a token from the back-channel issuer was accepted: %s %v", status, payload)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	idp := newFakeIdP(t)
	base := func() Config {
		return Config{
			Issuer:      idp.issuer(),
			ClientID:    testClientID,
			EditorRoles: testEditorRoles,
			PublicURL:   testPublicURL,
			HTTPClient:  idp.srv.Client(),
			Logger:      discardLogger(),
		}
	}
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"no issuer", func(c *Config) { c.Issuer = "" }},
		{"issuer is not a url", func(c *Config) { c.Issuer = "id.prodeko.org" }},
		{"no client id", func(c *Config) { c.ClientID = "" }},
		{"no editor roles", func(c *Config) { c.EditorRoles = nil }},
		{"an empty list of editor roles", func(c *Config) { c.EditorRoles = []string{} }},
		{"blank editor roles", func(c *Config) { c.EditorRoles = []string{"", "   "} }},
		{"no public url", func(c *Config) { c.PublicURL = "" }},
		{"public url with a path", func(c *Config) { c.PublicURL = "https://prodeko.org/cms" }},
		{"public url with a query", func(c *Config) { c.PublicURL = "https://prodeko.org?a=1" }},
		{"public url without a scheme", func(c *Config) { c.PublicURL = "cms.prodeko.org" }},
		{"cms origin with a path", func(c *Config) { c.CMSOrigins = []string{"https://prodeko.org/admin"} }},
		{"negative state ttl", func(c *Config) { c.StateTTL = -time.Second }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(&cfg)
			store, err := session.NewStore(session.Options{Secret: make([]byte, session.MinSecretLen)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := New(context.Background(), cfg, store); err == nil {
				t.Fatal("expected New to reject this configuration")
			}
		})
	}

	t.Run("nil issuer", func(t *testing.T) {
		if _, err := New(context.Background(), base(), nil); err == nil {
			t.Fatal("expected New to reject a nil session issuer")
		}
	})
	t.Run("a valid config is accepted", func(t *testing.T) {
		store, err := session.NewStore(session.Options{Secret: make([]byte, session.MinSecretLen)})
		if err != nil {
			t.Fatal(err)
		}
		cfg := base()
		cfg.PublicURL = testPublicURL + "/"
		if _, err := New(context.Background(), cfg, store); err != nil {
			t.Fatalf("New: %v", err)
		}
	})
}

func TestRegisterMountsExactPaths(t *testing.T) {
	idp := newFakeIdP(t)
	h, _ := newTestHandler(t, idp, nil)
	mux := http.NewServeMux()
	h.Register(mux)

	tests := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/auth", http.StatusFound},
		{http.MethodGet, "/callback", http.StatusOK}, // error page, but a page
		{http.MethodGet, "/auth/extra", http.StatusNotFound},
		{http.MethodGet, "/callbackx", http.StatusNotFound},
		{http.MethodPost, "/auth", http.StatusMethodNotAllowed},
	}
	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// The provider Decap asked for has to come back in the handshake, or the
// opener's === comparison fails and nothing happens.
func TestProviderIsEchoed(t *testing.T) {
	idp := newFakeIdP(t)
	h, _ := newTestHandler(t, idp, nil)

	rec := httptest.NewRecorder()
	h.Start(rec, httptest.NewRequest(http.MethodGet, "/auth?provider=gitlab", nil))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	state := loc.Query().Get("state")

	body := callback(t, h, url.Values{"state": {state}, "error": {"access_denied"}}).Body.String()
	if !strings.Contains(body, `var provider = "gitlab";`) {
		t.Errorf("provider was not echoed back:\n%s", body)
	}
}
