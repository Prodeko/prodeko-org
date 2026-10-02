package oauthas

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// One 2048-bit key for the whole test binary; generating one per test is slow.
var testKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
})

// fakeIdP is just enough Keycloak to drive [KeycloakClient.Exchange]:
// discovery, a JWKS, and a token endpoint that answers with whatever claims the
// test asks for.
type fakeIdP struct {
	t   *testing.T
	srv *httptest.Server
	key *rsa.PrivateKey

	mu            sync.Mutex
	tokenResponse func(form url.Values) (status int, body map[string]any)
	lastTokenForm url.Values
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	f := &fakeIdP{t: t, key: testKey()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeIdPJSON(w, map[string]any{
			"issuer":                                f.issuer(),
			"authorization_endpoint":                f.issuer() + "/protocol/openid-connect/auth",
			"token_endpoint":                        f.issuer() + "/protocol/openid-connect/token",
			"jwks_uri":                              f.issuer() + "/protocol/openid-connect/certs",
			"id_token_signing_alg_values_supported": []string{"RS256"},
			"response_types_supported":              []string{"code"},
			"subject_types_supported":               []string{"public"},
		})
	})
	mux.HandleFunc("GET /protocol/openid-connect/certs", func(w http.ResponseWriter, r *http.Request) {
		pub := f.key.Public().(*rsa.PublicKey)
		writeIdPJSON(w, map[string]any{"keys": []any{map[string]any{
			"kty": "RSA",
			"alg": "RS256",
			"use": "sig",
			"kid": "test-key",
			"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("POST /protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.lastTokenForm = r.PostForm
		build := f.tokenResponse
		f.mu.Unlock()
		if build == nil {
			http.Error(w, "no token response configured", http.StatusInternalServerError)
			return
		}
		status, body := build(r.PostForm)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) issuer() string { return f.srv.URL }

func (f *fakeIdP) setTokenResponse(fn func(form url.Values) (int, map[string]any)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenResponse = fn
}

func (f *fakeIdP) tokenForm() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastTokenForm
}

// sign produces an RS256 JWT with the given claims, filling in iss/iat/exp.
func (f *fakeIdP) sign(claims map[string]any) string {
	f.t.Helper()
	full := map[string]any{
		"iss": f.issuer(),
		"iat": time.Now().Add(-time.Minute).Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	for k, v := range claims {
		full[k] = v
	}
	header, err := json.Marshal(map[string]any{"alg": "RS256", "typ": "JWT", "kid": "test-key"})
	if err != nil {
		f.t.Fatal(err)
	}
	payload, err := json.Marshal(full)
	if err != nil {
		f.t.Fatal(err)
	}
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, f.key, crypto.SHA256, digest[:])
	if err != nil {
		f.t.Fatal(err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func writeIdPJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

const testKeycloakClientID = "prodeko-mcp"

func testKeycloakClient(t *testing.T, idp *fakeIdP) *KeycloakClient {
	t.Helper()
	k, err := NewKeycloak(context.Background(), KeycloakConfig{
		Issuer:       idp.issuer(),
		ClientID:     testKeycloakClientID,
		ClientSecret: "s3cret",
		RedirectURI:  "https://mcp.prodeko.org/oauth/callback",
		HTTPClient:   idp.srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewKeycloak: %v", err)
	}
	return k
}

// idTokenClaims is a Keycloak ID token for maija, with no roles in it: the
// built-in realm roles mapper is created with idToken=false, so this is what an
// untouched realm actually sends.
func idTokenClaims(nonce string) map[string]any {
	return map[string]any{
		"aud":                testKeycloakClientID,
		"sub":                "3f1c",
		"nonce":              nonce,
		"name":               "Maija Meikäläinen",
		"email":              "maija@prodeko.org",
		"preferred_username": "maija",
	}
}

func accessTokenClaims(roles ...string) map[string]any {
	return map[string]any{
		"aud":                "account",
		"sub":                "3f1c",
		"azp":                testKeycloakClientID,
		"preferred_username": "maija",
		"realm_access":       map[string]any{"roles": roles},
	}
}

func TestKeycloakAuthCodeURL(t *testing.T) {
	idp := newFakeIdP(t)
	k := testKeycloakClient(t, idp)

	raw := k.AuthCodeURL("the-state", "the-nonce", testVerifier)
	if raw == "" {
		t.Fatal("AuthCodeURL returned nothing; discovery should have succeeded")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("AuthCodeURL is not a URL: %v", err)
	}
	if !strings.HasPrefix(raw, idp.issuer()+"/protocol/openid-connect/auth") {
		t.Errorf("AuthCodeURL = %q, want the discovered authorization endpoint", raw)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"client_id":             testKeycloakClientID,
		"response_type":         "code",
		"state":                 "the-state",
		"nonce":                 "the-nonce",
		"redirect_uri":          "https://mcp.prodeko.org/oauth/callback",
		"code_challenge":        challengeFor(testVerifier),
		"code_challenge_method": "S256",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if !strings.Contains(q.Get("scope"), "openid") {
		t.Errorf("scope = %q, want openid among them", q.Get("scope"))
	}
}

// Discovery is deferred to first use so an unreachable Keycloak cannot keep the
// process from starting; it can only fail a sign-in.
func TestKeycloakAuthCodeURLIsEmptyWhenKeycloakIsUnreachable(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()

	// The only report of a discovery failure is a line on the default logger,
	// because the Keycloak interface gives AuthCodeURL nowhere else to put it.
	restore := slog.Default()
	slog.SetDefault(quietLogger())
	defer slog.SetDefault(restore)

	k, err := NewKeycloak(context.Background(), KeycloakConfig{
		Issuer:      dead.URL,
		ClientID:    testKeycloakClientID,
		RedirectURI: "https://mcp.prodeko.org/oauth/callback",
	})
	if err != nil {
		t.Fatalf("NewKeycloak did network I/O it should have deferred: %v", err)
	}
	if got := k.AuthCodeURL("s", "n", testVerifier); got != "" {
		t.Fatalf("AuthCodeURL = %q, want the empty string when discovery fails", got)
	}
}

func TestKeycloakExchange(t *testing.T) {
	idp := newFakeIdP(t)
	k := testKeycloakClient(t, idp)
	idp.setTokenResponse(func(form url.Values) (int, map[string]any) {
		return http.StatusOK, map[string]any{
			"access_token": idp.sign(accessTokenClaims("membership", "prodeko-org-media")),
			"id_token":     idp.sign(idTokenClaims("the-nonce")),
			"token_type":   "Bearer",
			"expires_in":   300,
		}
	})

	id, err := k.Exchange(context.Background(), "keycloak-code", "the-nonce", testVerifier)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if id.Subject != "3f1c" || id.Username != "maija" || id.Email != "maija@prodeko.org" {
		t.Errorf("identity = %+v", id)
	}
	if id.Name != "Maija Meikäläinen" {
		t.Errorf("name = %q", id.Name)
	}
	// The roles come from the access token, which is where Keycloak's built-in
	// mapper puts them.
	if strings.Join(id.Roles, ",") != "membership,prodeko-org-media" {
		t.Errorf("roles = %v", id.Roles)
	}

	form := idp.tokenForm()
	if form.Get("code") != "keycloak-code" {
		t.Errorf("code = %q", form.Get("code"))
	}
	if form.Get("code_verifier") != testVerifier {
		t.Errorf("code_verifier = %q, want our own PKCE verifier on the Keycloak leg", form.Get("code_verifier"))
	}
	if form.Get("client_secret") != "s3cret" {
		t.Errorf("client_secret = %q", form.Get("client_secret"))
	}
	if form.Get("redirect_uri") != "https://mcp.prodeko.org/oauth/callback" {
		t.Errorf("redirect_uri = %q", form.Get("redirect_uri"))
	}
}

// Roles in the ID token are used when the realm has been configured to put them
// there, and no access token is then required.
func TestKeycloakExchangeReadsRolesFromTheIDToken(t *testing.T) {
	idp := newFakeIdP(t)
	k := testKeycloakClient(t, idp)
	idp.setTokenResponse(func(form url.Values) (int, map[string]any) {
		claims := idTokenClaims("n")
		claims["realm_access"] = map[string]any{"roles": []string{"membership", "prodeko-org-media"}}
		return http.StatusOK, map[string]any{
			"id_token":     idp.sign(claims),
			"access_token": idp.sign(accessTokenClaims()),
			"token_type":   "Bearer",
		}
	})

	id, err := k.Exchange(context.Background(), "c", "n", testVerifier)
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if strings.Join(id.Roles, ",") != "membership,prodeko-org-media" {
		t.Errorf("roles = %v", id.Roles)
	}
}

func TestKeycloakExchangeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nonce string
		build func(idp *fakeIdP) map[string]any
		want  string
	}{
		{
			name:  "a replayed sign-in",
			nonce: "expected-nonce",
			build: func(idp *fakeIdP) map[string]any {
				return map[string]any{
					"id_token":     idp.sign(idTokenClaims("somebody-elses-nonce")),
					"access_token": idp.sign(accessTokenClaims("membership")),
				}
			},
			want: "different sign-in",
		},
		{
			name:  "no ID token",
			nonce: "n",
			build: func(idp *fakeIdP) map[string]any {
				return map[string]any{"access_token": idp.sign(accessTokenClaims("membership"))}
			},
			want: "no ID token",
		},
		{
			name:  "an ID token for another client",
			nonce: "n",
			build: func(idp *fakeIdP) map[string]any {
				claims := idTokenClaims("n")
				claims["aud"] = "decap-proxy"
				return map[string]any{
					"id_token":     idp.sign(claims),
					"access_token": idp.sign(accessTokenClaims("membership")),
				}
			},
			want: "did not verify",
		},
		{
			name:  "no realm roles anywhere",
			nonce: "n",
			build: func(idp *fakeIdP) map[string]any {
				return map[string]any{
					"id_token":     idp.sign(idTokenClaims("n")),
					"access_token": idp.sign(accessTokenClaims()),
				}
			},
			want: "realm roles",
		},
		{
			name:  "an access token issued to another client",
			nonce: "n",
			build: func(idp *fakeIdP) map[string]any {
				claims := accessTokenClaims("membership")
				claims["azp"] = "somebody-else"
				return map[string]any{"id_token": idp.sign(idTokenClaims("n")), "access_token": idp.sign(claims)}
			},
			want: "issued to",
		},
		{
			name:  "an account with no email address",
			nonce: "n",
			build: func(idp *fakeIdP) map[string]any {
				claims := idTokenClaims("n")
				delete(claims, "email")
				return map[string]any{
					"id_token":     idp.sign(claims),
					"access_token": idp.sign(accessTokenClaims("membership")),
				}
			},
			want: "no email address",
		},
		{
			name:  "an account with no name",
			nonce: "n",
			build: func(idp *fakeIdP) map[string]any {
				claims := idTokenClaims("n")
				delete(claims, "name")
				delete(claims, "preferred_username")
				return map[string]any{
					"id_token":     idp.sign(claims),
					"access_token": idp.sign(accessTokenClaims("membership")),
				}
			},
			want: "no name",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idp := newFakeIdP(t)
			k := testKeycloakClient(t, idp)
			idp.setTokenResponse(func(form url.Values) (int, map[string]any) {
				body := tc.build(idp)
				body["token_type"] = "Bearer"
				return http.StatusOK, body
			})

			_, err := k.Exchange(context.Background(), "code", tc.nonce, testVerifier)
			if err == nil {
				t.Fatal("Exchange accepted a response it should have refused")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestKeycloakExchangeReportsATokenEndpointRefusal(t *testing.T) {
	idp := newFakeIdP(t)
	k := testKeycloakClient(t, idp)
	idp.setTokenResponse(func(form url.Values) (int, map[string]any) {
		return http.StatusBadRequest, map[string]any{"error": "invalid_grant"}
	})

	_, err := k.Exchange(context.Background(), "stale-code", "n", testVerifier)
	if err == nil {
		t.Fatal("Exchange accepted a refused code")
	}
	if !strings.Contains(err.Error(), "code exchange failed") {
		t.Errorf("err = %v", err)
	}
}
