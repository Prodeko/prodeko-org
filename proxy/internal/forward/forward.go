// Package forward proxies Decap CMS's GitHub API calls to api.github.com using
// a server-side bot credential, pinned to one repository, with the commit
// author replaced by the verified Keycloak identity.
//
// Four rules hold everywhere in here:
//
//  1. Every request needs a session carrying every configured realm role.
//  2. Every commit is authored by that session's name and email. Nothing the
//     browser sends about authorship is trusted.
//  3. Owner and repo come from config and are substituted into the forwarded
//     path. The owner and repo the browser asked for are discarded.
//  4. The GitHub credential is attached here and never travels to the browser.
//     The caller's Authorization header is never forwarded upstream.
package forward

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prodeko/prodeko-org/proxy/internal/session"
)

const (
	defaultPrefix  = "/github"
	defaultAPIRoot = "https://api.github.com"
	defaultTimeout = 50 * time.Second

	// Decap aborts its own fetch after 60 seconds, so the upstream call has to
	// give up before that for the error to be ours rather than a dead socket.
	defaultMaxBodyBytes = 32 << 20 // media uploads arrive base64 in a JSON blob

	githubAPIVersion = "2022-11-28"
	userAgent        = "prodeko-cms-auth-proxy"
	jsonContentType  = "application/json; charset=utf-8"
)

// CORSRequestHeaders and CORSExposeHeaders are what the CORS middleware in
// front of this handler has to permit. Decap sets Content-Type on every
// request including GETs, so every call is preflighted; the session token
// travels in Authorization because cross-origin cookies are not sent.
var (
	CORSRequestHeaders = []string{"Authorization", "Content-Type", "Accept", "If-None-Match"}
	CORSExposeHeaders  = []string{"Link", "ETag", "X-RateLimit-Remaining", "X-RateLimit-Reset"}
)

// responseHeaders is the allowlist of headers relayed back to the browser.
// Everything else GitHub sends — Location, X-OAuth-Scopes, X-GitHub-Request-Id
// and the rest — is dropped rather than leaked. Link is rewritten, not copied.
// The keys are canonicalised on the way in, because http.CanonicalHeaderKey
// turns ETag into Etag and X-RateLimit-Reset into X-Ratelimit-Reset.
var responseHeaders = canonicalSet(
	"Content-Type",
	"ETag",
	"Last-Modified",
	"Retry-After",
	"X-RateLimit-Limit",
	"X-RateLimit-Remaining",
	"X-RateLimit-Reset",
	"X-RateLimit-Used",
	"X-RateLimit-Resource",
)

func canonicalSet(names ...string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[http.CanonicalHeaderKey(name)] = true
	}
	return set
}

// Config pins the repository and holds the credential. APIRoot, Prefix, Client
// and Logger default when zero; every other field is required.
type Config struct {
	Owner  string // GITHUB_OWNER, substituted into every forwarded path
	Repo   string // GITHUB_REPO
	Branch string // GITHUB_BRANCH, the only ref writable outside cms/*
	Token  string // GITHUB_TOKEN, never sent to the browser

	// EditorRoles are the realm roles from EDITOR_ROLES, all of which are
	// re-checked on every forwarded request. At least one is required.
	EditorRoles []string

	Committer Author // bot identity recorded as the git committer

	APIRoot      *url.URL     // nil => https://api.github.com
	PublicBase   *url.URL     // PUBLIC_URL; nil => paginated Link headers are dropped
	Prefix       string       // "" => "/github"; must match Decap's api_root path
	Client       *http.Client // nil => &http.Client{Timeout: 50 * time.Second}
	Logger       *slog.Logger // nil => slog.Default()
	MaxBodyBytes int64        // 0 => 32 MiB
}

// Handler serves GET {prefix}/user and ANY {prefix}/repos/{owner}/{repo}/...
// It expects a session.Identity in the request context; without one it is a 401.
type Handler struct {
	owner       string
	repo        string
	branch      string
	token       string
	editorRoles []string
	committer   Author

	apiRoot    *url.URL
	publicBase *url.URL
	prefix     string
	client     *http.Client
	log        *slog.Logger
	maxBody    int64

	// publicRepoPrefix is where a rewritten pagination link points.
	publicRepoPrefix string
}

