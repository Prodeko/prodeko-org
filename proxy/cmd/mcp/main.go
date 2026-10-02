// Command prodeko-content-mcp lets the media team edit prodeko.org from the
// Claude they already use.
//
// It serves four things:
//
//	POST /mcp                   the MCP streamable HTTP transport, sixteen tools
//	GET  /.well-known/oauth-*   the OAuth discovery documents
//	GET|POST /upload            the token-authorized image upload
//	GET  /healthz               liveness
//
// plus the authorization server's own /register, /authorize, /oauth/callback
// and /token. The server never calls a model: the media person's Claude does
// the editing, and this process is hands and guardrails. What contains the
// model is the fence, the media/<user>/ branch namespace, and a maintainer's
// review before anything merges.
//
// This file only wires the packages together, in the one order that keeps
// those boundaries true: the bearer token proves who is calling before any
// tool runs, the fence decides what that person can see, and git is the only
// way anything leaves the process.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/prodeko/prodeko-org/proxy/internal/mcpserver"
	"github.com/prodeko/prodeko-org/proxy/internal/oauthas"
	"github.com/prodeko/prodeko-org/proxy/internal/preview"
	"github.com/prodeko/prodeko-org/proxy/internal/session"
	"github.com/prodeko/prodeko-org/proxy/internal/toolset"
	"github.com/prodeko/prodeko-org/proxy/internal/upload"
	"github.com/prodeko/prodeko-org/proxy/internal/workdir"
)

// Exit codes. 2 means "your environment is wrong", which is worth telling
// apart from "something failed at runtime" when this is read out of a
// container that restarted in a loop.
const (
	exitStartupFailure = 1
	exitBadConfig      = 2
)

// Server timeouts. WriteTimeout has to outlast a build, which the worktree
// package bounds at 30 seconds, or a slow hugo becomes a truncated response
// instead of the error the model was supposed to read.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second
	writeTimeout      = 120 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 30 * time.Second
	maxHeaderBytes    = 1 << 20
)

// serverName and serverVersion are the MCP serverInfo. The name is what a
// connector shows the person who added it.
const (
	serverName    = "prodeko-content-editor"
	serverVersion = "0.1.0"
)

func main() {
	cfg, err := loadEnv(os.LookupEnv)
	if err != nil {
		// No logger yet, and a configuration error is for a human reading
		// `docker logs`, not for a log aggregator.
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "\nThe MCP server will not start with a partial configuration.")
		os.Exit(exitBadConfig)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(log)

	if err := run(cfg, log); err != nil {
		log.Error("mcp server stopped", "err", err)
		os.Exit(exitStartupFailure)
	}
}

func run(cfg *env, log *slog.Logger) error {
	// Signals are caught before anything is built, so a Ctrl-C during startup
	// actually stops the process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for _, line := range strings.Split(cfg.String(), "\n") {
		log.Info(line)
	}
	if cfg.DevBearer != "" {
		log.Warn("MCP_DEV_BEARER is set: one fixed token authenticates as the dev editor. " +
			"This is for the local demo and must never be set in production")
	}

	sessions, err := sessionStore(cfg)
	if err != nil {
		return err
	}

	keycloak, err := upstream(ctx, cfg, log)
	if err != nil {
		return err
	}

	as, err := oauthas.New(oauthas.Config{
		PublicURL:     cfg.PublicURL,
		ResourcePath:  mcpserver.Path,
		Keycloak:      keycloak,
		Sessions:      sessions,
		Tokens:        sessions,
		RequiredRoles: cfg.RequiredRoles,
		AccessContact: cfg.AccessContact,
		Logger:        log.With("component", "oauthas"),
	})
	if err != nil {
		return err
	}

	work, err := workdir.New(workdir.Config{
		RepoPath:    cfg.RepoPath,
		StateDir:    cfg.StateDir,
		Committer:   workdir.Author{Name: cfg.CommitterName, Email: cfg.CommitterEmail},
		GitHubToken: cfg.GitHubToken,
		GitHubRepo:  cfg.GitHubRepo,
		Logger:      log.With("component", "workdir"),
	})
	if err != nil {
		return err
	}
	if work.DryRun() {
		log.Warn("GITHUB_TOKEN or GITHUB_REPO is unset: submit will push to the local origin only and open no pull request")
	}

	// The upload tokens sign with the session secret: one secret, one
	// process, and a token that outlives a restart was going to expire in
	// fifteen minutes anyway.
	uploads, err := upload.NewTokens([]byte(cfg.SessionSecret), nil)
	if err != nil {
		return err
	}

	tools, err := toolset.New(toolset.Config{
		Workdir:     work,
		ChromiumBin: cfg.ChromiumBin,
		Uploads:     uploads,
		PublicURL:   cfg.PublicURL,
		Logger:      log.With("component", "toolset"),
	})
	if err != nil {
		return err
	}
	if bin, err := (preview.Shooter{Bin: cfg.ChromiumBin}).Find(); err != nil {
		log.Warn("no headless Chromium: screenshot will refuse every call", "err", err)
	} else {
		log.Info("headless Chromium", "bin", bin)
	}

	mcp, err := mcpserver.New(mcpserver.Config{
		Name:                serverName,
		Version:             serverVersion,
		Instructions:        tools.Instructions(),
		Tools:               tools.Tools(),
		Prompts:             toolset.Prompts(),
		Resources:           toolset.UIResources(cfg.PublicURL),
		Authenticate:        authenticator(cfg, as),
		ResourceMetadataURL: as.ResourceMetadataURL(),
		Logger:              log.With("component", "mcpserver"),
	})
	if err != nil {
		return err
	}

	up := &upload.Handler{
		Tokens: uploads,
		Save:   tools.SaveImage,
		Log:    log.With("component", "upload"),
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           routes(as, mcp, up),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	return serve(ctx, srv, log)
}

