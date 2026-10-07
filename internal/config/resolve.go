package config

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// GoogleIssuer is the OpenID Connect issuer of Google accounts.
const GoogleIssuer = "https://accounts.google.com"

// Setting keys. Each key is the name of a command-line flag (--host) and,
// upper-cased with a CHCLI_ prefix, of an environment variable (CHCLI_HOST).
//
//nolint:gosec // G101: these are setting names, not credentials
const (
	KeyHost               = "host"
	KeyPort               = "port"
	KeyDatabase           = "database"
	KeyProtocol           = "protocol"
	KeySecure             = "secure"
	KeyInsecureSkipVerify = "insecure-skip-verify"
	KeyCACert             = "ca-cert"
	KeyAuth               = "auth"
	KeyGoogleOAuth        = "google-oauth"
	KeyUser               = "user"
	KeyPassword           = "password"
	KeyJWTToken           = "jwt-token"
	KeyClientID           = "oauth-client-id"
	KeyClientSecret       = "oauth-client-secret"
	KeyIssuer             = "oauth-issuer"
	KeyAuthEndpoint       = "oauth-authorization-endpoint"
	KeyTokenEndpoint      = "oauth-token-endpoint"
	KeyDeviceEndpoint     = "oauth-device-endpoint"
	KeyAudience           = "oauth-audience"
	KeyRedirectURI        = "oauth-redirect-uri"
	KeyUsernameClaim      = "oauth-username-claim"
	KeyScope              = "oauth-scope"
	KeyFlow               = "oauth-flow"
	KeyTokenType          = "oauth-token-type"
)

// CommandKey returns the key of the command companion of a secret setting:
// "password" -> "password-command" (--password-command, CHCLI_PASSWORD_COMMAND).
func CommandKey(secretKey string) string { return secretKey + "-command" }

// SecretKeys lists the settings that hold secrets and so have a command companion.
var SecretKeys = []string{KeyPassword, KeyJWTToken, KeyClientSecret}

// Source is one layer of overrides (environment or command-line flags).
type Source struct {
	// Lookup returns the value for a setting key and whether it is set.
	Lookup func(key string) (string, bool)
	// Name returns how the key is spelled in this source, for error messages.
	Name func(key string) string
}

// EnvName returns the environment variable for a setting key.
func EnvName(key string) string {
	return "CHCLI_" + strings.ToUpper(strings.ReplaceAll(key, "-", "_"))
}

// EnvSource builds a Source over an environment lookup function such as
// os.LookupEnv. Empty variables count as unset.
func EnvSource(lookupEnv func(string) (string, bool)) Source {
	return Source{
		Lookup: func(key string) (string, bool) {
			v, ok := lookupEnv(EnvName(key))
			return v, ok && v != ""
		},
		Name: EnvName,
	}
}

// Resolved is the effective configuration of one connection after merging
// built-in defaults, the selected profile, environment variables and flags.
type Resolved struct {
	Profile            string `yaml:"profile,omitempty"`
	Host               string `yaml:"host"`
	Port               int    `yaml:"port"`
	Database           string `yaml:"database"`
	Protocol           string `yaml:"protocol"`
	Secure             bool   `yaml:"secure"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify,omitempty"`
	CACert             string `yaml:"ca_cert,omitempty"`
	Auth               Auth   `yaml:"auth"`

	// secureExplicit records that Secure was set by the user rather than inferred.
	secureExplicit bool
}

// Label names the connection for prompts and messages: the profile name when
// a profile is in use, the host otherwise.
func (r *Resolved) Label() string {
	if r.Profile != "" {
		return r.Profile
	}
	return r.Host
}

// Addr returns host:port.
func (r *Resolved) Addr() string {
	return net.JoinHostPort(r.Host, strconv.Itoa(r.Port))
}

// subject prefixes validation errors, naming the profile when there is one.
func (r *Resolved) subject() string {
	if r.Profile != "" {
		return fmt.Sprintf("profile %q", r.Profile)
	}
	return "connection"
}

