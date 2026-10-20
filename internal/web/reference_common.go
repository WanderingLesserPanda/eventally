package web

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// formInt parses a bounded optional integer form field. Empty -> 0 (no
// error); out of range -> error with a user-readable message.
func formInt(r *http.Request, name string, min, max int) (int, error) {
	s := strings.TrimSpace(r.FormValue(name))
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%s must be a whole number between %d and %d", name, min, max)
	}
	return n, nil
}

// flashField renders a list-page flash message (rendered by templates).
// Kept here so all reference list handlers share the convention:
// messages travel in ?msg= via 303 redirects and are one-shot.

// splitEditorKeys maps a split-editor row key ("user:N" / "cp:N") to the
// party id that owns the share — members resolve to their seeded member
// party row (per household), outsiders to their party id directly.
func (a *App) splitEditorKeys(ctx context.Context, hh int64, keys []string) ([]int64, error) {
	var ids []int64
	for _, k := range keys {
		kind, id, err := parseParticipantKey(k)
		if err != nil {
			return nil, err
		}
		pid := id
		if kind == "household_user" {
			if err := a.Pool.QueryRow(ctx,
				`SELECT p.id FROM party p WHERE p.app_user_id = $1 AND p.household_id = $2`,
				id, hh).Scan(&pid); err != nil {
				return nil, fmt.Errorf("member %d has no party row (membership missing?)", id)
			}
		}
		ids = append(ids, pid)
	}
	return ids, nil
}
