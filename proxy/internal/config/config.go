// Package config loads and validates the proxy's environment configuration.
//
// Load reports every problem it finds, not just the first, and returns no
// usable Config when anything is wrong. A half-configured auth proxy must
// refuse to start.
//
// Load touches no network. Reaching Keycloak's discovery document belongs to
// the caller, which keeps this package a pure function of its environment.
package config

import (
	"crypto/rsa"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/prodeko/prodeko-org/proxy/internal/ghauth"
)

// Environment variable names, in one place so error messages and the
// documentation cannot drift apart.
const (
	EnvListenAddr   = "LISTEN_ADDR"
	EnvPublicURL    = "PUBLIC_URL"
	EnvCMSOrigins   = "CMS_ORIGINS"
	EnvLogLevel     = "LOG_LEVEL"
	EnvIssuer       = "KEYCLOAK_ISSUER"
	EnvDiscoveryURL = "KEYCLOAK_DISCOVERY_URL"
	EnvClientID     = "KEYCLOAK_CLIENT_ID"
	EnvClientSecret = "KEYCLOAK_CLIENT_SECRET"
	EnvEditorRoles  = "EDITOR_ROLES"
	EnvOAuthScope   = "OAUTH_SCOPE"
	EnvGitHubToken  = "GITHUB_TOKEN"
	EnvGitHubAppID  = "GITHUB_APP_ID"
	EnvGitHubAppKey = "GITHUB_APP_PRIVATE_KEY"
	EnvGitHubOwner  = "GITHUB_OWNER"
	EnvGitHubRepo   = "GITHUB_REPO"
	EnvGitHubBranch = "GITHUB_BRANCH"
	EnvGitHubAPI    = "GITHUB_API_ROOT"

	// The bot identity recorded as the git *committer*. The author is always
	// the signed-in editor and is never configurable; this is the machine that
	// carried the commit, which git records separately.
	EnvCommitterName  = "GITHUB_COMMITTER_NAME"
	EnvCommitterEmail = "GITHUB_COMMITTER_EMAIL"

	EnvSessionKey = "SESSION_SECRET"
	EnvSessionTTL = "SESSION_TTL"
)

// Defaults applied when a variable is absent.
const (
	DefaultListenAddr = ":8080"
	DefaultAPIRoot    = "https://api.github.com"
	DefaultSessionTTL = 8 * time.Hour

	// DefaultCommitterName and DefaultCommitterEmail label the commits this
	// proxy carries when nothing better is configured. They affect the
	// committer only; the author is the editor's verified Keycloak identity.
	DefaultCommitterName  = "Prodeko CMS"
	DefaultCommitterEmail = "cms@prodeko.org"

	// CallbackPath is appended to PublicURL to form the OAuth redirect URI.
	// It must be registered verbatim on the Keycloak client.
	CallbackPath = "/callback"

	// MinSessionSecret is the shortest SESSION_SECRET we accept, in bytes.
	// 32 bytes is one HMAC-SHA256 block's worth of key material.
	MinSessionSecret = 32

	// MinSessionTTL guards against a typo such as SESSION_TTL=8 (8ns).
	MinSessionTTL = time.Minute
)

// DefaultScopes is the OIDC scope set requested when OAUTH_SCOPE is unset.
// Note that Decap appends its own `scope=repo` to the popup URL; that is a
// GitHub scope, Keycloak would reject it, and the proxy must ignore it in
// favour of these.
var DefaultScopes = []string{"openid", "profile", "email"}

// devSecrets are the placeholder SESSION_SECRET values shipped in
// .env.example and compose.yaml. They are rejected outright on any non-local
// PUBLIC_URL, because a dev secret copy-pasted into production is a silent
// session-forgery hole. This is a tripwire, not a guarantee: any other weak
// secret still passes.
var devSecrets = map[string]bool{
	"dev-session-secret-not-for-production-use-0000000": true,
	"change-me-change-me-change-me-change-me":           true,
	"insecure-dev-secret-insecure-dev-secret":           true,
}

