package main

import "testing"

func TestSelectMenuActionLinePrefersMinimumAggressiveOption(t *testing.T) {
	t.Parallel()

	menu := &tableTurnMenu{
		Options: []tableMenuOption{
			{Action: tableAction{Type: "check"}},
			{Action: tableAction{Type: "bet", TotalSats: intPtr(1200)}},
			{Action: tableAction{Type: "bet", TotalSats: intPtr(800)}},
		},
	}

	selected, ok := selectMenuActionLine(menu, "preflop", 0, 0, false, false)
	if !ok {
		t.Fatal("expected a deterministic menu-backed selection")
	}
	if selected != "bet 800" {
		t.Fatalf("expected minimum aggressive option, got %q", selected)
	}
}

func TestSelectMenuActionLineAvoidsShowdownOnRiver(t *testing.T) {
	t.Parallel()

	menu := &tableTurnMenu{
		Options: []tableMenuOption{
			{Action: tableAction{Type: "call"}},
			{Action: tableAction{Type: "fold"}},
			{Action: tableAction{Type: "raise", TotalSats: intPtr(1600)}},
		},
	}

	selected, ok := selectMenuActionLine(menu, "river", 0, 400, true, false)
	if !ok {
		t.Fatal("expected a deterministic menu-backed selection")
	}
	if selected != "fold" {
		t.Fatalf("expected river avoid-showdown selection to fold, got %q", selected)
	}
}

func TestSelectMenuActionLineSkipsZeroProgressAggression(t *testing.T) {
	t.Parallel()

	menu := &tableTurnMenu{
		Options: []tableMenuOption{
			{Action: tableAction{Type: "check"}},
			{Action: tableAction{Type: "bet", TotalSats: intPtr(330)}},
			{Action: tableAction{Type: "bet", TotalSats: intPtr(660)}},
		},
	}

	selected, ok := selectMenuActionLine(menu, "preflop", 330, 0, false, false)
	if !ok {
		t.Fatal("expected a deterministic menu-backed selection")
	}
	if selected != "bet 660" {
		t.Fatalf("expected selector to skip zero-progress aggression, got %q", selected)
	}
}

func TestSelectMenuActionLinePrefersEarlySettlement(t *testing.T) {
	t.Parallel()

	menu := &tableTurnMenu{
		Options: []tableMenuOption{
			{Action: tableAction{Type: "call"}},
			{Action: tableAction{Type: "fold"}},
			{Action: tableAction{Type: "raise", TotalSats: intPtr(1320)}},
		},
	}

	selected, ok := selectMenuActionLine(menu, "preflop", 330, 330, false, true)
	if !ok {
		t.Fatal("expected a deterministic menu-backed selection")
	}
	if selected != "fold" {
		t.Fatalf("expected selector to prefer early settlement, got %q", selected)
	}
}

func intPtr(value int) *int {
	return &value
}