// upstream is the Keycloak login the authorization server wraps.
//
// There is one configuration where it is absent: the local demo, where
// MCP_DEV_BEARER is the only credential and no realm exists to sign in to. The
// authorization server is still mounted there, because its two discovery
// documents are part of what a connector reads, but /authorize has nowhere to
// send a browser and says so.
func upstream(ctx context.Context, cfg *env, log *slog.Logger) (oauthas.Keycloak, error) {
	if cfg.KeycloakIssuer == "" {
		log.Warn("KEYCLOAK_ISSUER is unset: sign-in is unavailable and MCP_DEV_BEARER is the only credential")
		return signInUnavailable{}, nil
	}
	return oauthas.NewKeycloak(ctx, oauthas.KeycloakConfig{
		Issuer:       cfg.KeycloakIssuer,
		DiscoveryURL: cfg.KeycloakDiscoveryURL,
		ClientID:     cfg.KeycloakClientID,
		ClientSecret: cfg.KeycloakClientSecret,
		RedirectURI:  cfg.PublicURL + oauthas.CallbackPath,
	})
}

// signInUnavailable stands in for Keycloak when no realm is configured. The
// empty authorization URL is the [oauthas.Keycloak] contract's way of saying
// sign-in is unavailable; /authorize turns it into a temporarily_unavailable
// error rather than a redirect to nowhere.
type signInUnavailable struct{}

func (signInUnavailable) AuthCodeURL(state, nonce, verifier string) string { return "" }

func (signInUnavailable) Exchange(context.Context, string, string, string) (session.Identity, error) {
	return session.Identity{}, errors.New("no Keycloak realm is configured")
}

// sessionStore mints and verifies the access tokens the authorization server
// issues. The lifetime is the token lifetime: there is no refresh grant in the
// MVP, so a connector signs in again when it runs out.
func sessionStore(cfg *env) (*session.Store, error) {
	return session.NewStore(session.Options{
		Secret: []byte(cfg.SessionSecret),
		TTL:    oauthas.DefaultTokenTTL,
	})
}

// registrar is the slice of the authorization server and the transport the
// routing table needs. Naming it is what lets the wiring be tested without a
// Keycloak to discover or a clone on disk.
type registrar interface {
	Register(*http.ServeMux)
}

func routes(as, mcp, up registrar) http.Handler {
	mux := http.NewServeMux()

	// The authorization server first: its two well-known documents and its
	// flow endpoints are unauthenticated by definition, the transport
	// authenticates itself, and the upload endpoint's single-use token is
	// its whole session.
	as.Register(mux)
	mcp.Register(mux)
	up.Register(mux)

	// Liveness only: the process is listening. It says nothing about Keycloak,
	// GitHub or the clone, and it is deliberately unauthenticated so the
	// monitoring does not need a credential.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})

	return mux
}

