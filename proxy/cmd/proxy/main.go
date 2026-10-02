// Command cms-auth-proxy lets Decap CMS use Prodeko accounts instead of GitHub
// accounts.
//
// It serves three things:
//
//	GET  /auth        start the Keycloak sign-in (Decap's popup opens here)
//	GET  /callback    finish it and hand a session token back by postMessage
//	ANY  /github/*    the GitHub API, with our credential and the editor's name
//
// The four rules the whole service exists to enforce live in the packages
// below; this file only wires them together in the one order that keeps them
// true: CORS answers preflight, the session middleware proves who is calling,
// the role check proves they may edit, and only then does forward talk to
// GitHub with a credential the browser has never seen.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prodeko/prodeko-org/proxy/internal/auth"
	"github.com/prodeko/prodeko-org/proxy/internal/config"
	"github.com/prodeko/prodeko-org/proxy/internal/forward"
	"github.com/prodeko/prodeko-org/proxy/internal/session"
)

// Exit codes. 2 means "your environment is wrong", which is worth telling
// apart from "something failed at runtime" when this is read out of a
// container that restarted in a loop.
const (
	exitStartupFailure = 1
	exitBadConfig      = 2
)

// Server timeouts. WriteTimeout has to outlast the upstream call forward makes
// (50s), or a slow GitHub turns into a truncated response to the browser
// instead of the 502 forward would have written.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 60 * time.Second
	writeTimeout      = 70 * time.Second
	idleTimeout       = 120 * time.Second
	shutdownTimeout   = 20 * time.Second
	maxHeaderBytes    = 1 << 20
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		// No logger yet, and a configuration error is for a human reading
		// `docker logs`, not for a log aggregator.
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "\nThe proxy will not start with a partial configuration. "+
			"Every variable is documented in .env.example.")
		os.Exit(exitBadConfig)
	}

	log := newLogger(cfg.LogLevel)
	slog.SetDefault(log)

	if err := run(cfg, log); err != nil {
		log.Error("proxy stopped", "err", err)
		os.Exit(exitStartupFailure)
	}
}

