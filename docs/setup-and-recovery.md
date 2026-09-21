# Setup and recovery

## First run

1. `codexrelay setup`

   Detects your Codex installation, prints the exact lines it proposes to add, and waits for
   you to agree. Nothing is written before you answer. Use `--yes` to skip the prompt and
   `--addr` if you are not using the default `127.0.0.1:7788`.

   It takes a timestamped backup of `config.toml` next to the original and records the change
   in its own database so rollback is precise later.

   If Codex is already using another provider, that line is **commented out**, not deleted,
   and rollback restores it. Setup also sets Codex's native `default_subagent_model` to
   `gpt-5.6-luna`; an existing top-level value is commented out and restored on rollback.

2. `codexrelay serve`

   Prints the dashboard address. Open it.

3. Connect a workspace.

   Your browser opens for a normal ChatGPT sign-in. codex-relay stores the resulting
   credential in your OS credential store under its own entry. It never reads, copies or
   shares the credential Codex holds.

   One sign-in can produce several workspaces: they are listed separately because an account
   and a workspace are different identities.

Nothing here requires editing a configuration file by hand.

4. Create routing profiles on the Profiles screen.

   Give each profile a command and optional aliases, choose an ordered fallback list, and
   optionally pace selected workspaces toward a weekly reset target. Packaged builds include
   `relaypool`, so `relaypool profiles`, `relaypool status`, and `relaypool <command>` all use
   the same live service and policy as the dashboard. If the relay uses a non-default port,
   set `CODEXRELAY_ADDR`, for example `http://127.0.0.1:7815`.

   To use a Free workspace for delegated work, enable **Luna helpers** in the profile and
   choose that workspace. Parent turns still use the profile's normal priority or pace
   policy. Only requests Codex marks as subagents are preferred to the helper, and an
   unavailable helper falls back through the normal profile order.

### Verification requests

Setup does not send a model request on your behalf. Quota appears for a workspace after its
first real routed turn, because quota is read from the headers of responses you were already
going to receive. The dashboard says "No quota reading yet" until then rather than showing a
guess.

## Reconnecting a workspace

Use **Reconnect** on the Workspaces screen, or on the problem row on Overview.

Reconnect replaces the stored credential for that workspace with a fresh browser sign-in.
Conversations keep their owner unless an explicit preference now selects another eligible
workspace or an automatic handoff condition applies. If the credential is unavailable when
a turn starts, codex-relay can hand the conversation to an eligible alternative instead.

You will need this when:

- the stored sign-in expired and could not be refreshed;
- the refresh token was rotated elsewhere and ours was rejected (they are single use);
- you revoked access and signed in again.

The dashboard marks the workspace **needs sign-in** and puts a Reconnect action beside the
problem, rather than failing silently at the next turn.

## Pause versus remove

They are deliberately different.

| | Pause | Remove |
|---|---|---|
| New conversations | refused | refused |
| Existing bound conversations | hand off to an eligible alternative, or block if none is available | can no longer continue on the removed credential |
| Stored sign-in | kept | **deleted from your OS credential store** |
| Reversible | yes, click Resume | no, you must sign in again |

Remove shows you how many conversations are bound to that workspace **before** you confirm.

## Migrating from codex-lb

codex-relay does not read or import codex-lb's credentials. Refresh tokens are single use and
rotate; copying one would break both tools' sign-ins. Each workspace must be connected again
through the browser.

1. Stop codex-lb so it is not competing for the same conversations.
2. Run `codexrelay setup`. It detects the existing `model_provider` line, comments it out,
   and records the change. `codexrelay doctor` reports the provider it found.
3. Run `codexrelay serve` and connect your workspaces independently.
4. Existing codex-lb conversations are **not** migrated. No claim is made that upstream
   ownership of an in-flight conversation has moved. Start new conversations under
   codex-relay.
5. `codexrelay rollback` restores your previous provider line if you want to go back.

The binary is named `codexrelay` rather than `pool` because `pool` is commonly already taken
by a codex-lb installation.

## Rollback

```
codexrelay rollback
```

Two cases, and codex-relay tells you which one happened:

- **The file is exactly as we left it.** Your original is restored byte for byte from the
  backup.
- **The file changed after we wrote it.** Someone else edited it. Restoring the backup would
  destroy that edit, so instead only codex-relay's two managed regions are removed and any
  line codex-relay commented out is restored. Your later edits are kept, and the message says
  a conflict was detected.

If there is no recorded change, rollback says so and does nothing.

## Uninstall

1. `codexrelay rollback` to restore your Codex configuration.
2. Remove each workspace in the dashboard, which deletes its stored sign-in from your OS
   credential store. Removing the binary alone does **not** remove stored credentials.
3. Delete the data directory:
   - macOS: `~/Library/Application Support/codex-relay`
   - Linux: `${XDG_DATA_HOME:-~/.local/share}/codex-relay`
   - Windows: `%LOCALAPPDATA%\codex-relay`
4. Delete the executable.

Uninstalling does not interrupt a turn that is already streaming: stop the service first if
you want a clean stop.

## When credential storage is unavailable

codex-relay refuses to connect an account it cannot store securely. It never falls back to a
plaintext file.

- **Linux** is the common case: a Secret Service provider (gnome-keyring, KWallet, or
  KeePassXC with Secret Service enabled) must be installed **and unlocked**. A headless or
  freshly booted session often has neither. This is a supported setup step, not a bug, and
  `codexrelay doctor` names it.
- **macOS**: unlock your login keychain and allow codex-relay to store items.
- **Windows**: sign in to your Windows user profile so Credential Manager is available.

Only macOS Keychain has been verified live. See
[compatibility.md](compatibility.md#what-is-not-verified).

## Diagnostics

`codexrelay doctor` prints a redacted report: versions, schema version, credential store
health from a real round trip, and what it found in your Codex configuration. It contains no
account names, emails, tokens or conversation identifiers. The dashboard's Settings screen
produces the same report and can download it as JSON.

## If something is wrong

| Symptom | Where to look |
|---|---|
| Codex still uses your old provider | `codexrelay doctor` -> `codex.managed_by_codexrelay`. If false, setup did not take effect. |
| "codex-relay does not recognise this request" | A Codex version newer than this build is calling an endpoint we have not classified. It fails closed on purpose: no pooled credential was attached. Report the path from Diagnostics. |
| A conversation is blocked | Overview -> Needs attention. The explanation names the rule and shows the exact comparison. Restore the owner or make another workspace eligible; codex-relay hands off between turns when an eligible alternative exists. |
| Quota never appears | It appears after that workspace's first routed turn. If a workspace never wins a routing decision, it will have no readings. |
| Dashboard says the service is unreachable | The dashboard is only a subscriber. Routing continues without it. Check that `codexrelay serve` is still running. |
