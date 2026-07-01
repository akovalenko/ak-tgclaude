package main

// Authorizer decides whether a Telegram user may use the bot. The dispatcher
// consults it before doing anything with an update, so this is the single seam
// for access control: a future stateful/invite implementation (persisted
// allowlist mutated by an admin `/allow`) can replace the static one here
// without touching the routing.
type Authorizer interface {
	Allowed(userID int64) bool
}

// openAccess authorizes everyone — the explicit demo/open mode. Not real access
// control; the dispatcher logs a loud warning when it is in effect.
type openAccess struct{}

func (openAccess) Allowed(int64) bool { return true }

// allowList authorizes a fixed set of Telegram user ids (the static config
// whitelist). An empty list authorizes no one — default-closed, so an
// unconfigured bot is shut rather than open.
type allowList struct{ ids map[int64]bool }

func newAllowList(ids []int64) *allowList {
	m := make(map[int64]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return &allowList{ids: m}
}

func (a *allowList) Allowed(id int64) bool { return a.ids[id] }
