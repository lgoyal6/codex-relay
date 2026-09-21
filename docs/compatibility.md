# Compatibility

Every claim on this page is labelled with how it was established. The four labels are used
consistently across this repository:

- **source** - read from the client's own source.
- **simulated** - reproduced in a Go test against a fake.
- **live** - exercised against the real `codex` binary and a local mock upstream.
- **unverified** - not established. Stated as such rather than assumed.

## Versions this matrix describes

| Component | Version | How |
|---|---|---|
| Codex CLI | `codex-cli 0.154.0`, Mach-O arm64 | live |
| Codex Desktop's bundled client | `codex-cli 0.153.4` (ChatGPT.app 26.903.71938, `Contents/Resources/codex`) | live |
| Host | macOS 26.5.2, arm64 | live |
| codex-relay | 0.1.0 | this repository |

These are the versions tested. This is not a claim about the latest upstream Codex release.

### Codex Desktop, stated precisely

The client binary Codex Desktop ships was run against codex-relay with an isolated
`CODEX_HOME` and completed a routed turn served by the pooled identity, identifying itself
upstream as `Codex Desktop/0.153.4 (Mac OS 26.5.2; arm64) dumb (codex_exec; 0.153.4)` (live).

The Desktop application itself has now also sent real tasks through the configured provider
entry on this host. That establishes generation routing from the GUI. It does not establish
account-bound MCP file upload behavior or every Desktop sign-in and recovery path; those
remain separate limits below.

## Integration point

Codex is configured through a `[model_providers.<id>]` entry. **`chatgpt_base_url` is not the
integration point for generation** (live). With only `chatgpt_base_url` set to a local
address, codex-cli 0.154.0 redirected `wham/*`, `ps/*`, `plugins/*` and analytics to the local
address, but sent `GET /backend-api/codex/models` and the `codex/responses` turn to
`chatgpt.com` regardless, failing with 401.

The supported configuration is:

```toml
model_provider = "codexrelay"

[model_providers.codexrelay]
name = "openai"
base_url = "http://127.0.0.1:7788/backend-api/codex"
wire_api = "responses"
supports_websockets = true
requires_openai_auth = true
```

`chatgpt_base_url` is deliberately **not** set. Setting it also captures the plugin and MCP
surfaces, which this build does not serve, and Codex then logs plugin and MCP failures
(live).

### A TOML rule that bit us

`model_provider` is a bare key, so in TOML it belongs to whatever table precedes it.
Appending the whole block to the end of a `config.toml` that already contains any table (an
MCP server, a profile, another provider) makes `model_provider` a key of *that* table: Codex
keeps its previous provider, nothing routes, and setup appears to succeed. Reproduced against
0.154.0 (live).

codex-relay therefore writes **three** managed regions: the bare provider key above the first
table header, its own provider table, and `default_subagent_model` inside `[agents]` as
required by Codex's `agents.default_subagent_model` setting. Rollback removes all three.

## Generation request, as captured live

`POST {base_url}/responses`, `accept: text/event-stream`.

| Header | Meaning | Proxy behaviour |
|---|---|---|
| `authorization` | client's own bearer token | **replaced** with the selected identity's |
| `chatgpt-account-id` | workspace selector sent by the client | **replaced** with the selected workspace |
| `thread-id` | conversation identity | the ownership key |
| `session-id` | client session | not an ownership key |
| `x-codex-window-id` | context window | not an ownership key |
| `x-codex-turn-metadata` | JSON with `thread_id`, `turn_id`, `root_turn_id`, ... | fallback source of `thread_id`; `turn_id` distinguishes attempts |
| `x-openai-subagent`, `x-codex-parent-thread-id`, subagent fields in turn metadata | delegated task markers | select the active profile's helper preference and label Activity; never inferred from model alone |
| `originator`, `user-agent` | client surface and version | forwarded unchanged |

These identifier scopes are **not** interchangeable. Ownership is keyed on `thread-id` only.

## Quota protocol

Headers on a real response (source, then confirmed live):

- `x-{limit}-primary-used-percent` (f64; the window is dropped if absent)
- `x-{limit}-primary-window-minutes` (i64)
- `x-{limit}-primary-reset-at` (i64, **absolute unix seconds**, not a duration)
- the same three for `-secondary-`
- `x-{limit}-limit-name`
- `x-codex-credits-has-credits`, `-unlimited`, `-balance`
- `x-codex-rate-limit-reached-type`

`{limit}` defaults to `codex`; other families are discovered by scanning for any
`x-*-primary-used-percent` header. The SSE event `codex.rate_limits` carries the same shape.

Consequences this build honours:

- **Quota is reported as `used_percent`. There is no remaining field.** Remaining is
  `100 - used_percent`, derived in exactly one place.
- Windows are an anonymous `primary`/`secondary` pair with a reported `window_minutes`.
  "5-hour" and "weekly" are inferences from 300 and 10080, so the UI labels windows from the
  reported duration and never assumes both exist.