// Resolve computes the effective configuration. Precedence, highest first:
// flags, environment, the selected profile, built-in defaults. The stored
// profile is never modified.
func Resolve(file *File, profileName string, env, flags Source) (*Resolved, error) {
	r := &Resolved{Profile: profileName}
	var secure *bool

	if profileName != "" {
		p, ok := file.Connections[profileName]
		if !ok {
			return nil, unknownProfileError(file, profileName)
		}
		r.Host, r.Port, r.Database, r.Protocol = p.Host, p.Port, p.Database, p.Protocol
		r.InsecureSkipVerify, r.CACert = p.InsecureSkipVerify, p.CACert
		r.Auth = p.Auth
		r.Auth.Scopes = append([]string(nil), p.Auth.Scopes...)
		if p.Secure != nil {
			v := *p.Secure
			secure = &v
		}
	}

	for _, src := range []Source{env, flags} {
		if src.Lookup == nil {
			continue
		}
		if err := applySource(r, &secure, src); err != nil {
			return nil, err
		}
	}

	applyDefaults(r, secure)
	if err := r.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", r.subject(), err)
	}
	if err := checkFlagsFitAuth(r, flags); err != nil {
		return nil, err
	}
	return r, nil
}

// checkFlagsFitAuth rejects credential flags that the selected
// authentication type ignores, such as --jwt-token on a password profile:
// silently dropping what the user typed would hide a mistake. Environment
// variables are exempt, since they are often set for a whole shell session.
func checkFlagsFitAuth(r *Resolved, flags Source) error {
	if flags.Lookup == nil {
		return nil
	}
	oauth := []string{AuthOIDC, AuthGoogle}
	for _, rule := range []struct {
		types []string
		keys  []string
	}{
		{[]string{AuthPassword}, []string{KeyUser, KeyPassword, CommandKey(KeyPassword)}},
		{[]string{AuthJWT}, []string{KeyJWTToken, CommandKey(KeyJWTToken)}},
		{oauth, []string{KeyClientID, KeyClientSecret, CommandKey(KeyClientSecret), KeyIssuer, KeyAuthEndpoint, KeyTokenEndpoint,
			KeyDeviceEndpoint, KeyAudience, KeyRedirectURI, KeyUsernameClaim, KeyScope, KeyFlow, KeyTokenType}},
	} {
		if slices.Contains(rule.types, r.Auth.Type) {
			continue
		}
		for _, key := range rule.keys {
			if _, set := flags.Lookup(key); set {
				return fmt.Errorf("%s does not apply to %q authentication; add %s %s to switch", flags.Name(key), r.Auth.Type, flags.Name(KeyAuth), rule.types[0])
			}
		}
	}
	return nil
}

// isLoopback reports whether host refers to this machine.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func unknownProfileError(file *File, name string) error {
	names := file.ProfileNames()
	if len(names) == 0 {
		return fmt.Errorf("profile %q not found: no profiles are configured in %s", name, DefaultPath())
	}
	return fmt.Errorf("profile %q not found (available: %s)", name, strings.Join(names, ", "))
}

func applySource(r *Resolved, secure **bool, src Source) error {
	str := func(key string, dst *string) {
		if v, ok := src.Lookup(key); ok {
			*dst = v
		}
	}
	// A secret can be given directly or as a command that prints it.
	secret := func(key string, dst *Secret, cmd *Command) {
		if v, ok := src.Lookup(key); ok {
			*dst = Secret(v)
		}
		if v, ok := src.Lookup(CommandKey(key)); ok {
			*cmd = Command{Shell: v}
		}
	}
	boolean := func(key string) (value, set bool, err error) {
		v, ok := src.Lookup(key)
		if !ok {
			return false, false, nil
		}
		b, err := strconv.ParseBool(v)
		if err != nil {
			return false, false, fmt.Errorf("invalid %s value %q: expected true or false", src.Name(key), v)
		}
		return b, true, nil
	}

	str(KeyHost, &r.Host)
	str(KeyDatabase, &r.Database)
	str(KeyProtocol, &r.Protocol)
	str(KeyCACert, &r.CACert)
	if v, ok := src.Lookup(KeyPort); ok {
		port, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("invalid %s value %q: expected a port number", src.Name(KeyPort), v)
		}
		r.Port = port
	}
	if v, set, err := boolean(KeySecure); err != nil {
		return err
	} else if set {
		*secure = &v
	}
	if v, set, err := boolean(KeyInsecureSkipVerify); err != nil {
		return err
	} else if set {
		r.InsecureSkipVerify = v
	}

	authType, authSet := src.Lookup(KeyAuth)
	google, _, err := boolean(KeyGoogleOAuth)
	if err != nil {
		return err
	}
	if google {
		if authSet && authType != AuthGoogle {
			return fmt.Errorf("%s conflicts with %s=%s", src.Name(KeyGoogleOAuth), src.Name(KeyAuth), authType)
		}
		authType, authSet = AuthGoogle, true
	}
	if authSet {
		r.Auth.Type = authType
	}

	a := &r.Auth
	str(KeyUser, &a.Username)
	secret(KeyPassword, &a.Password, &a.PasswordCommand)
	secret(KeyJWTToken, &a.Token, &a.TokenCommand)
	str(KeyClientID, &a.ClientID)
	secret(KeyClientSecret, &a.ClientSecret, &a.ClientSecretCommand)
	str(KeyIssuer, &a.Issuer)
	str(KeyAuthEndpoint, &a.AuthorizationEndpoint)
	str(KeyTokenEndpoint, &a.TokenEndpoint)
	str(KeyDeviceEndpoint, &a.DeviceEndpoint)
	str(KeyAudience, &a.Audience)
	str(KeyRedirectURI, &a.RedirectURI)
	str(KeyUsernameClaim, &a.UsernameClaim)
	str(KeyFlow, &a.Flow)
	str(KeyTokenType, &a.TokenType)
	if v, ok := src.Lookup(KeyScope); ok {
		a.Scopes = strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
	}
	return nil
}