// nameRe matches a single GitHub owner or repository path segment. No slash,
// no dot-dot, no percent-encoding, no whitespace. This is where repository
// pinning is actually enforced: these values are substituted into a forwarded
// URL path, so anything that could escape a path segment must be rejected here.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Config is the validated runtime configuration. Every field is populated and
// syntactically checked by the time Load returns a nil error.
type Config struct {
	ListenAddr string   // LISTEN_ADDR, default ":8080"
	PublicURL  string   // PUBLIC_URL, a bare origin: scheme://host[:port], no path, no trailing slash
	CMSOrigins []string // CMS_ORIGINS, comma-separated bare origins allowed to call /github/*
	LogLevel   slog.Level

	Keycloak Keycloak
	GitHub   GitHub
	Session  Session

	// Warnings are non-fatal observations worth logging at startup, such as a
	// GITHUB_TOKEN that does not look like a GitHub token.
	Warnings []string
}

// Keycloak holds everything needed to run the OIDC code flow and check the
// editor role.
type Keycloak struct {
	// Issuer is the canonical `iss` value we require on every token.
	// Trailing slash trimmed. From KEYCLOAK_ISSUER.
	Issuer string

	// DiscoveryURL is the base URL used for the back-channel .well-known
	// fetch. Equals Issuer unless KEYCLOAK_DISCOVERY_URL is set, which is
	// needed only when the browser and the proxy reach Keycloak at different
	// addresses (the docker-compose dev stack). Drives
	// oidc.InsecureIssuerURLContext.
	DiscoveryURL string

	ClientID     string // KEYCLOAK_CLIENT_ID
	ClientSecret string // KEYCLOAK_CLIENT_SECRET

	// EditorRoles are the realm roles an editor must hold, from EDITOR_ROLES.
	// Every one of them is required, never any of them, and there is always at
	// least one.
	EditorRoles []string

	Scopes []string // OAUTH_SCOPE, default {"openid", "profile", "email"}

	// RedirectURL is derived: PublicURL + "/callback". It must be registered
	// verbatim on the Keycloak client.
	RedirectURL string
}

// GitHub pins the one repository this proxy may write to, and holds the
// credential for it: either Token, or AppID and AppKey. Exactly one is set.
type GitHub struct {
	Token   string          // GITHUB_TOKEN
	AppID   string          // GITHUB_APP_ID
	AppKey  *rsa.PrivateKey // GITHUB_APP_PRIVATE_KEY
	Owner   string          // GITHUB_OWNER, single path segment
	Repo    string          // GITHUB_REPO, single path segment
	Branch  string          // GITHUB_BRANCH
	APIRoot string          // GITHUB_API_ROOT, default "https://api.github.com", no trailing slash

	// CommitterName and CommitterEmail are the git committer written onto
	// every commit. The author is the editor and comes from Keycloak.
	CommitterName  string // GITHUB_COMMITTER_NAME
	CommitterEmail string // GITHUB_COMMITTER_EMAIL
}

// UsesApp reports whether the proxy authenticates as a GitHub App rather than
// with a personal access token.
func (g GitHub) UsesApp() bool { return g.AppKey != nil }

// Slug returns "owner/repo" for substitution into forwarded GitHub paths.
func (g GitHub) Slug() string { return g.Owner + "/" + g.Repo }

// Session configures the proxy's own session token, the only token the
// browser ever sees.
type Session struct {
	Secret []byte        // SESSION_SECRET, at least 32 bytes
	TTL    time.Duration // SESSION_TTL, default 8h
}

// SplitHorizon reports whether discovery and issuer validation use different
// URLs. True only in the dev stack; callers log it loudly.
func (c *Config) SplitHorizon() bool {
	return c.Keycloak.DiscoveryURL != c.Keycloak.Issuer
}

