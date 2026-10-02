// Package oauthas is the OAuth 2.1 authorization server the MCP endpoint sits
// behind. It wraps Keycloak rather than being one: Keycloak does the login and
// owns the roles, and this package owns dynamic client registration, the PKCE
// authorization code flow the MCP client speaks, and the access token.
//
// Making Keycloak the authorization server directly would hang the feature on
// its dynamic client registration or on hand-registering a connector's
// redirect URIs in the production admin console. Keeping the authorization
// server here keeps the role conjunction where it is already written and
// tested, and lets registration be as permissive as a connector needs.
//
// The access token is an [github.com/prodeko/prodeko-org/proxy/internal/session]
// sealed blob carrying the verified Keycloak identity. It is self-contained,
// so it survives a restart of the container; the client registry and the
// in-flight authorization codes are in memory and do not.
//
// MVP scope: the authorization_code grant only. No refresh token, no token
// exchange, no client credentials. A session lasts [DefaultTokenTTL] and then
// the connector signs in again.
package oauthas

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

	"github.com/prodeko/prodeko-org/proxy/internal/session"
)

// The routes this package owns. The two well-known documents are what lets a
// client that was handed nothing but https://mcp.prodeko.org/mcp find its way
// in (RFC 8414 and RFC 9728).
const (
	MetadataPath         = "/.well-known/oauth-authorization-server"
	ResourceMetadataPath = "/.well-known/oauth-protected-resource"
	RegisterPath         = "/register"
	AuthorizePath        = "/authorize"
	CallbackPath         = "/oauth/callback"
	TokenPath            = "/token"
)

// DefaultTokenTTL is how long an issued access token lives. An MCP connector
// lives for weeks and there is no refresh grant in the MVP, so this is a
// working day rather than the minutes an OAuth access token usually gets.
const DefaultTokenTTL = 8 * time.Hour

// DefaultAuthTTL bounds how long an authorization may sit half-finished:
// between /authorize and the Keycloak callback, and between our code being
// issued and being redeemed at /token.
const DefaultAuthTTL = 10 * time.Minute

// Limits on what an unauthenticated caller can make this process hold.
const (
	MaxClients        = 4096
	MaxPendingAuths   = 4096
	MaxRequestBytes   = 64 << 10
	MaxRedirectURIs   = 16
	MaxClientNameLen  = 200
	MaxRedirectURILen = 2000
)

var (
	ErrUnknownClient  = errors.New("oauthas: unknown client")
	ErrBadRedirectURI = errors.New("oauthas: redirect_uri does not match the registration")
	ErrUnknownCode    = errors.New("oauthas: unknown or expired authorization code")
	ErrBadVerifier    = errors.New("oauthas: the PKCE verifier does not match the challenge")
	ErrMissingRoles   = errors.New("oauthas: the account is missing a required realm role")
)

// DefaultAccessContact is who a refused sign-in is pointed at when
// MCP_ACCESS_CONTACT is unset.
const DefaultAccessContact = "the guild's IT team"

var (
	ErrNotImplemented  = errors.New("oauthas: not implemented")
	ErrUnsupportedFlow = errors.New("oauthas: only the authorization_code grant is supported")
)

// Issuer is the slice of [session.Store] this package mints tokens through.
type Issuer interface {
	Issue(session.Identity) (token string, expires time.Time, err error)
}

// Verifier is the slice of [session.Store] the MCP endpoint checks tokens
// through.
type Verifier interface {
	Verify(token string) (session.Identity, error)
}

// Keycloak is the upstream login. It is an interface so the authorization
// server can be tested end to end without a live Keycloak, and so the
// discovery this package does not own stays in one implementation.
type Keycloak interface {
	// AuthCodeURL is where the browser is sent to log in. The caller owns
	// state, nonce and the PKCE verifier; this leg's PKCE is between us and
	// Keycloak and is not the client's.
	AuthCodeURL(state, nonce, verifier string) string

	// Exchange finishes the Keycloak leg and returns the verified identity,
	// roles included. Every check that makes the identity trustworthy - ID
	// token signature, nonce, azp, realm_access.roles - happens inside it.
	Exchange(ctx context.Context, code, nonce, verifier string) (session.Identity, error)
}

