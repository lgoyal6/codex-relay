# Security policy

## Reporting a vulnerability

Please use GitHub's private vulnerability-reporting form for this repository. Do not open a
public issue for credential exposure, authentication bypass, cross-account routing, request
replay, or another issue that could put an account at risk.

Include the affected version or commit, operating system, reproduction steps, impact, and any
logs with credentials, account IDs, task IDs, and prompt content removed. You should receive
an acknowledgement within three business days and a status update within seven business days.

If private vulnerability reporting is not available, open a public issue containing only a
request for a private contact channel. Do not include vulnerability details in that issue.

## Supported versions

Until the first tagged stable release, only the latest commit on `main` receives security
fixes. After stable releases begin, this file will list the supported release lines.

## Security boundaries

- The dashboard and proxy are loopback-only and are not designed for remote exposure.
- OAuth credentials belong only in the operating system credential store.
- Unknown routes fail closed without a pooled credential.
- A streamed turn is never retried after output starts.
- MCP file uploads remain client-owned because their requests contain no task identity. See
  `docs/compatibility.md` before changing this behavior.
