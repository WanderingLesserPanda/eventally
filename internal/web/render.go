package web

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/WanderingLesserPanda/eventally/internal/auth"
	"github.com/WanderingLesserPanda/eventally/internal/money"
)

// staticFS serves the vendored HTMX/Alpine assets and app.css offline.
// The embed directory is "static", so paths in the embedded FS carry that
// prefix — staticSub is the FS rooted at it, which the /static/* route
// (StripPrefix) serves against. Without the Sub, every request 404s:
// the stripped path "app.css" is looked up at the FS root, where nothing
// lives (the files sit under "static/").
//
//go:embed static
var staticFS embed.FS

// staticSub is staticFS rooted at the "static" directory.
var staticSub = func() fs.FS {
	s, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // cannot happen: the embed matched at build time
	}
	return s
}()

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
	// HTML is never cached: the app ships unversioned assets and upgrades by
	// swapping the container image (docker compose pull). Without this, a
	// browser can keep serving a stale page after an upgrade. Force a
	// revalidation on every load.
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
	w.WriteHeader(status)
	// Two-stage render: execute the page template into a buffer, then the
	// layout with the page HTML as .Content (safeHTML skips escaping).
	var buf bytes.Buffer
	pageName := "page/" + strings.TrimSuffix(name, ".html")
	if err := a.Tmpl.ExecuteTemplate(&buf, pageName, data); err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	data["Content"] = template.HTML(buf.String())
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

// hasPrefix is a strings.HasPrefix alias for template funcs.
func hasPrefix(s, prefix string) bool {
	return strings.HasPrefix(s, prefix)
}

// containsStr reports whether a string slice contains a value (templates
// can't test membership natively — quick-capture checkbox preselect).
func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// acctIcon is the per-account-type glyph used in the accounts grid.
func acctIcon(t string) string {
	switch t {
	case "savings":
		return "🏦"
	case "ewallet":
		return "📱"
	case "credit_card":
		return "💳"
	case "loan":
		return "🧾"
	case "investment":
		return "📈"
	case "cash":
		return "💵"
	case "credits":
		return "🎟️"
	default:
		return "💠"
	}
}

// acctTypeLabel is the human-facing account type name.
func acctTypeLabel(t string) string {
	switch t {
	case "savings":
		return "Savings"
	case "ewallet":
		return "E-wallet"
	case "credit_card":
		return "Credit card"
	case "loan":
		return "Loan"
	case "investment":
		return "Investment"
	case "cash":
		return "Cash"
	case "credits":
		return "Credits"
	default:
		return t
	}
}

// utilPct renders a 0..1 fraction as a clamped integer percent (credit
// utilisation bars; over-limit reads as 100, not a broken width).
func utilPct(f float64) int {
	p := int(f*100 + 0.5)
	if p < 0 {
		p = 0
	}
	if p > 100 {
		p = 100
	}
	return p
}
