package oauthas

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/prodeko/prodeko-org/proxy/internal/session"
)

// KeycloakConfig is the upstream login. The values mirror the ones
// internal/auth takes, because it is the same Keycloak client flow with the
// same verification: two verifiers over one key set, strict on the ID token
// and lenient-but-azp-checked on the access token, because Keycloak's built-in
// realm roles mapper puts realm_access.roles in the access token only.
type KeycloakConfig struct {
	Issuer string // KEYCLOAK_ISSUER

	// DiscoveryURL is where the .well-known document is fetched from. Empty
	// means Issuer, which is the production case. It differs only when the
	// browser and this process reach Keycloak at different addresses.
	DiscoveryURL string

	ClientID     string // KEYCLOAK_CLIENT_ID
	ClientSecret string // KEYCLOAK_CLIENT_SECRET

	// RedirectURI must be registered verbatim on the Keycloak client. It is
	// PUBLIC_URL + CallbackPath.
	RedirectURI string

	Scopes     []string     // nil means openid, profile, email
	HTTPClient *http.Client // nil means a client with a 15s timeout
}

// KeycloakClient is the [Keycloak] implementation over a real Keycloak realm.
type KeycloakClient struct {
	cfg KeycloakConfig

	mu        sync.Mutex
	endpoints *kcEndpoints
}

// kcEndpoints is what OIDC discovery produces: the code flow and the two
// verifiers over one key set. A strict one for the ID token (aud == ClientID)
// and a lenient one for the access token (azp == ClientID, checked by hand),
// because Keycloak's built-in realm roles mapper puts realm_access.roles in the
// access token and not in the ID token.
type kcEndpoints struct {
	oauth      oauth2.Config
	idVerifier *oidc.IDTokenVerifier
	atVerifier *oidc.IDTokenVerifier
}

// discoveryTimeout bounds the discovery call made on behalf of a sign-in that
// has no context of its own.
const discoveryTimeout = 15 * time.Second

// discover runs OIDC discovery once and keeps the result. A failure is not
// kept: Keycloak being unreachable for the first sign-in after a restart must
// not leave this client broken once Keycloak is back.
func (k *KeycloakClient) discover(ctx context.Context) (*kcEndpoints, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.endpoints != nil {
		return k.endpoints, nil
	}

	oidcCtx := oidc.ClientContext(ctx, k.cfg.HTTPClient)
	if k.cfg.DiscoveryURL != k.cfg.Issuer {
		// Discovery reports the front-channel issuer, which is not the URL it
		// was fetched from. Keep validating iss against Issuer regardless.
		oidcCtx = oidc.InsecureIssuerURLContext(oidcCtx, k.cfg.Issuer)
	}
	provider, err := oidc.NewProvider(oidcCtx, k.cfg.DiscoveryURL)
	if err != nil {
		return nil, fmt.Errorf("oauthas: OIDC discovery against %s failed: %w", k.cfg.DiscoveryURL, err)
	}

	k.endpoints = &kcEndpoints{
		oauth: oauth2.Config{
			ClientID:     k.cfg.ClientID,
			ClientSecret: k.cfg.ClientSecret,
			Endpoint: oauth2.Endpoint{
				AuthURL:   provider.Endpoint().AuthURL,
				TokenURL:  provider.Endpoint().TokenURL,
				AuthStyle: oauth2.AuthStyleInParams,
			},
			RedirectURL: k.cfg.RedirectURI,
			Scopes:      k.cfg.Scopes,
		},
		idVerifier: provider.Verifier(&oidc.Config{
			ClientID:             k.cfg.ClientID,
			SupportedSigningAlgs: []string{oidc.RS256},
		}),
		atVerifier: provider.Verifier(&oidc.Config{
			// Keycloak's access token audience is resolved by a mapper and is
			// typically "account", not our client, so the audience check is
			// replaced by an explicit azp check in realmRoles.
			SkipClientIDCheck:    true,
			SupportedSigningAlgs: []string{oidc.RS256},
		}),
	}
	return k.endpoints, nil
}

// NewKeycloak validates cfg. OIDC discovery is a network call and belongs on
// the first use rather than at construction, so an unreachable Keycloak does
// not keep the MCP endpoint's health check from answering.
func NewKeycloak(ctx context.Context, cfg KeycloakConfig) (*KeycloakClient, error) {
	cfg.Issuer = strings.TrimRight(strings.TrimSpace(cfg.Issuer), "/")
	if cfg.Issuer == "" {
		return nil, errors.New("oauthas: KEYCLOAK_ISSUER must be set")
	}
	cfg.DiscoveryURL = strings.TrimRight(strings.TrimSpace(cfg.DiscoveryURL), "/")
	if cfg.DiscoveryURL == "" {
		cfg.DiscoveryURL = cfg.Issuer
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("oauthas: KEYCLOAK_CLIENT_ID must be set")
	}
	if strings.TrimSpace(cfg.RedirectURI) == "" {
		return nil, errors.New("oauthas: the Keycloak redirect URI must be set")
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"openid", "profile", "email"}
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 15 * time.Second}
	}
	return &KeycloakClient{cfg: cfg}, nil
}

// Issuer is the realm this client authenticates against.
func (k *KeycloakClient) Issuer() string { return k.cfg.Issuer }

