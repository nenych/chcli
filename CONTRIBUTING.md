# Contributing to chcli

Bug reports, fixes and new features are welcome. For anything larger than a
small fix, please open an issue first so we can agree on the approach.

## Development

Requires Go 1.26 or newer (an older Go from 1.21 on downloads the right
toolchain by itself) and no cgo on any platform. Docker is needed for the
integration tests only.

```sh
make build              # bin/chcli
make test               # unit tests with the race detector
make lint               # golangci-lint (v2)
make clickhouse-up antalya-up test-integration   # integration tests against ClickHouse and Altinity Antalya
make clickhouse-down antalya-down
make notices            # regenerate THIRD_PARTY_NOTICES.md after changing dependencies
make release-build      # the release archives for every platform, into dist/
```

Every platform can be checked from any other: `GOOS=windows go vet ./...` and
`GOOS=linux go vet ./...` compile the other platforms' files, and
`scripts/build-release.sh v0.0.0-dev /tmp/dist` cross-compiles all six
binaries. To run the tests on Linux from macOS or Windows:

```sh
docker run --rm -v "$PWD":/src:ro -w /src -e GOFLAGS=-buildvcs=false golang:1.26 go test ./...
```

The unit tests need no server: OAuth is tested against an in-process fake
identity provider, and the keyring against an in-memory mock. The integration
tests (`-tags integration`) run against a stock ClickHouse server over both
transports and against Altinity Antalya for token authentication; the
`make` targets above start both in Docker. No test needs real Google
credentials.

## Layout

| Package | Responsibility |
|---|---|
| `cmd/chcli` | entry point |
| `internal/cli` | flags, subcommands, exit codes, wiring |
| `internal/config` | configuration file, profiles, precedence, validation, redaction |
| `internal/auth` | `Provider` abstraction: password, JWT, OIDC (+ Google preset), token store |
| `internal/chclient` | ClickHouse transport on clickhouse-go: connect, query, cancel, reconnect |
| `internal/session` | executes one statement and renders it; shared by the shell and scripts |
| `internal/output` | table, vertical, TSV, CSV, JSON writers |
| `internal/metadata` | cache of server objects for completion |
| `internal/completion` | the completion engine (pure, no I/O) |
| `internal/sqlutil` | tolerant SQL tokenizer, statement splitting |
| `internal/history` | persistent history |
| `internal/repl` | the interactive shell and meta commands |
| `scripts/` | release archives, third-party notices, Homebrew tap update |
| `testdata/antalya/` | server configuration for the token authentication tests |

Authentication, transport and presentation are deliberately decoupled: the
transport receives `Credentials` from a `Provider` and does not know how a
token was obtained, and the shell does not know which transport is in use.
Adding an identity provider means adding a preset or a `Provider`
implementation in `internal/auth`.

## Guidelines

- Keep pull requests small and focused on one change.
- Run `gofmt` and `make lint`, and match the style of the surrounding code.
- Please discuss before adding a dependency. If one is added, run
  `make notices` and commit the updated `THIRD_PARTY_NOTICES.md`; CI rejects
  copyleft licences and a stale notices file.
- Credentials must never reach output, logs or error messages. Carry secrets
  as `config.Secret`, and extend `TestSecretRedaction` / the auth tests when
  touching that code.
- Behaviour visible to scripts (output formats, exit codes, flag names) must
  not change without discussion.
- Changes to the completion engine go with a case in
  `internal/completion/completion_test.go`; changes to statement handling
  with a case in `internal/sqlutil/sqlutil_test.go`.

## Releasing

Push a tag like `v1.2.3`. The `Release` workflow tests, checks the notices,
builds the archives for macOS, Linux and Windows with
`scripts/build-release.sh`, publishes a GitHub release with `checksums.txt`,
and points the formula in [nenych/homebrew-tap](https://github.com/nenych/homebrew-tap)
at the tag (`scripts/update-tap.sh`, which needs the `HOMEBREW_TAP_TOKEN`
secret; without it, update the formula by hand). Running the workflow by hand
builds the archives without publishing anything. Add the release to
`CHANGELOG.md` before tagging.

## Reporting a bug

Please include your operating system and its version, the ClickHouse server
version, the transport (native or HTTP), the command you ran and its full
output. Output from `--debug` is safe to attach: credentials are redacted.
For security problems see [SECURITY.md](SECURITY.md).

## Conduct and licence

Be respectful and constructive. By contributing you agree that your
contribution is licensed under the MIT License of this project.
