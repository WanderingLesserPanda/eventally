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
	DataDir  string // where uploaded receipts and other user files are stored
}

// New loads templates and returns a ready App.
func New(pool *pgxpool.Pool, tmplDir, dataDir string) (*App, error) {
	funcs := template.FuncMap{
		"money": moneyFmt,
		"date":  dateFmt,
		"now":   time.Now,
		"csrf":  func(r *http.Request) string { return csrfToken(r) },
		"add":   func(a, b int) int { return a + b },
		"orDash": orDash,
		"hasPrefix": hasPrefix,
	"contains":  containsStr,
	"icon":      acctIcon,
	"humanType": acctTypeLabel,
	"utilPct":   utilPct,
	}
	tmpl, err := template.New("").Funcs(funcs).ParseGlob(filepath.Join(tmplDir, "*.html"))
	if err != nil {
		return nil, err
	}
	return &App{Pool: pool, Sessions: auth.NewStore(pool), Tmpl: tmpl, DataDir: dataDir}, nil
}

// staticCacheControl wraps a handler so static assets are never served
// from browser cache without revalidation. The files are unversioned
// (/static/app.css, not /static/app.20261018.css), so a stale cached
// copy outlives image upgrades; "no-cache" forces a conditional
// revalidation request while still letting the browser reuse the copy
// when the server confirms it is unchanged (304, no re-download).
func staticCacheControl(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next(w, r)
	}
}