// String renders the configuration with every secret replaced by "[redacted]",
// so a Config can be logged at startup without leaking credentials.
func (c *Config) String() string {
	var b strings.Builder
	b.WriteString("config:\n")
	line := func(k, v string) {
		fmt.Fprintf(&b, "  %s=%s\n", k, v)
	}
	line("listen_addr", c.ListenAddr)
	line("public_url", c.PublicURL)
	line("cms_origins", strings.Join(c.CMSOrigins, ","))
	line("log_level", c.LogLevel.String())
	line("keycloak_issuer", c.Keycloak.Issuer)
	line("keycloak_discovery_url", c.Keycloak.DiscoveryURL)
	line("keycloak_split_horizon", fmt.Sprint(c.SplitHorizon()))
	line("keycloak_client_id", c.Keycloak.ClientID)
	line("keycloak_client_secret", redacted)
	line("editor_roles", strings.Join(c.Keycloak.EditorRoles, ","))
	line("oauth_scope", strings.Join(c.Keycloak.Scopes, " "))
	line("redirect_url", c.Keycloak.RedirectURL)
	line("github_slug", c.GitHub.Slug())
	line("github_branch", c.GitHub.Branch)
	line("github_api_root", c.GitHub.APIRoot)
	line("github_committer", c.GitHub.CommitterName+" <"+c.GitHub.CommitterEmail+">")
	if c.GitHub.UsesApp() {
		line("github_app_id", c.GitHub.AppID)
		line("github_app_private_key", redacted)
	} else {
		line("github_token", redacted)
	}
	line("session_secret", redacted)
	line("session_ttl", c.Session.TTL.String())
	return strings.TrimRight(b.String(), "\n")
}

const redacted = "[redacted]"

// VarError is one bad or missing variable. Value is never included for
// secrets.
type VarError struct {
	Var    string
	Reason string
}

func (e VarError) Error() string { return e.Var + ": " + e.Reason }

// VarErrors aggregates every problem found in one pass so a misconfigured
// deploy is fixed in one round trip rather than N.
type VarErrors []VarError

func (e VarErrors) Error() string {
	if len(e) == 1 {
		return "invalid configuration: " + e[0].Error()
	}
	parts := make([]string, 0, len(e))
	for _, v := range e {
		parts = append(parts, "  - "+v.Error())
	}
	return fmt.Sprintf("invalid configuration, %d problems:\n%s", len(e), strings.Join(parts, "\n"))
}

// Vars returns the names of the offending variables, sorted and deduplicated.
func (e VarErrors) Vars() []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range e {
		if !seen[v.Var] {
			seen[v.Var] = true
			out = append(out, v.Var)
		}
	}
	sort.Strings(out)
	return out
}

// Load reads os.LookupEnv.
func Load() (*Config, error) { return LoadFrom(os.LookupEnv) }

// LoadFrom reads from an arbitrary lookup, for tests.
func LoadFrom(lookup func(string) (string, bool)) (*Config, error) {
	l := &loader{lookup: lookup}
	cfg := &Config{}

	cfg.ListenAddr = l.optional(EnvListenAddr, DefaultListenAddr)
	if !strings.Contains(cfg.ListenAddr, ":") {
		l.fail(EnvListenAddr, fmt.Sprintf("must be a host:port or :port address, got %q", cfg.ListenAddr))
	}

	cfg.PublicURL = l.origin(EnvPublicURL, true)
	cfg.CMSOrigins = l.originList(EnvCMSOrigins)
	cfg.LogLevel = l.logLevel()

	cfg.Keycloak = Keycloak{
		Issuer:       l.issuer(EnvIssuer, "", true),
		ClientID:     l.required(EnvClientID),
		ClientSecret: l.required(EnvClientSecret),
		EditorRoles:  l.editorRoles(),
		Scopes:       l.scopes(),
	}
	cfg.Keycloak.DiscoveryURL = l.issuer(EnvDiscoveryURL, cfg.Keycloak.Issuer, false)
	if cfg.PublicURL != "" {
		cfg.Keycloak.RedirectURL = cfg.PublicURL + CallbackPath
	}

	cfg.GitHub = GitHub{
		Owner:          l.segment(EnvGitHubOwner),
		Repo:           l.segment(EnvGitHubRepo),
		Branch:         l.branch(),
		APIRoot:        l.apiRoot(),
		CommitterName:  l.committerName(),
		CommitterEmail: l.committerEmail(),
	}
	cfg.GitHub.Token, cfg.GitHub.AppID, cfg.GitHub.AppKey = l.githubCredential()

	cfg.Session = Session{
		Secret: l.sessionSecret(cfg.PublicURL),
		TTL:    l.ttl(),
	}

	cfg.Warnings = l.warnings
	if len(l.errs) > 0 {
		return nil, l.errs
	}
	return cfg, nil
}

