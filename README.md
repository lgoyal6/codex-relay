# codex-relay

Route Codex through the ChatGPT workspaces you are authorised to use, with quota rules you
can read in plain English and a dashboard that tells you what it is about to do and why.

One executable. No Node runtime, no Docker, no manual config editing for the supported
setup. It binds to loopback and keeps every credential in your operating system's own
credential store.

> The binary is `codexrelay`; the repository and module are `codex-relay`.

## What it does

- Connects one or more ChatGPT workspaces, each with its own independent browser sign-in.
- Chooses which workspace serves each turn, using rules you configure in the dashboard, and
  explains every choice.
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

Requires Go 1.25+ to build, and Node 20+ to compile the dashboard. Node is a contributor
build dependency only.

```
make build          # compiles the dashboard, then the executable, into bin/
./bin/codexrelay setup   # detects Codex, previews the config change, applies it
./bin/codexrelay serve   # runs the service and dashboard on 127.0.0.1:7788
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
| `codexrelay rollback` | Undo the Codex configuration change |
| `codexrelay doctor` | Redacted diagnostic report |
| `codexrelay version` | Print the version |

Every dashboard action has a CLI equivalent. Neither requires the other: closing the
dashboard does not affect routing.

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
- History never contains prompt text, conversation bodies, credentials or access tokens. It
  stores the model-reported input, cached-input, output and total token counts for each turn.
  The diagnostic export is redacted by design.
- codex-relay never reads, imports or competes over the refresh token Codex holds.