type Config struct {
	// PublicURL is the bare origin this server is reached at, e.g.
	// https://mcp.prodeko.org. It is the issuer identifier in the metadata
	// and the base of every URL in it.
	PublicURL string

	// ResourcePath is the protected resource this authorization server guards,
	// as a path below PublicURL. Empty means "/mcp".
	ResourcePath string

	Keycloak Keycloak
	Sessions Issuer
	Tokens   Verifier

	// AccessContact is who a refused sign-in is told to ask: the person or
	// team that grants the media role. Empty means DefaultAccessContact.
	AccessContact string

	// RequiredRoles is MCP_REQUIRED_ROLES: the realm roles an account must
	// hold, all of them. The conjunction is the point - hand-granted media
	// rights stop working when the automatically maintained membership role
	// lapses, instead of waiting for somebody to remember to revoke them.
	RequiredRoles []string

	TokenTTL time.Duration // zero means DefaultTokenTTL
	AuthTTL  time.Duration // zero means DefaultAuthTTL
	Logger   *slog.Logger  // nil means slog.Default
	Now      func() time.Time
}

// Server is the authorization server. Its stores are in memory and guarded by
// one mutex; a restart means every connector re-registers and re-authorizes,
// which is acceptable for a registry of clients and unacceptable for tokens,
// which is why tokens are sealed and self-contained instead.
type Server struct {
	cfg Config
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	clients map[string]*client
	auths   map[string]*pendingAuth // Keycloak state -> in-flight authorization
	codes   map[string]*authCode    // our code -> redeemable authorization
}

func New(cfg Config) (*Server, error) {
	origin, err := parseOrigin(cfg.PublicURL)
	if err != nil {
		return nil, fmt.Errorf("oauthas: PUBLIC_URL %q must be a bare origin such as https://mcp.prodeko.org: %w", cfg.PublicURL, err)
	}
	cfg.PublicURL = origin

	if cfg.Keycloak == nil {
		return nil, errors.New("oauthas: a Keycloak client is required")
	}
	if cfg.Sessions == nil || cfg.Tokens == nil {
		return nil, errors.New("oauthas: a session store is required for issuing and verifying tokens")
	}
	if cfg.ResourcePath == "" {
		cfg.ResourcePath = "/mcp"
	}
	if !strings.HasPrefix(cfg.ResourcePath, "/") {
		return nil, fmt.Errorf("oauthas: ResourcePath %q must start with /", cfg.ResourcePath)
	}

	roles := make([]string, 0, len(cfg.RequiredRoles))
	seen := make(map[string]bool, len(cfg.RequiredRoles))
	for _, role := range cfg.RequiredRoles {
		role = strings.TrimSpace(role)
		if role == "" || seen[role] {
			continue
		}
		seen[role] = true
		roles = append(roles, role)
	}
	if len(roles) == 0 {
		return nil, errors.New("oauthas: MCP_REQUIRED_ROLES must name at least one realm role; " +
			"without one any Prodeko account could open pull requests against the website")
	}
	cfg.RequiredRoles = roles

	if strings.TrimSpace(cfg.AccessContact) == "" {
		cfg.AccessContact = DefaultAccessContact
	}
	if cfg.TokenTTL == 0 {
		cfg.TokenTTL = DefaultTokenTTL
	}
	if cfg.AuthTTL == 0 {
		cfg.AuthTTL = DefaultAuthTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	return &Server{
		cfg:     cfg,
		log:     cfg.Logger,
		now:     cfg.Now,
		clients: make(map[string]*client),
		auths:   make(map[string]*pendingAuth),
		codes:   make(map[string]*authCode),
	}, nil
}

// Register wires the authorization server onto mux. The two metadata documents
// and registration are unauthenticated by definition; /authorize and
// /oauth/callback are browser legs; /token is called by the client.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+MetadataPath, s.Metadata)
	mux.HandleFunc("GET "+ResourceMetadataPath, s.ResourceMetadata)
	mux.HandleFunc("POST "+RegisterPath, s.RegisterClient)
	mux.HandleFunc("GET "+AuthorizePath, s.Authorize)
	mux.HandleFunc("GET "+CallbackPath, s.Callback)
	mux.HandleFunc("POST "+TokenPath, s.Token)
}

// RedirectURI is the Keycloak redirect URI this server uses. It must be
// registered verbatim on the Keycloak client.
func (s *Server) RedirectURI() string { return s.cfg.PublicURL + CallbackPath }