func newLogger(level slog.Level) *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func run(cfg *config.Config, log *slog.Logger) error {
	// Signals are caught before anything is built, so a Ctrl-C during the
	// blocking OIDC discovery below actually stops the process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	for _, line := range strings.Split(cfg.String(), "\n") {
		log.Info(line)
	}
	for _, w := range cfg.Warnings {
		log.Warn(w)
	}
	if cfg.SplitHorizon() {
		log.Warn("split-horizon Keycloak: discovery and issuer are different URLs; correct only in the dev stack",
			"discovery_url", cfg.Keycloak.DiscoveryURL, "issuer", cfg.Keycloak.Issuer)
	}

	sessions, err := session.NewStore(session.Options{
		Secret: cfg.Session.Secret,
		TTL:    cfg.Session.TTL,
	})
	if err != nil {
		return err
	}

	// Discovery is a network call, so an unreachable or misconfigured Keycloak
	// stops the proxy here rather than at the first editor's sign-in.
	authHandler, err := auth.New(ctx, auth.Config{
		Issuer:       cfg.Keycloak.Issuer,
		DiscoveryURL: cfg.Keycloak.DiscoveryURL,
		ClientID:     cfg.Keycloak.ClientID,
		ClientSecret: cfg.Keycloak.ClientSecret,
		EditorRoles:  cfg.Keycloak.EditorRoles,
		PublicURL:    cfg.PublicURL,
		CMSOrigins:   cfg.CMSOrigins,
		Scopes:       cfg.Keycloak.Scopes,
		Logger:       log.With("component", "auth"),
	}, sessions)
	if err != nil {
		return err
	}

	apiRoot, err := url.Parse(cfg.GitHub.APIRoot)
	if err != nil {
		return fmt.Errorf("%s: %w", config.EnvGitHubAPI, err)
	}
	publicBase, err := url.Parse(cfg.PublicURL)
	if err != nil {
		return fmt.Errorf("%s: %w", config.EnvPublicURL, err)
	}

	github, err := forward.New(forward.Config{
		Owner:       cfg.GitHub.Owner,
		Repo:        cfg.GitHub.Repo,
		Branch:      cfg.GitHub.Branch,
		Token:       cfg.GitHub.Token,
		EditorRoles: cfg.Keycloak.EditorRoles,
		Committer: forward.Author{
			Name:  cfg.GitHub.CommitterName,
			Email: cfg.GitHub.CommitterEmail,
		},
		APIRoot:    apiRoot,
		PublicBase: publicBase,
		Prefix:     githubPrefix,
		Logger:     log.With("component", "forward"),
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           routes(cfg, log, authHandler, sessions, github),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       idleTimeout,
		MaxHeaderBytes:    maxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	return serve(ctx, srv, log)
}

// githubPrefix must equal the path of Decap's api_root. With
// `api_root: https://cms.prodeko.org/github` this is what the browser calls,
// and the mux below hands the whole path through unstripped because forward
// strips the prefix itself.
const githubPrefix = "/github"

// registrar is the slice of [auth.Handler] the routing table needs. Naming it
// is what lets the wiring be tested without a live Keycloak to discover.
type registrar interface {
	Register(*http.ServeMux)
}

func routes(
	cfg *config.Config,
	log *slog.Logger,
	signin registrar,
	sessions session.Verifier,
	github http.Handler,
) http.Handler {
	mux := http.NewServeMux()
	signin.Register(mux)

	// Order matters. CORS is outermost so that a preflight, which carries no
	// Authorization header by definition, is answered instead of 401'd.
	// RequireRoles is redundant with the check inside forward and with the one
	// at sign-in; rule 1 is cheap enough to state three times.
	editors := cors(cfg.CMSOrigins, log,
		session.Middleware(sessions)(
			session.RequireRoles(cfg.Keycloak.EditorRoles)(github)))
	mux.Handle(githubPrefix+"/", editors)
	mux.Handle(githubPrefix, editors)

	// Liveness only: it says the process is listening, not that Keycloak or
	// GitHub can be reached. It is deliberately unauthenticated and says
	// nothing about the configuration.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})

	return logRequests(log, mux)
}

// cors permits exactly the origins the editing screen is served from. "*" is
// not an option: these responses are made against a bearer token and the
// browser is told it may read them.
func cors(origins []string, log *slog.Logger, next http.Handler) http.Handler {
	allowed := make(map[string]bool, len(origins))
	for _, o := range origins {
		allowed[o] = true
	}
	allowMethods := strings.Join([]string{
		http.MethodGet, http.MethodHead, http.MethodPost,
		http.MethodPatch, http.MethodPut, http.MethodDelete, http.MethodOptions,
	}, ", ")
	allowHeaders := strings.Join(forward.CORSRequestHeaders, ", ")
	exposeHeaders := strings.Join(forward.CORSExposeHeaders, ", ")

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		// Every response varies by Origin, including the ones that get no CORS
		// headers at all, or a shared cache will serve one origin's answer to
		// another.
		w.Header().Add("Vary", "Origin")

		switch {
		case origin == "":
			// Not a browser cross-origin call: curl, a health check, or a
			// same-origin fetch. Nothing to allow.
		case allowed[origin]:
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Set("Access-Control-Expose-Headers", exposeHeaders)
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", allowMethods)
				h.Set("Access-Control-Allow-Headers", allowHeaders)
				h.Set("Access-Control-Max-Age", "600")
			}
		default:
			// Answer without the headers and let the browser refuse it, but
			// say so in the log: a missing origin in CMS_ORIGINS looks to the
			// editor like the CMS silently doing nothing.
			log.Warn("cross-origin request from an origin that is not in CMS_ORIGINS",
				"origin", origin, "path", r.URL.EscapedPath(), "env", config.EnvCMSOrigins)
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// logRequests is one line per request at debug level. The Authorization header
// is never touched, so nothing here can print a token.
func logRequests(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !log.Enabled(r.Context(), slog.LevelDebug) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Debug("request",
			"method", r.Method,
			"path", r.URL.EscapedPath(),
			"status", rec.status,
			"duration", time.Since(start).Round(time.Millisecond),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *statusRecorder) WriteHeader(status int) {
	if !r.wroteHeader {
		r.status = status
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(status)
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
		// A save that was mid-flight to GitHub is worth naming: the commit may
		// have landed even though the editor saw an error.
		log.Error("shutdown timed out with requests still in flight", "err", err)
		_ = srv.Close()
		return err
	}
	log.Info("stopped")
	return <-errs
}
