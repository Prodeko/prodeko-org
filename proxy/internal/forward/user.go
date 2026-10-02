package forward

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/prodeko/prodeko-org/proxy/internal/session"
)

// githubUser is the shape Decap reads from GET /user. It uses name, login and
// email to label the signed-in editor and avatar_url for the header icon.
// Nothing else is needed, and nothing else is sent: the bot's real GitHub
// profile must not reach the browser.
//
// avatar_url is left out on purpose. Decap's Avatar component renders a generic
// user icon whenever the value is falsy, which is better than a broken image.
type githubUser struct {
	Login string `json:"login"`
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
	Type  string `json:"type"`
}

// serveUser answers GET {prefix}/user from the Keycloak session. The request
// never reaches GitHub.
func (h *Handler) serveUser(w http.ResponseWriter, id session.Identity) {
	name := strings.TrimSpace(id.Name)
	if name == "" {
		name = loginFor(id)
	}
	body, err := json.Marshal(githubUser{
		Login: loginFor(id),
		Name:  name,
		Email: strings.TrimSpace(id.Email),
		Type:  "User",
	})
	if err != nil {
		h.fail(w, nil, http.StatusInternalServerError, "the current user could not be described")
		return
	}

	w.Header().Set("Content-Type", jsonContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// loginFor picks the string Decap shows as the account name. Keycloak's
// preferred_username is the closest thing to a GitHub login; the local part of
// the email and then the subject are the fallbacks, so this is never empty for
// an identity that passed verification.
func loginFor(id session.Identity) string {
	if v := strings.TrimSpace(id.Username); v != "" {
		return v
	}
	if local, _, found := strings.Cut(strings.TrimSpace(id.Email), "@"); found && local != "" {
		return local
	}
	if v := strings.TrimSpace(id.Subject); v != "" {
		return v
	}
	return "editor"
}
