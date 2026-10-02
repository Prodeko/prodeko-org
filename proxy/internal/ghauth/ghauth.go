// Package ghauth supplies the credential both services put on their GitHub
// calls: either a fixed personal access token, or a GitHub App installation
// token minted on demand and renewed before it expires.
//
// An installation token lives for one hour, so it cannot be read once at
// startup the way a personal access token can. Callers ask a Source for a
// token on every request and never hold on to the answer.
package ghauth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Source hands out the token for the next GitHub call.
type Source interface {
	Token(ctx context.Context) (string, error)
}

// Static is a personal access token, the same on every call.
type Static string

func (s Static) Token(context.Context) (string, error) {
	if s == "" {
		return "", errors.New("ghauth: empty token")
	}
	return string(s), nil
}

// DefaultAPIRoot is the GitHub REST root used when AppConfig.APIRoot is empty.
const DefaultAPIRoot = "https://api.github.com"

const (
	// renewBefore is how long before expiry a cached installation token is
	// replaced. A forwarded request can run for close to a minute and a git
	// push longer, so the margin is several times either.
	renewBefore = 5 * time.Minute

	// jwtLifetime is under GitHub's ten-minute ceiling, and jwtBackdate absorbs
	// a clock on this host running ahead of GitHub's, which GitHub otherwise
	// answers with "'Issued at' claim must be an Integer representing a time in
	// the past".
	jwtLifetime = 9 * time.Minute
	jwtBackdate = 60 * time.Second

	apiVersion   = "2022-11-28"
	userAgent    = "prodeko-org-ghauth"
	maxRespBytes = 1 << 20
)

// AppConfig identifies a GitHub App and the one repository it acts on.
type AppConfig struct {
	AppID      string          // GITHUB_APP_ID, the numeric App ID
	PrivateKey *rsa.PrivateKey // from GITHUB_APP_PRIVATE_KEY
	Owner      string
	Repo       string

	APIRoot string       // "" means DefaultAPIRoot
	Client  *http.Client // nil means a client with a 30 second timeout
	Now     func() time.Time
}

// App mints installation tokens for one repository and caches each until
// renewBefore ahead of its expiry. It is safe for concurrent use; callers
// that arrive while a token is being minted wait for that one rather than
// minting their own.
type App struct {
	cfg AppConfig

	mu             sync.Mutex
	installationID int64
	token          string
	expires        time.Time
}

// NewApp validates cfg. It touches no network: the installation is looked up
// on the first Token call.
func NewApp(cfg AppConfig) (*App, error) {
	var missing []string
	if strings.TrimSpace(cfg.AppID) == "" {
		missing = append(missing, "AppID")
	}
	if cfg.PrivateKey == nil {
		missing = append(missing, "PrivateKey")
	}
	if cfg.Owner == "" {
		missing = append(missing, "Owner")
	}
	if cfg.Repo == "" {
		missing = append(missing, "Repo")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("ghauth: missing %s", strings.Join(missing, ", "))
	}
	if strings.ContainsAny(cfg.Owner+cfg.Repo, "/?#") {
		return nil, errors.New("ghauth: Owner and Repo must be bare names")
	}
	if cfg.APIRoot == "" {
		cfg.APIRoot = DefaultAPIRoot
	}
	cfg.APIRoot = strings.TrimSuffix(cfg.APIRoot, "/")
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &App{cfg: cfg}, nil
}

// Token returns a cached installation token, or mints a new one when the
// cached one is missing or close to expiry.
func (a *App) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.cfg.Now()
	if a.token != "" && now.Before(a.expires.Add(-renewBefore)) {
		return a.token, nil
	}

	jwt, err := a.jwt(now)
	if err != nil {
		return "", err
	}
	if a.installationID == 0 {
		id, err := a.lookupInstallation(ctx, jwt)
		if err != nil {
			return "", err
		}
		a.installationID = id
	}
	token, expires, err := a.mint(ctx, jwt)
	if err != nil {
		// An uninstalled and reinstalled App gets a new installation ID, and
		// the old one answers 404. Look it up again next time rather than
		// failing until a restart.
		var se *statusError
		if errors.As(err, &se) && se.status == http.StatusNotFound {
			a.installationID = 0
		}
		return "", err
	}
	a.token, a.expires = token, expires
	return token, nil
}

// lookupInstallation finds the installation that covers the repository, so
// the installation ID never has to be configured.
func (a *App) lookupInstallation(ctx context.Context, jwt string) (int64, error) {
	path := "/repos/" + url.PathEscape(a.cfg.Owner) + "/" + url.PathEscape(a.cfg.Repo) + "/installation"
	var res struct {
		ID int64 `json:"id"`
	}
	if err := a.call(ctx, http.MethodGet, path, jwt, nil, &res); err != nil {
		return 0, fmt.Errorf("ghauth: finding the App's installation on %s/%s (is the App installed there?): %w",
			a.cfg.Owner, a.cfg.Repo, err)
	}
	if res.ID == 0 {
		return 0, fmt.Errorf("ghauth: GitHub returned no installation for %s/%s", a.cfg.Owner, a.cfg.Repo)
	}
	return res.ID, nil
}

// mint asks for a token narrowed to the one repository, whatever else the
// installation was granted.
func (a *App) mint(ctx context.Context, jwt string) (string, time.Time, error) {
	path := "/app/installations/" + strconv.FormatInt(a.installationID, 10) + "/access_tokens"
	body := map[string]any{"repositories": []string{a.cfg.Repo}}
	var res struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := a.call(ctx, http.MethodPost, path, jwt, body, &res); err != nil {
		return "", time.Time{}, fmt.Errorf("ghauth: minting an installation token: %w", err)
	}
	if res.Token == "" || res.ExpiresAt.IsZero() {
		return "", time.Time{}, errors.New("ghauth: GitHub returned an installation token without a value or an expiry")
	}
	return res.Token, res.ExpiresAt, nil
}

// jwt is the RS256 token that authenticates as the App itself, which is good
// for nothing but the two calls above.
func (a *App) jwt(now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": a.cfg.AppID,
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + enc.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.cfg.PrivateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("ghauth: signing the App JWT: %w", err)
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

type statusError struct {
	status  int
	message string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("GitHub answered %d: %s", e.status, e.message)
}

func (a *App) call(ctx context.Context, method, path, jwt string, body, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.APIRoot+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := a.cfg.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var msg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &msg)
		if msg.Message == "" {
			msg.Message = http.StatusText(resp.StatusCode)
		}
		return &statusError{status: resp.StatusCode, message: msg.Message}
	}
	return json.Unmarshal(data, out)
}

// ParsePrivateKey reads GITHUB_APP_PRIVATE_KEY: the .pem GitHub hands out, as
// is or base64-encoded. The base64 form exists because a multi-line value is
// awkward in an env file, and either form decodes to the same key.
func ParsePrivateKey(raw string) (*rsa.PrivateKey, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("is empty")
	}
	data := []byte(raw)
	if !strings.HasPrefix(raw, "-----BEGIN") {
		decoded, err := base64.StdEncoding.DecodeString(raw)
		if err != nil {
			return nil, errors.New("is neither a PEM block nor base64 of one")
		}
		data = decoded
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("contains no PEM block")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("is not an RSA private key in PKCS #1 or PKCS #8 form")
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("is a private key, but not an RSA one")
	}
	return key, nil
}
