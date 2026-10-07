# Security

- **TLS** certificates are verified by default. `--insecure-skip-verify` is an
  explicit opt-out; prefer `--ca-cert` for a private CA.
- **Tokens in transit.** Token authentication to a remote host uses TLS
  unless you explicitly turn it off (see [JWT](authentication.md#jwt--bearer-token)). Identity
  provider endpoints must be `https://`; plain `http://` is accepted for
  loopback addresses only, for local development.
- **OAuth** uses the authorization code flow with PKCE (S256), a random
  `state` that is checked in constant time, and a `nonce`. The callback
  listener binds to the loopback interface only and rejects requests with a
  wrong state. ID tokens are verified against the provider's keys (signature,
  issuer, audience, expiry, nonce) whenever discovery is available; without
  it, issuer, audience, expiry and nonce are still checked. Public clients
  without a secret are supported.
- **Sessions are not lost to hiccups.** Only an explicit rejection of the
  refresh token by the provider ends a session. Timeouts, rate limiting and
  server errors during a refresh are reported and the session is kept.
- **Token storage.** Sessions are stored in the operating system's credential
  manager: macOS Keychain, Windows Credential Manager, or the Secret Service
  (GNOME Keyring, KWallet) on Linux. Only the token presented to ClickHouse
  and the refresh token are kept. Credential managers limit the size of an
  entry, so large token sets are spread over several entries.

  *Tradeoff:* when no credential manager is available (a headless Linux
  machine without a Secret Service), the session is written to
  `~/.local/state/chcli/tokens/` in a file readable only by you (mode 0600,
  directory 0700). That is as protected as an SSH private key without a
  passphrase: safe from other users, not from someone with access to your
  account or disk. `chcli auth status` shows where a session is stored, and
  `chcli auth logout` removes it.
- **Secrets are never printed.** Passwords, tokens and client secrets are
  redacted in `config show`, in `--debug` logs and in error messages. `--debug`
  is safe to paste into a bug report.
- **Secrets on the command line** are visible to other users of the machine
  through the process list. Use `--ask-password`, the `CHCLI_*` environment
  variables, or a `*_command` that reads the secret from a keychain or
  password manager (see [Secrets from commands](secrets.md)).
  Commands run with your privileges from your configuration file; keep that
  file writable by you alone.
- **Configuration file.** Storing secrets in it is supported but discouraged;
  `chcli` warns when a configuration file containing secrets is readable by
  other users.
- **History** is stored with mode 0600 and skips statements that look like
  they contain credentials.
- **Result rendering.** Control characters in table and vertical output are
  escaped, so data cannot inject terminal escape sequences.
