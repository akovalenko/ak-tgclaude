package main

import "testing"

func TestAllowList(t *testing.T) {
	a := newAllowList([]int64{1, 2})
	if !a.Allowed(1) || !a.Allowed(2) {
		t.Error("listed ids should be allowed")
	}
	if a.Allowed(3) {
		t.Error("unlisted id should be denied")
	}
	if newAllowList(nil).Allowed(1) {
		t.Error("empty allow list should deny everyone (default-closed)")
	}
	if newAllowList(nil).Allowed(0) {
		t.Error("id 0 (no sender) should be denied")
	}
}

func TestOpenAccess(t *testing.T) {
	if !(openAccess{}).Allowed(12345) || !(openAccess{}).Allowed(0) {
		t.Error("open access should allow every id")
	}
}