// ResourceMetadataURL is what a 401 from the MCP endpoint points at.
func (s *Server) ResourceMetadataURL() string { return s.cfg.PublicURL + ResourceMetadataPath }

// Authenticate verifies one of our access tokens and returns the identity
// sealed into it. It is what the MCP transport authenticates with.
//
// The roles in the token are the roles the account held at sign-in; removing a
// role in Keycloak takes effect when the token expires, or at once through the
// session store's revocation.
func (s *Server) Authenticate(bearer string) (session.Identity, error) {
	id, err := s.cfg.Tokens.Verify(bearer)
	if err != nil {
		return session.Identity{}, err
	}
	if missing := id.MissingRoles(s.cfg.RequiredRoles); len(missing) > 0 {
		return session.Identity{}, fmt.Errorf("%w: %s", ErrMissingRoles, strings.Join(missing, ", "))
	}
	return id, nil
}

// maxStateLen bounds the client state and the other strings /authorize records
// per in-flight authorization. Nothing in OAuth bounds them, and /authorize is
// unauthenticated, so without a limit MaxPendingAuths requests could hold an
// arbitrary amount of memory.
const maxStateLen = 2048

// Authorize handles GET /authorize. PKCE with S256 is required: a public
// client registered through permissive dynamic registration has no secret, so
// the challenge is the whole of what binds the code to the client that asked
// for it. It records the request and redirects the browser to Keycloak.
//
// Two failures cannot be reported by redirecting: an unknown client_id and a
// redirect_uri the client never registered. Redirecting either would make this
// server an open redirector and hand somebody else's authorization to whoever
// asked. They are answered in place instead (RFC 6749 section 4.1.2.1).
func (s *Server) Authorize(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	q := r.URL.Query()

	c, err := s.lookupClient(q.Get("client_id"))
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_client",
			"unknown client_id; register at "+RegisterPath+" first. Registrations are held in memory and are lost when the server restarts.")
		return
	}
	redirectURI, given, err := chooseRedirectURI(c, q.Get("redirect_uri"))
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	state := q.Get("state")
	if len(state) > maxStateLen {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("state is longer than the %d bytes this server accepts", maxStateLen))
		return
	}
	if len(q.Get("resource")) > maxStateLen {
		s.redirectError(w, r, redirectURI, state, "invalid_request",
			fmt.Sprintf("resource is longer than the %d bytes this server accepts", maxStateLen))
		return
	}
	if rt := q.Get("response_type"); rt != "code" {
		s.redirectError(w, r, redirectURI, state, "unsupported_response_type",
			"response_type must be code; this server issues no other grant")
		return
	}
	if m := q.Get("code_challenge_method"); m != "S256" {
		s.redirectError(w, r, redirectURI, state, "invalid_request",
			"code_challenge_method must be S256; plain would bind the code to nothing")
		return
	}
	challenge := q.Get("code_challenge")
	if !validChallenge(challenge) {
		s.redirectError(w, r, redirectURI, state, "invalid_request",
			fmt.Sprintf("code_challenge must be %d-%d characters of the unreserved set", minVerifierLen, maxVerifierLen))
		return
	}

	// Nothing below this is the client's fault, so it is reported as ours.
	unavailable := func(err error) {
		s.log.Error("could not start the Keycloak sign-in", "err", err, "client", c.ID)
		s.redirectError(w, r, redirectURI, state, "temporarily_unavailable",
			"the sign-in could not be started; Keycloak may be unreachable")
	}

	// The nonce and verifier belong to our leg into Keycloak. They are not the
	// client's PKCE, which stays recorded as CodeChallenge and is checked at
	// /token.
	nonce, err := randomToken()
	if err != nil {
		unavailable(err)
		return
	}
	verifier, err := newVerifier()
	if err != nil {
		unavailable(err)
		return
	}
	kcState, err := s.putAuth(&pendingAuth{
		ClientID:      c.ID,
		RedirectURI:   redirectURI,
		RedirectGiven: given,
		ClientState:   state,
		CodeChallenge: challenge,
		Scope:         Scope,
		Resource:      q.Get("resource"),
		Nonce:         nonce,
		Verifier:      verifier,
	})
	if err != nil {
		unavailable(err)
		return
	}

	// AuthCodeURL has nowhere to report why it failed; discovery against an
	// unreachable Keycloak is the only reason it ever does.
	authURL := s.cfg.Keycloak.AuthCodeURL(kcState, nonce, verifier)
	if authURL == "" {
		unavailable(errors.New("Keycloak discovery is not available"))
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, authURL, http.StatusFound)
}

