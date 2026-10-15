// Package web provides the HTTP app: chi router, templates, session and
// CSRF middleware, and shared render helpers.
package web

import (
	"context"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"time"

	"github.com/WanderingLesserPanda/eventally/internal/auth"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
)

// App bundles everything handlers need.
type App struct {
	Pool     *pgxpool.Pool
	Sessions *auth.Store
	Tmpl     *template.Template
}

// New loads templates and returns a ready App.
func New(pool *pgxpool.Pool, tmplDir string) (*App, error) {
	funcs := template.FuncMap{
		"money": moneyFmt,
		"date":  dateFmt,
		"now":   time.Now,
		"csrf":  func(r *http.Request) string { return csrfToken(r) },
		"add":   func(a, b int) int { return a + b },
	}
	tmpl, err := template.New("").Funcs(funcs).ParseGlob(filepath.Join(tmplDir, "*.html"))
	if err != nil {
		return nil, err
	}
	return &App{Pool: pool, Sessions: auth.NewStore(pool), Tmpl: tmpl}, nil
}

// Router builds the chi mux with middleware and all routes.
func (a *App) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Logger)
	r.Use(a.sessionLoader)

	r.Get("/static/*", http.StripPrefix("/static/", http.FileServerFS(staticFS)).ServeHTTP)

	r.Get("/login", a.handleLogin)
	r.Post("/login", a.handleLoginPost)
	r.Post("/logout", a.handleLogout)
	r.Get("/setup", a.handleFirstRun)
	r.Post("/setup", a.handleFirstRunPost)

	// Authenticated area: reference data, transactions, cards,
	// reconciliation, recurring, settings mount under this group.
	r.Group(func(authed chi.Router) {
		authed.Use(a.requireAuth)
		authed.Get("/", a.handleDashboard)
		// Phase P2+ routes attach here.
	})

	return r
}

// sessionLoader attaches the verified session (if any) to the request
// context.
func (a *App) sessionLoader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := auth.SessionIDFromRequest(r)
		if id != "" {
			if sess, err := a.Sessions.Get(r.Context(), id); err == nil {
				r = r.WithContext(contextWithSession(r.Context(), sess))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireAuth sends unauthenticated requests to /login.
func (a *App) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sessionFromContext(r.Context()) == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleLogin renders the login form, or the first-run setup page when no
// household exists yet.
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	n, _ := a.countHouseholds(r.Context())
	if n == 0 {
		a.render(w, r, http.StatusOK, "first_run.html", map[string]any{
			"Title": "Welcome to Eventally",
		})
		return
	}
	a.render(w, r, http.StatusOK, "login.html", map[string]any{
		"Title": "Sign in",
		"Error": r.URL.Query().Get("err") != "",
	})
}

// handleLoginPost verifies credentials and starts a session.
func (a *App) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")

	var household, userID int64
	var hash string
	err := a.Pool.QueryRow(r.Context(),
		`SELECT household_id, id, pass_hash
		 FROM "user" WHERE username = $1`, username).
		Scan(&household, &userID, &hash)
	if err != nil || auth.CheckPassword(hash, password) != nil {
		http.Redirect(w, r, "/login?err=1", http.StatusSeeOther)
		return
	}
	sess, err := a.Sessions.Create(r.Context(), household, userID,
		r.UserAgent(), r.RemoteAddr)
	if err != nil {
		log.Printf("session create: %v", err)
		http.Error(w, "could not start session", http.StatusInternalServerError)
		return
	}
	auth.SetSessionCookie(w, sess.ID)
	http.Redirect(w, r, auth.RefererOrFallback(r, "/"), http.StatusSeeOther)
}

// handleLogout ends the session.
func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	a.Sessions.Delete(r.Context(), auth.SessionIDFromRequest(r))
	auth.ClearSessionCookie(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// handleDashboard is the authenticated home page. The card grid arrives
// in phase P6; for now it proves the auth loop end-to-end.
func (a *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var stats struct {
		Accounts int
		Txns     int
		Pending  int
	}
	_ = a.Pool.QueryRow(ctx, `SELECT count(*) FROM account WHERE household_id = $1`,
		sessionFromContext(ctx).Household).Scan(&stats.Accounts)
	_ = a.Pool.QueryRow(ctx, `SELECT count(*) FROM "transaction" WHERE household_id = $1`,
		sessionFromContext(ctx).Household).Scan(&stats.Txns)
	_ = a.Pool.QueryRow(ctx, `SELECT count(*) FROM "transaction" WHERE household_id = $1 AND record_status = 'pending'`,
		sessionFromContext(ctx).Household).Scan(&stats.Pending)
	a.render(w, r, http.StatusOK, "dashboard.html", map[string]any{
		"Title":  "Dashboard",
		"Stats":  stats,
	})
}

// countHouseholds returns the number of households (0 means first run).
func (a *App) countHouseholds(ctx context.Context) (int, error) {
	var n int
	err := a.Pool.QueryRow(ctx, `SELECT count(*) FROM household`).Scan(&n)
	return n, err
}
