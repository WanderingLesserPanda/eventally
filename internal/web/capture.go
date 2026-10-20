package web

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/WanderingLesserPanda/eventally/internal/money"
	"github.com/WanderingLesserPanda/eventally/internal/split"
)

// ---------------------------------------------------------------------------
// Phase P4: quick capture (D12).
//
// Mobile-first, ~4 taps: amount + card + merchant (datalist autocomplete
// with inline quick-create) + who paid. Lands in pending (D9). The split
// is always equal across the participants selected — the full form
// (/txns/new) handles percent/amount splits.
//
// Fields: csrf, amount, date, account_id, merchant_id, merchant_new
// (name used when merchant_id is empty), description, part[] (repeated,
// "user:N" / "cp:N").
// ---------------------------------------------------------------------------

type captureForm struct {
	Err          string
	Amount       string
	Date         string
	CardID       string // account id as string for easy template binding
	Merch        string
	Desc         string
	Parts        []string // selected participant keys (re-posted on error)
	Users        []opt
	CardAccounts []opt // credit cards only
	Merchants    []opt
}

func (a *App) captureRender(w http.ResponseWriter, r *http.Request, f *captureForm) {
	a.render(w, r, http.StatusOK, "capture.html", map[string]any{
		"Title": "Quick capture",
		"F":     f,
		"Flash": flashFrom(r),
	})
}

func (a *App) captureGET(w http.ResponseWriter, r *http.Request) {
	hh := sessionFromContext(r.Context()).Household
	ctx := r.Context()

	users, err := a.loadUsers(ctx, hh)
	if err != nil {
		http.Error(w, "load users: "+err.Error(), 500)
		return
	}
	cards, err := a.creditCardAccounts(ctx, hh)
	if err != nil {
		http.Error(w, "load accounts: "+err.Error(), 500)
		return
	}
	merchants, err := a.activeMerchants(ctx, hh)
	if err != nil {
		http.Error(w, "load merchants: "+err.Error(), 500)
		return
	}

	f := &captureForm{
		Date:         time.Now().Format("2006-01-02"),
		Users:        users,
		CardAccounts: cards,
		Merchants:    merchants,
	}
	// Default: all household users (the usual expense split).
	for _, u := range users {
		f.Parts = append(f.Parts, "user:"+strconv.FormatInt(u.ID, 10))
	}
	// Preselect the card when there is exactly one.
	if len(cards) == 1 {
		f.CardID = strconv.FormatInt(cards[0].ID, 10)
	}
	a.captureRender(w, r, f)
}

