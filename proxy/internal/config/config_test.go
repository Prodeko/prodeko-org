package config

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// validEnv is a configuration that must always load cleanly. Tests mutate a
// copy of it, so a failure points at exactly one variable.
func validEnv() map[string]string {
	return map[string]string{
		EnvPublicURL:    "https://cms.prodeko.org",
		EnvCMSOrigins:   "https://prodeko.org,https://www.prodeko.org",
		EnvIssuer:       "https://id.prodeko.org/realms/membership-registry",
		EnvClientID:     "cms-auth-proxy",
		EnvClientSecret: "s3cret",
		EnvEditorRoles:  "membership,prodeko-org-admin",
		EnvGitHubToken:  "github_pat_11ABCDEFG",
		EnvGitHubOwner:  "prodeko",
		EnvGitHubRepo:   "prodeko-org",
		EnvGitHubBranch: "main",
		EnvSessionKey:   "0123456789abcdef0123456789abcdef",
	}
}

func lookupFrom(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

// loadWith applies overrides to validEnv. A nil value deletes the key.
func loadWith(t *testing.T, overrides map[string]*string) (*Config, error) {
	t.Helper()
	env := validEnv()
	for k, v := range overrides {
		if v == nil {
			delete(env, k)
		} else {
			env[k] = *v
		}
	}
	return LoadFrom(lookupFrom(env))
}

func set(v string) *string { return &v }

func errVars(t *testing.T, err error) []string {
	t.Helper()
	var ve VarErrors
	if !errors.As(err, &ve) {
		t.Fatalf("error is %T, want VarErrors: %v", err, err)
	}
	return ve.Vars()
}

func TestLoadValid(t *testing.T) {
	cfg, err := loadWith(t, nil)
	if err != nil {
		t.Fatalf("valid environment rejected: %v", err)
	}

	if cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, DefaultListenAddr)
	}
	if cfg.GitHub.APIRoot != DefaultAPIRoot {
		t.Errorf("APIRoot = %q, want %q", cfg.GitHub.APIRoot, DefaultAPIRoot)
	}
	if cfg.Session.TTL != DefaultSessionTTL {
		t.Errorf("TTL = %v, want %v", cfg.Session.TTL, DefaultSessionTTL)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if got, want := cfg.Keycloak.RedirectURL, "https://cms.prodeko.org/callback"; got != want {
		t.Errorf("RedirectURL = %q, want %q", got, want)
	}
	if got, want := cfg.GitHub.Slug(), "prodeko/prodeko-org"; got != want {
		t.Errorf("Slug() = %q, want %q", got, want)
	}
	if got, want := strings.Join(cfg.Keycloak.Scopes, " "), "openid profile email"; got != want {
		t.Errorf("Scopes = %q, want %q", got, want)
	}
	if cfg.SplitHorizon() {
		t.Error("SplitHorizon() = true with no KEYCLOAK_DISCOVERY_URL set")
	}
	if len(cfg.Warnings) != 0 {
		t.Errorf("unexpected warnings: %v", cfg.Warnings)
	}
}

