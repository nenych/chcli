package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zalando/go-keyring"
)

// TokenSet is a cached OAuth session.
type TokenSet struct {
	AccessToken  string    `json:"access_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	AccessExpiry time.Time `json:"access_expiry,omitempty"`
	IDExpiry     time.Time `json:"id_expiry,omitempty"`
	Identity     string    `json:"identity,omitempty"`

	// Fingerprint identifies the provider configuration the tokens were
	// issued for, so that editing a profile invalidates its cached session.
	Fingerprint string `json:"fingerprint"`

	// Storage says where the set was loaded from or saved to.
	Storage string `json:"-"`
}

// TokenStore persists token sets between runs.
type TokenStore interface {
	// Load returns the token set stored under key, or nil if there is none.
	Load(key string) (*TokenSet, error)
	Save(key string, ts *TokenSet) error
	Delete(key string) error
}

// NewTokenStore returns the default store: the operating system's credential
// manager (macOS Keychain, Windows Credential Manager, Secret Service on
// Linux), falling back to owner-only files under dir when the credential
// manager is unavailable.
func NewTokenStore(dir string) TokenStore {
	return &fallbackStore{primary: keyringStore{}, secondary: &FileStore{Dir: dir}}
}

const keyringService = "chcli"

// OS credential managers cap the size of an entry: 2560 bytes in Windows
// Credential Manager, and about 3 KB through the macOS security tool. Token
// sets from providers with large tokens exceed that, so anything bigger than
// keyringChunk is base64-encoded and spread over several entries: the main
// entry then holds "chcli-chunks:<n>" and the data lives in "<key>#0" ...
const (
	keyringChunk   = 1800
	keyringChunked = "chcli-chunks:"
)

type keyringStore struct{}

func keyringPart(key string, i int) string { return key + "#" + strconv.Itoa(i) }

// keyringParts returns how many chunk entries key currently has.
func keyringParts(key string) int {
	head, err := keyring.Get(keyringService, key)
	if err != nil || !strings.HasPrefix(head, keyringChunked) {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(head, keyringChunked))
	return n
}

func (keyringStore) Load(key string) (*TokenSet, error) {
	data, err := keyring.Get(keyringService, key)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(data, keyringChunked) {
		var encoded strings.Builder
		for i := range keyringParts(key) {
			part, err := keyring.Get(keyringService, keyringPart(key, i))
			if err != nil {
				return nil, fmt.Errorf("incomplete keyring entry: %w", err)
			}
			encoded.WriteString(part)
		}
		raw, err := base64.RawStdEncoding.DecodeString(encoded.String())
		if err != nil {
			return nil, fmt.Errorf("corrupt keyring entry: %w", err)
		}
		data = string(raw)
	}
	ts := &TokenSet{Storage: "system keyring"}
	if err := json.Unmarshal([]byte(data), ts); err != nil {
		return nil, fmt.Errorf("corrupt keyring entry: %w", err)
	}
	return ts, nil
}

func (keyringStore) Save(key string, ts *TokenSet) error {
	data, err := json.Marshal(ts) //nolint:gosec // G117: storing the tokens is this type's purpose
	if err != nil {
		return err
	}
	previous := keyringParts(key)
	parts := 0
	if len(data) <= keyringChunk {
		if err := keyring.Set(keyringService, key, string(data)); err != nil {
			return err
		}
	} else {
		encoded := base64.RawStdEncoding.EncodeToString(data)
		for ; len(encoded) > 0; parts++ {
			n := min(len(encoded), keyringChunk)
			if err := keyring.Set(keyringService, keyringPart(key, parts), encoded[:n]); err != nil {
				return err
			}
			encoded = encoded[n:]
		}
		if err := keyring.Set(keyringService, key, keyringChunked+strconv.Itoa(parts)); err != nil {
			return err
		}
	}
	for i := parts; i < previous; i++ { // chunks of an earlier, larger entry
		_ = keyring.Delete(keyringService, keyringPart(key, i))
	}
	ts.Storage = "system keyring"
	return nil
}

func (keyringStore) Delete(key string) error {
	for i := range keyringParts(key) {
		if err := keyring.Delete(keyringService, keyringPart(key, i)); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return err
		}
	}
	if err := keyring.Delete(keyringService, key); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	return nil
}

// FileStore keeps each token set in a JSON file readable only by its owner.
type FileStore struct {
	Dir string
}

// path maps a key to a file name that is valid on every platform (keys
// contain ':' and may contain path separators) and still tells entries apart.
func (s *FileStore) path(key string) string {
	var name strings.Builder
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			name.WriteRune(r)
		default:
			name.WriteByte('_')
		}
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(s.Dir, name.String()+"-"+hex.EncodeToString(sum[:4])+".json")
}

func (s *FileStore) Load(key string) (*TokenSet, error) {
	data, err := os.ReadFile(s.path(key))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ts := &TokenSet{Storage: "file " + s.path(key)}
	if err := json.Unmarshal(data, ts); err != nil {
		return nil, fmt.Errorf("corrupt token file %s: %w", s.path(key), err)
	}
	return ts, nil
}

// Save writes the token set atomically with 0600 permissions.
func (s *FileStore) Save(key string, ts *TokenSet) error {
	data, err := json.Marshal(ts) //nolint:gosec // G117: storing the tokens is this type's purpose
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, ".token-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.path(key)); err != nil {
		return err
	}
	ts.Storage = "file " + s.path(key)
	return nil
}

func (s *FileStore) Delete(key string) error {
	if err := os.Remove(s.path(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// fallbackStore prefers primary and uses secondary when primary fails. An
// entry lives in exactly one of them: saving to one removes it from the other
// so a stale copy can never shadow a fresh one.
type fallbackStore struct {
	primary, secondary TokenStore
}

func (s *fallbackStore) Load(key string) (*TokenSet, error) {
	ts, err := s.primary.Load(key)
	if err != nil {
		slog.Debug("token store: primary load failed, trying fallback", "error", err)
	} else if ts != nil {
		return ts, nil
	}
	return s.secondary.Load(key)
}

func (s *fallbackStore) Save(key string, ts *TokenSet) error {
	err := s.primary.Save(key, ts)
	if err == nil {
		return s.secondary.Delete(key)
	}
	slog.Debug("token store: primary save failed, using fallback", "error", err)
	if err := s.secondary.Save(key, ts); err != nil {
		return err
	}
	_ = s.primary.Delete(key)
	return nil
}

func (s *fallbackStore) Delete(key string) error {
	err := s.secondary.Delete(key)
	if perr := s.primary.Delete(key); perr != nil {
		// A primary store that cannot be reached at all (no keyring daemon)
		// holds nothing we could have saved. Only fail when the entry is
		// demonstrably still there, so a logout never silently leaves tokens.
		if ts, lerr := s.primary.Load(key); lerr == nil && ts != nil {
			err = errors.Join(err, perr)
		}
	}
	return err
}
