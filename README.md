# codex-relay

Route Codex through the ChatGPT workspaces you are authorised to use, with quota rules you
can read in plain English and a dashboard that tells you what it is about to do and why.

One executable. No Node runtime, no Docker, no manual config editing for the supported
setup. It binds to loopback and keeps every credential in your operating system's own
credential store.

> The binary is `codexrelay`; the repository and module are `codex-relay`.

![The codex-relay dashboard: the account that will serve the next conversation, and what
each pooled account has left in every quota window it reports. Account names in this
screenshot are examples.](docs/screenshots/dashboard.png)

## What it does

- Connects one or more ChatGPT workspaces, each with its own independent browser sign-in.
- Chooses which workspace serves each turn, using rules and reusable routing profiles you
  configure in the dashboard, and explains every choice.
- Turns a complete routing strategy into a custom command such as `relaypool mine` or
  `relaypool weekend`. Commands and aliases are profile data, not hardcoded account names.
- Can pace selected workspaces toward a chosen amount of weekly quota remaining at reset,
  using an overflow workspace while they are on schedule.
- Can keep the active profile's normal model and workspace for a parent task while sending
  Codex-marked delegated Luna subagents to a chosen helper workspace. An unavailable helper
  falls back to the profile instead of blocking the parent task.
- Shows per-workspace quota with the reserve threshold marked on the same bar, and labels a
  reading that is stale or missing instead of guessing.
- Keeps conversation ownership across restarts. Between turns, an explicit preference can
  move an existing conversation to the preferred eligible workspace; automatic handoff also
  applies when its owner is nearly exhausted, paused, signed out, protected by a rule or
  unable to serve the requested model.
- Retries a quota refusal on another workspace only before any output reaches the client;
  once output starts, it never replays the turn.
- Forwards HTTP/SSE and WebSocket traffic, including cancellation.
- Records a bounded local history of decisions, with an explanation you can expand.

The signature rule, configured entirely in the dashboard:

> Protect Personal and prefer Acme Work when its weekly quota remaining is <= 30% and its
> reset is >= 4 days away. If Acme Work cannot serve the request, stop and explain.

## Install and run

Download the archive for your platform from [the latest
release](https://github.com/lgoyal6/codex-relay/releases/latest), check it against
`SHA256SUMS`, and unpack it. It contains the `codexrelay` executable and the `relaypool`
wrapper, and needs no runtime: no Go, no Node, no Docker.

```
./codexrelay setup   # detects Codex, previews the config change, applies it
./codexrelay serve   # runs the service and dashboard on 127.0.0.1:7788
```

To build it yourself instead, you need Go 1.25+ and Node 20+ to compile the dashboard.
Node is a build dependency only; the compiled dashboard ships inside the executable.

```
make build          # compiles the dashboard, codexrelay, and the relaypool wrapper into bin/
```

`setup` shows you the exact lines it will add before it writes anything, and takes a backup.
`codexrelay rollback` undoes it.

Open the dashboard at the address `serve` prints, then connect a workspace.

## Commands

| Command | What it does |
|---|---|
| `codexrelay serve` | Run the local service, proxy and dashboard |
| `codexrelay setup` | Detect Codex, preview the configuration change, apply it |
| `codexrelay status` | Which workspace a new conversation would use, and why |
| `codexrelay rules` | List the current rules |
| `codexrelay profile status` | Active profile, current decision, quota and pacing status |
| `codexrelay profile profiles` | List every custom profile command and alias |
| `codexrelay profile <command>` | Activate a profile through the running local service |
| `relaypool <command>` | Short form of `codexrelay profile <command>` from packaged builds |
| `codexrelay rollback` | Undo the Codex configuration change |
| `codexrelay doctor` | Redacted diagnostic report |
| `codexrelay version` | Print the version |

Every dashboard action has a CLI equivalent. Neither requires the other: closing the
dashboard does not affect routing.

Create and edit profiles on the dashboard's Profiles screen. A priority profile tries its
ordered workspaces after hard eligibility and reserve checks. A pace profile evaluates the
reported weekly windows on every turn. If a paced workspace is above its linear target, the
one furthest above target is preferred; otherwise the configured overflow workspace is
preferred. An eligible profile preference can move an existing conversation between turns,
but it never replays a turn after output starts.

Profiles can also enable **Luna helpers**. `codexrelay setup` sets Codex's native
`agents.default_subagent_model` to `gpt-5.6-luna`; the relay then prefers the profile's
helper workspace only when Codex explicitly marks a request as delegated work. Choosing Luna
for an ordinary parent task does not trigger helper routing. Activity and CSV exports label
every row as `parent` or `subagent`.

## How it connects to Codex

Codex is pointed at codex-relay through a `[model_providers.codexrelay]` entry in
`config.toml`, not through `chatgpt_base_url`. That distinction was established by testing,
not assumed: with only `chatgpt_base_url` set, codex-cli 0.154.0 still sent generation
traffic to chatgpt.com. See [docs/compatibility.md](docs/compatibility.md).

## Building and testing

```
make check      # go vet, go test, go build
make test       # go test ./...
make web        # compile the dashboard into internal/httpapi/dist
make cross      # build every supported OS/architecture target
```

`make cross` proves the code compiles for macOS, Linux and Windows on amd64 and arm64.
Platform CI separately proves native credential-store write, read and delete on hosted
macOS, Linux and Windows runners. Neither proves archive installation or full app behavior;
the exact boundary is stated in [docs/compatibility.md](docs/compatibility.md).

## Documentation

- [docs/compatibility.md](docs/compatibility.md) - versioned compatibility matrix, the
  protocol facts this build relies on, and what is unverified.
- [docs/setup-and-recovery.md](docs/setup-and-recovery.md) - setup, reconnect, migration from
  codex-lb, rollback and uninstall.
- [docs/architecture.md](docs/architecture.md) - how the pieces fit together and why.
- [CONTRIBUTING.md](CONTRIBUTING.md) - local checks and pull-request rules.
- [SECURITY.md](SECURITY.md) - private reporting and supported security boundaries.
- [docs/releasing.md](docs/releasing.md) - tagged releases and macOS signing requirements.

## Privacy and safety

- Credentials live only in the OS credential store. If that store is unavailable, connecting
  an account is refused rather than downgraded to a plaintext file.
- The dashboard binds to loopback and requires a session token, a literal loopback `Host`,
  and a loopback `Origin`. A hostname that merely resolves to 127.0.0.1 is refused.
- The Codex proxy cannot require that token, because Codex never loads the dashboard. It
  requires a literal loopback `Host` and refuses any `Origin` a web page could send, so a
  page open in your browser cannot run a Codex turn through your accounts.
- History never contains prompt text, conversation bodies, credentials or access tokens. It
  stores the model-reported input, cached-input, output and total token counts for each turn.
  The diagnostic export is redacted by design.
- codex-relay never reads, imports or competes over the refresh token Codex holds.
