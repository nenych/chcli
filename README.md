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
  then the profile, then defaults.
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

One example per authentication type; the details, including every flag, are
in [docs/authentication.md](docs/authentication.md).

```sh
# Password
chcli --host clickhouse.example.com --secure --user chronicle --ask-password

# A token you already have (CI jobs, service accounts) ...
CHCLI_JWT_TOKEN="$(my-token-command)" chcli --host clickhouse.example.com --auth jwt

# ... or a command that prints one, re-run when the token expires
chcli --host clickhouse.example.com --jwt-token-command "gcloud auth print-identity-token --audiences=..."

# Google login (opens the browser once; the session is cached and refreshed)
chcli --host clickhouse.example.com --google-oauth \
  --oauth-client-id 1234567890-abc.apps.googleusercontent.com

# Any OpenID Connect provider
chcli --host clickhouse.example.com --auth oidc \
  --oauth-issuer https://auth.example.com/realms/analytics --oauth-client-id clickhouse-cli
```

Put the same settings in a profile and select it with `--profile`:

```yaml
# ~/.config/chcli/config.yaml
connections:
  production:
    host: clickhouse.example.com
    port: 9440
    secure: true
    auth:
      type: google
      client_id: 1234567890-abc.apps.googleusercontent.com
```

```sh
export CHCLI_OAUTH_CLIENT_SECRET='GOCSPX-...'   # secrets stay out of the file
chcli auth login --profile production
chcli --profile production
```

Secrets are never written to the configuration file by chcli, and never
printed: `config show`, `--debug` and error messages redact them.

## Documentation

| Document | Contents |
|---|---|
| [docs/authentication.md](docs/authentication.md) | password, JWT, Google, generic OIDC; login sessions; the Google Cloud setup guide; Altinity Antalya examples |
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
