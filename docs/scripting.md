# Scripting

Statements come from `--query`, `--file` or standard input. Several
statements are executed in order, stopping at the first failure.

```sh
chcli --profile production -q "SELECT version()"
chcli --profile production --file query.sql
chcli --profile production < query.sql
echo "SELECT count(*) FROM events" | chcli --profile production
```

**Output formats** (`--format`, or `FORMAT <name>` at the end of a statement):

| Format | Description | ClickHouse names also accepted |
|---|---|---|
| `table` | boxed table | `Pretty`, `PrettyCompact`, ... |
| `vertical` | one `column: value` line per column | `Vertical` |
| `tsv`, `tsvwithnames` | tab-separated, ClickHouse escaping, `\N` for NULL | `TabSeparated`, `TSVWithNames` |
| `csv`, `csvwithnames` | RFC 4180 CSV, `\N` for NULL | `CSV`, `CSVWithNames` |
| `json` | one document: `{"meta": [...], "data": [...], "rows": N}` | `JSON` |
| `jsonlines` | one JSON object per row | `JSONEachRow`, `ndjson` |
| `null` | discard the result | `Null` |

The default is `table` when standard output is a terminal and `tsv` when it is
piped or redirected, so `chcli -q ... | cut -f2` works without flags. In
scripted runs nothing but the result is written to standard output: no banner
and no timing line.

```sh
chcli --profile production --format json \
  -q "SELECT name, engine FROM system.tables LIMIT 10" | jq '.data[].name'
```

**Exit codes**

| Code | Meaning |
|---|---|
| 0 | success |
| 1 | a statement failed, or another runtime error |
| 2 | invalid flags or configuration |
| 3 | authentication failed, or an OAuth login is required |
| 4 | the server could not be reached |
| 130 | interrupted |

**OAuth in scripts and CI.** A scripted run never opens a browser, whether or
not a terminal is attached. If no valid or refreshable session is cached, it
fails with exit code 3:

```console
$ chcli --profile production -q "SELECT 1"
chcli: OAuth credentials for profile "production" are not available.
Run:
  chcli auth login --profile production
from an interactive terminal first.
```

After `chcli auth login`, scripted runs reuse and refresh the session
silently. For fully unattended jobs, obtain a token through your provider's
machine-to-machine flow and pass it with `--auth jwt` and `CHCLI_JWT_TOKEN`.
