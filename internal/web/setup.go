package web

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/WanderingLesserPanda/eventally/internal/auth"
)

// handleFirstRun renders the one-time setup page: create the household
// and the first user in a single form.
func (a *App) handleFirstRun(w http.ResponseWriter, r *http.Request) {
	n, _ := a.countHouseholds(r.Context())
	if n > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	a.render(w, r, http.StatusOK, "first_run.html", map[string]any{
		"Title": "Welcome to Eventally",
	})
}

// handleFirstRunPost creates the household + first user atomically.
func (a *App) handleFirstRunPost(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	householdName := strings.TrimSpace(r.FormValue("household"))
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	if householdName == "" || username == "" || password == "" {
		a.render(w, r, http.StatusUnprocessableEntity, "first_run.html", map[string]any{
			"Title": "Welcome to Eventally",
			"Error": "All fields are required.",
		})
		return
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		log.Printf("hash: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx)

	var householdID, userID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO household (name, base_currency) VALUES ($1, 'MYR') RETURNING id`,
		householdName).Scan(&householdID); err != nil {
		log.Printf("first-run household insert: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO "user" (household_id, username, display_name, pass_hash)
		 VALUES ($1, $2, $3, $4) RETURNING id`,
		householdID, username, username, hash).Scan(&userID); err != nil {
		log.Printf("first-run user insert: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("first-run commit: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	sess, err := a.Sessions.Create(ctx, householdID, userID, r.UserAgent(), r.RemoteAddr)
	if err != nil {
		log.Printf("first-run session: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	auth.SetSessionCookie(w, sess.ID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
