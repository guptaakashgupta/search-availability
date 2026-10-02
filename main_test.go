package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func Test_categoryAncestors(t *testing.T) {
	got := categoryAncestors("dairy/milk/fresh-milk")
	want := []string{"dairy", "dairy/milk", "dairy/milk/fresh-milk"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func Test_buildSearch_storeFilterVsBoost(t *testing.T) {
	hide, _ := json.Marshal(buildSearch("milk", "S001", "", true))
	if !strings.Contains(string(hide), `"filter":[{"term":{"available_store_ids":"S001"}}]`) {
		t.Errorf("hideOOS should filter by store: %s", hide)
	}
	demote, _ := json.Marshal(buildSearch("milk", "S001", "", false))
	if !strings.Contains(string(demote), `"should":[{"constant_score"`) {
		t.Errorf("show-oos should boost by store, not filter: %s", demote)
	}
}