// applyDefaults fills in everything the user left unset and drops auth
// fields that do not belong to the selected authentication type, so that an
// override such as "--profile production --auth password" does not drag the
// profile's OAuth settings along.
func applyDefaults(r *Resolved, secure *bool) {
	if r.Protocol == "" {
		r.Protocol = ProtocolNative
	}
	if r.Database == "" {
		r.Database = "default"
	}
	applyAuthDefaults(&r.Auth)

	plaintextPort := r.Port == 9000 || r.Port == 8123
	switch {
	case secure != nil:
		r.Secure, r.secureExplicit = *secure, true
	case r.Port == 9440 || r.Port == 8443 || r.Port == 443:
		r.Secure = true // the well-known TLS ports
	case r.Auth.Type != AuthPassword && !isLoopback(r.Host) && !plaintextPort:
		// A bearer token is a credential for the identity provider, not just
		// for this server: never send one over the network unencrypted
		// unless that was asked for explicitly.
		r.Secure = true
	}
	if r.Port == 0 {
		switch {
		case r.Protocol == ProtocolHTTP && r.Secure:
			r.Port = 8443
		case r.Protocol == ProtocolHTTP:
			r.Port = 8123
		case r.Secure:
			r.Port = 9440
		default:
			r.Port = 9000
		}
	}
}

func applyAuthDefaults(a *Auth) {
	if a.Type == "" {
		switch {
		case a.Token != "" || a.TokenCommand.IsSet():
			a.Type = AuthJWT
		case a.ClientID != "" && a.Issuer == GoogleIssuer:
			a.Type = AuthGoogle
		case a.ClientID != "" || a.Issuer != "":
			a.Type = AuthOIDC
		default:
			a.Type = AuthPassword
		}
	}

	switch a.Type {
	case AuthPassword:
		*a = Auth{Type: a.Type, Username: a.Username, Password: a.Password, PasswordCommand: a.PasswordCommand}
		if a.Username == "" {
			a.Username = "default"
		}
	case AuthJWT:
		*a = Auth{Type: a.Type, Token: a.Token, TokenCommand: a.TokenCommand}
	case AuthGoogle, AuthOIDC:
		a.Username, a.Password, a.PasswordCommand = "", "", Command{}
		a.Token, a.TokenCommand = "", Command{}
		if a.Type == AuthGoogle && a.Issuer == "" {
			a.Issuer = GoogleIssuer
		}
		if len(a.Scopes) == 0 {
			a.Scopes = []string{"openid", "email", "profile"}
		}
		if a.UsernameClaim == "" {
			a.UsernameClaim = "email"
		}
		if a.Flow == "" {
			a.Flow = FlowBrowser
		}
		if a.TokenType == "" {
			// The access token is what OAuth resource servers expect, and
			// what Altinity Antalya's token processors validate (including
			// its built-in Google processor). Servers that instead verify
			// OIDC ID tokens against the provider's JWKS need id_token.
			a.TokenType = TokenTypeAccess
		}
	}
}