// Rule: PUBLIC_URL must be a bare origin, because Decap compares
// `event.origin === base_url` and an event origin never has a path.
func TestPublicURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // normalised value; "" means the input must be rejected
	}{
		{"plain https", "https://cms.prodeko.org", "https://cms.prodeko.org"},
		{"with port", "http://localhost:8080", "http://localhost:8080"},
		{"trailing slash trimmed", "https://cms.prodeko.org/", "https://cms.prodeko.org"},
		{"surrounding space trimmed", "  https://cms.prodeko.org  ", "https://cms.prodeko.org"},
		{"trailing newline trimmed", "https://cms.prodeko.org\n", "https://cms.prodeko.org"},
		{"path rejected", "https://prodeko.org/cms", ""},
		{"deep path rejected", "https://prodeko.org/cms/", ""},
		{"query rejected", "https://cms.prodeko.org?a=b", ""},
		{"fragment rejected", "https://cms.prodeko.org#x", ""},
		{"userinfo rejected", "https://user:pw@cms.prodeko.org", ""},
		{"no scheme rejected", "cms.prodeko.org", ""},
		{"scheme-relative rejected", "//cms.prodeko.org", ""},
		{"host:port without scheme rejected", "localhost:8080", ""},
		{"ftp rejected", "ftp://cms.prodeko.org", ""},
		{"empty rejected", "", ""},
		{"no host rejected", "https://", ""},
		{"embedded newline rejected", "https://cms.\nprodeko.org", ""},
		{"header injection rejected", "https://cms.prodeko.org\r\nX-Evil: 1", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadWith(t, map[string]*string{EnvPublicURL: set(tc.in)})
			if tc.want == "" {
				if err == nil {
					t.Fatalf("PUBLIC_URL=%q accepted, want rejected", tc.in)
				}
				if got := errVars(t, err); len(got) != 1 || got[0] != EnvPublicURL {
					t.Fatalf("offending vars = %v, want [%s]", got, EnvPublicURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("PUBLIC_URL=%q rejected: %v", tc.in, err)
			}
			if cfg.PublicURL != tc.want {
				t.Errorf("PublicURL = %q, want %q", cfg.PublicURL, tc.want)
			}
			if cfg.Keycloak.RedirectURL != tc.want+CallbackPath {
				t.Errorf("RedirectURL = %q, want %q", cfg.Keycloak.RedirectURL, tc.want+CallbackPath)
			}
		})
	}
}

func TestPublicURLMissing(t *testing.T) {
	_, err := loadWith(t, map[string]*string{EnvPublicURL: nil})
	if err == nil {
		t.Fatal("missing PUBLIC_URL accepted")
	}
	if got := errVars(t, err); len(got) != 1 || got[0] != EnvPublicURL {
		t.Fatalf("offending vars = %v, want [%s]", got, EnvPublicURL)
	}
}

func TestCMSOrigins(t *testing.T) {
	tests := []struct {
		name string
		in   *string
		want []string // nil means reject
	}{
		{"single", set("https://prodeko.org"), []string{"https://prodeko.org"}},
		{"two", set("https://prodeko.org,https://www.prodeko.org"),
			[]string{"https://prodeko.org", "https://www.prodeko.org"}},
		{"spaces and trailing slashes", set(" https://prodeko.org/ , http://localhost:1313 "),
			[]string{"https://prodeko.org", "http://localhost:1313"}},
		{"duplicates collapsed", set("https://prodeko.org,https://prodeko.org"),
			[]string{"https://prodeko.org"}},
		{"trailing comma tolerated", set("https://prodeko.org,"), []string{"https://prodeko.org"}},
		{"wildcard rejected", set("*"), nil},
		{"wildcard among others rejected", set("https://prodeko.org,*"), nil},
		{"path rejected", set("https://prodeko.org/admin"), nil},
		{"one bad entry rejects all", set("https://prodeko.org,not-a-url"), nil},
		{"empty rejected", set(""), nil},
		{"only commas rejected", set(",,"), nil},
		{"unset rejected", nil, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadWith(t, map[string]*string{EnvCMSOrigins: tc.in})
			if tc.want == nil {
				if err == nil {
					t.Fatal("accepted, want rejected")
				}
				if got := errVars(t, err); len(got) != 1 || got[0] != EnvCMSOrigins {
					t.Fatalf("offending vars = %v, want [%s]", got, EnvCMSOrigins)
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if strings.Join(cfg.CMSOrigins, "|") != strings.Join(tc.want, "|") {
				t.Errorf("CMSOrigins = %v, want %v", cfg.CMSOrigins, tc.want)
			}
		})
	}
}

// Rule 3: repository pinning. These values are substituted into a forwarded
// GitHub path, so anything that can escape a path segment must be rejected.
func TestGitHubOwnerAndRepo(t *testing.T) {
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"simple", "prodeko", true},
		{"with dash", "prodeko-org", true},
		{"with dot", "prodeko.org", true},
		{"with underscore", "prodeko_hack", true},
		{"digits", "2026", true},
		{"slash rejected", "prodeko/evil", false},
		{"dot dot rejected", "..", false},
		{"traversal rejected", "prodeko/../evil", false},
		{"encoded slash rejected", "prodeko%2Fevil", false},
		{"encoded dot dot rejected", "%2e%2e", false},
		{"leading dot rejected", ".prodeko", false},
		{"leading dash rejected", "-prodeko", false},
		{"space rejected", "prodeko evil", false},
		{"newline rejected", "prodeko\nevil", false},
		{"query rejected", "prodeko?x=1", false},
		{"colon rejected", "prodeko:evil", false},
		{"at sign rejected", "prodeko@evil", false},
		{"backslash rejected", "prodeko\\evil", false},
		{"empty rejected", "", false},
	}
	for _, v := range []string{EnvGitHubOwner, EnvGitHubRepo} {
		for _, tc := range tests {
			t.Run(v+"/"+tc.name, func(t *testing.T) {
				_, err := loadWith(t, map[string]*string{v: set(tc.in)})
				if tc.ok && err != nil {
					t.Fatalf("%s=%q rejected: %v", v, tc.in, err)
				}
				if !tc.ok {
					if err == nil {
						t.Fatalf("%s=%q accepted, want rejected", v, tc.in)
					}
					if got := errVars(t, err); len(got) != 1 || got[0] != v {
						t.Fatalf("offending vars = %v, want [%s]", got, v)
					}
				}
			})
		}
	}
}