- A window with no `used_percent` is **absent**, which is unknown, not clear.

The header names in the original project handoff (`reset-after-seconds`) do not exist in this
client.

## Transport

`supports_websockets = true` makes Codex attempt `GET ws://.../backend-api/codex/responses`
first. Scheme derivation is `http -> ws`, so a plain-HTTP loopback proxy is supported
(source, confirmed live).

When the upgrade fails, Codex retries roughly seven times across five visible reconnect
attempts and then **falls back to `POST .../responses` SSE and completes the turn** (live).
codex-relay forwards the upgrade rather than refusing it, so that dead reconnect time is not
spent. A refused upgrade returns the upstream's status so the client can decide to fall back,
and does **not** bind the conversation: binding on a failed upgrade would constrain the
fallback request for a turn that never ran (simulated).

## OAuth

From `codex-rs/login/` (source):

- issuer `https://auth.openai.com`; `/oauth/authorize`, `/oauth/token`, `/oauth/revoke`
- public PKCE client id `app_EMoamEEZ73f0CkXaXp7hrann`, S256
- redirect `http://localhost:{port}/auth/callback`, **ports 1455 and 1457 only**; the client
  id's redirect allow-list does not accept arbitrary ports
- scopes `openid profile email offline_access api.connectors.read api.connectors.invoke`
- refresh: `POST /oauth/token` with `{client_id, grant_type: "refresh_token", refresh_token}`

**Refresh tokens rotate and are single use.** The client carries a distinct failure reason for
a reused one. Two holders of the same refresh token invalidate each other. This is why every
connected identity gets its own independent browser sign-in and why codex-relay never reads
Codex's own refresh token.

Because ports 1455 and 1457 are the only usable redirects, a codex-relay sign-in cannot run at
the same time as a `codex login`.

## Identity

`auth.json` is `{auth_mode, tokens:{id_token, access_token, refresh_token, account_id}, last_refresh}`.
The client parses `email`, `chatgpt_plan_type`, `chatgpt_user_id`, `chatgpt_account_id` and
`chatgpt_account_is_fedramp` from the id token, decoding the JWT payload **without signature
verification** (source).

- account identity = `chatgpt_user_id`
- workspace identity = `chatgpt_account_id`
- human-readable name = `GET {chatgpt_base_url}/wham/accounts/check` -> `accounts[].name`

Email is a property of an account, never a key: one account can hold several workspaces.

Codex requests `id_token_add_organizations=true` but does not parse an organizations claim
(source), so names come from `accounts/check`.

The client pins its own sign-in session to its startup `account_id` plus `chatgpt_user_id`
and ignores a reloaded auth that does not match (source). That does not pin a proxied
conversation to one relay workspace: the Responses request resends the conversation on each
turn, so codex-relay can select a different eligible credential between turns. It never
replays a turn after response output has started.

## Which requests actually reach codex-relay

This matters more than the route table below, and it was established by capturing every
request from a real client against two separate mock origins (one as the provider
`base_url`, one as `chatgpt_base_url`) so the two could be told apart.

**Only `{provider.base_url}/...` traffic reaches codex-relay.** Across 132 proxied requests in
the live runs, exactly two paths ever arrived:

```
  16  /backend-api/codex/models
 116  /backend-api/codex/responses
   0  anything else  (no fail-closed refusal was triggered in any run)
```

Everything else is built from `chatgpt_base_url` and goes **straight to chatgpt.com under the
client's own credential**, bypassing codex-relay entirely. Observed live:

```
 /backend-api/wham/settings/user
 /backend-api/ps/mcp, /ps/plugins/installed, /ps/plugins/list, /ps/plugins/suggested/codex
 /backend-api/plugins/featured
 /backend-api/codex/analytics-events/events
```

Source confirms the same split: `backend-client/src/client.rs` builds every `/wham/*` and
`/api/codex/*` route from its `base_url`, which is `chatgpt_base_url`.

### Consequence: file attachments are NOT pooled

`codex-rs/codex-api/src/files.rs::upload_openai_file` is called with
`turn_context.config.chatgpt_base_url` (`core/src/mcp_openai_file.rs`). So the two-step file
upload - `POST {chatgpt_base_url}/files` then `POST {chatgpt_base_url}/files/{id}/uploaded`,
with the blob itself going to an absolute Azure URL - never passes through codex-relay and
uses the client's own account.

**A file uploaded this way belongs to the signed-in client's account, not to the pooled
workspace that serves the turn.** If a later turn is served by a different workspace and
references that `file_id`, the owning account is not the serving account. codex-relay cannot
currently record ownership for these files because it never sees them.

This path is reached through MCP file inputs. It was **not** exercised in the live runs:
`codex exec --image` inlines an image into the turn body instead (verified live - the turn
body grew and no `/files` request appeared at either origin), so image attachments do work
normally through the pool.

#### Why it is not simply fixed by also serving `chatgpt_base_url`

Investigated properly in session 4, and the blocker is not the extra surface area; it is
identity.

