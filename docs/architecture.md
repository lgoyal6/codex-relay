# Architecture

One process. A Go service that is simultaneously the proxy Codex talks to, the API the
dashboard talks to, and the CLI.

```
codex ──HTTP/SSE, WebSocket──> codexrelay :7788 ──> chatgpt.com/backend-api
                                  │
                                  ├── policy evaluator (in memory, no database on this path)
                                  ├── SQLite (ownership, config, bounded history)
                                  ├── OS credential store (tokens, never SQLite)
                                  └── embedded React dashboard + local event stream
```

## Packages

| Package | Responsibility |
|---|---|
| `internal/policy` | The rule model and the **single** evaluator. Live admission and dashboard preview both call `Evaluate`, so a preview cannot promise something admission would not do. |
| `internal/routing` | Immutable state snapshots published atomically. The decision path never touches the database. |
| `internal/store` | SQLite. Migrations, durable thread and resource ownership, bounded history. |
| `internal/secrets` | OS credential storage, with a real round-trip probe. Never falls back to plaintext. |
| `internal/oauth` | PKCE browser flow and identity claim parsing. |
| `internal/upstream` | Rate-limit header parsing for the live header family. |
| `internal/proxy` | Route classification, credential replacement, streaming, cancellation, the WebSocket bridge. |
| `internal/service` | Wires the above together: credential manager, snapshot builder, rules, preview, connector. |
| `internal/httpapi` | The loopback JSON API, its security guard, and the embedded dashboard. |
| `internal/integration` | Codex detection, the change plan, apply and precise rollback. |
| `internal/clock` | Injected time, so time-dependent rules are tested by advancing a fake clock rather than by sleeping. |

## Decisions worth knowing

**One evaluator, one snapshot.** `policy.Evaluate` is a pure function of an immutable
`State`, a `Request` and a `time.Time`. Preview deep-copies the published snapshot, applies
scenario overrides to the copy, and calls the same function. It cannot mutate live quota or
ownership, and it consumes no model quota.

**Ownership is required state, history is not.** Thread ownership is written
transactionally, before any account-bound state is exposed, and a claim never steals an
existing binding. Activity rows go through a bounded channel to a background writer: a full
queue drops rows rather than slowing a stream. Routing is not allowed to wait on history.

**Unknown is not false.** A missing quota window, a missing reset time and an unobserved
model list are each explicitly unknown. A protection-sensitive input that cannot be
established holds the reserve rather than assuming it is clear. There are no hidden threshold
margins and no undocumented latches.

**We integrate through the provider entry only, not `chatgpt_base_url`.** Codex builds two
families of URL. Generation and the model catalog come from the provider `base_url` and
reach us; everything else - `wham/*`, `ps/*`, `plugins/*`, file uploads, analytics - is built
from `chatgpt_base_url` and goes straight to chatgpt.com under the client's own credential.
Setting `chatgpt_base_url` as well would capture those surfaces, which means we would have to
serve the plugin and MCP endpoints too or the client logs failures for them. For the first
release we take the narrow integration and accept the consequence: **file attachments
uploaded through MCP belong to the signed-in client's account, not to the pooled workspace
that serves the turn.** Measured live and written up in
[compatibility.md](compatibility.md#which-requests-actually-reach-codex-relay).

**Unknown routes fail closed.** A path we have not classified gets no pooled credential, and
an unknown mutation is refused with a message naming the route, so a newer Codex shows up as
an actionable diagnostic rather than silently borrowing someone's token.

**Never replay a turn after output has started.** Replay safety has not been demonstrated for
this protocol, so a mid-stream failure is surfaced, not re-sent. A retry budget would bound
retries; it would not make replay safe. Proxy-level retries would also multiply with the
client's own retries.

**Refresh is serialized per credential chain.** The issuer rotates refresh tokens and rejects
a reused one, so two concurrent refreshes of one chain would permanently break that identity.
A per-chain mutex plus a re-read after acquiring it means the second caller uses the first
caller's rotated token. Tested with 25 concurrent callers asserting exactly one rotation.

**Loopback is not by itself protection.** Any page the browser loads can reach a loopback
port, and an attacker-controlled name can resolve to 127.0.0.1. State-changing calls require
all three of: a literal loopback `Host` with our port, a loopback `Origin` when one is
present, and a session token issued to the dashboard page itself. The token is delivered
inside the HTML, never in a URL or a cookie.

**Failure classes are distinguished.** Credential, quota, transient, blocked-by-policy,
ownership conflict, upstream upgrade refusal and client cancellation are separate classes in
Activity. A cancelled turn and a completed turn both end on a 200, so only how the stream
ended tells them apart.

## Data locations

| | Path |
|---|---|
| Database, logs | macOS `~/Library/Application Support/codex-relay`; Linux `${XDG_DATA_HOME:-~/.local/share}/codex-relay`; Windows `%LOCALAPPDATA%\codex-relay` |
| Credentials | OS credential store, service name `codex-relay`, one entry per workspace |
| Codex configuration | `$CODEX_HOME/config.toml`, default `~/.codex/config.toml` |

Per-user, never system-wide: this is a personal tool holding personal credentials, and a
per-user install needs no administrator rights.

Override the data directory with `CODEXRELAY_HOME` and the upstream with
`CODEXRELAY_UPSTREAM`. Both exist so the test harness can run fully isolated from a real
installation.

## Frontend

React 19 and TypeScript, built by Vite into `internal/httpapi/dist` and embedded with
`go:embed`. All assets are local; a strict Content-Security-Policy forbids remote scripts,
styles, fonts, images and connections. Node is a contributor build dependency, not an end
user requirement.

The dashboard follows a local event stream and re-reads state when the service publishes a
new snapshot. It is a subscriber: closing it does not affect routing, and when the stream
drops the page says so and falls back to polling.
