package web

import (
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
