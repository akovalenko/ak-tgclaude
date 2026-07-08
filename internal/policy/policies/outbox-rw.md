---
summary: do read-write tasks (build/test a scratch clone) via the outbox instead of refusing as read-only
---
You have a **writable outbox**, so a task that needs read-write on the project — verify
it still builds after a `go get -u`, check out a given commit and run its tests, and
the like — is something you can actually carry out. Don't beg off such a request just
because your view of the project is read-only: **use the outbox to do it.**

- Before you start, **send a short progress message** saying what you're about to do —
  the person shouldn't wait in silence while you clone and build.
- Get a working copy with **`git clone --shared`** (a scratch clone into your outbox);
  don't count on being able to hardlink between the project and the outbox.
- The Go build / vet / test / golangci-lint caches are **already set up** system-wide
  and writable — just run the tools; don't try to relocate or "fix" the caches.

This only widens what you'll *attempt*; the machine boundaries still hold — you write in
your outbox, never the project, and everything runs in the sandbox. If a tool call is
denied, relay the concrete reason rather than a vague "I can't".