// loader accumulates problems instead of returning on the first one.
type loader struct {
	lookup   func(string) (string, bool)
	errs     VarErrors
	warnings []string
}

func (l *loader) fail(name, reason string) {
	l.errs = append(l.errs, VarError{Var: name, Reason: reason})
}

func (l *loader) warn(format string, args ...any) {
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}

// get returns the trimmed value and whether it was set to anything non-empty.
func (l *loader) get(name string) (string, bool) {
	v, ok := l.lookup(name)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	return v, v != ""
}

func (l *loader) optional(name, def string) string {
	if v, ok := l.get(name); ok {
		return v
	}
	return def
}

func (l *loader) required(name string) string {
	v, ok := l.get(name)
	if !ok {
		l.fail(name, "is required but not set")
		return ""
	}
	return v
}

// editorRoles reads the comma-separated realm roles an editor must hold. All
// of them are required, so an editor who has lost any one of them is refused;
// that is what makes the hand-granted permission to edit lapse together with
// the automatically maintained membership. At least one role has to be
// configured, because an empty set would admit every Prodeko account.
func (l *loader) editorRoles() []string {
	raw, ok := l.get(EnvEditorRoles)
	if !ok {
		l.fail(EnvEditorRoles, "is required but not set; list every realm role an editor must hold, "+
			"comma-separated, e.g. membership,prodeko-org-admin. All of them are required, and without "+
			"any of them every Prodeko account could edit the website")
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" || seen[part] {
			continue
		}
		seen[part] = true
		out = append(out, part)
	}
	if len(out) == 0 {
		l.fail(EnvEditorRoles, fmt.Sprintf("is set but names no role (got %q); "+
			"at least one realm role has to be required to edit", raw))
		return nil
	}
	return out
}

// origin validates a bare origin: scheme://host[:port], nothing else.
//
// This is load-bearing for PUBLIC_URL. Decap's popup handshake compares
// `event.origin === base_url`, and event.origin is always scheme://host[:port]
// with no path. A PUBLIC_URL carrying a path or a trailing slash makes every
// login hang silently: the popup opens, closes, and the CMS never reacts.
func (l *loader) origin(name string, required bool) string {
	raw, ok := l.get(name)
	if !ok {
		if required {
			l.fail(name, "is required but not set; it must be a bare origin such as https://cms.prodeko.org")
		}
		return ""
	}
	o, err := parseOrigin(raw)
	if err != nil {
		l.fail(name, fmt.Sprintf("%v (got %q). Decap compares the browser event origin against this value "+
			"exactly, and an event origin is always scheme://host[:port]; a mismatch makes login hang silently", err, raw))
		return ""
	}
	return o
}