// Callback handles GET /oauth/callback, the end of the Keycloak leg. It
// verifies the Keycloak response, requires every role in RequiredRoles, and
// redirects back to the client's redirect_uri with an authorization code of
// our own.
func (s *Server) Callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	q := r.URL.Query()

	// The state is ours and single use. Until it resolves there is no known
	// client and so nowhere to redirect to.
	p, err := s.takeAuth(q.Get("state"))
	if err != nil {
		s.log.Warn("callback with an unknown state", "remote", r.RemoteAddr)
		writeOAuthError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("this sign-in was not recognised, or it took longer than %s; start again from the connector. "+
				"Restarting the server cancels any sign-in already under way.", s.cfg.AuthTTL))
		return
	}

	// Keycloak reports its own refusals on the redirect, and those are more
	// useful than anything inferred later, so they are read first.
	if kcErr := q.Get("error"); kcErr != "" {
		s.log.Warn("Keycloak refused the sign-in", "error", kcErr, "description", q.Get("error_description"))
		s.redirectError(w, r, p.RedirectURI, p.ClientState, "access_denied",
			"Keycloak refused the sign-in ("+safeErrorCode(kcErr)+")")
		return
	}
	code := q.Get("code")
	if code == "" {
		s.redirectError(w, r, p.RedirectURI, p.ClientState, "server_error", "Keycloak returned no authorization code")
		return
	}

	id, err := s.cfg.Keycloak.Exchange(r.Context(), code, p.Nonce, p.Verifier)
	if err != nil {
		s.log.Error("the Keycloak code exchange failed", "err", err, "client", p.ClientID)
		s.redirectError(w, r, p.RedirectURI, p.ClientState, "server_error", "the Keycloak sign-in could not be completed")
		return
	}

	// The conjunction, not a disjunction: hand-granted media rights stop
	// working when the automatically maintained membership role lapses.
	if missing := id.MissingRoles(s.cfg.RequiredRoles); len(missing) > 0 {
		s.log.Warn("sign-in refused: missing realm roles",
			"sub", id.Subject, "user", id.Username,
			"required", s.cfg.RequiredRoles, "missing", missing)
		s.redirectError(w, r, p.RedirectURI, p.ClientState, "access_denied",
			fmt.Sprintf("editing the website requires the realm roles %s, and this account does not hold all of them. "+
				"A lapsed guild membership is the usual cause; otherwise ask %s for the rest.",
				strings.Join(s.cfg.RequiredRoles, ", "), s.cfg.AccessContact))
		return
	}

	ourCode, err := s.putCode(&authCode{
		ClientID:      p.ClientID,
		RedirectURI:   p.RedirectURI,
		RedirectGiven: p.RedirectGiven,
		CodeChallenge: p.CodeChallenge,
		Identity:      id,
	})
	if err != nil {
		s.log.Error("could not mint an authorization code", "err", err, "client", p.ClientID)
		s.redirectError(w, r, p.RedirectURI, p.ClientState, "server_error", "the authorization could not be completed")
		return
	}

	s.log.Info("authorization granted",
		"sub", id.Subject, "user", id.Username, "client", p.ClientID)

	params := url.Values{"code": {ourCode}}
	if p.ClientState != "" {
		params.Set("state", p.ClientState)
	}
	s.redirect(w, r, p.RedirectURI, params)
}

// tokenResponse is the RFC 6749 section 5.1 success body.
type tokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
	Scope       string `json:"scope,omitempty"`
}

