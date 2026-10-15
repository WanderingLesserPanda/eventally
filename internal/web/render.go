package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/WanderingLesserPanda/eventally/internal/auth"
	"github.com/WanderingLesserPanda/eventally/internal/money"
)

// staticFS serves the vendored HTMX/Alpine assets offline.
//
//go:embed static
var staticFS embed.FS

// context key for the verified session.
type sessionCtxKey struct{}

// contextWithSession stores the session on the request context.
func contextWithSession(ctx context.Context, s *auth.Session) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, s)
}

// sessionFromContext retrieves the session, or nil.
func sessionFromContext(ctx context.Context) *auth.Session {
	s, _ := ctx.Value(sessionCtxKey{}).(*auth.Session)
	return s
}

// csrfField is the <input> helper used in templates.
func csrfField(r *http.Request) string {
	return fmt.Sprintf(`<input type="hidden" name="csrf" value="%s">`,
		template.HTMLEscapeString(csrfToken(r)))
}

// csrfToken returns the session's CSRF token, or "" when unauthenticated.
func csrfToken(r *http.Request) string {
	if s := sessionFromContext(r.Context()); s != nil {
		return s.CSRFToken
	}
	return ""
}

// checkCSRF validates the "csrf" form field against the session token.
// Public forms (login, first-run setup) are exempt: they are GET-rendered
// with no session, and the SameSite=Strict cookie is the same-origin
// guarantee there.
func (a *App) checkCSRF(r *http.Request) error {
	if r.URL.Path == "/login" || r.URL.Path == "/setup" {
		return nil
	}
	sess := sessionFromContext(r.Context())
	if sess == nil {
		return fmt.Errorf("no session")
	}
	got := r.FormValue("csrf")
	if got == "" || got != sess.CSRFToken {
		return fmt.Errorf("csrf mismatch")
	}
	return nil
}

// render executes a template in the base layout and writes the response.
func (a *App) render(w http.ResponseWriter, r *http.Request, status int, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["Session"] = sessionFromContext(r.Context())
	data["Path"] = r.URL.Path
	data["Year"] = time.Now().Year()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := a.Tmpl.ExecuteTemplate(w, "layout.html", data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
	}
}

// moneyFmt renders a money value for templates. The household's base
// currency symbol is passed via template data later; RM is the MVP
// default (D13 makes it a setting).
func moneyFmt(m money.Money) string {
	return m.Format("RM")
}

// dateFmt renders a date for templates.
func dateFmt(t time.Time) string {
	return t.Format("2 Jan 2006")
}

// hasPrefix is a tiny strings.HasPrefix alias for template funcs.
var _ = strings.HasPrefix