func (l *loader) originList(name string) []string {
	raw, ok := l.get(name)
	if !ok {
		l.fail(name, "is required but not set; list every origin the site is served from, "+
			"comma-separated, e.g. https://prodeko.org,https://www.prodeko.org. "+
			"Decap's browser fetches to api_root are cross-origin, so without this every GitHub call is blocked by CORS")
		return nil
	}
	var out []string
	seen := map[string]bool{}
	before := len(l.errs)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if part == "*" {
			l.fail(name, "must not be \"*\"; the proxy sends credentials on cross-origin requests, "+
				"so the allowed origins have to be listed explicitly")
			continue
		}
		o, err := parseOrigin(part)
		if err != nil {
			l.fail(name, fmt.Sprintf("entry %q is not a bare origin: %v", part, err))
			continue
		}
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	if len(out) == 0 && len(l.errs) == before {
		l.fail(name, "is set but contains no usable origin")
	}
	return out
}

// issuer validates a Keycloak realm URL. When def is non-empty it is used if
// the variable is unset, and the /realms/ requirement still applies.
func (l *loader) issuer(name, def string, required bool) string {
	raw, ok := l.get(name)
	if !ok {
		if required {
			l.fail(name, "is required but not set; it looks like https://id.prodeko.org/realms/membership-registry")
		}
		return def
	}
	u, err := parseAbsURL(raw)
	if err != nil {
		l.fail(name, fmt.Sprintf("%v (got %q)", err, raw))
		return def
	}
	if u.RawQuery != "" || u.Fragment != "" {
		l.fail(name, fmt.Sprintf("must not have a query or fragment (got %q)", raw))
		return def
	}
	trimmed := strings.TrimRight(u.String(), "/")
	if !strings.Contains(trimmed, "/realms/") {
		l.fail(name, fmt.Sprintf("must be a realm URL containing /realms/ (got %q); "+
			"this value has to equal the iss claim Keycloak puts in its tokens, character for character", raw))
		return def
	}
	return trimmed
}

func (l *loader) logLevel() slog.Level {
	raw, ok := l.get(EnvLogLevel)
	if !ok {
		return slog.LevelInfo
	}
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		l.fail(EnvLogLevel, fmt.Sprintf("must be one of debug, info, warn, error (got %q)", raw))
		return slog.LevelInfo
	}
}

func (l *loader) scopes() []string {
	raw, ok := l.get(EnvOAuthScope)
	if !ok {
		return append([]string(nil), DefaultScopes...)
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t' || r == '\n'
	})
	var out []string
	seen := map[string]bool{}
	for _, f := range fields {
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		l.fail(EnvOAuthScope, "is set but empty; unset it to take the default \"openid profile email\"")
		return nil
	}
	if !seen["openid"] {
		l.fail(EnvOAuthScope, fmt.Sprintf("must include the openid scope (got %q), otherwise Keycloak "+
			"returns no ID token and the proxy cannot identify the editor", raw))
	}
	if seen["repo"] {
		l.fail(EnvOAuthScope, "must not include \"repo\"; that is a GitHub scope. Decap appends it to the "+
			"popup URL and the proxy ignores it, but Keycloak would reject it")
	}
	return out
}

func (l *loader) segment(name string) string {
	v, ok := l.get(name)
	if !ok {
		l.fail(name, "is required but not set")
		return ""
	}
	if !nameRe.MatchString(v) {
		l.fail(name, fmt.Sprintf("must be a single path segment matching %s (got %q); "+
			"this value is substituted into the forwarded GitHub path, which is how the repository is pinned", nameRe, v))
		return ""
	}
	if strings.Contains(v, "..") {
		l.fail(name, fmt.Sprintf("must not contain \"..\" (got %q)", v))
		return ""
	}
	return v
}

func (l *loader) branch() string {
	v, ok := l.get(EnvGitHubBranch)
	if !ok {
		l.fail(EnvGitHubBranch, "is required but not set")
		return ""
	}
	switch {
	case strings.ContainsAny(v, " \t\n\r\v\f"):
		l.fail(EnvGitHubBranch, fmt.Sprintf("must not contain whitespace (got %q)", v))
	case strings.Contains(v, ".."):
		l.fail(EnvGitHubBranch, fmt.Sprintf("must not contain \"..\" (got %q)", v))
	case strings.HasPrefix(v, "-"):
		l.fail(EnvGitHubBranch, fmt.Sprintf("must not start with \"-\" (got %q)", v))
	case strings.ContainsAny(v, "~^:?*[\\"):
		l.fail(EnvGitHubBranch, fmt.Sprintf("contains a character git forbids in a ref name (got %q)", v))
	case hasControl(v):
		l.fail(EnvGitHubBranch, "must not contain control characters")
	}
	return v
}

