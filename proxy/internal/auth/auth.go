// Package auth owns editor sign-in: GET /auth and GET /callback.
//
// /auth redirects the Decap popup to Keycloak. /callback verifies the
// response, requires every configured realm role, mints a proxy session through
// an [Issuer], and completes Decap's two-step postMessage handshake. Both the
// success and the failure path complete that handshake; see handshake.go.
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/prodeko/prodeko-org/proxy/internal/session"
)

// DefaultStateTTL bounds how long a sign-in may sit half-finished.
const DefaultStateTTL = 10 * time.Minute

type Config struct {
	Issuer string // KEYCLOAK_ISSUER, e.g. https://id.prodeko.org/realms/membership-registry

	// DiscoveryURL is where the .well-known document is fetched from. Empty
	// means Issuer, which is the production case. It differs only when the
	// browser and the proxy reach Keycloak at different addresses (the
	// docker-compose dev stack): discovery happens here, but every token must
	// still carry iss == Issuer.
	DiscoveryURL string

	ClientID     string // KEYCLOAK_CLIENT_ID
	ClientSecret string // KEYCLOAK_CLIENT_SECRET; empty selects a public client

	// EditorRoles are the realm roles from EDITOR_ROLES. An editor must hold
	// every one of them; at least one must be configured.
	EditorRoles []string

	// PublicURL is where this service is reachable. It must be a bare origin:
	// scheme://host[:port], no path, no trailing slash. Decap compares the
	// popup's window origin against base_url with ===, so anything else makes
	// sign-in hang with no error at all. New rejects a PublicURL with a path.
	PublicURL string

	// CMSOrigins are the origins allowed to complete the handshake, e.g.
	// https://prodeko.org. Empty means accept any origin and log a warning at
	// startup. Suggested environment variable: CMS_ORIGINS.
	CMSOrigins []string

	Scopes     []string      // nil means {"openid", "profile", "email"}
	StateTTL   time.Duration // zero means DefaultStateTTL
	HTTPClient *http.Client  // nil means http.DefaultClient
	Now        func() time.Time
	Logger     *slog.Logger // nil means slog.Default
}

// Issuer is the slice of [session.Store] that auth depends on.
type Issuer interface {
	Issue(session.Identity) (token string, expires time.Time, err error)
}

type Handler struct {
	cfg        Config
	oauth      oauth2.Config
	idVerifier *oidc.IDTokenVerifier
	atVerifier *oidc.IDTokenVerifier
	states     *stateStore
	sessions   Issuer
	log        *slog.Logger
	httpClient *http.Client

	roleSourceOnce sync.Once
}

// New runs OIDC discovery against cfg.Issuer, so it does network I/O and can
// fail at startup. It builds two verifiers over the same key set: a strict one
// for the ID token (aud == ClientID) and a lenient one for the access token
// (azp == ClientID, checked here), because Keycloak's built-in "realm roles"
// mapper puts realm_access.roles in the access token and not in the ID token.
func New(ctx context.Context, cfg Config, sessions Issuer) (*Handler, error) {
	if sessions == nil {
		return nil, errors.New("auth: sessions must not be nil")
	}
	cfg, err := normalise(cfg)
	if err != nil {
		return nil, err
	}

	oidcCtx := oidc.ClientContext(ctx, cfg.HTTPClient)
	if cfg.DiscoveryURL != cfg.Issuer {
		// Discovery will report the front-channel issuer, which is not the URL
		// we fetched from. Keep validating iss against Issuer regardless.
		oidcCtx = oidc.InsecureIssuerURLContext(oidcCtx, cfg.Issuer)
	}
	provider, err := oidc.NewProvider(oidcCtx, cfg.DiscoveryURL)
	if err != nil {
		return nil, fmt.Errorf("auth: OIDC discovery against %s failed: %w", cfg.DiscoveryURL, err)
	}

	h := &Handler{
		cfg: cfg,
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint: oauth2.Endpoint{
				AuthURL:   provider.Endpoint().AuthURL,
				TokenURL:  provider.Endpoint().TokenURL,
				AuthStyle: oauth2.AuthStyleInParams,
			},
			RedirectURL: cfg.PublicURL + callbackPath,
			Scopes:      cfg.Scopes,
		},
		idVerifier: provider.Verifier(&oidc.Config{
			ClientID:             cfg.ClientID,
			SupportedSigningAlgs: []string{oidc.RS256},
			Now:                  cfg.Now,
		}),
		atVerifier: provider.Verifier(&oidc.Config{
			// Keycloak's access token audience is resolved by a mapper and is
			// typically "account", not our client, so the audience check is
			// replaced by an explicit azp check in Callback.
			SkipClientIDCheck:    true,
			SupportedSigningAlgs: []string{oidc.RS256},
			Now:                  cfg.Now,
		}),
		states:     newStateStore(cfg.StateTTL, cfg.Now),
		sessions:   sessions,
		log:        cfg.Logger,
		httpClient: cfg.HTTPClient,
	}

	h.log.Info("editor sign-in configured",
		"issuer", cfg.Issuer,
		"discovery_url", cfg.DiscoveryURL,
		"client_id", cfg.ClientID,
		"client_type", map[bool]string{true: "public (PKCE only)", false: "confidential"}[cfg.ClientSecret == ""],
		"editor_roles", cfg.EditorRoles,
		"redirect_uri", h.oauth.RedirectURL,
		"decap_base_url", cfg.PublicURL,
		"cms_origins", cfg.CMSOrigins,
		"state_ttl", cfg.StateTTL,
	)
	if len(cfg.CMSOrigins) == 0 {
		h.log.Warn("CMSOrigins is empty: the sign-in popup will hand the session token to whichever origin answers the handshake; set CMS_ORIGINS")
	}
	if cfg.DiscoveryURL != cfg.Issuer {
		h.log.Warn("split-horizon Keycloak: discovery and iss use different URLs; this is only correct in the dev stack",
			"discovery_url", cfg.DiscoveryURL, "issuer", cfg.Issuer)
	}
	return h, nil
}

