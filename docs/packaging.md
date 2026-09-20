# Packaging and distribution

`make dist VERSION=x.y.z` builds every supported target and writes checksummed archives to
`dist/`. It publishes nothing.

## What `make dist` produces

| Target | Archive |
|---|---|
| macOS arm64, amd64 | `codexrelay-<version>-darwin-<arch>.tar.gz` |
| Linux arm64, amd64 | `codexrelay-<version>-linux-<arch>.tar.gz` |
| Windows arm64, amd64 | `codexrelay-<version>-windows-<arch>.zip` |

Each archive contains the `codexrelay` executable, a tiny `relaypool` command wrapper,
`README.md` and `SETUP.md`. Windows receives `relaypool.cmd`; macOS and Linux receive the
POSIX wrapper. `dist/SHA256SUMS` carries the checksums.

The build is `CGO_ENABLED=0` and `-trimpath`, so there is no C toolchain dependency and no
build-machine paths in the binary. The dashboard is compiled by Vite and embedded, so an end
user needs neither Node nor another application runtime. The wrapper only launches the
adjacent `codexrelay` executable with its `profile` subcommand.

Verified on 2026-09-11: the `darwin-arm64` archive was extracted and run. It reported its
version, probed macOS Keychain successfully, served the embedded dashboard on a loopback
port, and the page carried its session-token block. The other five archives were built but
only the native one was executed; see the honesty note below.

## What this is NOT

This is a build, not an installer. None of the following exist, and none should be described
as "nearly done":

| Missing | Why it is missing |
|---|---|
| macOS signing and notarisation | Needs an Apple Developer ID certificate and an Apple ID with notarisation access. Neither has been available to this build. Without it, macOS Gatekeeper will refuse the binary on first run unless the user clears the quarantine attribute themselves. |
| A `.pkg`, `.dmg`, `.deb`, `.rpm` or `.msi` | Each needs its own tooling and, for signed formats, its own signing identity. |
| Background service registration | No launchd plist, no systemd unit, no Windows service. `codexrelay serve` runs in the foreground. |
| Homebrew, winget, apt or similar | All require publishing, which is explicitly out of scope. |
| Auto-update | Out of scope for the first release. |

**We do not tell users to disable OS protections as the normal installation path.** Until the
macOS build is signed and notarised, the honest instruction is that this is an unsigned
developer build, with the consequence stated plainly, rather than a `xattr -d` incantation
presented as a normal step.

## Reproducibility, stated precisely

The build sets `-trimpath` and `SOURCE_DATE_EPOCH=0`, and the archives are created with
`--numeric-owner --owner=0 --group=0`, so the same source and the same toolchain version
produce the same bytes. This has **not** been verified by building twice on two different
machines and comparing checksums; it is a property the build is set up for, not a measured
result.

## Cross-compilation is not platform verification

Six targets compile. That is evidence about the compiler, not about the platform.

- **macOS**: run, verified live, including Keychain.
- **Linux**: archives built but not installed or executed. A GitHub-hosted Ubuntu job verifies
  a real Secret Service write, read and delete through gnome-keyring in a D-Bus session.
- **Windows**: archives built but not installed or executed. A GitHub-hosted Windows job
  verifies a real Credential Manager write, read and delete. WSL behavior remains unverified.