func (l *loader) apiRoot() string {
	raw, ok := l.get(EnvGitHubAPI)
	if !ok {
		return DefaultAPIRoot
	}
	u, err := parseAbsURL(raw)
	if err != nil {
		l.fail(EnvGitHubAPI, fmt.Sprintf("%v (got %q)", err, raw))
		return DefaultAPIRoot
	}
	if u.RawQuery != "" || u.Fragment != "" {
		l.fail(EnvGitHubAPI, fmt.Sprintf("must not have a query or fragment (got %q)", raw))
		return DefaultAPIRoot
	}
	return strings.TrimRight(u.String(), "/")
}

// committerName and committerEmail describe the bot, not the editor, so both
// default rather than failing. An empty value is still refused: forward.New
// requires both halves of a git identity, and a commit with half an identity
// is rejected by GitHub rather than being made badly.
func (l *loader) committerName() string {
	v := l.optional(EnvCommitterName, DefaultCommitterName)
	if hasControl(v) {
		l.fail(EnvCommitterName, "must not contain control characters")
		return DefaultCommitterName
	}
	return v
}

func (l *loader) committerEmail() string {
	v := l.optional(EnvCommitterEmail, DefaultCommitterEmail)
	switch {
	case hasControl(v):
		l.fail(EnvCommitterEmail, "must not contain control characters")
	case strings.ContainsAny(v, " \t<>,"):
		l.fail(EnvCommitterEmail, fmt.Sprintf("must be a bare address with no whitespace or angle brackets (got %q)", v))
	case strings.Count(v, "@") != 1 || strings.HasPrefix(v, "@") || strings.HasSuffix(v, "@"):
		l.fail(EnvCommitterEmail, fmt.Sprintf("must look like an email address (got %q)", v))
	default:
		return v
	}
	return DefaultCommitterEmail
}