const (
	authPath     = "/auth"
	callbackPath = "/callback"
)

// Register wires Start and Callback onto mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+authPath, h.Start)
	mux.HandleFunc("GET "+callbackPath, h.Callback)
}

// Start handles GET /auth. Mounts at exactly "/auth".
//
// Decap calls it as /auth?provider=github&site_id=<host>&scope=repo. site_id
// and scope are Netlify's and mean nothing here; provider is echoed back in
// the handshake and must match what Decap is listening for.
func (h *Handler) Start(w http.ResponseWriter, r *http.Request) {
	provider := safeProvider(r.URL.Query().Get("provider"))

	nonce, err := randomToken()
	if err != nil {
		h.log.Error("could not generate nonce", "err", err)
		http.Error(w, "sign-in unavailable", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()
	state, err := h.states.put(pending{nonce: nonce, verifier: verifier, provider: provider})
	if err != nil {
		h.log.Error("could not generate state", "err", err)
		http.Error(w, "sign-in unavailable", http.StatusInternalServerError)
		return
	}

	// PKCE is sent whether or not the client is confidential: Keycloak accepts
	// it either way and it is the only protection a public client has.
	authURL := h.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, authURL, http.StatusFound)
}

// Callback handles GET /callback. Mounts at exactly "/callback".
// The redirect URI registered in Keycloak must be exactly PublicURL+"/callback".
//
// Every exit from here renders the handshake page, because a popup that simply
// closes tells the editor nothing.
func (h *Handler) Callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	provider := safeProvider(q.Get("provider"))

	p, err := h.states.take(q.Get("state"))
	if err == nil {
		provider = p.provider
	}

	// Keycloak reports its own refusals on the redirect, and those are more
	// useful than "unknown state", so they are reported first.
	if kcErr := q.Get("error"); kcErr != "" {
		h.log.Warn("Keycloak refused the sign-in", "error", kcErr, "description", q.Get("error_description"))
		h.writeFailure(w, provider, "Keycloak refused the sign-in ("+kcErr+"). "+q.Get("error_description"))
		return
	}
	if err != nil {
		h.log.Warn("callback with unknown state", "remote", r.RemoteAddr)
		h.writeFailure(w, provider, "This sign-in was not recognised, or it took too long. Close this window, "+
			"reload /admin and sign in again. Restarting the proxy, or running more than one copy of it, "+
			"cancels any sign-in that is already under way.")
		return
	}

	code := q.Get("code")
	if code == "" {
		h.writeFailure(w, provider, "Keycloak did not return an authorization code.")
		return
	}

	ctx := oidc.ClientContext(r.Context(), h.httpClient)
	tok, err := h.oauth.Exchange(ctx, code, oauth2.VerifierOption(p.verifier))
	if err != nil {
		h.log.Error("token exchange failed", "err", err)
		h.writeFailure(w, provider, "Could not exchange the Keycloak authorization code. Check that the client "+
			"secret is correct and that "+h.oauth.RedirectURL+" is registered as a redirect URI on the "+
			h.cfg.ClientID+" client.")
		return
	}

	rawIDToken, _ := tok.Extra("id_token").(string)
	if rawIDToken == "" {
		h.writeFailure(w, provider, "Keycloak returned no ID token. The openid scope must be allowed on the "+
			h.cfg.ClientID+" client.")
		return
	}
	idToken, err := h.idVerifier.Verify(ctx, rawIDToken)
	if err != nil {
		h.log.Error("ID token verification failed", "err", err)
		h.writeFailure(w, provider, "The Keycloak ID token did not verify: "+err.Error())
		return
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(p.nonce)) != 1 {
		h.log.Error("ID token nonce mismatch")
		h.writeFailure(w, provider, "The Keycloak ID token was issued for a different sign-in. Try again.")
		return
	}

	var claims identityClaims
	if err := idToken.Claims(&claims); err != nil {
		h.writeFailure(w, provider, "Could not read the claims out of the Keycloak ID token.")
		return
	}

	roles, source, err := h.realmRoles(ctx, claims, tok.AccessToken)
	if err != nil {
		h.log.Error("could not read realm roles", "err", err, "sub", idToken.Subject)
		h.writeFailure(w, provider, err.Error())
		return
	}
	h.roleSourceOnce.Do(func() {
		h.log.Info("realm roles read from Keycloak", "source", source)
	})

	id := session.Identity{
		Subject:  idToken.Subject,
		Name:     displayName(claims),
		Email:    strings.TrimSpace(claims.Email),
		Username: claims.Username,
		Roles:    roles,
	}

	if missing := id.MissingRoles(h.cfg.EditorRoles); len(missing) > 0 {
		h.log.Warn("sign-in refused: missing editor roles",
			"sub", id.Subject, "user", id.Username,
			"required", h.cfg.EditorRoles, "missing", missing)
		h.writeFailure(w, provider, fmt.Sprintf(
			"Editing the website requires the roles %s, and your Prodeko account is missing %s. "+
				"A lapsed guild membership is the usual cause; otherwise ask the guild's IT team to grant the rest.",
			strings.Join(h.cfg.EditorRoles, ", "), strings.Join(missing, ", ")))
		return
	}
	// Rule 2 needs a real name and a real address on every commit, and a commit
	// author cannot be repaired after the fact, so an incomplete profile is a
	// hard failure rather than a synthesised noreply address.
	if id.Email == "" {
		h.writeFailure(w, provider, "Your Prodeko account has no email address. Commits are made in your name, "+
			"so an address is required. Add one in your Prodeko profile and sign in again.")
		return
	}
	if id.Name == "" {
		h.writeFailure(w, provider, "Your Prodeko account has no name. Commits are made in your name, so one is "+
			"required. Add it in your Prodeko profile and sign in again.")
		return
	}

	token, expires, err := h.sessions.Issue(id)
	if err != nil {
		h.log.Error("could not issue session", "err", err, "sub", id.Subject)
		h.writeFailure(w, provider, "Could not create an editing session. Try again.")
		return
	}

	h.log.Info("editor signed in",
		"sub", id.Subject, "user", id.Username, "expires", expires)
	h.writeSuccess(w, provider, token)
}