// devIdentity is who MCP_DEV_BEARER authenticates as. It holds no roles,
// because the role conjunction is checked where a Keycloak identity is minted
// and this identity never passes through there.
var devIdentity = mcpserver.Identity{
	Username: "dev-editor",
	Name:     "Dev Editor",
	Email:    "dev@prodeko.org",
}

// authenticator is how a bearer token becomes an identity: the dev token when
// one is configured, otherwise a sealed session issued by our own
// authorization server.
func authenticator(cfg *env, as *oauthas.Server) mcpserver.Authenticator {
	oauth := func(bearer string) (mcpserver.Identity, error) {
		id, err := as.Authenticate(bearer)
		if err != nil {
			return mcpserver.Identity{}, err
		}
		return mcpserver.Identity{
			Username: id.Username,
			Name:     id.Name,
			Email:    id.Email,
		}, nil
	}
	if cfg.DevBearer == "" {
		return oauth
	}
	return mcpserver.FirstOf(mcpserver.StaticBearer(cfg.DevBearer, devIdentity), oauth)
}

// env is the whole environment contract of this binary.
type env struct {
	ListenAddr string
	PublicURL  string
	LogLevel   slog.Level

	RepoPath string
	StateDir string

	// ChromiumBin is empty in the server image, where the browser is installed
	// under a name the preview package already looks for.
	ChromiumBin string

	KeycloakIssuer       string
	KeycloakDiscoveryURL string
	KeycloakClientID     string
	KeycloakClientSecret string
	RequiredRoles        []string
	AccessContact        string

	SessionSecret string
	DevBearer     string

	GitHubToken string
	GitHubRepo  string

	CommitterName  string
	CommitterEmail string
}

// Environment variable names, in one place so the error messages and the
// deployment role cannot drift apart.
const (
	envListenAddr     = "LISTEN_ADDR"
	envPublicURL      = "PUBLIC_URL"
	envLogLevel       = "LOG_LEVEL"
	envRepoPath       = "MCP_REPO_PATH"
	envStateDir       = "MCP_STATE_DIR"
	envDevBearer      = "MCP_DEV_BEARER"
	envChromiumBin    = "MCP_CHROMIUM_BIN"
	envIssuer         = "KEYCLOAK_ISSUER"
	envDiscoveryURL   = "KEYCLOAK_DISCOVERY_URL"
	envClientID       = "KEYCLOAK_CLIENT_ID"
	envClientSecret   = "KEYCLOAK_CLIENT_SECRET"
	envRequiredRoles  = "MCP_REQUIRED_ROLES"
	envAccessContact  = "MCP_ACCESS_CONTACT"
	envSessionSecret  = "SESSION_SECRET"
	envGitHubToken    = "GITHUB_TOKEN"
	envGitHubRepo     = "GITHUB_REPO"
	envCommitterName  = "GIT_COMMITTER_NAME"
	envCommitterEmail = "GIT_COMMITTER_EMAIL"
)

// Defaults applied when a variable is absent.
const (
	defaultListenAddr     = "0.0.0.0:8093"
	defaultCommitterName  = "Prodeko media bot"
	defaultCommitterEmail = "media-bot@prodeko.org"
)

// defaultRequiredRoles is the conjunction from the design: the hand-granted
// media role and the automatically maintained membership role, both of them.
var defaultRequiredRoles = []string{"prodeko-org-media", "membership"}

