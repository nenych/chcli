# The interactive shell

The prompt shows the profile (or host) and the current database:
`production/chronicle :)`.

| Key | Action |
|---|---|
| `Enter` | run the statement if it ends with `;` (or `\G`), otherwise start a new line |
| `Tab`, `Shift+Tab` | cycle through completions |
| `Up`, `Down` | previous / next history entry (or move within a multiline statement) |
| `Ctrl+C` | clear the current input; while a query runs, cancel it |
| `Ctrl+D` | exit (on an empty line) |
| `Ctrl+A`, `Ctrl+E`, `Ctrl+W`, ... | the usual Emacs-style line editing |

**Cancelling.** `Ctrl+C` during a query asks the server to stop it and returns
you to the prompt with the rows received so far. If the server does not react,
a second `Ctrl+C` exits the program.

**Output.** Results are shown as a table by default.

- End a statement with `\G` instead of `;` for one-off vertical output.
- Append `FORMAT <name>` to pick a format for one statement.
- `\format <name>` switches the format for the session.

**History** persists per connection under `~/.local/state/chcli/history/`
(`%LocalAppData%\chcli` on Windows). Statements that appear to contain
credentials are not written to it: `IDENTIFIED BY ...`, anything mentioning a
password, secret, token or access key, and calls such as
`s3(url, key, secret)` or `mysql(host, db, table, user, password)` that take
credentials as plain arguments. This is a heuristic. Input that starts with a
space is never recorded, which is the reliable way to keep a statement out.

**Session state.** `USE db` and `SET name = value` are tracked by the client
and stay in effect when the connection is re-established. Other session state
is more fragile; see [Limitations](troubleshooting.md#limitations).

**Lost connections** are re-established automatically on the next statement,
for example after a server restart. Expired OAuth tokens are refreshed
transparently, and the connection is rebuilt with the new token.

## Autocomplete

Completions appear as you type and depend on where the cursor is:

| You type | You get |
|---|---|
| `SELECT * FROM chr` | databases and tables: `chronicle`, `chronicle_draft` |
| `SELECT * FROM chronicle.` | tables, views and dictionaries of `chronicle` |
| `SELECT * FROM chronicle.events WHERE ev` | columns of `chronicle.events` first, then functions and keywords |
| `SELECT * FROM chronicle.events e WHERE e.` | columns of the aliased table |
| `SELECT ` (cursor) ` FROM events` | columns of `events`: the whole statement is considered |
| `INSERT INTO events (` | columns of `events` |
| `USE `, `DROP DATABASE ` | databases |
| `ENGINE = `, `x::`, `CAST(x AS `, `FORMAT `, `SET `, `SETTINGS ` | engines, data types, formats, settings |
| `\d `, `\use ` | tables, databases |

Metadata comes from `system.databases`, `system.tables`, `system.columns`,
`system.functions`, `system.table_functions`, `system.data_type_families`,
`system.table_engines`, `system.formats`, `system.settings` and, where
available, `system.keywords`. It is loaded once in the background over a
separate connection, so typing never waits for the server. The cache refreshes
after DDL statements, when it is older than ten minutes, and on `\refresh`.
A system table you may not read only disables the completions it would have
provided; `\status` shows what could not be loaded.

Keywords are completed in the case you are typing in.

## Meta commands

| Command | Action |
|---|---|
| `\l`, `\databases` | list databases |
| `\dt [database]`, `\tables [database]` | list tables of the current or given database |
| `\d table`, `\describe table` | describe a table |
| `\use database` | switch the current database |
| `\status` | connection, authentication and server information (no secrets) |
| `\refresh` | reload the autocomplete metadata |
| `\format [name]` | show or set the output format |
| `\history [n]` | show the last *n* history entries |
| `\help`, `\?` | help |
| `\q`, `\quit`, `exit`, `quit` | leave |

```console
production/chronicle :) \status
Profile:             production
Host:                clickhouse.example.com:9440
Database:            chronicle
Protocol:            native
Authentication:      Google OAuth
User:                user@example.com
Token expires:       in 43m
TLS:                 enabled
Server version:      26.3.13.20001.altinityantalya
Completion metadata: loaded 2m ago
```
