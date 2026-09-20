# Releasing codex-relay

## Automated release

Push a semantic-version tag such as `v0.1.0`. The Release workflow runs the Go tests, builds
all six OS and architecture archives through `make dist`, writes SHA-256 checksums, and
attaches the artifacts to a GitHub release.

The tag must point at a commit whose CI checks are green. Create and push it only after
reviewing the exact commit:

```sh
git tag -s v0.1.0 <commit>
git push origin v0.1.0
```

## macOS signing and notarization

The current archives are unsigned developer builds. Source availability is not blocked by
code signing, but a polished macOS binary distribution is. Do not describe an archive as
signed or notarized until all of these have been completed for the exact release artifact:

1. Sign both macOS binaries with a Developer ID Application identity.
2. Submit the final archive to Apple's notarization service.
3. Staple the notarization result where the package format supports it.
4. Verify Gatekeeper on a separate clean macOS account or machine.
5. Publish checksums for the final signed artifacts, not the pre-signing files.

This requires maintainer-controlled Apple credentials and therefore cannot be emulated by CI
without those secrets. The release notes must continue to label macOS artifacts unsigned
until this procedure has fresh evidence.