// AuthCodeURL is where the browser is sent to sign in. PKCE is sent whether or
// not the client is confidential: Keycloak accepts it either way.
//
// The only way this fails is discovery against an unreachable Keycloak, and the
// [Keycloak] interface gives it nowhere to say so, so it returns the empty
// string and logs. The caller treats that as "sign-in unavailable".
func (k *KeycloakClient) AuthCodeURL(state, nonce, verifier string) string {
	ctx, cancel := context.WithTimeout(context.Background(), discoveryTimeout)
	defer cancel()

	e, err := k.discover(ctx)
	if err != nil {
		slog.Default().Error("Keycloak discovery failed", "err", err, "discovery_url", k.cfg.DiscoveryURL)
		return ""
	}
	return e.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
	)
}

// Exchange finishes the Keycloak leg: it trades the code for tokens, verifies
// the ID token and its nonce, reads realm_access.roles from the ID token or
// the access token, and returns the identity that goes into our own token.
//
// An account with no name or no email address is refused rather than repaired:
// commits are made in the person's name and a commit author cannot be fixed
// after the fact.
func (k *KeycloakClient) Exchange(ctx context.Context, code, nonce, verifier string) (session.Identity, error) {
	e, err := k.discover(ctx)
	if err != nil {
		return session.Identity{}, err
	}
	ctx = oidc.ClientContext(ctx, k.cfg.HTTPClient)

	tok, err := e.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return session.Identity{}, fmt.Errorf("oauthas: the Keycloak code exchange failed; check the client secret "+
			"and that %s is registered as a redirect URI on the %s client: %w", k.cfg.RedirectURI, k.cfg.ClientID, err)
	}

	rawIDToken, _ := tok.Extra("id_token").(string)
	if rawIDToken == "" {
		return session.Identity{}, fmt.Errorf("oauthas: Keycloak returned no ID token; the openid scope must be "+
			"allowed on the %s client", k.cfg.ClientID)
	}
	idToken, err := e.idVerifier.Verify(ctx, rawIDToken)
	if err != nil {
		return session.Identity{}, fmt.Errorf("oauthas: the Keycloak ID token did not verify: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(nonce)) != 1 {
		return session.Identity{}, errors.New("oauthas: the Keycloak ID token was issued for a different sign-in")
	}

	var claims keycloakClaims
	if err := idToken.Claims(&claims); err != nil {
		return session.Identity{}, fmt.Errorf("oauthas: could not read the claims out of the Keycloak ID token: %w", err)
	}
	roles, err := k.realmRoles(ctx, e, claims, tok.AccessToken)
	if err != nil {
		return session.Identity{}, err
	}

	id := session.Identity{
		Subject:  idToken.Subject,
		Name:     displayName(claims),
		Email:    strings.TrimSpace(claims.Email),
		Username: strings.TrimSpace(claims.Username),
		Roles:    roles,
	}
	if id.Email == "" {
		return session.Identity{}, errors.New("oauthas: this Prodeko account has no email address, and commits are " +
			"made in the editor's name; add one in the Prodeko profile and sign in again")
	}
	if id.Name == "" {
		return session.Identity{}, errors.New("oauthas: this Prodeko account has no name, and commits are made in " +
			"the editor's name; add one in the Prodeko profile and sign in again")
	}
	if id.Username == "" {
		// The username is the branch namespace: media/<username>/<slug>.
		return session.Identity{}, errors.New("oauthas: the Keycloak identity carries no preferred_username, and it " +
			"is what names the editor's branches")
	}
	return id, nil
}

type keycloakClaims struct {
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
// idToken=false, so on an untouched realm the ID token carries no roles at all
// and the fallback is the normal path rather than the exception.
func (k *KeycloakClient) realmRoles(ctx context.Context, e *kcEndpoints, idClaims keycloakClaims, accessToken string) ([]string, error) {
	if len(idClaims.RealmAccess.Roles) > 0 {
		return idClaims.RealmAccess.Roles, nil
	}

	missing := fmt.Errorf("oauthas: neither the Keycloak ID token nor the access token contains "+
		"realm_access.roles, so the required roles cannot be checked. Add the built-in \"realm roles\" mapper to "+
		"the %s client (client scope \"roles\", mapper \"realm roles\", claim realm_access.roles) and make sure it "+
		"is included in the access token", k.cfg.ClientID)

	if accessToken == "" {
		return nil, missing
	}
	at, err := e.atVerifier.Verify(ctx, accessToken)
	if err != nil {
		return nil, fmt.Errorf("oauthas: the ID token carried no realm_access.roles and the Keycloak access token "+
			"did not verify: %w", err)
	}
	var atClaims keycloakClaims
	if err := at.Claims(&atClaims); err != nil {
		return nil, fmt.Errorf("oauthas: could not read the claims out of the Keycloak access token: %w", err)
	}
	if atClaims.AZP != k.cfg.ClientID {
		return nil, fmt.Errorf("oauthas: the Keycloak access token was issued to %q, not to %q", atClaims.AZP, k.cfg.ClientID)
	}
	if len(atClaims.RealmAccess.Roles) == 0 {
		return nil, missing
	}
	return atClaims.RealmAccess.Roles, nil
}

func displayName(c keycloakClaims) string {
	if n := strings.TrimSpace(c.Name); n != "" {
		return n
	}
	if n := strings.TrimSpace(c.GivenName + " " + c.FamilyName); n != "" {
		return n
	}
	return strings.TrimSpace(c.Username)
}