func TestGitHubBranch(t *testing.T) {
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"main", "main", true},
		{"slashed", "release/2026", true},
		{"dotted", "v1.2.3", true},
		{"space rejected", "ma in", false},
		{"tab rejected", "ma\tin", false},
		{"dot dot rejected", "a..b", false},
		{"leading dash rejected", "-main", false},
		{"caret rejected", "main^", false},
		{"tilde rejected", "main~1", false},
		{"colon rejected", "main:evil", false},
		{"glob rejected", "main*", false},
		{"backslash rejected", "main\\evil", false},
		{"control char rejected", "main\x01", false},
		{"empty rejected", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWith(t, map[string]*string{EnvGitHubBranch: set(tc.in)})
			if tc.ok && err != nil {
				t.Fatalf("branch %q rejected: %v", tc.in, err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatalf("branch %q accepted, want rejected", tc.in)
				}
				if got := errVars(t, err); len(got) != 1 || got[0] != EnvGitHubBranch {
					t.Fatalf("offending vars = %v, want [%s]", got, EnvGitHubBranch)
				}
			}
		})
	}
}

func TestIssuer(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string // "" means reject
	}{
		{"canonical", "https://id.prodeko.org/realms/membership-registry",
			"https://id.prodeko.org/realms/membership-registry"},
		{"trailing slash trimmed", "https://id.prodeko.org/realms/membership-registry/",
			"https://id.prodeko.org/realms/membership-registry"},
		{"local http", "http://localhost:8180/realms/membership-registry",
			"http://localhost:8180/realms/membership-registry"},
		{"no realms segment rejected", "https://id.prodeko.org", ""},
		{"wrong path rejected", "https://id.prodeko.org/auth/membership-registry", ""},
		{"query rejected", "https://id.prodeko.org/realms/m?x=1", ""},
		{"fragment rejected", "https://id.prodeko.org/realms/m#x", ""},
		{"not a url rejected", "id.prodeko.org/realms/m", ""},
		{"empty rejected", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadWith(t, map[string]*string{EnvIssuer: set(tc.in)})
			if tc.want == "" {
				if err == nil {
					t.Fatalf("issuer %q accepted, want rejected", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("issuer %q rejected: %v", tc.in, err)
			}
			if cfg.Keycloak.Issuer != tc.want {
				t.Errorf("Issuer = %q, want %q", cfg.Keycloak.Issuer, tc.want)
			}
			if cfg.Keycloak.DiscoveryURL != tc.want {
				t.Errorf("DiscoveryURL = %q, want it to default to the issuer", cfg.Keycloak.DiscoveryURL)
			}
		})
	}
}

