# Configuration

How chcli is configured: named connection profiles, the precedence of flags,
environment and profile, and every environment variable.

## Profiles

Profiles are named connections in `~/.config/chcli/config.yaml`
(`$XDG_CONFIG_HOME/chcli/config.yaml` if set; `%AppData%\chcli\config.yaml` on
Windows). `--config` or `CHCLI_CONFIG` selects another file, and
`chcli config path` prints the one in use.

```yaml
connections:
  production:
    host: clickhouse.example.com
    port: 9440
    database: default
    secure: true
    auth:
      type: google
      client_id: 1234567890-abc.apps.googleusercontent.com
      # client_secret: prefer CHCLI_OAUTH_CLIENT_SECRET over storing it here
      username_claim: email

  staging:
    host: clickhouse-staging.example.com
    port: 9440
    secure: true
    auth:
      type: oidc
      issuer: https://auth.example.com
      client_id: clickhouse-cli
      audience: clickhouse
      scopes: [openid, email, offline_access]
      redirect_uri: http://127.0.0.1:8765/callback

  local:
    host: localhost
    auth:
      type: password
      username: default

history:
  enabled: true        # default
  max_entries: 10000   # default

output:
  format: table        # default format of the interactive shell
  pager: less -FRSX    # optional: page table/vertical output
```

A profile is always selected explicitly:

```sh
chcli --profile production
```

There is no positional shorthand; `chcli production` is an error. Profiles are
optional, and individual values can be overridden per invocation without
touching the file:

```sh
chcli --profile production --database chronicle
chcli --profile production --host another-clickhouse.example.com
chcli --profile production --auth password --user admin --ask-password
```

Inspect what a combination resolves to (secrets are redacted):

```console
$ chcli config show --profile production --database chronicle
profile: production
host: clickhouse.example.com
port: 9440
database: chronicle
protocol: native
secure: true
auth:
  type: google
  client_id: 1234567890-abc.apps.googleusercontent.com
  client_secret: '***'
  issuer: https://accounts.google.com
  ...
$ chcli config list
PROFILE     HOST                            AUTH
local       localhost                       Password
production  clickhouse.example.com          Google OAuth
staging     clickhouse-staging.example.com  OIDC
```

The complete set of profile keys: `host`, `port`, `database`, `protocol`,
`secure`, `insecure_skip_verify`, `ca_cert`, and under `auth`: `type`,
`username`, `password`, `token`, `token_command`, `client_id`, `client_secret`, `issuer`,
`authorization_endpoint`, `token_endpoint`, `device_endpoint`, `audience`,
`scopes`, `username_claim`, `redirect_uri`, `flow`, `token_type`. Unknown keys
are rejected, so typos do not go unnoticed.

## Secrets from commands

Every setting that holds a secret has a companion that names a command to
obtain it from, in the spirit of the exec credential plugins in a kubeconfig.
One rule covers the file, the command line and the environment:

| Secret | Configuration file | Flag | Environment variable |
|---|---|---|---|
| password | `password` / `password_command` | `--password` / `--password-command` | `CHCLI_PASSWORD` / `CHCLI_PASSWORD_COMMAND` |
| bearer token | `token` / `token_command` | `--jwt-token` / `--jwt-token-command` | `CHCLI_JWT_TOKEN` / `CHCLI_JWT_TOKEN_COMMAND` |
| OAuth client secret | `client_secret` / `client_secret_command` | `--oauth-client-secret` / `--oauth-client-secret-command` | `CHCLI_OAUTH_CLIENT_SECRET` / `CHCLI_OAUTH_CLIENT_SECRET_COMMAND` |

```yaml
connections:
  production:
    host: clickhouse.example.com
    port: 9440
    secure: true
    auth:
      type: google
      client_id: 1234567890-abc.apps.googleusercontent.com
      client_secret_command: security find-generic-password -s chcli-google -w   # macOS Keychain
  reporting:
    host: clickhouse.example.com
    port: 9440
    secure: true
    auth:
      type: password
      username: reporting
      password_command: [op, read, "op://Vault/ClickHouse reporting/password"]  # 1Password CLI
```

- A **string** is run through the shell (`sh -c` on macOS and Linux,
  `cmd /C` on Windows), so pipes and `~` work. A **list** is run directly,
  as program and arguments, without a shell. Flags and environment variables
  take the string form.
- The secret is the last non-empty line the command writes to standard
  output. A JSON object is accepted too: its `token`, `access_token`,
  `id_token`, `status.token` (the kubeconfig `ExecCredential` shape),
  `password`, `secret`, `client_secret` or `value` field is used.
- Standard error is shown to you. In the interactive shell the command may
  use the terminal, so a tool that needs you to unlock a vault or log in
  can do so; background work (loading completion metadata) never lets it.
- The command has two minutes to finish. Its output is never logged and never
  appears in an error message.
- A password or client secret is fetched once per `chcli` process. A bearer
  token with an `exp` claim is fetched again one minute before it expires,
  so a long session keeps working; a token without `exp` is kept for the
  process.
- A secret and its command are mutually exclusive: setting both is a
  configuration error. `--ask-password` replaces a configured password command.
- `chcli config show` prints the command (it is not a secret) and still
  redacts secret values.

The command runs with your privileges, from the configuration file in your
home directory, so keep that file writable by you alone.

## Configuration precedence

```
command-line flags
      ↓ override
environment variables
      ↓ override
the selected profile
      ↓ override
built-in defaults
```

## Environment variables

Every connection flag has an environment variable: upper-case the flag name,
replace dashes with underscores and prefix `CHCLI_`.

| Variable | Flag |
|---|---|
| `CHCLI_PASSWORD` | `--password` |
| `CHCLI_JWT_TOKEN` | `--jwt-token` |
| `CHCLI_OAUTH_CLIENT_SECRET` | `--oauth-client-secret` |
| `CHCLI_HOST`, `CHCLI_PORT`, `CHCLI_DATABASE`, `CHCLI_PROTOCOL` | `--host`, `--port`, `--database`, `--protocol` |
| `CHCLI_SECURE`, `CHCLI_INSECURE_SKIP_VERIFY`, `CHCLI_CA_CERT` | `--secure`, `--insecure-skip-verify`, `--ca-cert` |
| `CHCLI_AUTH`, `CHCLI_USER` | `--auth`, `--user` |
| `CHCLI_OAUTH_CLIENT_ID`, `CHCLI_OAUTH_ISSUER`, `CHCLI_OAUTH_AUDIENCE`, ... | the matching `--oauth-*` flag |
| `CHCLI_OAUTH_SCOPE` | `--oauth-scope` (comma or space separated) |
| `CHCLI_CONFIG` | `--config` |

Secrets are best supplied this way rather than written into the configuration
file. The profile itself is deliberately not selectable through the
environment: `--profile` is always explicit.