type identityClaims struct {
	Name        string `json:"name"`
	GivenName   string `json:"given_name"`
	FamilyName  string `json:"family_name"`
	Email       string `json:"email"`
	Username    string `json:"preferred_username"`
	AZP         string `json:"azp"`
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// realmRoles reads realm_access.roles from the ID token, falling back to the
// access token. Keycloak's built-in "realm roles" mapper is created with
// idToken=false, so on an untouched realm the ID token has no roles at all and
// the fallback is the normal path, not the exception.
func (h *Handler) realmRoles(ctx context.Context, idClaims identityClaims, accessToken string) ([]string, string, error) {
	if len(idClaims.RealmAccess.Roles) > 0 {
		return idClaims.RealmAccess.Roles, "id_token", nil
	}

	missing := fmt.Errorf("neither the Keycloak ID token nor the access token contains realm_access.roles, "+
		"so the editor roles cannot be checked. Add the built-in \"realm roles\" mapper to the %s client "+
		"(client scope \"roles\", mapper \"realm roles\", claim realm_access.roles) and make sure it is "+
		"included in the access token", h.cfg.ClientID)

	if accessToken == "" {
		return nil, "", missing
	}
	at, err := h.atVerifier.Verify(ctx, accessToken)
	if err != nil {
		return nil, "", fmt.Errorf("the ID token carried no realm_access.roles and the Keycloak access "+
			"token did not verify: %w", err)
	}
	var atClaims identityClaims
	if err := at.Claims(&atClaims); err != nil {
		return nil, "", errors.New("could not read the claims out of the Keycloak access token")
	}
	if atClaims.AZP != h.cfg.ClientID {
		return nil, "", fmt.Errorf("the Keycloak access token was issued to %q, not to %q",
			atClaims.AZP, h.cfg.ClientID)
	}
	if len(atClaims.RealmAccess.Roles) == 0 {
		return nil, "", missing
	}
	return atClaims.RealmAccess.Roles, "access_token", nil
}

func displayName(c identityClaims) string {
	if n := strings.TrimSpace(c.Name); n != "" {
		return n
	}
	if n := strings.TrimSpace(c.GivenName + " " + c.FamilyName); n != "" {
		return n
	}
	return strings.TrimSpace(c.Username)
}

func normalise(cfg Config) (Config, error) {
	cfg.Issuer = strings.TrimRight(strings.TrimSpace(cfg.Issuer), "/")
	if cfg.Issuer == "" {
		return cfg, errors.New("auth: KEYCLOAK_ISSUER must be set")
	}
	if u, err := url.Parse(cfg.Issuer); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return cfg, fmt.Errorf("auth: KEYCLOAK_ISSUER %q is not an absolute http(s) URL", cfg.Issuer)
	}

	cfg.DiscoveryURL = strings.TrimRight(strings.TrimSpace(cfg.DiscoveryURL), "/")
	if cfg.DiscoveryURL == "" {
		cfg.DiscoveryURL = cfg.Issuer
	}
	if u, err := url.Parse(cfg.DiscoveryURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return cfg, fmt.Errorf("auth: KEYCLOAK_DISCOVERY_URL %q is not an absolute http(s) URL", cfg.DiscoveryURL)
	}

	cfg.ClientID = strings.TrimSpace(cfg.ClientID)
	if cfg.ClientID == "" {
		return cfg, errors.New("auth: KEYCLOAK_CLIENT_ID must be set")
	}
	seenRoles := make(map[string]bool, len(cfg.EditorRoles))
	roles := make([]string, 0, len(cfg.EditorRoles))
	for _, role := range cfg.EditorRoles {
		role = strings.TrimSpace(role)
		if role == "" || seenRoles[role] {
			continue
		}
		seenRoles[role] = true
		roles = append(roles, role)
	}
	if len(roles) == 0 {
		return cfg, errors.New("auth: EDITOR_ROLES must name at least one realm role; " +
			"without one any Prodeko member could edit the website")
	}
	cfg.EditorRoles = roles

	origin, err := parseOrigin(cfg.PublicURL)
	if err != nil {
		return cfg, fmt.Errorf("auth: PUBLIC_URL %q must be a bare origin such as https://cms.prodeko.org, "+
			"because Decap compares the popup's window origin against base_url with === and will hang "+
			"silently otherwise: %w", cfg.PublicURL, err)
	}
	cfg.PublicURL = origin

	seen := make(map[string]bool, len(cfg.CMSOrigins))
	cleaned := make([]string, 0, len(cfg.CMSOrigins))
	for _, o := range cfg.CMSOrigins {
		if strings.TrimSpace(o) == "" {
			continue
		}
		p, err := parseOrigin(o)
		if err != nil {
			return cfg, fmt.Errorf("auth: CMS_ORIGINS entry %q must be a bare origin: %w", o, err)
		}
		if !seen[p] {
			seen[p] = true
			cleaned = append(cleaned, p)
		}
	}
	cfg.CMSOrigins = cleaned

	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	if cfg.StateTTL == 0 {
		cfg.StateTTL = DefaultStateTTL
	}
	if cfg.StateTTL < 0 {
		return cfg, errors.New("auth: StateTTL must not be negative")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return cfg, nil
}

// parseOrigin accepts scheme://host[:port] and nothing else. A single trailing
// slash is tolerated because it is the one mistake that is unambiguous.
func parseOrigin(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", errors.New("empty")
	}
	s = strings.TrimRight(s, "/")
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	case u.Host == "":
		return "", errors.New("no host")
	case u.User != nil:
		return "", errors.New("must not contain credentials")
	case u.Path != "":
		return "", fmt.Errorf("must not contain a path, got %q", u.Path)
	case u.RawQuery != "" || u.Fragment != "":
		return "", errors.New("must not contain a query or fragment")
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}