func TestSplitHorizon(t *testing.T) {
	cfg, err := loadWith(t, map[string]*string{
		EnvIssuer:       set("http://localhost:8180/realms/membership-registry"),
		EnvDiscoveryURL: set("http://keycloak:8180/realms/membership-registry/"),
	})
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if !cfg.SplitHorizon() {
		t.Error("SplitHorizon() = false, want true")
	}
	if got, want := cfg.Keycloak.DiscoveryURL, "http://keycloak:8180/realms/membership-registry"; got != want {
		t.Errorf("DiscoveryURL = %q, want %q", got, want)
	}
	if got, want := cfg.Keycloak.Issuer, "http://localhost:8180/realms/membership-registry"; got != want {
		t.Errorf("Issuer = %q, want %q", got, want)
	}
}

func TestSessionSecret(t *testing.T) {
	const dev = "dev-session-secret-not-for-production-use-0000000"
	tests := []struct {
		name      string
		secret    *string
		publicURL string
		ok        bool
		warns     bool
	}{
		{"32 bytes", set(strings.Repeat("a", 32)), "https://cms.prodeko.org", true, false},
		{"long", set(strings.Repeat("a", 64)), "https://cms.prodeko.org", true, false},
		{"31 bytes rejected", set(strings.Repeat("a", 31)), "https://cms.prodeko.org", false, false},
		{"empty rejected", set(""), "https://cms.prodeko.org", false, false},
		{"unset rejected", nil, "https://cms.prodeko.org", false, false},
		{"dev placeholder on public url rejected", set(dev), "https://cms.prodeko.org", false, false},
		{"dev placeholder on localhost warns", set(dev), "http://localhost:8080", true, true},
		{"dev placeholder on 127.0.0.1 warns", set(dev), "http://127.0.0.1:8080", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadWith(t, map[string]*string{
				EnvSessionKey: tc.secret,
				EnvPublicURL:  set(tc.publicURL),
			})
			if !tc.ok {
				if err == nil {
					t.Fatal("accepted, want rejected")
				}
				if got := errVars(t, err); len(got) != 1 || got[0] != EnvSessionKey {
					t.Fatalf("offending vars = %v, want [%s]", got, EnvSessionKey)
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if got := len(cfg.Session.Secret); got < MinSessionSecret {
				t.Errorf("Secret length = %d, want >= %d", got, MinSessionSecret)
			}
			if tc.warns != (len(cfg.Warnings) > 0) {
				t.Errorf("warnings = %v, want any = %v", cfg.Warnings, tc.warns)
			}
		})
	}
}

// Whitespace is key material. Trimming it would silently change the key
// between a .env file and a secret manager.
func TestSessionSecretNotTrimmed(t *testing.T) {
	raw := " " + strings.Repeat("a", 32) + " "
	cfg, err := loadWith(t, map[string]*string{EnvSessionKey: set(raw)})
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if string(cfg.Session.Secret) != raw {
		t.Errorf("Secret = %q, want the raw value %q", cfg.Session.Secret, raw)
	}
}

func TestSessionTTL(t *testing.T) {
	tests := []struct {
		name string
		in   *string
		want time.Duration // 0 means reject
	}{
		{"unset defaults", nil, DefaultSessionTTL},
		{"hours", set("12h"), 12 * time.Hour},
		{"minutes", set("90m"), 90 * time.Minute},
		{"bare number rejected", set("8"), 0},
		{"too short rejected", set("30s"), 0},
		{"negative rejected", set("-1h"), 0},
		{"garbage rejected", set("soon"), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadWith(t, map[string]*string{EnvSessionTTL: tc.in})
			if tc.want == 0 {
				if err == nil {
					t.Fatal("accepted, want rejected")
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if cfg.Session.TTL != tc.want {
				t.Errorf("TTL = %v, want %v", cfg.Session.TTL, tc.want)
			}
		})
	}
}

func TestScopes(t *testing.T) {
	tests := []struct {
		name string
		in   *string
		want string // "" means reject
	}{
		{"unset defaults", nil, "openid profile email"},
		{"space separated", set("openid email"), "openid email"},
		{"comma separated", set("openid,email"), "openid email"},
		{"duplicates collapsed", set("openid openid email"), "openid email"},
		{"missing openid rejected", set("profile email"), ""},
		{"github repo scope rejected", set("openid repo"), ""},
		{"empty falls back to the default", set(""), "openid profile email"},
		{"only separators rejected", set(", ,"), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadWith(t, map[string]*string{EnvOAuthScope: tc.in})
			if tc.want == "" {
				if err == nil {
					t.Fatal("accepted, want rejected")
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if got := strings.Join(cfg.Keycloak.Scopes, " "); got != tc.want {
				t.Errorf("Scopes = %q, want %q", got, tc.want)
			}
		})
	}
}

// EDITOR_ROLES names every role an editor must hold, and there has to be at
// least one. A proxy that starts with none would let every Prodeko account
// edit the website, so each of these has to stop it starting.
func TestEditorRoles(t *testing.T) {
	tests := []struct {
		name string
		in   *string
		want []string // nil means the value must be rejected
	}{
		{"the production pair", set("membership,prodeko-org-admin"), []string{"membership", "prodeko-org-admin"}},
		{"a single role", set("prodeko-org-admin"), []string{"prodeko-org-admin"}},
		{"spaces around the entries", set(" membership , prodeko-org-admin "), []string{"membership", "prodeko-org-admin"}},
		{"order is kept and duplicates dropped", set("b,a,b"), []string{"b", "a"}},
		{"trailing comma", set("membership,"), []string{"membership"}},
		{"unset", nil, nil},
		{"empty", set(""), nil},
		{"whitespace only", set("   "), nil},
		{"commas and spaces only", set(" , , "), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadWith(t, map[string]*string{EnvEditorRoles: tc.in})
			if tc.want == nil {
				if err == nil {
					t.Fatal("accepted, want rejected: nobody may edit without a required role")
				}
				if got := errVars(t, err); len(got) != 1 || got[0] != EnvEditorRoles {
					t.Fatalf("offending vars = %v, want [%s]", got, EnvEditorRoles)
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if got := strings.Join(cfg.Keycloak.EditorRoles, ","); got != strings.Join(tc.want, ",") {
				t.Errorf("EditorRoles = %v, want %v", cfg.Keycloak.EditorRoles, tc.want)
			}
		})
	}
}

func TestRequiredStrings(t *testing.T) {
	for _, name := range []string{EnvClientID, EnvClientSecret, EnvGitHubToken} {
		for _, val := range []*string{nil, set(""), set("   ")} {
			_, err := loadWith(t, map[string]*string{name: val})
			if err == nil {
				t.Errorf("%s=%v accepted, want rejected", name, val)
				continue
			}
			if got := errVars(t, err); len(got) != 1 || got[0] != name {
				t.Errorf("%s: offending vars = %v, want [%s]", name, got, name)
			}
		}
	}
}

func TestGitHubTokenPrefixWarns(t *testing.T) {
	tests := []struct {
		token string
		warns bool
	}{
		{"github_pat_11ABCDEFG", false},
		{"ghp_abcdefghijklmnop", false},
		{"some-other-token", true},
	}
	for _, tc := range tests {
		cfg, err := loadWith(t, map[string]*string{EnvGitHubToken: set(tc.token)})
		if err != nil {
			t.Fatalf("token %q rejected: %v", tc.token, err)
		}
		if got := len(cfg.Warnings) > 0; got != tc.warns {
			t.Errorf("token %q: warnings %v, want any = %v", tc.token, cfg.Warnings, tc.warns)
		}
	}
}

func TestAPIRoot(t *testing.T) {
	tests := []struct {
		name string
		in   *string
		want string // "" means reject
	}{
		{"unset defaults", nil, DefaultAPIRoot},
		{"github", set("https://api.github.com"), "https://api.github.com"},
		{"trailing slash trimmed", set("https://api.github.com/"), "https://api.github.com"},
		{"enterprise path kept", set("https://ghe.example.com/api/v3"), "https://ghe.example.com/api/v3"},
		{"enterprise trailing slash trimmed", set("https://ghe.example.com/api/v3/"), "https://ghe.example.com/api/v3"},
		{"query rejected", set("https://api.github.com?x=1"), ""},
		{"no scheme rejected", set("api.github.com"), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadWith(t, map[string]*string{EnvGitHubAPI: tc.in})
			if tc.want == "" {
				if err == nil {
					t.Fatal("accepted, want rejected")
				}
				return
			}
			if err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if cfg.GitHub.APIRoot != tc.want {
				t.Errorf("APIRoot = %q, want %q", cfg.GitHub.APIRoot, tc.want)
			}
		})
	}
}

func TestLogLevel(t *testing.T) {
	tests := []struct {
		in   *string
		want slog.Level
		ok   bool
	}{
		{nil, slog.LevelInfo, true},
		{set("debug"), slog.LevelDebug, true},
		{set("DEBUG"), slog.LevelDebug, true},
		{set("info"), slog.LevelInfo, true},
		{set("warn"), slog.LevelWarn, true},
		{set("warning"), slog.LevelWarn, true},
		{set("error"), slog.LevelError, true},
		{set("trace"), 0, false},
	}
	for _, tc := range tests {
		cfg, err := loadWith(t, map[string]*string{EnvLogLevel: tc.in})
		if !tc.ok {
			if err == nil {
				t.Errorf("LOG_LEVEL=%v accepted, want rejected", *tc.in)
			}
			continue
		}
		if err != nil {
			t.Fatalf("rejected: %v", err)
		}
		if cfg.LogLevel != tc.want {
			t.Errorf("LogLevel = %v, want %v", cfg.LogLevel, tc.want)
		}
	}
}

func TestListenAddr(t *testing.T) {
	cfg, err := loadWith(t, map[string]*string{EnvListenAddr: set("0.0.0.0:9000")})
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	if cfg.ListenAddr != "0.0.0.0:9000" {
		t.Errorf("ListenAddr = %q", cfg.ListenAddr)
	}
	if _, err := loadWith(t, map[string]*string{EnvListenAddr: set("9000")}); err == nil {
		t.Error("LISTEN_ADDR=9000 accepted, want rejected")
	}
}

// A misconfigured deploy should be fixable in one round trip, not N.
func TestAllProblemsReportedAtOnce(t *testing.T) {
	_, err := LoadFrom(lookupFrom(map[string]string{}))
	if err == nil {
		t.Fatal("empty environment accepted")
	}
	got := errVars(t, err)
	want := []string{
		EnvCMSOrigins, EnvEditorRoles, EnvGitHubBranch, EnvGitHubOwner,
		EnvGitHubRepo, EnvGitHubToken, EnvClientID, EnvClientSecret,
		EnvIssuer, EnvPublicURL, EnvSessionKey,
	}
	missing := map[string]bool{}
	for _, w := range want {
		missing[w] = true
	}
	for _, g := range got {
		delete(missing, g)
	}
	if len(missing) > 0 {
		t.Errorf("no error reported for %v; got %v", missing, got)
	}
	if !strings.Contains(err.Error(), "problems") {
		t.Errorf("aggregate error does not read as a list: %v", err)
	}
}

func TestNoConfigReturnedOnError(t *testing.T) {
	cfg, err := LoadFrom(lookupFrom(map[string]string{}))
	if err == nil {
		t.Fatal("empty environment accepted")
	}
	if cfg != nil {
		t.Fatalf("Load returned a Config alongside an error: %v", cfg)
	}
}

// A Config is logged at startup, so String must never leak a secret.
func TestStringRedactsSecrets(t *testing.T) {
	const (
		clientSecret = "super-secret-client-value"
		token        = "github_pat_supersecrettoken"
		session      = "0123456789abcdef0123456789abcdef"
	)
	cfg, err := loadWith(t, map[string]*string{
		EnvClientSecret: set(clientSecret),
		EnvGitHubToken:  set(token),
		EnvSessionKey:   set(session),
	})
	if err != nil {
		t.Fatalf("rejected: %v", err)
	}
	s := cfg.String()
	for _, secret := range []string{clientSecret, token, session} {
		if strings.Contains(s, secret) {
			t.Errorf("String() leaks %q:\n%s", secret, s)
		}
	}
	for _, want := range []string{"prodeko/prodeko-org", "https://cms.prodeko.org", redacted} {
		if !strings.Contains(s, want) {
			t.Errorf("String() missing %q:\n%s", want, s)
		}
	}
}

// Error messages are read by whoever is fixing the deploy, and are often
// pasted into a chat. A secret must not travel with them.
func TestErrorsDoNotLeakSecrets(t *testing.T) {
	const secret = "super-secret-client-value"
	_, err := LoadFrom(lookupFrom(map[string]string{
		EnvClientSecret: secret,
		EnvSessionKey:   "short",
		EnvGitHubToken:  "not-a-github-token-but-still-a-secret",
	}))
	if err == nil {
		t.Fatal("accepted, want rejected")
	}
	for _, s := range []string{secret, "short", "not-a-github-token-but-still-a-secret"} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("error leaks %q: %v", s, err)
		}
	}
}

func TestVarErrorsSingular(t *testing.T) {
	e := VarErrors{{Var: "X", Reason: "is broken"}}
	if got, want := e.Error(), "invalid configuration: X: is broken"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// The committer describes the bot, not the editor, so it defaults rather than
// failing. What it must never be is half an identity: forward refuses to build
// a commit from one, and by then the editor has already pressed save.
func TestCommitterDefaultsAndValidation(t *testing.T) {
	cfg, err := loadWith(t, nil)
	if err != nil {
		t.Fatalf("valid environment rejected: %v", err)
	}
	if cfg.GitHub.CommitterName != DefaultCommitterName || cfg.GitHub.CommitterEmail != DefaultCommitterEmail {
		t.Errorf("committer = %q <%s>, want the defaults",
			cfg.GitHub.CommitterName, cfg.GitHub.CommitterEmail)
	}

	cfg, err = loadWith(t, map[string]*string{
		EnvCommitterName:  set("Prodeko Bot"),
		EnvCommitterEmail: set("bot@prodeko.org"),
	})
	if err != nil {
		t.Fatalf("explicit committer rejected: %v", err)
	}
	if cfg.GitHub.CommitterName != "Prodeko Bot" || cfg.GitHub.CommitterEmail != "bot@prodeko.org" {
		t.Errorf("committer = %q <%s>, want the configured values",
			cfg.GitHub.CommitterName, cfg.GitHub.CommitterEmail)
	}

	for _, bad := range []string{"not-an-address", "Bot <bot@prodeko.org>", "a@b@c", "bot @prodeko.org"} {
		if _, err := loadWith(t, map[string]*string{EnvCommitterEmail: set(bad)}); err == nil {
			t.Errorf("GITHUB_COMMITTER_EMAIL=%q accepted, want rejected", bad)
		}
	}

	// Set-but-empty takes the default, the same as every other optional
	// variable in here.
	cfg, err = loadWith(t, map[string]*string{EnvCommitterEmail: set("")})
	if err != nil || cfg.GitHub.CommitterEmail != DefaultCommitterEmail {
		t.Errorf("empty GITHUB_COMMITTER_EMAIL = %q, %v; want the default", cfg.GitHub.CommitterEmail, err)
	}
}
