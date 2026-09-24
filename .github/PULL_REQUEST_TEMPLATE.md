## What changes, and what a user sees

<!-- The user-visible behaviour, not just the mechanism. -->

## Evidence

<!-- Fresh output from `go vet ./...`, `go test -count=1 ./...` and the build. For a bug
     fix, the regression test that fails without the change. -->

## Invariants

- [ ] One focused change.
- [ ] Routing stays fail-closed: an unknown route never receives a pooled credential.
- [ ] No turn is retried after response output has started.
- [ ] No prompt text, conversation bodies or credentials reach logs or SQLite.
- [ ] Compatibility documentation updated if a route, identity, ownership or platform claim changed.
- [ ] No credentials, database files, logs, diagnostic exports or real account ids committed.