func (a *App) capturePOST(w http.ResponseWriter, r *http.Request) {
	hh := sessionFromContext(r.Context()).Household
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	amountStr := strings.TrimSpace(r.FormValue("amount"))
	dateStr := strings.TrimSpace(r.FormValue("date"))
	cardID, _ := strconv.ParseInt(r.FormValue("account_id"), 10, 64)
	merchName := strings.TrimSpace(r.FormValue("merchant"))
	desc := strings.TrimSpace(r.FormValue("description"))
	partKeys := r.Form["part[]"]

	fail := func(msg string) {
		f := &captureForm{
			Err:      msg,
			Amount:   amountStr,
			Date:     dateStr,
			CardID:   strconv.FormatInt(cardID, 10),
			Merch:    merchName,
			Desc:     desc,
			Parts:    partKeys,
		}
		if users, err := a.loadUsers(ctx, hh); err == nil {
			f.Users = users
		}
		if cards, err := a.creditCardAccounts(ctx, hh); err == nil {
			f.CardAccounts = cards
		}
		if merchants, err := a.activeMerchants(ctx, hh); err == nil {
			f.Merchants = merchants
		}
		a.captureRender(w, r, f)
	}

	total, err := money.Parse(amountStr)
	if err != nil {
		fail("Amount is invalid.")
		return
	}
	if total <= 0 {
		fail("Amount must be greater than zero.")
		return
	}
	if !isDateStr(dateStr) {
		fail("Date is required (YYYY-MM-DD).")
		return
	}
	if cardID <= 0 {
		fail("Choose a card.")
		return
	}

	// Card must belong to the household and be a credit card (D12: the
	// daily surface is card spend).
	var cardType string
	err = a.Pool.QueryRow(ctx,
		`SELECT account_type FROM account WHERE id = $1 AND household_id = $2`,
		cardID, hh).Scan(&cardType)
	if err != nil || cardType != "credit_card" {
		fail("Choose a valid card.")
		return
	}

	// Merchant: one free-text field (D12 UX — a datalist suggests known
	// names; anything not typed is a new merchant). Names are matched
	// case-insensitively per household so "makro" and "Makro" are the
	// same merchant.
	var merchID int64
	if merchName != "" {
		err := a.Pool.QueryRow(ctx,
			`SELECT id FROM merchant WHERE household_id = $1 AND lower(name) = lower($2)`,
			hh, merchName).Scan(&merchID)
		if errors.Is(err, sql.ErrNoRows) {
			err = a.Pool.QueryRow(ctx,
				`INSERT INTO merchant (household_id, name) VALUES ($1, $2) RETURNING id`,
				hh, merchName).Scan(&merchID)
			if err != nil {
				http.Error(w, "create merchant: "+err.Error(), 500)
				return
			}
		} else if err != nil {
			http.Error(w, "lookup merchant: "+err.Error(), 500)
			return
		}
	}

	// Participants: de-duplicate, validate keys exist in this household.
	seen := map[string]bool{}
	var parts []string
	for _, k := range partKeys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		typ, id, perr := parseParticipantKey(k)
		if perr != nil {
			fail("A participant is invalid.")
			return
		}
		table := "party"
		if typ == "household_user" {
			table = `app_user`
		}
		var n int
		if err := a.Pool.QueryRow(ctx,
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE id = $1 AND household_id = $2`, table),
			id, hh).Scan(&n); err != nil || n == 0 {
			fail("A participant is not valid for this household.")
			return
		}
		seen[k] = true
		parts = append(parts, k)
	}
	if len(parts) == 0 {
		fail("Choose at least one person.")
		return
	}
	shares := split.EqualSplit(total, len(parts))

	// Transaction + equal splits in one commit. Origin = card account,
	// destination = the merchant (mirrors the full form's convention).
	sess := sessionFromContext(r.Context())
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		http.Error(w, "internal error", 500)
		return
	}
	defer tx.Rollback(ctx)

	var merch any
	if merchID > 0 {
		merch = merchID
	}
	// Destination: the merchant display attribute when known, otherwise
	// nothing extra — the expense's origin is the card account itself.
	// (Gate 3c: the merchant is no longer an endpoint.)
	var destAcc, destParty any
	var txnID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO "transaction" (
			household_id, recorded_by_user_id, transaction_date, class,
			category_id, merchant_id, description, description_extra,
			total_amount, origin_account_id, destination_account_id,
			destination_party_id,
			record_status, source, notes
		) VALUES ($1, $2, $3, 'expense', NULL, $4, $5, '', $6,
			$7, $8, $9,
			'pending', 'manual', '')
		RETURNING id`,
		hh, sess.UserID, dateStr, merch, desc, total, cardID,
		destAcc, destParty).
		Scan(&txnID)
	if err != nil {
		http.Error(w, "create transaction: "+err.Error(), 500)
		return
	}
	partyIDs, err := a.splitEditorKeys(ctx, hh, parts)
	if err != nil {
		tx.Rollback(ctx)
		http.Error(w, "bad participant: "+err.Error(), 400)
		return
	}
	for i := range parts {
		if _, err := tx.Exec(ctx, `
			INSERT INTO transaction_split (household_id, transaction_id, party_id, amount)
			VALUES ($1, $2, $3, $4)`,
			hh, txnID, partyIDs[i], shares[i]); err != nil {
			http.Error(w, "internal error", 500)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		http.Error(w, "could not save: "+err.Error(), 500)
		return
	}

	msg := "Captured — pending review."
	http.Redirect(w, r, fmt.Sprintf("/capture?msg=%s", url.QueryEscape(msg)), http.StatusSeeOther)
}

// --- small loaders (P4) ------------------------------------------------------

func (a *App) loadUsers(ctx context.Context, hh int64) ([]opt, error) {
	var users []opt
	rows, err := a.Pool.Query(ctx,
		`SELECT id, display_name FROM app_user WHERE household_id = $1 ORDER BY id`, hh)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u opt
		if err := rows.Scan(&u.ID, &u.Name); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, nil
}

func (a *App) creditCardAccounts(ctx context.Context, hh int64) ([]opt, error) {
	var out []opt
	rows, err := a.Pool.Query(ctx, `
		SELECT id, name FROM account
		WHERE household_id = $1 AND account_type = 'credit_card' AND is_active
		ORDER BY name`, hh)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var c opt
		if err := rows.Scan(&c.ID, &c.Name); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (a *App) activeMerchants(ctx context.Context, hh int64) ([]opt, error) {
	var out []opt
	rows, err := a.Pool.Query(ctx, `
		SELECT id, name FROM merchant
		WHERE household_id = $1 AND is_active
		ORDER BY name`, hh)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m opt
		if err := rows.Scan(&m.ID, &m.Name); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}
