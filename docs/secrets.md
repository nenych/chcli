# Secrets from commands

chcli never needs a secret written into its configuration file. Every setting
that holds one has a companion that names a command to obtain it from, in the
spirit of the exec credential plugins in a kubeconfig (`aws eks get-token`,
`gke-gcloud-auth-plugin`). One rule covers the file, the command line and the
environment:

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
      client_id: 1234567890-xxxx.apps.googleusercontent.com
      client_secret_command: security find-generic-password -s chcli-google -w   # macOS Keychain

  reporting:
    host: clickhouse.example.com
    port: 9440
    secure: true
    auth:
      type: jwt
      token_command: gcloud auth print-identity-token --impersonate-service-account=reporting@my-project.iam.gserviceaccount.com --audiences=1234567890-xxxx.apps.googleusercontent.com --include-email

  warehouse:
    host: warehouse.example.com
    port: 9440
    secure: true
    auth:
      type: password
      username: analyst
      password_command: [op, read, "op://Engineering/ClickHouse warehouse/password"]   # 1Password CLI
```

## How a command is run

- A **string** is run through the shell: `sh -c` on macOS and Linux,
  `cmd /C` on Windows. Pipes, quoting and `~` work as in your shell.
- A **list** is run directly, as program and arguments, without a shell:
  `token_command: [gcloud, auth, print-identity-token, --audiences=...]`.
  Use it when an argument contains spaces or shell metacharacters.
- Flags and environment variables take the string form.
- The command runs with your user, in chcli's working directory, with chcli's
  environment. Standard error is shown to you. In the interactive shell the
  command also gets the terminal as standard input, so a tool that needs you
  to unlock a vault, touch a key or log in can ask; chcli's background work
  (loading completion metadata) never lets a command use the terminal.
- A command has two minutes to finish; on timeout or `Ctrl+C` it is killed
  together with any process it started.

## What chcli reads from it

The secret is the **last non-empty line** of standard output, so a tool that
prints a progress line first still works. If the output is a JSON object,
the first of these fields that is present is used: `token`, `access_token`,
`id_token`, `status.token` (the kubeconfig `ExecCredential` shape),
`password`, `secret`, `client_secret`, `value`. This is why an OAuth token
endpoint's response, or `aws eks get-token`-style output, can be used as is.

The output is never logged and never appears in an error message, not even
with `--debug`.

## When it runs again

- A **password** or **client secret** is fetched once per chcli process and
  kept in memory. Nothing is written to disk.
- A **bearer token** with an `exp` claim is fetched again one minute before
  it expires and the connection is re-established with the new token, so a
  long interactive session keeps working. A token without `exp` (an opaque
  token) is kept for the lifetime of the process.
- `chcli auth status` and `chcli doctor` run the command and report on what
  it returned (user, issuer, audience, expiry), without printing the secret.

## Rules

- A secret and its command are mutually exclusive: setting both is a
  configuration error. Command-line flags and environment variables that do
  not fit the profile's authentication type are rejected as well.
- `--ask-password` replaces a configured password command for that run.
- `chcli config show` prints the command (it is not a secret) and still
  redacts secret values.
- The command runs with your privileges, from a file in your home directory:
  keep `config.yaml` writable by you alone. chcli warns when the file is
  readable by others and holds plaintext secrets.

## Recipes

Each of these prints one secret on standard output. Store the secret once
with the tool's own command; the chcli side is a single line. Use the exact
names your tool shows, and test the command on its own first.

**macOS Keychain**

```sh
security add-generic-password -a "$USER" -s chcli-google -w     # store (prompts for the value)
```

```yaml
client_secret_command: security find-generic-password -s chcli-google -w
```

**Linux Secret Service (GNOME Keyring, KWallet)**

```sh
secret-tool store --label='chcli warehouse' service chcli account warehouse
```

```yaml
password_command: secret-tool lookup service chcli account warehouse
```

**Windows Credential Manager** (PowerShell with the `CredentialManager` module)

```yaml
password_command: powershell -NoProfile -Command "(Get-StoredCredential -Target chcli-warehouse).GetNetworkCredential().Password"
```

**1Password CLI** — `op` signs in through the desktop app when needed.

```yaml
password_command: [op, read, "op://Engineering/ClickHouse warehouse/password"]
```

**pass** (and gopass)

```yaml
password_command: pass show clickhouse/warehouse
```

**Bitwarden CLI** — needs an unlocked session (`BW_SESSION`).

```yaml
password_command: bw get password "ClickHouse warehouse"
```

**HashiCorp Vault**

```yaml
password_command: vault kv get -field=password secret/clickhouse/warehouse
```

**AWS Secrets Manager** — with `jq` when the secret is a JSON document.

```yaml
password_command: aws secretsmanager get-secret-value --secret-id clickhouse/warehouse --query SecretString --output text | jq -r .password
```

**Google Secret Manager**

```yaml
client_secret_command: gcloud secrets versions access latest --secret=chcli-google-client-secret
```

**Azure Key Vault**

```yaml
password_command: az keyvault secret show --vault-name my-vault --name clickhouse-warehouse --query value -o tsv
```

**A Kubernetes secret**

```yaml
password_command: kubectl -n clickhouse get secret clickhouse-admin -o jsonpath='{.data.password}' | base64 -d
```

**Google service account ID token** (Antalya with a `jwt_dynamic_jwks` or
`google` processor). The audience must be the OAuth client ID the server
expects, and `--include-email` puts the e-mail claim in; without it the
server cannot map the token to a user.

```yaml
auth:
  type: jwt
  token_command: gcloud auth print-identity-token --impersonate-service-account=reporting@my-project.iam.gserviceaccount.com --audiences=1234567890-xxxx.apps.googleusercontent.com --include-email
```

**Microsoft Entra ID access token** for a registered application.

```yaml
auth:
  type: jwt
  token_command: az account get-access-token --resource api://clickhouse --query accessToken -o tsv
```

**Any OAuth token endpoint** (client credentials). The endpoint answers with
JSON, which chcli reads directly; the client secret stays in a file only you
can read instead of on the command line.

```sh
printf 'grant_type=client_credentials&client_id=reporting&client_secret=%s' "$SECRET" > ~/.config/chcli/keycloak-reporting
chmod 600 ~/.config/chcli/keycloak-reporting
```

```yaml
auth:
  type: jwt
  token_command: curl -fsS --data @"$HOME/.config/chcli/keycloak-reporting" https://auth.example.com/realms/analytics/protocol/openid-connect/token
```

**Your own script**: anything that prints the secret, or a JSON object with
one of the fields above. A kubeconfig-style plugin that prints
`{"apiVersion": "client.authentication.k8s.io/v1", "kind": "ExecCredential", "status": {"token": "..."}}`
works unchanged.
