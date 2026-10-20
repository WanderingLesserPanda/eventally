package web

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/WanderingLesserPanda/eventally/internal/auth"
)

// setupForm is the full first-run setup state (initial setup page).
type setupForm struct {
	Username      string
	DisplayName   string
	Email         string
	Password      string
	Confirm       string
	HouseholdName string
	Language      string
	Currency      string
	FirstDay      string // "1".."7"
}

// setupLanguages / setupCurrencies are the allowed regional choices for
// now: English only, Malaysia Ringgit only. The selects list exactly
// these; widening them later is a one-line change per list.
var setupLanguages = []string{"English"}
var setupCurrencies = []string{"MYR — Malaysia Ringgit"}

// firstDayOptions: 1..7, ISO-8601 (Monday=1 .. Sunday=7).
var firstDayOptions = []struct{ Value, Label string }{
	{"1", "Monday"}, {"2", "Tuesday"}, {"3", "Wednesday"},
	{"4", "Thursday"}, {"5", "Friday"}, {"6", "Saturday"}, {"7", "Sunday"},
}

// handleFirstRun renders the one-time setup page: create the household
// and the first user in a single form.
func (a *App) handleFirstRun(w http.ResponseWriter, r *http.Request) {
	n, _ := a.countHouseholds(r.Context())
	if n > 0 {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	a.renderSetup(w, r, setupForm{
		Language:  "English",
		Currency:  setupCurrencies[0],
		FirstDay:  "1",
	}, "")
}

// handleFirstRunPost creates the household + first user atomically.
func (a *App) handleFirstRunPost(w http.ResponseWriter, r *http.Request) {
	if err := a.checkCSRF(r); err != nil {
		http.Error(w, "CSRF check failed", http.StatusForbidden)
		return
	}
	f := setupForm{
		Username:      strings.TrimSpace(r.FormValue("username")),
		DisplayName:   strings.TrimSpace(r.FormValue("display_name")),
		Email:         strings.TrimSpace(r.FormValue("email")),
		Password:      r.FormValue("password"),
		Confirm:       r.FormValue("confirm"),
		HouseholdName: strings.TrimSpace(r.FormValue("household")),
		Language:      r.FormValue("language"),
		Currency:      r.FormValue("currency"),
		FirstDay:      r.FormValue("first_day"),
	}
	if f.FirstDay == "" {
		f.FirstDay = "1"
	}

	// Validate.
	var errs []string
	if m := validateUsername(f.Username); m != "" {
		errs = append(errs, m)
	}
	if m := validatePersonalName(f.DisplayName, 2); m != "" {
		errs = append(errs, "Display name: "+m)
	}
	if m := validateHouseholdName(f.HouseholdName); m != "" {
		errs = append(errs, m)
	}
	if m := validateEmail(f.Email); m != "" {
		errs = append(errs, m)
	}
	if m := validateSetupPassword(f.Password); m != "" {
		errs = append(errs, m)
	}
	if f.Password != f.Confirm {
		errs = append(errs, "Passwords do not match.")
	}
	if f.Language != "English" {
		errs = append(errs, "Language must be English (for now).")
	}
	if f.Currency != setupCurrencies[0] {
		errs = append(errs, "Default currency must be MYR — Malaysia Ringgit (for now).")
	}
	firstDay, ok := 0, false
	for _, d := range firstDayOptions {
		if d.Value == f.FirstDay {
			firstDay, ok = int(d.Value[0]-'0'), true
		}
	}
	if !ok {
		errs = append(errs, "Choose a first day of the week.")
	}
	if len(errs) > 0 {
		f.Password, f.Confirm = "", "" // never echo passwords back
		a.renderSetup(w, r, f, strings.Join(errs, " "))
		return
	}

	hash, err := auth.HashPassword(f.Password)
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

	var hh, userID int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO household (name, base_currency, language, first_day_of_week)
		 VALUES ($1, 'MYR', 'en', $2) RETURNING id`,
		f.HouseholdName, firstDay).Scan(&hh); err != nil {
		log.Printf("first-run household insert: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := tx.QueryRow(ctx,
		`INSERT INTO app_user (household_id, username, display_name, email, password_hash)
		 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		hh, f.Username, f.DisplayName, f.Email, hash).Scan(&userID); err != nil {
		if isUniqueViolation(err) {
			// uq_user_username: usernames are global (login matches on
			// the username alone), so this is "already taken".
			f.Password, f.Confirm = "", ""
			a.renderSetup(w, r, f, "That username is already taken — pick another.")
			return
		}
		log.Printf("first-run user insert: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// Multi-household membership (000006): the founding user is the
	// household's owner. user.household_id stays set as the legacy
	// primary-household pointer (login, email uniqueness).
	if _, err := tx.Exec(ctx,
		`INSERT INTO household_member (user_id, household_id, role)
		 VALUES ($1, $2, 'owner')`, userID, hh); err != nil {
		log.Printf("first-run membership insert: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("first-run commit: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	sess, err := a.Sessions.Create(ctx, hh, userID, r.UserAgent(), r.RemoteAddr)
	if err != nil {
		log.Printf("first-run session: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	auth.SetSessionCookie(w, sess.ID)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// renderSetup renders the setup page (GET or a rejected POST re-render).
// errMsg is "" on a fresh GET, an error string on a rejected POST.
func (a *App) renderSetup(w http.ResponseWriter, r *http.Request, f setupForm, errMsg string) {
	a.render(w, r, http.StatusOK, "first_run.html", map[string]any{
		"Title":   "Welcome to Eventally",
		"F":       f,
		"Error":   errMsg,
		"Langs":   setupLanguages,
		"Currencies": setupCurrencies,
		"FirstDays": firstDayOptions,
	})
}

// --- field validators ------------------------------------------------------
//
// Each returns "" when the value is valid, or a user-facing message.
// Lengths are counted in runes (not bytes) so multi-byte names are
// measured as people see them.

// validateUsername: the login alias. ASCII letters and digits only,
// 6-30 characters. No spaces, accents, or special characters of any
// kind — it is the credential, so keep it to the unambiguous set.
// Uniqueness is enforced in the schema (uq_user_username, global and
// case-INSENSITIVE) and surfaced by the handler's 23505 catch; login
// matching stays case-sensitive (handleLoginPost).
func validateUsername(s string) string {
	if s == "" {
		return "Username is required."
	}
	// Character set first: a name with bad characters is invalid at any
	// length, and that is the more actionable message.
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return "Username may only contain letters and numbers — no spaces or special characters."
		}
	}
	if n := len([]rune(s)); n < 6 {
		return "Username must be at least 6 characters."
	}
	if n := len([]rune(s)); n > 30 {
		return "Username is at most 30 characters."
	}
	return ""
}

// validatePersonalName: display name. Letters, numbers, spaces, periods
// and commas; minLen..60 characters.
func validatePersonalName(s string, minLen int) string {
	if s == "" {
		return "required."
	}
	// Character set first (see validateUsername).
	for _, r := range s {
		switch {
		case unicode.Is(unicode.L, r), unicode.Is(unicode.N, r):
		case r == ' ', r == '.', r == ',':
		default:
			return "use only letters, numbers, spaces, periods and commas."
		}
	}
	if n := len([]rune(s)); n < minLen {
		return fmt.Sprintf("must be at least %d characters.", minLen)
	}
	if len([]rune(s)) > 60 {
		return "at most 60 characters."
	}
	return ""
}

// validateHouseholdName: same character set as display name (letters,
// numbers, spaces, periods, commas), 3-60 characters.
func validateHouseholdName(s string) string {
	if m := validatePersonalName(s, 3); m != "" {
		return "Household name " + m
	}
	return ""
}

// emailRe is a pragmatic standard-address check: local@domain.tld, with
// the local part limited to the characters people actually use and the
// domain requiring at least one dot. (It is intentionally not the full
// RFC 5322 grammar — quoted strings and IP literals are not real-world
// setup values and accepting them only weakens the feedback.)
var emailRe = regexp.MustCompile(`^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`)

// validateEmail: required, standard address shape.
func validateEmail(s string) string {
	if s == "" {
		return "Email address is required."
	}
	if len(s) > 254 {
		return "Email address is too long."
	}
	if !emailRe.MatchString(s) {
		return "Enter a valid email address, e.g. you@example.com."
	}
	return ""
}

// validateSetupPassword: 8-64 characters; letters, digits and the
// typical special characters are all allowed — the same policy family
// as the Postgres password rule (8-64, specials supported, no "must
// contain a special" requirement).
func validateSetupPassword(s string) string {
	n := len([]rune(s))
	switch {
	case n == 0:
		return "Password is required (8-64 characters)."
	case n < 8:
		return "Password must be at least 8 characters."
	case n > 64:
		return "Password is at most 64 characters."
	}
	return ""
}