// New validates cfg and returns the handler for everything under cfg.Prefix.
func New(cfg Config) (*Handler, error) {
	missing := []string{}
	for name, value := range map[string]string{
		"Owner":  cfg.Owner,
		"Repo":   cfg.Repo,
		"Branch": cfg.Branch,
		"Token":  cfg.Token,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	// An empty role set would wave every signed-in Prodeko account through, so
	// it is a configuration error rather than a permissive default.
	editorRoles := make([]string, 0, len(cfg.EditorRoles))
	for _, role := range cfg.EditorRoles {
		if role = strings.TrimSpace(role); role != "" {
			editorRoles = append(editorRoles, role)
		}
	}
	if len(editorRoles) == 0 {
		missing = append(missing, "EditorRoles")
	}
	if !cfg.Committer.valid() {
		missing = append(missing, "Committer")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("forward: missing required config: %s", strings.Join(missing, ", "))
	}
	if strings.ContainsAny(cfg.Owner+cfg.Repo, "/?#") {
		return nil, errors.New("forward: Owner and Repo must be bare names")
	}

	apiRoot := cfg.APIRoot
	if apiRoot == nil {
		parsed, err := url.Parse(defaultAPIRoot)
		if err != nil {
			return nil, err
		}
		apiRoot = parsed
	}
	if apiRoot.Scheme == "" || apiRoot.Host == "" {
		return nil, errors.New("forward: APIRoot needs a scheme and a host")
	}
	if apiRoot.RawQuery != "" || apiRoot.Fragment != "" {
		return nil, errors.New("forward: APIRoot must not carry a query or fragment")
	}

	prefix := cfg.Prefix
	if prefix == "" {
		prefix = defaultPrefix
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	prefix = strings.TrimSuffix(prefix, "/")

	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBodyBytes
	}

	h := &Handler{
		owner:       cfg.Owner,
		repo:        cfg.Repo,
		branch:      cfg.Branch,
		token:       cfg.Token,
		editorRoles: editorRoles,
		committer:   cfg.Committer,
		apiRoot:     apiRoot,
		publicBase:  cfg.PublicBase,
		prefix:      prefix,
		client:      client,
		log:         logger,
		maxBody:     maxBody,
	}
	if cfg.PublicBase != nil {
		h.publicRepoPrefix = strings.TrimSuffix(cfg.PublicBase.String(), "/") + prefix +
			"/repos/" + url.PathEscape(cfg.Owner) + "/" + url.PathEscape(cfg.Repo)
	}
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Preflight carries no credentials by definition, so it cannot be
	// authenticated. The CORS headers themselves belong to the middleware in
	// front of this handler; answering here only avoids a confusing 401.
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	identity, ok := session.FromContext(r.Context())
	if !ok {
		h.fail(w, r, http.StatusUnauthorized, "not signed in")
		return
	}
	// Re-checked on every forwarded request, against the roles the session was
	// sealed with. Tightening EDITOR_ROLES therefore bites the sessions already
	// issued; a role removed in Keycloak does not, until the session expires.
	if missing := identity.MissingRoles(h.editorRoles); len(missing) > 0 {
		h.log.Warn("forward: editor roles missing",
			"subject", identity.Subject, "required", h.editorRoles, "missing", missing,
			"path", r.URL.EscapedPath())
		h.fail(w, r, http.StatusForbidden, "your Prodeko account is not allowed to edit the website")
		return
	}

	// EscapedPath, never Path: Decap sends cms%2Fpages%2Fslug as a single
	// segment and decoding it here would both mis-segment the allowlist match
	// and change the path we forward.
	escaped := r.URL.EscapedPath()
	rest, ok := strings.CutPrefix(escaped, h.prefix)
	if !ok || (rest != "" && !strings.HasPrefix(rest, "/")) {
		h.fail(w, r, http.StatusNotFound, "not a proxied GitHub path")
		return
	}
	rest = strings.TrimSuffix(rest, "/")

	if rest == "/user" {
		if r.Method != http.MethodGet {
			h.fail(w, r, http.StatusMethodNotAllowed, "only GET is allowed on the current user")
			return
		}
		h.serveUser(w, identity)
		return
	}

	subPath, ok := h.stripRepo(rest)
	if !ok {
		h.log.Warn("forward: denied, outside the pinned repository",
			"subject", identity.Subject, "method", r.Method, "path", escaped)
		h.fail(w, r, http.StatusForbidden, "only the configured repository can be reached through this proxy")
		return
	}

	if d := Allow(r.Method, subPath, h.branch); !d.Allowed {
		h.log.Warn("forward: denied by allowlist",
			"subject", identity.Subject, "method", r.Method, "subpath", subPath, "reason", d.Reason)
		h.fail(w, r, http.StatusForbidden, "%s", d.Reason)
		return
	}

	h.proxy(w, r, identity, subPath)
}

// stripRepo discards the owner and repo the browser asked for and returns the
// path below them. Rule 3 lives here: the caller's owner and repo are read only
// to find where the interesting part of the path starts.
func (h *Handler) stripRepo(rest string) (string, bool) {
	seg := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	if len(seg) < 3 || seg[0] != "repos" || seg[1] == "" || seg[2] == "" {
		return "", false
	}
	return strings.Join(seg[3:], "/"), true
}

func (h *Handler) proxy(w http.ResponseWriter, r *http.Request, identity session.Identity, subPath string) {
	rule, refName := classifyBody(r.Method, subPath)

	var body io.Reader
	var bodyLen int64 = -1

	if rule.buffers() {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.maxBody))
		if err != nil {
			h.fail(w, r, http.StatusRequestEntityTooLarge, "request body is too large or could not be read")
			return
		}
		if d := h.checkBody(rule, refName, raw); !d.Allowed {
			h.log.Warn("forward: denied by body check",
				"subject", identity.Subject, "method", r.Method, "subpath", subPath, "reason", d.Reason)
			h.fail(w, r, http.StatusForbidden, "%s", d.Reason)
			return
		}
		if rule == bodyAuthor {
			editor := authorFor(identity)
			rewritten, err := InjectAuthor(raw, editor, h.committer)
			if err != nil {
				// Refusing is the point: a commit that cannot carry the
				// verified identity must not be made at all.
				h.log.Warn("forward: could not set the commit author",
					"subject", identity.Subject, "error", err)
				h.fail(w, r, http.StatusBadRequest,
					"your commit could not be attributed to your Prodeko account; it needs a name and an email address")
				return
			}
			h.log.Info("forward: commit authored",
				"subject", identity.Subject, "author", editor.String(), "committer", h.committer.String())
			raw = rewritten
		}
		body = bytes.NewReader(raw)
		bodyLen = int64(len(raw))
	} else if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
		if r.ContentLength > h.maxBody {
			h.fail(w, r, http.StatusRequestEntityTooLarge, "request body is too large")
			return
		}
		body = http.MaxBytesReader(w, r.Body, h.maxBody)
		bodyLen = r.ContentLength
	}

	target, err := h.targetURL(subPath, r.URL.RawQuery)
	if err != nil {
		h.fail(w, r, http.StatusBadRequest, "the request path could not be understood")
		return
	}

	out, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), body)
	if err != nil {
		h.fail(w, r, http.StatusInternalServerError, "the upstream request could not be built")
		return
	}
	if body != nil {
		out.ContentLength = bodyLen
	}

	// Rule 4: our credential goes on here, and the caller's Authorization is
	// dropped on the floor rather than forwarded.
	out.Header.Set("Authorization", "Bearer "+h.token)
	out.Header.Set("Accept", "application/vnd.github+json")
	out.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	out.Header.Set("User-Agent", userAgent)
	if body != nil && bodyLen != 0 {
		out.Header.Set("Content-Type", jsonContentType)
	}
	// The repo root gets rewritten, so it must always come back with a body.
	if subPath != "" {
		if v := r.Header.Get("If-None-Match"); v != "" {
			out.Header.Set("If-None-Match", v)
		}
	}

	resp, err := h.client.Do(out)
	if err != nil {
		h.log.Error("forward: upstream call failed",
			"subject", identity.Subject, "method", r.Method, "subpath", subPath, "error", err)
		h.fail(w, r, http.StatusBadGateway, "GitHub could not be reached")
		return
	}
	defer resp.Body.Close()

	h.writeResponse(w, resp, subPath)
}

