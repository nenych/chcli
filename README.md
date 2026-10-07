# chcli

A modern interactive ClickHouse client with smart autocomplete, connection
profiles and first-class OAuth/OIDC support.

`chcli` is to ClickHouse what `pgcli` and `mycli` are to PostgreSQL and MySQL:
a terminal client for daily use. It is a single static binary for macOS, Linux
and Windows, built on the official
[clickhouse-go](https://github.com/ClickHouse/clickhouse-go) driver.

```console
$ chcli --profile production
Authenticated as user@example.com
Connected to clickhouse.example.com (ClickHouse 26.3.13.20001.altinityantalya)

production/default :) SELECT count(*)
..................... FROM chronicle.events
..................... WHERE created_at > now() - INTERVAL 1 HOUR;
┌─count()─┐
│ 1382991 │
└─────────┘
1 row in set. 0.087 sec. Processed 1.38 million rows, 11.06 MB.

production/default :) SELECT * FROM chronicle.events e WHERE e.ev
                                                             ┌──────────────────────────────┐
                                                             │ event_id    column  UInt64   │
                                                             │ event_type  column  String   │
                                                             └──────────────────────────────┘
```

## Features

- **Interactive shell**: multiline editing, syntax highlighting, persistent
  per-connection history, query timing, `Ctrl+C` to cancel a running query.
- **Context-aware autocomplete** from live server metadata: databases, tables,
  views, dictionaries, columns, table aliases, functions, table functions,
  data types, engines, formats, settings and keywords.
- **Connection profiles** with a strict precedence: flags, then environment,
  then the profile, then defaults. Secrets come from commands (keychain,
  1Password, Vault, `gcloud`, ...) rather than from the file.
- **Authentication**: password, static JWT / bearer token, generic OIDC, and
  Google as a preset on top of the generic OIDC implementation. Browser login
  with PKCE or device flow, token caching in the OS keychain, automatic
  refresh. Works with [Altinity Antalya](docs/authentication.md#altinity-antalya)
  token authentication out of the box.
- **Scripting**: `--query`, `--file` or stdin; table, vertical, TSV, CSV, JSON
  and JSON Lines output; meaningful exit codes; never opens a browser.
- **Native and HTTP transports**, TLS with certificate verification on by
  default, streaming results.
- `chcli doctor` to diagnose a connection layer by layer.

## Installation

**Homebrew** (macOS and Linux):

```sh
brew install nenych/tap/chcli
```

**Script** (macOS and Linux) — downloads the latest release, verifies its
checksum and installs to `~/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/nenych/chcli/main/install.sh | sh
```

**Windows**: download `chcli_windows_amd64.zip` (or `arm64`) from the
[releases page](https://github.com/nenych/chcli/releases), unzip it and put
`chcli.exe` in a folder on your `PATH`. Use Windows Terminal or PowerShell for
the interactive shell; details in [docs/windows.md](docs/windows.md).

**Any platform with Go**:

```sh
go install github.com/nenych/chcli/cmd/chcli@latest
```

Every release archive contains the binary, `LICENSE`, `THIRD_PARTY_NOTICES.md`
and shell completion scripts; `checksums.txt` on the release page has the
SHA-256 of each archive. The macOS binaries are not signed by Apple, so one
downloaded with a browser is blocked by Gatekeeper; Homebrew and the script do
not have that problem.

**Shell completion** (Homebrew installs it for you):

```sh
chcli completion zsh  > "${fpath[1]}/_chcli"                       # zsh
chcli completion bash > /usr/local/etc/bash_completion.d/chcli    # bash
chcli completion fish > ~/.config/fish/completions/chcli.fish     # fish
chcli completion powershell | Out-String | Invoke-Expression      # PowerShell
```

Completion knows the flags and their values, including your profile names
after `chcli --profile <TAB>`.

## Quick start

```console
$ chcli --host localhost --user default --ask-password
Password for default:
Connected to localhost (ClickHouse 25.8.1.1)

localhost/default :) SELECT version();
```

Statements end with a semicolon; `Enter` without one continues on the next
line. Type `\help` for the meta commands and `Ctrl+D` to leave.

Run one statement and exit:

```sh
chcli --host localhost -q "SELECT count() FROM system.tables"
```

## Connecting

One example per authentication type; every flag is described in
[docs/authentication.md](docs/authentication.md).

```sh
# Password
chcli --host clickhouse.example.com --secure --user chronicle --ask-password

# A token you already have (CI jobs, service accounts)
CHCLI_JWT_TOKEN="$(my-token-command)" chcli --host clickhouse.example.com --auth jwt

# Google login: opens the browser once, then reuses and refreshes the session
chcli --host clickhouse.example.com --google-oauth \
  --oauth-client-id 1234567890-xxxx.apps.googleusercontent.com

# Any OpenID Connect provider
chcli --host clickhouse.example.com --auth oidc \
  --oauth-issuer https://auth.example.com/realms/analytics --oauth-client-id clickhouse-cli
```

Profiles keep this in `~/.config/chcli/config.yaml` and are selected with
`--profile`. Secrets do not go into the file: a `*_command` names a command
that prints them, like an exec credential plugin in a kubeconfig.

```yaml
connections:
  production:                     # you, through Google login
    host: clickhouse.example.com
    port: 9440
    secure: true
    auth:
      type: google
      client_id: 1234567890-xxxx.apps.googleusercontent.com
      client_secret_command: security find-generic-password -s chcli-google -w   # macOS Keychain

  reporting:                      # a Google service account, no browser
    host: clickhouse.example.com
    port: 9440
    secure: true
    auth:
      type: jwt
      token_command: gcloud auth print-identity-token --impersonate-service-account=reporting@my-project.iam.gserviceaccount.com --audiences=1234567890-xxxx.apps.googleusercontent.com --include-email

  warehouse:                      # a classic password, from 1Password
    host: warehouse.example.com
    port: 9440
    secure: true
    auth:
      type: password
      username: analyst
      password_command: [op, read, "op://Engineering/ClickHouse warehouse/password"]
```

```console
$ chcli auth login --profile production
Opening browser for Google authentication...
Authenticated as user@example.com

$ chcli --profile production
Authenticated as user@example.com
Connected to clickhouse.example.com (ClickHouse 26.6.4.20001.altinityantalya)

production/default :)

$ chcli --profile reporting -q "SELECT currentUser(), currentRoles()"
reporting@my-project.iam.gserviceaccount.com	['analyst']
```

A token command is run again whenever the token it printed is about to
expire, so long sessions keep working. Secrets are never printed: `config show`,
`--debug` and error messages redact them. Recipes for Vault, Bitwarden, `pass`,
cloud secret managers, Entra ID and more are in [docs/secrets.md](docs/secrets.md).

## Documentation

| Document | Contents |
|---|---|
| [docs/authentication.md](docs/authentication.md) | password, JWT, Google, generic OIDC; login sessions; the Google Cloud setup guide; Altinity Antalya examples |
| [docs/secrets.md](docs/secrets.md) | getting passwords, tokens and client secrets from commands; recipes per secret store |
| [docs/configuration.md](docs/configuration.md) | profiles, configuration precedence, environment variables |
| [docs/shell.md](docs/shell.md) | keys, autocomplete, meta commands, output formats, history |
| [docs/scripting.md](docs/scripting.md) | `--query`, `--file`, stdin, output formats, exit codes, OAuth in CI |
| [docs/security.md](docs/security.md) | what chcli does with credentials, tokens and TLS |
| [docs/troubleshooting.md](docs/troubleshooting.md) | `chcli doctor`, symptoms and fixes, known limitations |
| [docs/windows.md](docs/windows.md) | installing and using chcli on Windows |
| [CONTRIBUTING.md](CONTRIBUTING.md) | building, testing and releasing |
| [CHANGELOG.md](CHANGELOG.md) | what changed in each release |

## Licence and trademarks

chcli is released under the [MIT License](LICENSE). The binaries also contain
open source software from other authors, listed with their licences in
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

ClickHouse® is a registered trademark of ClickHouse, Inc. Altinity and
Antalya are trademarks of Altinity, Inc. Google is a trademark of Google LLC.
chcli is an independent project and is not affiliated with, endorsed by or
supported by any of them.

## Acknowledgements

chcli stands on [clickhouse-go](https://github.com/ClickHouse/clickhouse-go),
the official ClickHouse driver; [go-prompt](https://github.com/elk-language/go-prompt)
(Mateusz Drewniak's maintained fork of Masashi Shibata's library), which gives
it the pgcli-style line editor; [go-oidc](https://github.com/coreos/go-oidc)
and [golang.org/x/oauth2](https://pkg.go.dev/golang.org/x/oauth2) for OpenID
Connect; [cobra](https://github.com/spf13/cobra) for the command line; and
[go-keyring](https://github.com/zalando/go-keyring) for the OS credential
managers. The Antalya examples follow Altinity's
[OAuth documentation](https://docs.altinity.com/altinityantalya/integrating-oauth/).
