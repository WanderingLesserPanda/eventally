package web

import (
	"strings"
	"testing"
)

// TestD39NavHighlightShape pins the D39 nav-pane highlight parity: the
// side-nav active-page and hover states use the same treatment as the
// Categories pane rows (settled across the D38 feedback rounds) - flat
// (no border radius), cream var(--sel) active tint with the full-height
// 3px accent bar via border-left (same geometry as .cat-row), and a flat
// var(--bg) hover tint. The old peach var(--band) tint and the inset
// rounded ::before bar are gone.
func TestD39NavHighlightShape(t *testing.T) {
	css := shapeRead(t, "static/app.css")

	// Base: the transparent 3px border-left keeps every label's text column
	// aligned whether or not the active bar is drawn (same as .cat-row).
	if !hasRule(css, ".sidenav a", "border-left: 3px solid transparent") {
		t.Errorf("base: missing the 3px transparent border-left placeholder")
	}

	// Active page: cream tint + accent bar, flat.
	if !hasRule(css, ".sidenav a.active", "background: var(--sel)") {
		t.Errorf("active: missing the cream var(--sel) tint (same as the selected category row)")
	}
	if !hasRule(css, ".sidenav a.active", "border-left-color: var(--accent)") {
		t.Errorf("active: missing the accent border-left bar")
	}
	if !hasRule(css, ".sidenav a.active", "border-radius: 0") {
		t.Errorf("active: must be flat (no border radius)")
	}

	// Hover: subtle flat tint, same as the category row hover.
	if !hasRule(css, ".sidenav a:hover", "background: var(--bg)") {
		t.Errorf("hover: missing the var(--bg) tint")
	}
	if !hasRule(css, ".sidenav a:hover", "border-radius: 0") {
		t.Errorf("hover: must be flat (no border radius)")
	}

	// The old treatment is gone.
	if hasRule(css, ".sidenav a.active", "var(--band)") {
		t.Errorf("active: the old peach var(--band) tint must be gone")
	}
	if strings.Contains(css, ".sidenav a.active::before") {
		t.Errorf("active: the old inset ::before accent bar must be gone")
	}
}
