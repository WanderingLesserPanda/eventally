package web

import (
	"strings"
	"testing"
)

// cssBlock returns the full declaration block for `sel` (multi-line rules
// included; hasRule only matches single-line rules).
func cssBlock(t *testing.T, css, sel string) string {
	t.Helper()
	idx := strings.Index(css, sel+" {")
	if idx < 0 {
		t.Fatalf("selector %s not found in app.css", sel)
	}
	start := strings.Index(css[idx:], "{") + idx
	end := strings.Index(css[start:], "}")
	if end < 0 {
		t.Fatalf("unterminated block for %s", sel)
	}
	return css[start : start+end+1]
}

// TestD40PaneHeaderControlsShape pins the D40 header-control look: the
// "Back to Home" button and the household switcher share one treatment -
// 8px rounded corners (the old 999px pill was too round, per the user's
// screenshot round) and a white var(--card) fill (the switcher used to sit
// bare on the page background).
func TestD40PaneHeaderControlsShape(t *testing.T) {
	css := shapeRead(t, "static/app.css")

	backHome := cssBlock(t, css, ".back-home")
	if !strings.Contains(backHome, "border-radius: 8px") {
		t.Errorf(".back-home: missing border-radius: 8px (the 999px pill was too round)")
	}
	if strings.Contains(backHome, "999px") {
		t.Errorf(".back-home: the old 999px pill radius must be gone")
	}
	if !strings.Contains(backHome, "background: var(--card)") {
		t.Errorf(".back-home: missing the white var(--card) fill")
	}

	hh := cssBlock(t, css, ".hh-current")
	if !strings.Contains(hh, "background: var(--card)") {
		t.Errorf(".hh-current: missing the white var(--card) fill (was bare on the background)")
	}
	if strings.Contains(hh, "background: none") {
		t.Errorf(".hh-current: the bare background: none must be gone")
	}
}
