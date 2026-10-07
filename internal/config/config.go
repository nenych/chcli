// Package config loads the chcli configuration file and resolves the
// effective connection settings from profiles, environment variables and
// command-line flags.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"

	"go.yaml.in/yaml/v3"
)

// Authentication types.
const (
	AuthPassword = "password"
	AuthJWT      = "jwt"
	AuthOIDC     = "oidc"
	AuthGoogle   = "google"
)

// Transport protocols.
const (
	ProtocolNative = "native"
	ProtocolHTTP   = "http"
)

// OAuth login flows.
const (
	FlowBrowser = "browser"
	FlowDevice  = "device"
)

// Which OAuth token is presented to ClickHouse.
const (
	TokenTypeID     = "id_token"
	TokenTypeAccess = "access_token"
)

// File is the on-disk configuration.
type File struct {
	Connections map[string]Profile `yaml:"connections"`
	History     History            `yaml:"history"`
	Output      Output             `yaml:"output"`

	// Warnings collects non-fatal problems found while loading.
	Warnings []string `yaml:"-"`
}

// History configures the persistent REPL history.
type History struct {
	Enabled    *bool `yaml:"enabled"`
	MaxEntries int   `yaml:"max_entries"`
}

// DefaultHistoryEntries is used when history.max_entries is not set.
const DefaultHistoryEntries = 10000

// IsEnabled reports whether history is enabled (the default).
func (h History) IsEnabled() bool { return h.Enabled == nil || *h.Enabled }

// Limit returns the maximum number of history entries to keep.
func (h History) Limit() int {
	if h.MaxEntries > 0 {
		return h.MaxEntries
	}
	return DefaultHistoryEntries
}

// Output configures result rendering.
type Output struct {
	// Format is the default output format for interactive sessions.
	Format string `yaml:"format"`
	// Pager is a command (for example "less -FRSX") that interactive table
	// output is piped through. Empty disables paging.
	Pager string `yaml:"pager"`
}

// Profile is a named connection in the configuration file.
type Profile struct {
	Host               string `yaml:"host"`
	Port               int    `yaml:"port"`
	Database           string `yaml:"database"`
	Protocol           string `yaml:"protocol"`
	Secure             *bool  `yaml:"secure"`
	InsecureSkipVerify bool   `yaml:"insecure_skip_verify"`
	CACert             string `yaml:"ca_cert"`
	Auth               Auth   `yaml:"auth"`
}

// Auth holds the authentication settings of a connection. Which fields are
// meaningful depends on Type.
type Auth struct {
	Type string `yaml:"type"`

	// Password authentication.
	Username string `yaml:"username,omitempty"`
	Password Secret `yaml:"password,omitempty"`

	// Static JWT / bearer token authentication.
	Token Secret `yaml:"token,omitempty"`

	// OAuth 2.0 / OpenID Connect (types "oidc" and "google").
	ClientID              string   `yaml:"client_id,omitempty"`
	ClientSecret          Secret   `yaml:"client_secret,omitempty"`
	Issuer                string   `yaml:"issuer,omitempty"`
	AuthorizationEndpoint string   `yaml:"authorization_endpoint,omitempty"`
	TokenEndpoint         string   `yaml:"token_endpoint,omitempty"`
	DeviceEndpoint        string   `yaml:"device_endpoint,omitempty"`
	Audience              string   `yaml:"audience,omitempty"`
	Scopes                []string `yaml:"scopes,omitempty"`
	UsernameClaim         string   `yaml:"username_claim,omitempty"`
	RedirectURI           string   `yaml:"redirect_uri,omitempty"`
	Flow                  string   `yaml:"flow,omitempty"`
	TokenType             string   `yaml:"token_type,omitempty"`
}

// hasPlaintextSecret reports whether the auth block stores a secret in the file.
func (a Auth) hasPlaintextSecret() bool {
	return a.Password != "" || a.Token != "" || a.ClientSecret != ""
}

// Load reads the configuration file at path. A missing file is not an error:
// profiles are optional, so an empty configuration is returned.
func Load(path string) (*File, error) {
	f := &File{}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(f); err != nil && !errors.Is(err, io.EOF) { // EOF: empty file
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	if runtime.GOOS != "windows" {
		if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
			for _, name := range f.ProfileNames() {
				if f.Connections[name].Auth.hasPlaintextSecret() {
					f.Warnings = append(f.Warnings, fmt.Sprintf(
						"%s contains secrets and is readable by other users; run: chmod 600 %s", path, path))
					break
				}
			}
		}
	}
	return f, nil
}

// ProfileNames returns the configured profile names in sorted order.
func (f *File) ProfileNames() []string {
	names := make([]string, 0, len(f.Connections))
	for name := range f.Connections {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DefaultPath returns the configuration file location: $CHCLI_CONFIG if set,
// otherwise $XDG_CONFIG_HOME/chcli/config.yaml (~/.config/chcli/config.yaml)
// on Unix-like systems and %AppData%\chcli\config.yaml on Windows.
func DefaultPath() string {
	if p := os.Getenv("CHCLI_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(baseDir("XDG_CONFIG_HOME", ".config", "AppData"), "chcli", "config.yaml")
}

// StateDir returns the directory for history and the fallback token cache:
// $XDG_STATE_HOME/chcli (~/.local/state/chcli) on Unix-like systems and
// %LocalAppData%\chcli on Windows.
func StateDir() string {
	return filepath.Join(baseDir("XDG_STATE_HOME", filepath.Join(".local", "state"), "LocalAppData"), "chcli")
}

func baseDir(xdgVar, homeRel, windowsVar string) string {
	if runtime.GOOS == "windows" {
		if dir := os.Getenv(windowsVar); dir != "" {
			return dir
		}
	} else if dir := os.Getenv(xdgVar); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, homeRel)
}