// targetURL rebuilds the upstream URL from configuration. subPath is carried
// across in its escaped form and url.Parse sets both Path and RawPath from it,
// so %2F survives as %2F rather than turning into a path separator.
func (h *Handler) targetURL(subPath, rawQuery string) (*url.URL, error) {
	raw := strings.TrimSuffix(h.apiRoot.String(), "/") +
		"/repos/" + url.PathEscape(h.owner) + "/" + url.PathEscape(h.repo)
	if subPath != "" {
		raw += "/" + subPath
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	if u.Host != h.apiRoot.Host || u.Scheme != h.apiRoot.Scheme {
		return nil, errors.New("forward: rebuilt URL left the configured API root")
	}
	u.RawQuery = rawQuery
	return u, nil
}

func (h *Handler) writeResponse(w http.ResponseWriter, resp *http.Response, subPath string) {
	header := w.Header()
	for name, values := range resp.Header {
		if !responseHeaders[http.CanonicalHeaderKey(name)] {
			continue
		}
		for _, v := range values {
			header.Add(name, v)
		}
	}
	if link := h.rewriteLink(resp.Header.Values("Link")); link != "" {
		header.Set("Link", link)
	}

	// The repo root is the one response we rewrite: Decap refuses to load
	// unless it reports permissions.push, and whether a fine-grained token
	// surfaces that honestly is not something we can rely on. Write authority
	// is enforced by the role check and the allowlist, not by this field.
	if subPath == "" && resp.StatusCode == http.StatusOK {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, h.maxBody))
		if readErr == nil {
			rewritten, err := forcePushPermission(body)
			if err == nil {
				header.Del("ETag")
				header.Del("Last-Modified")
				header.Set("Cache-Control", "no-store")
				header.Set("Content-Type", jsonContentType)
				header.Set("Content-Length", fmt.Sprint(len(rewritten)))
				w.WriteHeader(resp.StatusCode)
				_, _ = w.Write(rewritten)
				return
			}
			// Decap will refuse to load if GitHub reported push: false. Say so
			// here rather than leaving it as a silent blank editing screen.
			h.log.Error("forward: could not set permissions.push on the repository response", "error", err)
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return
	}

	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// forcePushPermission sets permissions.push on the repository response and
// leaves every other field, owner.login above all, exactly as GitHub sent it.
// Decap reads owner.login back and uses it to build pull request heads.
func forcePushPermission(body []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	permissions, err := json.Marshal(map[string]bool{
		"admin": false, "maintain": false, "push": true, "triage": true, "pull": true,
	})
	if err != nil {
		return nil, err
	}
	fields["permissions"] = permissions
	return json.Marshal(fields)
}

func authorFor(id session.Identity) Author {
	name := strings.TrimSpace(id.Name)
	if name == "" {
		name = loginFor(id)
	}
	return Author{Name: name, Email: strings.TrimSpace(id.Email)}
}

// fail writes a denial Decap can read. It must be JSON with a message field:
// decap-cms-lib-util parses every 403 body as JSON and calls .match() on
// json.message, so a body without that field throws inside the client and
// sends it into five rounds of exponential backoff.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, status int, format string, args ...any) {
	message := scrubRateLimit(fmt.Sprintf(format, args...))
	body, err := json.Marshal(struct {
		Message string `json:"message"`
	}{Message: message})
	if err != nil {
		body = []byte(`{"message":"request refused"}`)
	}
	w.Header().Set("Content-Type", jsonContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(status)
	if r == nil || r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// scrubRateLimit keeps that one phrase out of our own error messages. Decap
// treats any 403 containing it as a GitHub rate limit and stalls every
// subsequent request until the window it invents has passed.
func scrubRateLimit(message string) string {
	const trigger = "API rate limit exceeded"
	if strings.Contains(message, trigger) {
		return strings.ReplaceAll(message, trigger, "request refused")
	}
	return message
}