// githubCredential reads either a personal access token or a GitHub App. One
// of the two, never both: with both set, which one is in use would depend on
// code nobody reads while editing the env file.
func (l *loader) githubCredential() (token, appID string, appKey *rsa.PrivateKey) {
	token, hasToken := l.get(EnvGitHubToken)
	appID, hasID := l.get(EnvGitHubAppID)
	rawKey, hasKey := l.get(EnvGitHubAppKey)

	switch {
	case hasToken && (hasID || hasKey):
		l.fail(EnvGitHubToken, fmt.Sprintf("is set alongside %s/%s; set the token or the App, not both",
			EnvGitHubAppID, EnvGitHubAppKey))
		return "", "", nil

	case hasToken:
		if !strings.HasPrefix(token, "github_pat_") && !strings.HasPrefix(token, "ghp_") {
			l.warn("%s does not start with github_pat_ (fine-grained) or ghp_ (classic); "+
				"check it is a GitHub token and not something else", EnvGitHubToken)
		}
		return token, "", nil

	case !hasID && !hasKey:
		l.fail(EnvGitHubToken, fmt.Sprintf("is required but not set, unless %s and %s are",
			EnvGitHubAppID, EnvGitHubAppKey))
		return "", "", nil
	}

	ok := true
	if !hasID {
		l.fail(EnvGitHubAppID, fmt.Sprintf("is required when %s is set", EnvGitHubAppKey))
		ok = false
	} else if !isDigits(appID) {
		l.fail(EnvGitHubAppID, fmt.Sprintf("must be the numeric App ID from the App's settings page (got %q)", appID))
		ok = false
	}
	if !hasKey {
		l.fail(EnvGitHubAppKey, fmt.Sprintf("is required when %s is set", EnvGitHubAppID))
		return "", "", nil
	}
	// The parse error describes the shape, never the value.
	key, err := ghauth.ParsePrivateKey(rawKey)
	if err != nil {
		l.fail(EnvGitHubAppKey, err.Error()+"; it must be the App's .pem, as is or base64-encoded")
		return "", "", nil
	}
	if !ok {
		return "", "", nil
	}
	return "", appID, key
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func (l *loader) sessionSecret(publicURL string) []byte {
	// Deliberately not trimmed: whitespace is key material, and trimming
	// would silently change the key between environments.
	raw, ok := l.lookup(EnvSessionKey)
	if !ok || raw == "" {
		l.fail(EnvSessionKey, fmt.Sprintf("is required but not set; supply at least %d bytes, "+
			"e.g. `openssl rand -base64 48`", MinSessionSecret))
		return nil
	}
	if len(raw) < MinSessionSecret {
		l.fail(EnvSessionKey, fmt.Sprintf("must be at least %d bytes, got %d", MinSessionSecret, len(raw)))
		return nil
	}
	if devSecrets[raw] && publicURL != "" && !isLocal(publicURL) {
		l.fail(EnvSessionKey, fmt.Sprintf("is the placeholder value from the dev stack, and %s (%s) is not local. "+
			"Anyone who has read this repository could forge a session; generate a real secret",
			EnvPublicURL, publicURL))
		return nil
	}
	if devSecrets[raw] {
		l.warn("%s is the dev placeholder; never deploy this", EnvSessionKey)
	}
	return []byte(raw)
}

func (l *loader) ttl() time.Duration {
	raw, ok := l.get(EnvSessionTTL)
	if !ok {
		return DefaultSessionTTL
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		l.fail(EnvSessionTTL, fmt.Sprintf("must be a Go duration such as 8h or 45m (got %q)", raw))
		return DefaultSessionTTL
	}
	if d < MinSessionTTL {
		l.fail(EnvSessionTTL, fmt.Sprintf("must be at least %s, got %s; a bare number is nanoseconds", MinSessionTTL, d))
		return DefaultSessionTTL
	}
	return d
}

// parseOrigin accepts only scheme://host[:port].
func parseOrigin(raw string) (string, error) {
	u, err := parseAbsURL(raw)
	if err != nil {
		return "", err
	}
	if u.User != nil {
		return "", fmt.Errorf("must not contain userinfo")
	}
	if p := strings.TrimSuffix(u.EscapedPath(), "/"); p != "" {
		return "", fmt.Errorf("must be a bare origin with no path, got path %q", u.EscapedPath())
	}
	if u.RawQuery != "" || u.ForceQuery {
		return "", fmt.Errorf("must not have a query string")
	}
	if u.Fragment != "" {
		return "", fmt.Errorf("must not have a fragment")
	}
	return u.Scheme + "://" + u.Host, nil
}

// parseAbsURL enforces the shared prefix of every URL we accept: an absolute
// http or https URL with a host.
func parseAbsURL(raw string) (*url.URL, error) {
	if hasControl(raw) {
		return nil, fmt.Errorf("must not contain control characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("is not a valid URL: %w", err)
	}
	switch u.Scheme {
	case "http", "https":
	case "":
		return nil, fmt.Errorf("must be an absolute URL beginning with http:// or https://")
	default:
		return nil, fmt.Errorf("must use the http or https scheme, got %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("must include a host")
	}
	if u.Opaque != "" {
		return nil, fmt.Errorf("must be an absolute URL beginning with http:// or https://")
	}
	return u, nil
}

// isLocal reports whether an origin points at this machine, used only to
// decide how loudly to complain about a placeholder secret.
func isLocal(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "localhost" || host == "127.0.0.1" || host == "::1" ||
		strings.HasSuffix(host, ".localhost")
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
