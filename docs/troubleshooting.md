# Troubleshooting

Start with the built-in diagnosis. It checks one layer at a time, prints no
credentials and opens no browser:

```console
$ chcli doctor --profile production
✓ Configuration      clickhouse.example.com:9440, native protocol, Google OAuth authentication
✓ DNS                clickhouse.example.com resolves to 203.0.113.10
✓ TCP                connected to clickhouse.example.com:9440 in 12.3ms
✓ TLS                TLS 1.3, certificate for clickhouse.example.com valid until 2027-01-15
✓ OIDC endpoints     discovered from https://accounts.google.com
✓ OAuth session      logged in as user@example.com, token valid for 43m
✓ ClickHouse         version 26.3.13.20001.altinityantalya, authenticated as user@example.com
```

`--debug` prints what the client is doing to standard error, and
`chcli config show` shows the configuration a set of flags resolves to.

| Symptom | Likely cause and fix |
|---|---|
| `connection ... failed: EOF` right after connecting | TLS mismatch: a TLS port without `--secure`, or the reverse. 9440 and 8443 are TLS, 9000 and 8123 are not. |
| `secure=true but port 9000 is ClickHouse's plaintext port` | Use the TLS port, or drop `secure`. |
| `certificate signed by unknown authority` | The server uses a private CA: pass `--ca-cert ca.pem`. |
| `Code: 516 ... Authentication failed` with a password | Wrong user or password. |
| `auth type "jwt" would send the token to ... without TLS` | A token combined with a plaintext port on a remote host. Use the TLS port (`--secure`), or `--secure=false` if you really mean it. |
| `--jwt-token does not apply to "password" authentication` | A credential flag that the profile's authentication type would ignore. Add the matching `--auth`. |
| `this statement only has an effect within a server session` | `SET ROLE` or a temporary table over HTTP; use `--protocol native`. |
| `Code: 516. Token is invalid` | The server rejected the token: wrong issuer or audience, an expired token, or the wrong kind of token. Compare the server's token processor with `token_type` (see [Altinity Antalya](authentication.md#altinity-antalya)). |
| `OAuth credentials for profile ... are not available` | A scripted run without a cached session: run `chcli auth login --profile ...` once. |
| The browser shows `redirect_uri_mismatch` | Register the redirect URI with the provider and set `redirect_uri` to the same value. |
| `cannot listen for the OAuth callback on 127.0.0.1:8765` | Another program uses the port of your `redirect_uri`. |
| You have to log in again every hour | The provider issues no refresh token: add the `offline_access` scope (generic OIDC). |
| `OIDC discovery for ... failed` | The issuer must match the provider's `issuer` value exactly, including a trailing slash. Alternatively set the endpoints explicitly. |
| `INSERT with data supplied by the client ... is not supported` | See [Limitations](#limitations). |
| Completions are missing | `\status` shows whether metadata loaded; `\refresh` reloads it and reports what failed. |

## Limitations

- **`INSERT ... FORMAT` with data from the client** (`INSERT INTO t FORMAT CSV`
  followed by rows, or `INSERT INTO t VALUES` with the rows piped in) is not
  supported and is rejected. `INSERT ... VALUES (...)` and `INSERT ... SELECT`
  work. In a script, everything after an `INSERT ... FORMAT` is treated as its
  data and is never executed as statements.
- **Output formats** are rendered by the client; ClickHouse formats beyond the
  [list in Scripting](scripting.md) (Parquet, Arrow, ...) are not available.
- **Session state** other than `USE` and `SET name = value` (for example
  `SET ROLE` and temporary tables) lives on the server connection and is lost
  whenever that connection is re-established: after a network failure, when an
  OAuth token is refreshed, and on `USE`, which reconnects so that the current
  database survives later reconnects.
- **The HTTP protocol has no server session.** `USE` and `SET name = value`
  work because the client applies them to every request. Statements that only
  make sense within a session (`SET ROLE`, `CREATE TEMPORARY TABLE`) are
  rejected with an explanation instead of silently doing nothing; use the
  native protocol for those.
- **`WITH TOTALS`**: the totals row is shown over the native protocol only.
- **Result sets of statements that are not queries** are not shown. Statements
  other than `SELECT`, `WITH`, `SHOW`, `DESCRIBE`, `EXISTS`, `EXPLAIN`,
  `CHECK`, `KILL` and `WATCH` report `Ok.`; this includes the per-host status
  table of `ON CLUSTER` DDL.
- **Autocomplete** does not offer identifiers that contain spaces or
  punctuation (they can still be typed, quoted with backticks). Completion is
  heuristic rather than grammar-based, so unusual statements may get generic
  suggestions.
- **Large results** in `table` format are rendered in consecutive tables of
  10,000 rows, so that memory use stays flat. The machine-readable formats
  stream row by row.
