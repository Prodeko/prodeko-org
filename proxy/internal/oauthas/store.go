package oauthas

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/prodeko/prodeko-org/proxy/internal/session"
)

// The in-memory stores: registered clients, in-flight authorizations, and
// authorization codes waiting to be redeemed. All three are lost on restart,
// which costs a connector one re-registration and one sign-in. Tokens are not
// kept here at all; they are sealed blobs and survive.

// client is one dynamically registered MCP client.
type client struct {
	ID           string
	Name         string
	RedirectURIs []string
	CreatedAt    time.Time
}

// allows reports whether uri is one this client registered. The comparison is
// exact: an authorization code is only ever returned to a URI the client named
// at registration.
func (c *client) allows(uri string) bool {
	for _, u := range c.RedirectURIs {
		if u == uri {
			return true
		}
	}
	return false
}

// pendingAuth is one authorization between /authorize and the Keycloak
// callback. It is keyed by the state we sent Keycloak, which is single use.
type pendingAuth struct {
	ClientID      string
	RedirectURI   string
	RedirectGiven bool   // the client named it, so /token must name it too
	ClientState   string // the client's state, echoed back untouched
	CodeChallenge string // the client's PKCE challenge, S256
	Scope         string
	Resource      string

	Nonce    string // OIDC nonce for the Keycloak leg
	Verifier string // our PKCE verifier for the Keycloak leg

	CreatedAt time.Time
	ExpiresAt time.Time
}