// Router builds the chi mux with middleware and all routes.
func (a *App) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Logger)
	r.Use(a.sessionLoader)

	r.Get("/static/*", staticCacheControl(http.StripPrefix("/static/", http.FileServerFS(staticSub)).ServeHTTP))

	// Phase P3: receipts are served by direct link (D14) — unauthenticated,
	// home-LAN only, unguessable filename (timestamp-suffixed).
	r.Get("/receipts/*", a.serveReceipts)

	r.Get("/login", a.handleLogin)
	r.Post("/login", a.handleLoginPost)
	r.Post("/logout", a.handleLogout)
	r.Get("/setup", a.handleFirstRun)
	r.Post("/setup", a.handleFirstRunPost)

	// D28 permission-UI wave: public join (new users) + household creation.
	r.Get("/join", a.handleJoinGET)
	r.Post("/join", a.handleJoinPOST)
	r.Get("/households/new", a.handleNewHouseholdGET)
	r.Post("/households/new", a.handleNewHouseholdPOST)

	// Authenticated area: reference data, transactions, cards,
	// reconciliation, recurring, settings mount under this group.
	r.Group(func(authed chi.Router) {
		authed.Use(a.requireAuth)
		authed.Use(a.requireCSRF)
		authed.Get("/", a.handleDashboard)

		// Phase P2: reference data CRUD.
		authed.Get("/accounts", a.listAccounts)
		authed.Get("/accounts/new", a.handleAccountCreate)
		authed.Post("/accounts", a.handleAccountCreate)
		authed.Get("/accounts/{id}", a.handleAccountEdit)
		authed.Post("/accounts/{id}", a.handleAccountEdit)
		authed.Post("/accounts/{id}/delete", a.handleAccountDelete)

		authed.Get("/categories", a.listCategories)
		authed.Get("/categories/new", a.handleCategoryCreate)
		authed.Post("/categories", a.handleCategoryCreate)
		authed.Get("/categories/{id}", a.handleCategoryEdit)
		authed.Post("/categories/{id}", a.handleCategoryEdit)
		authed.Post("/categories/{id}/deactivate", a.handleCategoryDelete)

		authed.Get("/merchants", a.listMerchants)
		authed.Get("/merchants/new", a.handleMerchantCreate)
		authed.Post("/merchants", a.handleMerchantCreate)
		authed.Get("/merchants/{id}", a.handleMerchantEdit)
		authed.Post("/merchants/{id}", a.handleMerchantEdit)
		authed.Post("/merchants/{id}/deactivate", a.handleMerchantDelete)

		authed.Get("/parties", a.listParties)
		authed.Get("/parties/new", a.handlePartyCreate)
		authed.Post("/parties", a.handlePartyCreate)
		authed.Get("/parties/{id}", a.handlePartyEdit)
		authed.Post("/parties/{id}", a.handlePartyEdit)
		authed.Post("/parties/{id}/deactivate", a.handlePartyDelete)

		// Phase P3: transactions (list, form w/ transaction_split editor, detail, status, receipts).
		authed.Get("/txns", a.listTxns)
		authed.Get("/txns/new", a.handleTxnCreate)
		authed.Post("/txns", a.handleTxnCreate)
		authed.Get("/txns/{id}", a.handleTxnDetail)
		authed.Get("/txns/{id}/edit", a.handleTxnEdit)
		authed.Post("/txns/{id}", a.handleTxnEditPost)
		authed.Post("/txns/{id}/status", a.handleTxnStatus)
		authed.Post("/txns/{id}/receipt", a.handleTxnReceiptUpload)
		authed.Post("/txns/{id}/receipt/delete", a.handleTxnReceiptDelete)

		// Phase P4: quick capture (mobile-first, D12) — a few taps to a
		// pending transaction.
		authed.Get("/capture", a.captureGET)
		authed.Post("/capture", a.capturePOST)

		// Phase P5: card view + payments with allocations (D5, D6).
		authed.Get("/cards", a.handleCards)
		authed.Get("/cards/{id}", a.handleCardDetail)
		authed.Get("/cards/{id}/pay", a.handleCardPayGet)
		authed.Post("/cards/{id}/pay", a.handleCardPayPost)
		authed.Post("/settlements/{id}/delete", a.handleSettlementDelete)

		// Phase P6: reconciliation (D8) — monthly anchors + drill-in.
		authed.Get("/reconcile", a.handleReconcile)
		authed.Get("/reconcile/{id}/{month}", a.handleReconcileMonth)
		authed.Post("/reconcile/{id}/{month}", a.handleReconcileMonthPost)

		// Phase P7: recurring templates (D11) + pending-review queue (D9).
		authed.Get("/recurring", a.handleRecurring)
		authed.Get("/recurring/new", a.handleRecurringCreate)
		authed.Post("/recurring", a.handleRecurringCreate)
		authed.Get("/recurring/{id}", a.handleRecurringEdit)
		authed.Post("/recurring/{id}", a.handleRecurringEdit)
		authed.Post("/recurring/{id}/deactivate", a.handleRecurringDeactivate)
		authed.Get("/review", a.handleReview)
		authed.Post("/review/bulk", a.handleReviewBulk)

		// D28 permission-UI wave: settings (3 tabs), household switcher,
		// invitations, in-app join.
		authed.Get("/settings", a.handleSettings)
		authed.Post("/settings/household", a.handleDefaultHouseholdPOST)
		authed.Post("/settings/invite", a.handleInviteCreate)
		authed.Post("/settings/invite/revoke/{id}", a.handleInviteRevoke)
		authed.Post("/settings/invite/expiry", a.handleSetDefaultExpiry)
		authed.Post("/settings/grants/household/{id}", a.handleHouseholdGrantSave)
		authed.Post("/settings/grants/account", a.handleAccountGrantSave)
		authed.Post("/households/switch", a.handleHouseholdSwitch)
		authed.Post("/join/dismiss", a.handleJoinDismiss)
		authed.Post("/join/complete", a.handleJoinComplete)
		authed.Get("/join-with-code", a.handleJoinWithCodeGET)
		authed.Post("/join-with-code", a.handleJoinWithCodePOST)
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

// requireAuth sends unauthenticated requests to /login. A session with no
// current household (D28 recovery state) may only reach the recovery
// surface: the dashboard, household creation, join flows, and the settings
// household tab — everything else bounces back to the dashboard.
func (a *App) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sess := sessionFromContext(r.Context())
		if sess == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if sess.Household == 0 && !householdlessAllowed(r.URL.Path) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// householdlessAllowed lists the paths a no-household session may use.
func householdlessAllowed(path string) bool {
	switch path {
	case "/", "/households/new", "/join/complete", "/join/dismiss", "/settings", "/join-with-code":
		return true
	}
	return false
}

// requireCSRF rejects authenticated POSTs without a valid csrf field.
func (a *App) requireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		if err := a.checkCSRF(r); err != nil {
			http.Error(w, "CSRF check failed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleLogin renders the login form. With no household yet, the first-run
// setup page owns first contact: /setup renders the full form (options,
// preselected defaults, full-bleed layout), so redirect there instead of
// rendering a degraded copy inline at /login (which lacks the template
// data and the /setup path, and used to show blank selects + a boxed
// gradient + the footer).
func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	n, _ := a.countHouseholds(r.Context())
	if n == 0 {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	a.render(w, r, http.StatusOK, "login.html", map[string]any{
		"Title": "Sign in",
		"Error": r.URL.Query().Get("err") != "",
	})
}

// handleLoginPost verifies credentials and starts a session. Landing
// (D28): default_household_id if still a membership, else the first
// remaining membership, else a household-less session (the dashboard
// recovery screen offers creating a household).
func (a *App) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")

	var userID int64
	var hash string
	err := a.Pool.QueryRow(r.Context(),
		`SELECT id, password_hash FROM app_user WHERE username = $1`, username).
		Scan(&userID, &hash)
	if err != nil || auth.CheckPassword(hash, password) != nil {
		http.Redirect(w, r, "/login?err=1", http.StatusSeeOther)
		return
	}
	household, err := a.landHousehold(r.Context(), userID)
	if err != nil {
		log.Printf("login land: %v", err)
		http.Error(w, "could not determine your household", http.StatusInternalServerError)
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

// countHouseholds returns the number of households (0 means first run).
func (a *App) countHouseholds(ctx context.Context) (int, error) {
	var n int
	err := a.Pool.QueryRow(ctx, `SELECT count(*) FROM household`).Scan(&n)
	return n, err
}
