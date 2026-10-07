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
