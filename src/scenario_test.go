package main

import "testing"

func TestScenarioUsers(t *testing.T) {
	users := generateUsers(newRng(scenarioSeedFor("users")), 12)
	want := []string{"adaiversen", "quinnkeller", "olgabauer", "dmitriquist", "adanovak", "zanerossi", "pavelcosta", "wrenkeller", "coraquist", "elenaulrich", "tesssato", "svenrossi"}
	if len(users) != len(want) {
		t.Fatalf("got %d users, want %d", len(users), len(want))
	}
	for i, w := range want {
		if users[i].Login != w {
			t.Fatalf("user %d: got %q, want %q", i, users[i].Login, w)
		}
	}
	if got := scenarioViewerLogin(); got != "adaiversen" {
		t.Fatalf("viewer login %q, want adaiversen", got)
	}
}