// loadEnv reports every problem it finds, not just the first. A half
// configured authorization server must refuse to start.
func loadEnv(lookup func(string) (string, bool)) (*env, error) {
	var problems []string
	fail := func(name, reason string) { problems = append(problems, name+": "+reason) }

	get := func(name string) string {
		v, _ := lookup(name)
		return strings.TrimSpace(v)
	}
	required := func(name string) string {
		v := get(name)
		if v == "" {
			fail(name, "must be set")
		}
		return v
	}
	withDefault := func(name, def string) string {
		if v := get(name); v != "" {
			return v
		}
		return def
	}

	// Keycloak is required for everything except the local demo, where
	// MCP_DEV_BEARER is the only credential and there is no realm to sign in
	// to. Demanding an issuer there would mean inventing a fake one to get the
	// process to start, which is worse than naming the exception here.
	devBearer := get(envDevBearer)
	keycloakVar := required
	if devBearer != "" {
		keycloakVar = get
	}

	cfg := &env{
		ListenAddr:           withDefault(envListenAddr, defaultListenAddr),
		PublicURL:            strings.TrimRight(required(envPublicURL), "/"),
		RepoPath:             required(envRepoPath),
		StateDir:             required(envStateDir),
		ChromiumBin:          get(envChromiumBin),
		KeycloakIssuer:       keycloakVar(envIssuer),
		KeycloakDiscoveryURL: get(envDiscoveryURL),
		KeycloakClientID:     keycloakVar(envClientID),
		KeycloakClientSecret: get(envClientSecret),
		SessionSecret:        required(envSessionSecret),
		DevBearer:            devBearer,
		GitHubToken:          get(envGitHubToken),
		GitHubRepo:           get(envGitHubRepo),
		AccessContact:        get(envAccessContact),
		CommitterName:        withDefault(envCommitterName, defaultCommitterName),
		CommitterEmail:       withDefault(envCommitterEmail, defaultCommitterEmail),
	}

	if n := len(cfg.SessionSecret); n > 0 && n < session.MinSecretLen {
		fail(envSessionSecret, fmt.Sprintf("must be at least %d bytes, got %d", session.MinSecretLen, n))
	}
	if cfg.PublicURL != "" && !strings.HasPrefix(cfg.PublicURL, "https://") && !strings.HasPrefix(cfg.PublicURL, "http://") {
		fail(envPublicURL, "must be an absolute http(s) origin such as https://edit.prodeko.org")
	}
	if cfg.GitHubRepo != "" && strings.Count(cfg.GitHubRepo, "/") != 1 {
		fail(envGitHubRepo, "must be owner/repo")
	}

	cfg.RequiredRoles = splitRoles(get(envRequiredRoles))
	if len(cfg.RequiredRoles) == 0 {
		cfg.RequiredRoles = defaultRequiredRoles
	}

	level, err := parseLevel(get(envLogLevel))
	if err != nil {
		fail(envLogLevel, err.Error())
	}
	cfg.LogLevel = level

	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, errors.New("configuration:\n  " + strings.Join(problems, "\n  "))
	}
	return cfg, nil
}

func splitRoles(raw string) []string {
	var roles []string
	seen := make(map[string]bool)
	for _, part := range strings.Split(raw, ",") {
		role := strings.TrimSpace(part)
		if role == "" || seen[role] {
			continue
		}
		seen[role] = true
		roles = append(roles, role)
	}
	return roles
}

func parseLevel(raw string) (slog.Level, error) {
	if raw == "" {
		return slog.LevelInfo, nil
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(raw)); err != nil {
		return slog.LevelInfo, errors.New("must be one of debug, info, warn, error")
	}
	return level, nil
}

// String is the startup banner. Secrets are named, never printed.
func (e *env) String() string {
	const redacted = "[redacted]"
	secret := func(v string) string {
		if v == "" {
			return "[unset]"
		}
		return redacted
	}
	return strings.Join([]string{
		"configuration:",
		"  listen_addr    " + e.ListenAddr,
		"  public_url     " + e.PublicURL,
		"  repo_path      " + e.RepoPath,
		"  state_dir      " + e.StateDir,
		"  chromium_bin   " + orUnset(e.ChromiumBin),
		"  issuer         " + e.KeycloakIssuer,
		"  client_id      " + e.KeycloakClientID,
		"  client_secret  " + secret(e.KeycloakClientSecret),
		"  required_roles " + strings.Join(e.RequiredRoles, ", "),
		"  session_secret " + secret(e.SessionSecret),
		"  dev_bearer     " + secret(e.DevBearer),
		"  github_repo    " + orUnset(e.GitHubRepo),
		"  github_token   " + secret(e.GitHubToken),
		"  committer      " + e.CommitterName + " <" + e.CommitterEmail + ">",
	}, "\n")
}

func orUnset(v string) string {
	if v == "" {
		return "[unset]"
	}
	return v
}

// serve runs srv until the context is cancelled, then drains it.
func serve(ctx context.Context, srv *http.Server, log *slog.Logger) error {
	errs := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr)
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errs <- err
	}()

	select {
	case err := <-errs:
		return err
	case <-ctx.Done():
	}

	log.Info("shutting down", "grace", shutdownTimeout)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// A submit that was mid-flight is worth naming: the push may have
		// landed even though the caller saw an error.
		log.Error("shutdown timed out with requests still in flight", "err", err)
		_ = srv.Close()
		return err
	}
	log.Info("stopped")
	return <-errs
}
