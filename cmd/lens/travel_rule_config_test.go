package main

import "testing"

// B48.7 — LENS_TRAVEL_RULE: unset is off, "test" the Test adapter, "test:<address>=<provider>,…" names addresses a
// regulated provider holds, and anything else stops Lens starting.
func TestNewTravelRule(t *testing.T) {
	if tr, err := newTravelRule(""); tr != nil || err != nil {
		t.Fatalf("unset = %v, %v; want off", tr, err)
	}
	tr, err := newTravelRule("test:0xABC=Example Exchange Ltd, 0xdef = Other Ltd")
	if err != nil {
		t.Fatal(err)
	}
	if vasp, _ := tr.Provider(t.Context(), "base", "0xabc"); vasp != "Example Exchange Ltd" {
		t.Fatalf("0xabc's provider = %q", vasp)
	}
	if vasp, _ := tr.Provider(t.Context(), "base", "0xDEF"); vasp != "Other Ltd" {
		t.Fatalf("0xdef's provider = %q", vasp)
	}
	if _, err := newTravelRule("live"); err == nil {
		t.Fatal(`"live" was taken`)
	}
}