// Token handles POST /token. authorization_code only: the code must be
// unredeemed, unexpired, issued to this client, and matched by the PKCE
// verifier. The access token is a sealed session carrying the identity.
func (s *Server) Token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the token request is not a readable form body")
		return
	}
	if grant := r.PostFormValue("grant_type"); grant != "authorization_code" {
		s.log.Warn("unsupported grant", "grant_type", grant)
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type",
			fmt.Sprintf("%v: this server issues no refresh tokens, so a connector signs in again every %s",
				ErrUnsupportedFlow, s.cfg.TokenTTL))
		return
	}

	// The code is spent by being presented, whatever the rest of the request
	// turns out to be: a leaked code must not survive a wrong verifier.
	code, err := s.takeCode(r.PostFormValue("code"))
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant",
			"unknown, expired or already redeemed authorization code")
		return
	}

	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("client_id")), []byte(code.ClientID)) != 1 {
		s.log.Warn("token request from the wrong client", "client", code.ClientID)
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant",
			"the authorization code was issued to a different client")
		return
	}
	// RFC 6749 section 4.1.3: redirect_uri is required here exactly when the
	// authorization request carried one.
	if redirectURI := r.PostFormValue("redirect_uri"); code.RedirectGiven || redirectURI != "" {
		if subtle.ConstantTimeCompare([]byte(redirectURI), []byte(code.RedirectURI)) != 1 {
			writeOAuthError(w, http.StatusBadRequest, "invalid_grant",
				"redirect_uri does not match the one the authorization code was issued for")
			return
		}
	}
	if err := verifyPKCE(code.CodeChallenge, r.PostFormValue("code_verifier")); err != nil {
		s.log.Warn("token request with a bad PKCE verifier", "client", code.ClientID)
		writeOAuthError(w, http.StatusBadRequest, "invalid_grant", ErrBadVerifier.Error())
		return
	}

	token, expires, err := s.cfg.Sessions.Issue(code.Identity)
	if err != nil {
		s.log.Error("could not issue an access token", "err", err, "sub", code.Identity.Subject)
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the access token could not be issued")
		return
	}

	// The store's expiry governs, not TokenTTL: it is what Verify enforces.
	// Both ends are measured on the whole second the session store stamps its
	// tokens with, so a full TTL reports as the whole number of seconds it is
	// rather than one less.
	expiresIn := int64(s.cfg.TokenTTL / time.Second)
	if !expires.IsZero() {
		expiresIn = int64(expires.Sub(s.now().Truncate(time.Second)) / time.Second)
	}
	if expiresIn < 0 {
		expiresIn = 0
	}

	s.log.Info("access token issued",
		"sub", code.Identity.Subject, "user", code.Identity.Username,
		"client", code.ClientID, "expires", expires)

	// RFC 6749 section 5.1 requires both no-store headers on this response.
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, tokenResponse{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresIn:   expiresIn,
		Scope:       Scope,
	})
}

// safeErrorCode reduces an error code that came back from Keycloak to
// something that can be repeated to the client: an OAuth error code and
// nothing else. The full text stays in the log.
func safeErrorCode(code string) string {
	const max = 64
	if len(code) > max {
		code = code[:max]
	}
	var b strings.Builder
	for i := 0; i < len(code); i++ {
		c := code[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('.')
		}
	}
	return b.String()
}

// chooseRedirectURI resolves the redirect_uri an authorization request is to be
// answered at. The comparison against the registration is exact: a code is only
// ever returned to a URI the client named at registration.
//
// A request that omits redirect_uri is answered at the registration's only URI,
// and refused if there is more than one, because guessing which one was meant
// is guessing where to send an authorization code.
func chooseRedirectURI(c *client, requested string) (uri string, given bool, err error) {
	if requested == "" {
		if len(c.RedirectURIs) != 1 {
			return "", false, fmt.Errorf("redirect_uri is required: this client registered %d of them", len(c.RedirectURIs))
		}
		return c.RedirectURIs[0], false, nil
	}
	if !c.allows(requested) {
		return "", false, ErrBadRedirectURI
	}
	return requested, true, nil
}

// redirect sends the browser back to the client with params added to whatever
// query the registered redirect URI already carried.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, redirectURI string, params url.Values) {
	u, err := url.Parse(redirectURI)
	if err != nil {
		// Unreachable: every redirect URI was parsed at registration.
		s.log.Error("registered redirect_uri does not parse", "err", err)
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the registered redirect_uri is not a URI")
		return
	}
	q := u.Query()
	for k, values := range params {
		for _, v := range values {
			q.Set(k, v)
		}
	}
	u.RawQuery = q.Encode()

	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// redirectError reports a failure to the client at its redirect URI. Only
// failures found after the client and its redirect URI are known may come this
// way; everything before that is answered in place.
func (s *Server) redirectError(w http.ResponseWriter, r *http.Request, redirectURI, state, code, description string) {
	params := url.Values{"error": {code}, "error_description": {description}}
	if state != "" {
		params.Set("state", state)
	}
	s.redirect(w, r, redirectURI, params)
}