func (r *Resolved) validate() error {
	if r.Host == "" {
		return fmt.Errorf("no host configured (use --host, %s or a profile)", EnvName(KeyHost))
	}
	if r.Port < 1 || r.Port > 65535 {
		return fmt.Errorf("invalid port %d", r.Port)
	}
	if r.Protocol != ProtocolNative && r.Protocol != ProtocolHTTP {
		return fmt.Errorf("unknown protocol %q (expected %s or %s)", r.Protocol, ProtocolNative, ProtocolHTTP)
	}
	if r.Secure && (r.Port == 9000 || r.Port == 8123) {
		return fmt.Errorf("secure=true but port %d is ClickHouse's plaintext port; use the TLS port (9440 native, 8443 http) or disable secure", r.Port)
	}
	if !r.Secure && (r.InsecureSkipVerify || r.CACert != "") {
		return fmt.Errorf("TLS options (insecure_skip_verify, ca_cert) are set but secure=false")
	}
	if r.Auth.Type != AuthPassword && !r.Secure && !r.secureExplicit && !isLoopback(r.Host) {
		return fmt.Errorf("auth type %q would send the token to %s without TLS; use the TLS port with secure=true, or set secure=false explicitly to allow it", r.Auth.Type, r.Addr())
	}

	a := r.Auth
	for _, s := range []struct {
		name    string
		value   Secret
		command Command
	}{
		{"password", a.Password, a.PasswordCommand},
		{"token", a.Token, a.TokenCommand},
		{"client_secret", a.ClientSecret, a.ClientSecretCommand},
	} {
		if s.value != "" && s.command.IsSet() {
			return fmt.Errorf("auth.%s and auth.%s_command are both set; use one of them", s.name, s.name)
		}
	}
	switch a.Type {
	case AuthPassword:
	case AuthJWT:
		if a.Token == "" && !a.TokenCommand.IsSet() {
			return fmt.Errorf("auth type %q requires a token (--%s or %s) or a command that prints one (auth.token_command, --%s)",
				a.Type, KeyJWTToken, EnvName(KeyJWTToken), CommandKey(KeyJWTToken))
		}
	case AuthGoogle, AuthOIDC:
		if a.ClientID == "" {
			return fmt.Errorf("auth type %q requires auth.client_id (--%s or %s)", a.Type, KeyClientID, EnvName(KeyClientID))
		}
		if a.Flow != FlowBrowser && a.Flow != FlowDevice {
			return fmt.Errorf("unknown auth.flow %q (expected %s or %s)", a.Flow, FlowBrowser, FlowDevice)
		}
		if a.TokenType != TokenTypeID && a.TokenType != TokenTypeAccess {
			return fmt.Errorf("unknown auth.token_type %q (expected %s or %s)", a.TokenType, TokenTypeID, TokenTypeAccess)
		}
		if a.Issuer == "" {
			if a.TokenEndpoint == "" {
				return fmt.Errorf("auth type %q requires auth.issuer (--%s) or explicit endpoints", a.Type, KeyIssuer)
			}
			if a.Flow == FlowBrowser && a.AuthorizationEndpoint == "" {
				return fmt.Errorf("auth type %q without auth.issuer requires auth.authorization_endpoint", a.Type)
			}
			if a.Flow == FlowDevice && a.DeviceEndpoint == "" {
				return fmt.Errorf("auth flow %q without auth.issuer requires auth.device_endpoint", a.Flow)
			}
		}
		for name, endpoint := range map[string]string{
			"issuer": a.Issuer, "authorization_endpoint": a.AuthorizationEndpoint,
			"token_endpoint": a.TokenEndpoint, "device_endpoint": a.DeviceEndpoint,
		} {
			if endpoint == "" {
				continue
			}
			u, err := url.Parse(endpoint)
			if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
				return fmt.Errorf("auth.%s %q is not a valid URL", name, endpoint)
			}
			if u.Scheme == "http" && !isLoopback(u.Hostname()) {
				return fmt.Errorf("auth.%s %q must use https", name, endpoint)
			}
		}
		if a.RedirectURI != "" {
			if err := ValidateRedirectURI(a.RedirectURI); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unknown auth type %q (expected %s, %s, %s or %s)",
			a.Type, AuthPassword, AuthJWT, AuthOIDC, AuthGoogle)
	}
	return nil
}

// ValidateRedirectURI checks that an OAuth redirect URI points at a local
// loopback HTTP listener, the only kind this client can serve.
func ValidateRedirectURI(redirect string) error {
	u, err := url.Parse(redirect)
	if err != nil || u.Scheme != "http" || u.Hostname() == "" {
		return fmt.Errorf("auth.redirect_uri %q must be an http:// loopback URL such as http://127.0.0.1:8765/callback", redirect)
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("auth.redirect_uri %q must point at a loopback address (127.0.0.1, [::1] or localhost)", redirect)
	}
	return nil
}
