# Authentication

How chcli authenticates to ClickHouse: passwords, static JWT / bearer tokens,
Google login, and any OpenID Connect provider; how login sessions are managed;
the Google Cloud setup; and worked examples for Altinity Antalya.

## Connecting

Every connection setting can be given as a flag, as an environment variable or
in a [profile](configuration.md#profiles). Run `chcli --help` for the full list.

| Setting | Flag | Default |
|---|---|---|
| Host | `--host` | (required) |
| Port | `--port` | 9000; 9440 with TLS; 8123 / 8443 for `--protocol http` |
| Database | `--database`, `-d` | `default` |
| Transport | `--protocol native\|http` | `native` |
| TLS | `--secure` | on if the port is 9440, 8443 or 443, and for token authentication to a remote host; otherwise off |
| Custom CA | `--ca-cert file.pem` | system trust store |
| Skip verification | `--insecure-skip-verify` | off |
| Authentication | `--auth password\|jwt\|oidc\|google` | inferred, else `password` |

### Password

```sh
chcli --host clickhouse.example.com --secure --user chronicle --ask-password
```

`--password secret` works too, but the password then shows up in your shell
history and in the process list. Prefer `--ask-password`, or `CHCLI_PASSWORD`
in scripts.

### JWT / bearer token

For a token you already have (from a CI job, a service account, another tool):

```sh
export CHCLI_JWT_TOKEN="$(my-token-command)"
chcli --host clickhouse.example.com --secure --auth jwt
```

The token is sent the way ClickHouse expects it: in the handshake of the
native protocol, or as `Authorization: Bearer` over HTTP. This is the same
mechanism as `clickhouse-client --jwt`.

#### Getting the token from a command

Instead of a fixed token, name a command that prints one, the way a
kubeconfig names `aws eks get-token` for EKS:

```yaml
connections:
  reporting:
    host: clickhouse.example.com
    port: 9440
    secure: true
    auth:
      type: jwt
      token_command: gcloud auth print-identity-token --impersonate-service-account=reporting@my-project.iam.gserviceaccount.com --audiences=1234567890-abc.apps.googleusercontent.com --include-email
```

On the command line: `--jwt-token-command "..."` or `CHCLI_JWT_TOKEN_COMMAND`;
`auth.type: jwt` is implied. chcli runs the command when it connects and runs
it again whenever the token it printed is about to expire, so a long
interactive session keeps working. The same mechanism serves passwords and
OAuth client secrets; the forms a command can take and how its output is read
are described in [Secrets from commands](configuration.md#secrets-from-commands).

**Tokens and TLS.** A bearer token is a credential for your identity provider,
not only for this server, so `chcli` does not send one over the network
unencrypted by accident. With `jwt`, `oidc` or `google` authentication TLS is
the default for any host other than `localhost`, even without `--secure`. If
you combine a token with a plaintext port (9000, 8123) on a remote host,
`chcli` stops and asks you to choose: the TLS port, or an explicit
`--secure=false`.

### Google OAuth

```sh
chcli --host clickhouse.example.com --secure \
  --google-oauth \
  --oauth-client-id 1234567890-abc.apps.googleusercontent.com \
  --oauth-client-secret "$GOOGLE_CLIENT_SECRET"
```

```console
Opening browser for Google authentication...
If the browser did not open, visit:
https://accounts.google.com/o/oauth2/v2/auth?...
Waiting for authentication...
Authenticated as user@example.com
Connected to clickhouse.example.com (ClickHouse 26.3.13.20001.altinityantalya)

clickhouse.example.com/default :)
```

The next start reuses the cached session and goes straight to the prompt; no
browser opens while the token is valid or can be refreshed. Google also needs
the client secret for every token refresh, so keep it available in each
shell: as `CHCLI_OAUTH_CLIENT_SECRET`, or better, as a
`client_secret_command` that reads it from your keychain or password manager
(see [Secrets from commands](configuration.md#secrets-from-commands)).

`--google-oauth` is a shortcut for `--auth google`, which is the generic OIDC
provider with Google's specifics preset: the issuer, the `openid email profile`
scopes, `email` as the displayed identity, and a request for offline access so
that Google issues a refresh token. See the
[setup guide](#google-oauth-setup-guide) for creating the client ID.

### Generic OIDC

Any OpenID Connect provider (Keycloak, Okta, Auth0, Microsoft Entra ID, Dex,
...) works without Google-specific assumptions:

```sh
chcli --host clickhouse.example.com --secure \
  --auth oidc \
  --oauth-issuer https://auth.example.com/realms/analytics \
  --oauth-client-id clickhouse-cli \
  --oauth-audience clickhouse
```

Endpoints are discovered from `<issuer>/.well-known/openid-configuration`.
Each can be overridden, and with all of them given no issuer is needed at all.

| Setting | Flag | Profile key | Notes |
|---|---|---|---|
| Client ID | `--oauth-client-id` | `client_id` | required |
| Client secret | `--oauth-client-secret` | `client_secret` | optional; public clients use PKCE alone |
| Issuer | `--oauth-issuer` | `issuer` | enables discovery and ID token verification |
| Authorization endpoint | `--oauth-authorization-endpoint` | `authorization_endpoint` | overrides discovery |
| Token endpoint | `--oauth-token-endpoint` | `token_endpoint` | overrides discovery |
| Device endpoint | `--oauth-device-endpoint` | `device_endpoint` | overrides discovery |
| Audience | `--oauth-audience` | `audience` | sent as the `audience` request parameter (not for Google) |
| Scopes | `--oauth-scope` (repeatable) | `scopes` | default `openid email profile` |
| Redirect URI | `--oauth-redirect-uri` | `redirect_uri` | loopback only; default `http://127.0.0.1:<random port>/callback` |
| Identity claim | `--oauth-username-claim` | `username_claim` | for display; default `email`, falling back to `preferred_username`, `sub` |
| Login flow | `--oauth-flow browser\|device` | `flow` | default `browser` |
| Token sent to ClickHouse | `--oauth-token-type access_token\|id_token` | `token_type` | default `access_token` |

Things that commonly need attention:

- **Redirect URI.** Many providers only accept an exactly registered redirect
  URI. Register for example `http://127.0.0.1:8765/callback` and set
  `redirect_uri` to the same value.
- **Refresh tokens.** Several providers only issue one when the
  `offline_access` scope is requested: add `--oauth-scope offline_access`
  (alongside the others). Without a refresh token you log in again whenever
  the access token expires.
- **Which token.** By default the *access token* is presented to ClickHouse.
  If your server validates OIDC *ID tokens* instead, set
  `token_type: id_token`.
- **No browser on this machine** (SSH session, container): use
  `--oauth-flow device`, which prints a URL and a code to enter on any device.

#### Managing the login session

```console
$ chcli auth login --profile production      # log in (or switch account) explicitly
$ chcli auth status --profile production
Profile: production
Authentication: Google OAuth
User: user@example.com
Token status: valid
Expires in: 43m
Refresh token: present
Token storage: system keyring
$ chcli auth logout --profile production     # forget the cached tokens
```

`auth status` never prints token material, and exits with status 3 when there
is no usable session, which makes it convenient in scripts.

## Altinity Antalya

[Altinity Antalya](https://docs.altinity.com/altinityantalya/) builds of
ClickHouse can authenticate users by OAuth token: the server validates the
token with a *token processor*, takes the user name from a claim such as
`email`, `preferred_username` or `sub`, and assigns roles. The user does
**not** have to exist as a ClickHouse user, and no ClickHouse password is
involved. Altinity's
[OAuth documentation](https://docs.altinity.com/altinityantalya/integrating-oauth/)
is the reference for the server side; what follows is the part that matters to
`chcli`.

### Example: an OIDC provider that issues JWT access tokens

Server (`/etc/clickhouse-server/config.d/token-auth.xml`):

```xml
<clickhouse>
    <token_processors>
        <corp_idp>
            <type>jwt_dynamic_jwks</type>
            <jwks_uri>https://auth.example.com/realms/analytics/protocol/openid-connect/certs</jwks_uri>
            <expected_issuer>https://auth.example.com/realms/analytics</expected_issuer>
            <expected_audience>clickhouse</expected_audience>
            <username_claim>email</username_claim>
        </corp_idp>
    </token_processors>

    <!-- Users known only by their token: everyone with a valid token gets these roles. -->
    <user_directories>
        <token>
            <processor>corp_idp</processor>
            <common_roles>
                <analyst />
            </common_roles>
        </token>
    </user_directories>
</clickhouse>
```

```sql
CREATE ROLE analyst;
GRANT SELECT ON chronicle.* TO analyst;
```

Client (`~/.config/chcli/config.yaml`):

```yaml
connections:
  antalya:
    host: clickhouse.example.com
    port: 9440
    secure: true
    auth:
      type: oidc
      issuer: https://auth.example.com/realms/analytics
      client_id: clickhouse-cli
      audience: clickhouse
      scopes: [openid, email, offline_access]
```

```console
$ chcli --profile antalya
Opening browser for OIDC authentication...
Authenticated as alice@example.com
Connected to clickhouse.example.com (ClickHouse 26.3.13.20001.altinityantalya)

antalya/default :) SELECT currentUser(), currentRoles();
┌─currentUser()─────┬─currentRoles()─┐
│ alice@example.com │ ['analyst']    │
└───────────────────┴────────────────┘
```

`alice@example.com` was never created in ClickHouse. Roles can also be derived
from the token's group claim (`groups_claim`, `roles_filter`,
`roles_transform`, `roles_mapping`); see Altinity's documentation.

### Google with Antalya

Google access tokens are opaque rather than JWTs. Antalya has a processor for
them that asks Google who the token belongs to:

```xml
<clickhouse>
    <token_processors>
        <google>
            <type>google</type>
            <username_claim>email</username_claim>
            <!-- Only accept tokens issued to your OAuth client. -->
            <expected_audience>1234567890-abc.apps.googleusercontent.com</expected_audience>
        </google>
    </token_processors>
    <user_directories>
        <token>
            <processor>google</processor>
            <common_roles>
                <analyst />
            </common_roles>
        </token>
    </user_directories>
</clickhouse>
```

This pairs with `chcli`'s default for `--auth google`, which presents the
access token. If your server instead validates Google **ID tokens** (a
`jwt_dynamic_jwks` processor with
`jwks_uri` `https://www.googleapis.com/oauth2/v3/certs`, `expected_issuer`
`https://accounts.google.com` and your client ID as `expected_audience`), set
`token_type: id_token` in the profile.

### What has been verified

- The `jwt_static_key` and `jwt_dynamic_jwks` processors, `username_claim`,
  `expected_issuer`, `expected_audience` and `user_directories.token` with
  `common_roles` were exercised with `chcli` against Antalya
  26.3.13.20001 over both the native and the HTTP protocol. The static-key
  variant is part of this repository's integration tests
  (`testdata/antalya`, `make antalya-up`).
- The `google` processor is present in that build and treats the token as a
  Google access token. It is not listed in Altinity's documentation at the
  time of writing, and a login with real Google credentials has **not** been
  tested as part of this project. Check it against your build.

Good to know:

- The server takes the user name from `sub` unless `username_claim` says
  otherwise. `chcli`'s own `username_claim` only controls what is displayed;
  `\status` shows the ClickHouse user as well when it differs.
- The server caches token validation results (`token_cache_lifetime`), so a
  revoked token can remain usable for that long.
- `chcli` uses TLS for token authentication to any remote host by default;
  see [Tokens and TLS](#jwt--bearer-token).

## Google OAuth setup guide

1. **Create or pick a Google Cloud project** in the
   [Google Cloud Console](https://console.cloud.google.com/).

2. **Configure the OAuth consent screen** (*APIs & Services → OAuth consent
   screen*, called *Google Auth Platform* in newer consoles).
   - Choose **Internal** if every user is in your Google Workspace
     organisation; only they can then log in. Otherwise choose **External**
     and add your users as test users, or publish the app.
   - Add the scopes `openid`, `.../auth/userinfo.email` and
     `.../auth/userinfo.profile`.

3. **Create the client** (*APIs & Services → Credentials → Create credentials
   → OAuth client ID*).
   - Application type **Desktop app**. Desktop clients may redirect to any
     loopback port, which is what `chcli` uses, so no redirect URI has to be
     registered.
   - Note the **client ID** and **client secret**.

   Google requires the client secret even though the flow is protected by
   PKCE. For a desktop client it is not a confidential value in the OAuth
   sense (Google's documentation says as much), but keep it out of the
   configuration file anyway and supply it through the environment.

4. **Add a profile**:

   ```yaml
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
   export CHCLI_OAUTH_CLIENT_SECRET='GOCSPX-...'
   ```

5. **Configure the server** to accept Google tokens and to map users to roles;
   see [Google with Antalya](#google-with-antalya).

6. **Log in**:

   ```console
   $ chcli auth login --profile production
   Opening browser for Google authentication...
   Waiting for authentication...
   Authenticated as user@example.com
   $ chcli --profile production
   ```

Notes:

- **"Web application" clients** work too, but need the exact redirect URI
  registered: add `http://127.0.0.1:8765/callback` under *Authorized redirect
  URIs* and set `redirect_uri` to the same value in the profile.
- **Device flow** (`flow: device`) needs a client of type *TVs and Limited
  Input devices*.
- **Refresh tokens.** `chcli` requests offline access and consent, so Google
  returns a refresh token on every login. While an External app is in
  *Testing* status, Google expires refresh tokens after seven days.
- `chcli doctor --profile production` checks discovery and the cached session.