`codex-api/src/files.rs::authorized_request` builds the upload request's headers from
`auth.add_auth_headers()` and nothing else. That yields `Authorization` and
`ChatGPT-Account-ID` only. **There is no `thread-id`, no `session-id` and no
`x-codex-turn-metadata` on a file upload**, unlike a generation request, which carries all
three (see the captured request table above).

So even with `chatgpt_base_url` pointed at codex-relay, we would receive the upload with no way
to know which conversation it belongs to, and therefore no way to know which workspace will
serve the turn that references it. Any pooled attribution would be a guess, and a wrong guess
uploads the file under one account and references it from another. That failure surfaces
inside a model turn, where the user cannot do anything about it, and it is worse than the
limitation it would be trying to fix.

**Decision for the first release: file uploads keep the client's own credential.** They are
classified `client_owned` in `internal/proxy/routes.go`, so if a user does point
`chatgpt_base_url` at codex-relay, the upload is forwarded untouched and behaves exactly as it
does without codex-relay, rather than failing closed. Before this was classified it fell into
the unknown-route case and was refused with HTTP 502, which would have broken MCP file inputs
outright for that configuration.

`TestFileUploadsStayWithTheSignedInClient` pins this. If a `thread-id` ever appears on
`/files` upstream, that test is the place that should fail, so the decision gets revisited
deliberately rather than by accident.

The limitation is stated to the user in the dashboard under Settings, "Known limitations",
not only in this document.

### Consequence: most of the route table is defensive, not active

The classes below for `/wham/*`, `/ps/*`, `/plugins*` and `/analytics-events*` describe what
codex-relay would do **if** a user also pointed `chatgpt_base_url` at it. In the supported
configuration those requests never arrive. The classification is kept so the behaviour is
defined rather than accidental.

## Route and identity matrix

| Class | Routes | Credential |
|---|---|---|
| Generation | `POST /backend-api/codex/responses` and its `GET` upgrade | selected pooled identity, subject to thread ownership |
| Model catalog | `GET /backend-api/codex/models` | selected pooled identity; the reported slugs are recorded as that workspace's eligibility |
| Identity-scoped | `/wham/usage`, `/wham/accounts/check`, `/wham/profiles/me` | that identity's own credential; the answer describes **one** identity |
| Client-owned | `/wham/settings/user`, `/ps/*`, `/plugins*`, `/analytics-events*` | the client's own credential, passed through untouched |
| Anything else | unknown | **fail closed**: no pooled credential is attached |

`GET /wham/usage` carries `account_id` and `user_id` in its body, so it describes one identity
and cannot describe whichever account is serving an arbitrary live conversation. codex-relay
does not synthesise a pooled total for it.

A model catalog response must be a complete `ModelInfo`, or the client rejects the **whole**
catalog and falls back to default model metadata. Establishing this took three rounds against
the real client (live): `supported_reasoning_levels` must be a list of
`{effort, description}` objects rather than strings, `shell_type` is required, and a model
must carry either `base_instructions` or `model_messages.instructions_template`. codex-relay
itself is tolerant here - it records whatever slugs it can parse and passes the bytes through
unchanged - but a test harness that omits these fields is quietly unrepresentative.

## Verification status

This table separates live, CI, simulated and unverified evidence.

| Area | Status |
|---|---|
| Live OAuth against `auth.openai.com` | **unverified.** Requires the user's real sign-in. The flow, PKCE, callback and claim parsing are covered by tests against a fake issuer (simulated). |
| Live refresh-token rotation against the real issuer | **unverified.** Rotation, per-chain serialization and persistence of the rotated token are covered against a fake issuer (simulated). |
| Real `chatgpt.com` behaviour for compaction, tool calls, file uploads and resume under pooling | **unverified.** |
| Codex Desktop generation routing | **live.** The bundled client has sent real turns through codex-relay on this host. Account-bound MCP file upload behavior remains unverified. |
| Windows credential storage and installation | **Credential storage CI-verified.** A GitHub-hosted `windows-latest` runner completed a real Credential Manager write, read and delete. Archive installation remains unverified. |
| Linux Secret Service storage and installation | **Credential storage CI-verified.** A GitHub-hosted `ubuntu-latest` runner completed a real write, read and delete through gnome-keyring in a D-Bus session. Archive installation and integration with a user's desktop keyring remain unverified. |
| Windows native versus WSL | **unverified.** A Codex installed inside WSL has its own `~/.codex` and must be configured from inside WSL. `codexrelay doctor` says so on Windows, but this has not been tested. |
| Agent Identity behind a loopback base URL | **unverified, and known to be at risk.** The client accepts only chatgpt.com, chat.openai.com and chatgpt-staging.com when deriving an agent identity environment (source), so enabling that feature behind a loopback base URL would fail. It did not block any turn in these runs. |
| Usability with real users | **unverified.** No external testers were available. |
| macOS Keychain | **live.** Write, read back and delete verified by a real round trip. |
