# Changelog

All notable changes to chcli are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-10-07

First release.

### Added

- Interactive shell with multiline editing, syntax highlighting, persistent
  per-connection history, query timing and `Ctrl+C` cancellation of running
  queries, including over HTTP.
- Context-aware autocomplete fed by `system.databases`, `system.tables`,
  `system.columns`, `system.functions`, `system.table_functions`,
  `system.data_type_families`, `system.table_engines`, `system.formats`,
  `system.settings` and `system.keywords`, loaded in the background.
- Meta commands: `\l`, `\dt`, `\d`, `\use`, `\status`, `\refresh`, `\format`,
  `\history`, `\help`, `\q`.
- Connection profiles in `config.yaml` with flags > environment > profile >
  defaults precedence; `chcli config show` and `chcli config list` with
  secrets redacted.
- Authentication: password, static JWT / bearer token, generic OpenID Connect
  (browser flow with PKCE, state and nonce, or device flow) and Google as a
  preset on top of it; sessions cached in the OS credential manager with
  automatic refresh; `chcli auth login|logout|status`.
- Native and HTTP transports on clickhouse-go; TLS with certificate
  verification by default; TLS defaults on for token authentication to remote
  hosts.
- Scripted use with `--query`, `--file` and standard input; `table`,
  `vertical`, `tsv`, `csv`, `json` and `jsonlines` output; exit codes 0, 1, 2,
  3, 4 and 130.
- `chcli doctor`: DNS, TCP, TLS, OIDC discovery, cached session and ClickHouse
  checks without revealing credentials.
- Shell completion for bash, zsh, fish and PowerShell, including profile names.
- Verified against Altinity Antalya token authentication (native and HTTP).

[Unreleased]: https://github.com/nenych/chcli/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/nenych/chcli/releases/tag/v0.1.0
