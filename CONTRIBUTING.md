# Contributing

Thanks for helping improve codex-relay. Keep changes small, explain the user-visible behavior,
and include a regression test for bugs.

## Local setup

You need Go 1.25 or newer and Node.js 20 or newer.

```sh
cd web
npm ci
npm run build
cd ..
go vet ./...
go test -count=1 ./...
go build ./cmd/codexrelay
```

Some tests start loopback HTTP servers. Credential-store tests use the native OS store when
one is available and otherwise skip locally. Platform CI sets
`CODEXRELAY_REQUIRE_OS_KEYRING=1`, which makes an unavailable store fail instead of skip.

## Pull requests

- Open one focused change per pull request.
- Do not commit credentials, database files, logs, diagnostic exports, or real account IDs.
- Preserve fail-closed routing. An unknown route must never receive a pooled credential.
- Never retry a turn after response output has started.
- Keep prompt text and conversation bodies out of logs and SQLite.
- Update compatibility documentation when changing a route, identity, ownership, or
  platform claim.
- Include fresh output from the build and test commands above.

By contributing, you agree that your contribution is licensed under the repository's MIT
License.
