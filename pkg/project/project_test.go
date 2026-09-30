package project

import "testing"

var all = []Status{StatusQueued, StatusBuilding, StatusPushing, StatusDeploying, StatusReady, StatusFailed}

func TestLegalTransitions(t *testing.T) {
	legal := map[[2]Status]bool{
		{StatusQueued, StatusBuilding}:   true,
		{StatusQueued, StatusDeploying}:  true, // an existing image needs no build
		{StatusQueued, StatusFailed}:     true,
		{StatusBuilding, StatusPushing}:  true,
		{StatusBuilding, StatusFailed}:   true,
		{StatusPushing, StatusDeploying}: true,
		{StatusPushing, StatusFailed}:    true,
		{StatusDeploying, StatusReady}:   true,
		{StatusDeploying, StatusFailed}:  true,
	}
	for _, from := range all {
		for _, to := range all {
			if got := CanMove(from, to); got != legal[[2]Status{from, to}] {
				t.Errorf("CanMove(%s, %s) = %v, want %v", from, to, got, !got)
			}
		}
	}
}

// A finished deployment must never come back to life: a rollback is a new
// deployment, not an edit of an old one.
func TestFinalStatusesHaveNoWayOut(t *testing.T) {
	for _, from := range []Status{StatusReady, StatusFailed} {
		if !from.Final() {
			t.Errorf("%s should be final", from)
		}
		for _, to := range all {
			if CanMove(from, to) {
				t.Errorf("%s -> %s must be illegal", from, to)
			}
		}
	}
	for _, s := range []Status{StatusQueued, StatusBuilding, StatusPushing, StatusDeploying} {
		if s.Final() {
			t.Errorf("%s should not be final", s)
		}
	}
}
