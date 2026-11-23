package web

import (
	"testing"

	"github.com/WanderingLesserPanda/eventally/internal/money"
)

// TestCatEnvelopeTotal pins the detail-pane "Total Budget" envelope rule
// (ADR-0012) and the 2026-11-24 note revision (4th-review item 8): the no-budget
// note is shown only when the part-of-parent slices are the ENTIRE total, and is
// suppressed when standalone sub budgets also contribute to the total (the note's
// slice sum would then understate the displayed total).
func TestCatEnvelopeTotal(t *testing.T) {
	pop := "part_of_parent"
	st := "standalone"
	cases := []struct {
		name     string
		cat      Category
		subs     []Category
		wantTot  money.Money
		wantHas  bool
		wantNote string // "" means no note
	}{
		{
			name:    "no-budget with a standalone sub: note suppressed, total = standalone + slices",
			cat:     Category{Name: "TestCat", HasBudget: false},
			subs:    []Category{{Name: "PartA", Budget: 1200, HasBudget: true, BudgetMode: pop},
				{Name: "PartB", Budget: 1100, HasBudget: true, BudgetMode: pop},
				{Name: "StandaloneA", Budget: 10000, HasBudget: true, BudgetMode: st}},
			wantTot: 12300, wantHas: true, wantNote: "",
		},
		{
			name:    "no-budget slices only: note shows (multi), total = slices",
			cat:     Category{Name: "Bills", HasBudget: false},
			subs:    []Category{{Name: "Internet", Budget: 6000, HasBudget: true, BudgetMode: pop},
				{Name: "Water", Budget: 9000, HasBudget: true, BudgetMode: pop}},
			wantTot: 15000, wantHas: true,
			wantNote: "No budget on Bills, 150 from 2 subcategories adds to the total.",
		},
		{
			name:    "no-budget single slice: note shows (single), total = slice",
			cat:     Category{Name: "Rent", HasBudget: false},
			subs:    []Category{{Name: "Security", Budget: 3000, HasBudget: true, BudgetMode: pop}},
			wantTot: 3000, wantHas: true,
			wantNote: "No budget on Rent, 30 from Security adds to the total.",
		},
		{
			name:    "budgeted over-allocated: over-allocation note (unchanged)",
			cat:     Category{Name: "Food", HasBudget: true, Budget: 10000},
			subs:    []Category{{Name: "Groceries", Budget: 8000, HasBudget: true, BudgetMode: pop},
				{Name: "Dining", Budget: 7000, HasBudget: true, BudgetMode: pop}},
			wantTot: 15000, wantHas: true,
			wantNote: "Over-allocated by 50, the total budget for Food becomes 150.",
		},
		{
			name:    "no budget, no subs: no total, no note",
			cat:     Category{Name: "Empty", HasBudget: false}, subs: nil,
			wantTot: 0, wantHas: false, wantNote: "",
		},
		{
			name:    "budgeted, slices under envelope, no note",
			cat:     Category{Name: "Tools", HasBudget: true, Budget: 5000},
			subs:    []Category{{Name: "Drill", Budget: 2000, HasBudget: true, BudgetMode: pop}},
			wantTot: 5000, wantHas: true, wantNote: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			total, has, note := catEnvelopeTotal(tc.cat, tc.subs)
			if total != tc.wantTot {
				t.Errorf("total = %d, want %d", total, tc.wantTot)
			}
			if has != tc.wantHas {
				t.Errorf("hasTotal = %v, want %v", has, tc.wantHas)
			}
			if note != tc.wantNote {
				t.Errorf("note = %q, want %q", note, tc.wantNote)
			}
		})
	}
}
