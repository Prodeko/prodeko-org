package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prodeko/prodeko-org/proxy/internal/mcpserver"
	"github.com/prodeko/prodeko-org/proxy/internal/oauthas"
	"github.com/prodeko/prodeko-org/proxy/internal/upload"
)

func completeEnv() map[string]string {
	return map[string]string{
		"PUBLIC_URL":             "https://mcp.prodeko.org",
		"MCP_REPO_PATH":          "/srv/mcp/repo",
		"MCP_STATE_DIR":          "/srv/mcp/state",
		"KEYCLOAK_ISSUER":        "https://id.prodeko.org/realms/membership-registry",
		"KEYCLOAK_CLIENT_ID":     "prodeko-mcp",
		"SESSION_SECRET":         strings.Repeat("s", 32),
		"GIT_COMMITTER_NAME":     "Prodeko media bot",
		"GIT_COMMITTER_EMAIL":    "media-bot@prodeko.org",
		"MCP_REQUIRED_ROLES":     "prodeko-org-media,membership",
		"KEYCLOAK_CLIENT_SECRET": "shh",
	}
}

func lookupFrom(m map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := m[name]
		return v, ok
	}
}

func TestLoadEnvDefaults(t *testing.T) {
	cfg, err := loadEnv(lookupFrom(completeEnv()))
	if err != nil {
		t.Fatalf("loadEnv: %v", err)
	}
	if cfg.ListenAddr != defaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, defaultListenAddr)
	}
	if strings.Join(cfg.RequiredRoles, ",") != "prodeko-org-media,membership" {
		t.Errorf("RequiredRoles = %v", cfg.RequiredRoles)
	}
	if cfg.DevBearer != "" {
		t.Errorf("DevBearer = %q, want empty", cfg.DevBearer)
	}
}

// The role conjunction is the one default that must not be a surprise: with
// MCP_REQUIRED_ROLES absent, both roles are still required.
func TestRequiredRolesDefaultToTheConjunction(t *testing.T) {
	vars := completeEnv()
	delete(vars, "MCP_REQUIRED_ROLES")
	cfg, err := loadEnv(lookupFrom(vars))
	if err != nil {
		t.Fatalf("loadEnv: %v", err)
	}
	if strings.Join(cfg.RequiredRoles, ",") != "prodeko-org-media,membership" {
		t.Fatalf("RequiredRoles = %v, want the conjunction", cfg.RequiredRoles)
	}
}

func TestLoadEnvReportsEveryMissingVariable(t *testing.T) {
	_, err := loadEnv(lookupFrom(map[string]string{}))
	if err == nil {
		t.Fatal("loadEnv accepted an empty environment")
	}
	for _, name := range []string{
		"PUBLIC_URL", "MCP_REPO_PATH", "MCP_STATE_DIR",
		"KEYCLOAK_ISSUER", "KEYCLOAK_CLIENT_ID", "SESSION_SECRET",
	} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not name %s: %v", name, err)
		}
	}
}

// The local demo has no realm to sign in to, so MCP_DEV_BEARER is what lets
// the Keycloak variables be absent. Without the dev bearer they are required,
// because then a sealed session is the only credential and only Keycloak mints
// one.
func TestDevBearerMakesKeycloakOptional(t *testing.T) {
	vars := completeEnv()
	delete(vars, "KEYCLOAK_ISSUER")
	delete(vars, "KEYCLOAK_CLIENT_ID")

	if _, err := loadEnv(lookupFrom(vars)); err == nil {
		t.Fatal("loadEnv accepted a configuration with neither Keycloak nor a dev bearer")
	}

	vars["MCP_DEV_BEARER"] = "demo-bearer-prodeko"
	cfg, err := loadEnv(lookupFrom(vars))
	if err != nil {
		t.Fatalf("loadEnv: %v", err)
	}
	if cfg.KeycloakIssuer != "" {
		t.Errorf("KeycloakIssuer = %q, want empty", cfg.KeycloakIssuer)
	}

	// Sign-in must fail as unavailable rather than redirect somewhere.
	if url := (signInUnavailable{}).AuthCodeURL("state", "nonce", "verifier"); url != "" {
		t.Errorf("AuthCodeURL = %q, want the empty string", url)
	}
	if _, err := (signInUnavailable{}).Exchange(t.Context(), "code", "nonce", "verifier"); err == nil {
		t.Error("Exchange succeeded without a Keycloak realm")
	}
}

func TestLoadEnvRejectsBadValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(map[string]string)
		names string
	}{
		{"short secret", func(m map[string]string) { m["SESSION_SECRET"] = "too-short" }, "SESSION_SECRET"},
		{"public url without a scheme", func(m map[string]string) { m["PUBLIC_URL"] = "mcp.prodeko.org" }, "PUBLIC_URL"},
		{"github repo without an owner", func(m map[string]string) { m["GITHUB_REPO"] = "prodeko-org" }, "GITHUB_REPO"},
		{"unknown log level", func(m map[string]string) { m["LOG_LEVEL"] = "chatty" }, "LOG_LEVEL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vars := completeEnv()
			tc.edit(vars)
			_, err := loadEnv(lookupFrom(vars))
			if err == nil {
				t.Fatal("loadEnv accepted an unusable value")
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("error does not name %s: %v", tc.names, err)
			}
		})
	}
}

// Secrets are named in the banner, never printed.
func TestBannerRedactsSecrets(t *testing.T) {
	vars := completeEnv()
	vars["MCP_DEV_BEARER"] = "dev-token-value"
	vars["GITHUB_TOKEN"] = "ghp_secret_value"
	cfg, err := loadEnv(lookupFrom(vars))
	if err != nil {
		t.Fatalf("loadEnv: %v", err)
	}
	banner := cfg.String()
	for _, secret := range []string{"dev-token-value", "ghp_secret_value", "shh", strings.Repeat("s", 32)} {
		if strings.Contains(banner, secret) {
			t.Errorf("banner prints a secret: %s", banner)
		}
	}
	for _, want := range []string{"listen_addr", "public_url", "required_roles", "committer"} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner does not report %s", want)
		}
	}
}

type fakeRegistrar struct {
	path string
}

func (f fakeRegistrar) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+f.path, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(f.path))
	})
}

// Health is unauthenticated and says only that the process is listening.
func TestHealthz(t *testing.T) {
	h := routes(fakeRegistrar{path: oauthas.MetadataPath}, fakeRegistrar{path: mcpserver.Path}, fakeRegistrar{path: upload.Path})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != "ok" {
		t.Fatalf("body = %q, want ok", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
}

func TestRoutesMountAllThreeServers(t *testing.T) {
	h := routes(fakeRegistrar{path: oauthas.MetadataPath}, fakeRegistrar{path: mcpserver.Path}, fakeRegistrar{path: upload.Path})

	for _, path := range []string{oauthas.MetadataPath, mcpserver.Path, upload.Path} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", path, rec.Code)
		}
	}
}

// MCP_DEV_BEARER exists for the local demo. It authenticates one fixed token
// as one fixed identity and nothing else.
func TestDevBearerAuthenticator(t *testing.T) {
	vars := completeEnv()
	vars["MCP_DEV_BEARER"] = "dev-token"
	cfg, err := loadEnv(lookupFrom(vars))
	if err != nil {
		t.Fatalf("loadEnv: %v", err)
	}

	as := testAuthServer(t, cfg)
	auth := authenticator(cfg, as)

	id, err := auth("dev-token")
	if err != nil {
		t.Fatalf("dev bearer refused: %v", err)
	}
	if id != devIdentity {
		t.Fatalf("identity = %+v, want %+v", id, devIdentity)
	}
	if _, err := auth("some-other-token"); err == nil {
		t.Fatal("an unknown token authenticated")
	}
}

func TestWithoutDevBearerNothingIsAccepted(t *testing.T) {
	cfg, err := loadEnv(lookupFrom(completeEnv()))
	if err != nil {
		t.Fatalf("loadEnv: %v", err)
	}
	auth := authenticator(cfg, testAuthServer(t, cfg))
	if _, err := auth("dev-token"); err == nil {
		t.Fatal("a token authenticated with MCP_DEV_BEARER unset")
	}
}

func testAuthServer(t *testing.T, cfg *env) *oauthas.Server {
	t.Helper()
	sessions, err := sessionStore(cfg)
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	keycloak, err := oauthas.NewKeycloak(t.Context(), oauthas.KeycloakConfig{
		Issuer:      cfg.KeycloakIssuer,
		ClientID:    cfg.KeycloakClientID,
		RedirectURI: cfg.PublicURL + oauthas.CallbackPath,
	})
	if err != nil {
		t.Fatalf("NewKeycloak: %v", err)
	}
	as, err := oauthas.New(oauthas.Config{
		PublicURL:     cfg.PublicURL,
		ResourcePath:  mcpserver.Path,
		Keycloak:      keycloak,
		Sessions:      sessions,
		Tokens:        sessions,
		RequiredRoles: cfg.RequiredRoles,
	})
	if err != nil {
		t.Fatalf("oauthas.New: %v", err)
	}
	return as
}