// authCode is an authorization code of ours, waiting at /token. It carries the
// identity Keycloak verified, so redeeming it needs no further network call.
type authCode struct {
	ClientID      string
	RedirectURI   string
	RedirectGiven bool
	CodeChallenge string
	Identity      session.Identity
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// addClient records a registration. c.ID is minted by the caller. The registry
// is bounded: registration is unauthenticated, so without a cap anyone who can
// reach /register can make this process hold memory until it dies. At the cap
// the oldest registration is dropped, which costs that connector one
// re-registration and one sign-in.
func (s *Server) addClient(c *client) error {
	if c == nil || c.ID == "" {
		return errors.New("oauthas: a client needs an id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for len(s.clients) >= MaxClients {
		var oldestID string
		var oldest time.Time
		for id, existing := range s.clients {
			if oldestID == "" || existing.CreatedAt.Before(oldest) {
				oldestID, oldest = id, existing.CreatedAt
			}
		}
		if oldestID == "" {
			break
		}
		delete(s.clients, oldestID)
	}
	s.clients[c.ID] = c
	return nil
}

// lookupClient returns a registered client. Registrations do not expire; the
// process restarting is what ends them.
func (s *Server) lookupClient(id string) (*client, error) {
	if id == "" {
		return nil, ErrUnknownClient
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[id]
	if !ok {
		return nil, ErrUnknownClient
	}
	return c, nil
}

// putAuth records an in-flight authorization under a fresh Keycloak state and
// returns that state.
func (s *Server) putAuth(p *pendingAuth) (string, error) {
	state, err := randomToken()
	if err != nil {
		return "", fmt.Errorf("oauthas: %w", err)
	}
	now := s.now()
	p.CreatedAt = now
	p.ExpiresAt = now.Add(s.cfg.AuthTTL)

	s.mu.Lock()
	defer s.mu.Unlock()
	for key, existing := range s.auths {
		if !now.Before(existing.ExpiresAt) {
			delete(s.auths, key)
		}
	}
	for len(s.auths) >= MaxPendingAuths {
		var oldestKey string
		var oldest time.Time
		for key, existing := range s.auths {
			if oldestKey == "" || existing.CreatedAt.Before(oldest) {
				oldestKey, oldest = key, existing.CreatedAt
			}
		}
		if oldestKey == "" {
			break
		}
		delete(s.auths, oldestKey)
	}
	s.auths[state] = p
	return state, nil
}

// takeAuth removes and returns an in-flight authorization. A state is single
// use: a replayed Keycloak callback finds nothing.
func (s *Server) takeAuth(state string) (*pendingAuth, error) {
	if state == "" {
		return nil, ErrUnknownCode
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.auths[state]
	if !ok {
		return nil, ErrUnknownCode
	}
	delete(s.auths, state)
	if !now.Before(p.ExpiresAt) {
		return nil, ErrUnknownCode
	}
	return p, nil
}

// putCode records a redeemable authorization code and returns it.
func (s *Server) putCode(c *authCode) (string, error) {
	code, err := randomToken()
	if err != nil {
		return "", fmt.Errorf("oauthas: %w", err)
	}
	now := s.now()
	c.CreatedAt = now
	c.ExpiresAt = now.Add(s.cfg.AuthTTL)

	s.mu.Lock()
	defer s.mu.Unlock()
	for key, existing := range s.codes {
		if !now.Before(existing.ExpiresAt) {
			delete(s.codes, key)
		}
	}
	s.codes[code] = c
	return code, nil
}

// takeCode removes and returns an authorization code. Single use, as the grant
// requires: the code is spent by being presented, whether or not the rest of
// the token request turns out to be good, so a leaked code cannot be retried
// against a guessed verifier.
func (s *Server) takeCode(code string) (*authCode, error) {
	if code == "" {
		return nil, ErrUnknownCode
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.codes[code]
	if !ok {
		return nil, ErrUnknownCode
	}
	delete(s.codes, code)
	if !now.Before(c.ExpiresAt) {
		return nil, ErrUnknownCode
	}
	return c, nil
}

// PKCE verifier lengths are RFC 7636's: 43 to 128 characters of the unreserved
// set. The challenge is the base64url SHA-256 of one, so it is always 43, but
// the range is checked rather than the exact length because a client that pads
// is still speaking S256.
const (
	minVerifierLen = 43
	maxVerifierLen = 128
)

// verifyPKCE checks a code verifier against the S256 challenge recorded at
// /authorize. Nothing else binds a code to the client that asked for it: a
// client registered through open registration has no secret at all.
func verifyPKCE(challenge, verifier string) error {
	if challenge == "" || !validVerifier(verifier) {
		return ErrBadVerifier
	}
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(want), []byte(challenge)) != 1 {
		return ErrBadVerifier
	}
	return nil
}

// validVerifier reports whether s is a syntactically valid code_verifier:
// 43-128 characters of ALPHA / DIGIT / "-" / "." / "_" / "~".
func validVerifier(s string) bool {
	if len(s) < minVerifierLen || len(s) > maxVerifierLen {
		return false
	}
	return onlyUnreserved(s)
}

// validChallenge reports whether s could be an S256 code_challenge. It is the
// same character set and length range as a verifier.
func validChallenge(s string) bool {
	if len(s) < minVerifierLen || len(s) > maxVerifierLen {
		return false
	}
	return onlyUnreserved(s)
}

func onlyUnreserved(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '-', c == '.', c == '_', c == '~':
		default:
			return false
		}
	}
	return true
}

// newVerifier mints the PKCE verifier for our own leg into Keycloak. That leg's
// PKCE is between this server and Keycloak and is not the MCP client's.
func newVerifier() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// randomToken is the source of every identifier this package mints: client
// ids, Keycloak state values, and authorization codes.
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// parseOrigin accepts scheme://host[:port] and nothing else. A single trailing
// slash is tolerated because it is the one mistake that is unambiguous.
func parseOrigin(raw string) (string, error) {
	s := strings.TrimRight(strings.TrimSpace(raw), "/")
	if s == "" {
		return "", errors.New("empty")
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return "", errors.New("scheme must be http or https")
	case u.Host == "":
		return "", errors.New("no host")
	case u.User != nil:
		return "", errors.New("must not contain credentials")
	case u.Path != "":
		return "", errors.New("must not contain a path")
	case u.RawQuery != "" || u.Fragment != "":
		return "", errors.New("must not contain a query or fragment")
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), nil
}
