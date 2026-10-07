# Security

chcli handles passwords, OAuth tokens and client secrets. What it does with
them is described in [docs/security.md](docs/security.md): TLS verification by
default, PKCE and state checks in the OAuth flow, token storage in the OS
credential manager, redaction in logs and error messages.

## Reporting a vulnerability

Please do not open a public issue for a security problem. Report it privately
through [GitHub's private vulnerability reporting](https://github.com/nenych/chcli/security/advisories/new)
for this repository. Include the chcli version (`chcli version`), your
operating system, and steps to reproduce; `--debug` output is safe to attach
because credentials are redacted.

You will get an acknowledgement within a few days. Fixes are released as a new
version with a note in the changelog; please keep the details private until
then.

## Supported versions

Only the latest release receives fixes.
