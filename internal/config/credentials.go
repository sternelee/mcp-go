package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo"
	"github.com/mark3labs/mcp-go/client/transport"
)

// backend is where a CredentialStore keeps its secrets.
type backend interface {
	get(key string) ([]byte, bool, error)
	set(key string, value []byte) error
	delete(key string) error
}

// CredentialStore keeps the secrets the gateway needs: OAuth tokens, and the
// client credentials dynamic registration handed out. Values live in the
// system's credential store, and in files only where there is none.
type CredentialStore struct {
	backend backend
}

// NewCredentialStore returns a store backed by the operating system's
// credential store, with a fallback directory for systems that have none.
func NewCredentialStore() *CredentialStore {
	return &CredentialStore{backend: newKeyringBackend()}
}

// NewMemoryCredentialStore returns a store that keeps everything in this
// process. Tests use it so they never reach the user's real keychain.
func NewMemoryCredentialStore() *CredentialStore {
	return &CredentialStore{backend: memoryBackend{values: map[string][]byte{}}}
}

// TokenKey names the stored token of a server.
func TokenKey(serverID string) string { return "oauth-token/" + serverID }

// clientKey names the stored client credentials of a server.
func clientKey(serverID string) string { return "oauth-client/" + serverID }

// UpstreamTokenKey names the stored static token of a server.
func UpstreamTokenKey(serverID string) string { return "upstream-token/" + serverID }

// inboundTokenKey names the bearer token callers of the gateway must present.
const inboundTokenKey = "inbound-token"

// UpstreamToken returns the static token saved for a server, or "".
func (c *CredentialStore) UpstreamToken(serverID string) string {
	return c.text(UpstreamTokenKey(serverID))
}

// HasUpstreamToken reports whether a server has a static token, without
// revealing it.
func (c *CredentialStore) HasUpstreamToken(serverID string) bool {
	return c.UpstreamToken(serverID) != ""
}

// SetUpstreamToken saves a server's static token, or forgets it when empty.
func (c *CredentialStore) SetUpstreamToken(serverID, token string) error {
	return c.setSecret(UpstreamTokenKey(serverID), token)
}

// InboundToken returns the bearer token callers of the gateway must present, or
// "" when the gateway asks for none.
func (c *CredentialStore) InboundToken() string { return c.text(inboundTokenKey) }

// SetInboundToken saves the token callers must present, or clears it.
func (c *CredentialStore) SetInboundToken(token string) error {
	return c.setSecret(inboundTokenKey, token)
}

// text reads a secret that is kept as plain text.
func (c *CredentialStore) text(key string) string {
	value, ok, err := c.backend.get(key)
	if err != nil || !ok {
		return ""
	}
	return string(value)
}

// setSecret writes a plain-text secret, deleting it when it is empty. It is
// what keeps a secret out of the configuration file.
func (c *CredentialStore) setSecret(key, value string) error {
	if strings.TrimSpace(value) == "" {
		return c.backend.delete(key)
	}
	return c.backend.set(key, []byte(value))
}

// ClientCredentials is what dynamic client registration returned, kept so a
// server is registered once rather than at every sign-in.
type ClientCredentials struct {
	ClientID   string `json:"client_id"`
	Secret     string `json:"client_secret,omitempty"`
	Issuer     string `json:"issuer,omitempty"`
	AuthMethod string `json:"auth_method,omitempty"`
}

// Client returns the saved client credentials of a server, or an empty value.
func (c *CredentialStore) Client(serverID string) ClientCredentials {
	var cc ClientCredentials
	value, ok, err := c.backend.get(clientKey(serverID))
	if err != nil || !ok {
		return cc
	}
	_ = json.Unmarshal(value, &cc)
	return cc
}

// SaveClient stores the client credentials of a server.
func (c *CredentialStore) SaveClient(serverID string, cc ClientCredentials) error {
	value, err := json.Marshal(cc)
	if err != nil {
		return err
	}
	return c.backend.set(clientKey(serverID), value)
}

// SignedIn reports whether a server has a token that has not expired. It reads
// the credential store, so it belongs off the interface thread.
func (c *CredentialStore) SignedIn(serverID string) bool {
	value, ok, err := c.backend.get(TokenKey(serverID))
	if err != nil || !ok {
		return false
	}
	var token transport.Token
	if err := json.Unmarshal(value, &token); err != nil {
		return false
	}
	return token.AccessToken != "" && !token.IsExpired()
}

// SignOut forgets a server's token, keeping its registered client.
func (c *CredentialStore) SignOut(serverID string) error {
	return c.backend.delete(TokenKey(serverID))
}

// TokenStore returns the transport.TokenStore the OAuth handler of a server
// reads and writes.
func (c *CredentialStore) TokenStore(serverID string) transport.TokenStore {
	return tokenStore{store: c, key: TokenKey(serverID)}
}

// tokenStore adapts CredentialStore to transport.TokenStore.
type tokenStore struct {
	store *CredentialStore
	key   string
}

func (s tokenStore) GetToken(ctx context.Context) (*transport.Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	value, ok, err := s.store.backend.get(s.key)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.key, err)
	}
	if !ok {
		return nil, transport.ErrNoToken
	}
	var token transport.Token
	if err := json.Unmarshal(value, &token); err != nil {
		return nil, fmt.Errorf("read %s: %w", s.key, err)
	}
	if token.AccessToken == "" {
		return nil, transport.ErrNoToken
	}
	return &token, nil
}

func (s tokenStore) SaveToken(ctx context.Context, token *transport.Token) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	value, err := json.Marshal(token)
	if err != nil {
		return err
	}
	return s.store.backend.set(s.key, value)
}

// keyringBackend keeps secrets in the system's credential store, falling back
// to files where there is none.
type keyringBackend struct {
	dir string
}

func newKeyringBackend() *keyringBackend {
	dir, _ := mygo.App.Path(mygo.PathUserData)
	if dir != "" {
		dir = filepath.Join(dir, "credentials")
	}
	return &keyringBackend{dir: dir}
}

func (b *keyringBackend) get(key string) ([]byte, bool, error) {
	value, err := mygo.Secrets.Get(key)
	switch {
	case err == nil:
		return value, true, nil
	case errors.Is(err, mygo.ErrSecretNotFound):
		// The credential store answered and has no such secret. A file left by
		// a run on a system without a credential store still counts.
		return b.readFile(key)
	case errors.Is(err, errors.ErrUnsupported):
		return b.readFile(key)
	default:
		return nil, false, err
	}
}

func (b *keyringBackend) set(key string, value []byte) error {
	if len(value) <= mygo.MaxSecretSize {
		err := mygo.Secrets.Set(key, value)
		switch {
		case err == nil:
			// A value that fits leaves no stale file behind.
			b.removeFile(key)
			return nil
		case errors.Is(err, errors.ErrUnsupported):
			// Fall through to the file.
		default:
			return err
		}
	} else {
		// Too large for the credential store. Clear whatever is there, or an
		// older, smaller value would be read back in place of this one.
		if err := mygo.Secrets.Delete(key); err != nil && !errors.Is(err, errors.ErrUnsupported) {
			return err
		}
	}
	return b.writeFile(key, value)
}

func (b *keyringBackend) delete(key string) error {
	err := mygo.Secrets.Delete(key)
	if err != nil && !errors.Is(err, mygo.ErrSecretNotFound) && !errors.Is(err, errors.ErrUnsupported) {
		return err
	}
	b.removeFile(key)
	return nil
}

func (b *keyringBackend) filePath(key string) (string, error) {
	if b.dir == "" {
		return "", errors.New("no credential directory")
	}
	// The key names a subdirectory so the file name stays readable.
	return filepath.Join(b.dir, filepath.FromSlash(key)+".json"), nil
}

func (b *keyringBackend) readFile(key string) ([]byte, bool, error) {
	path, err := b.filePath(key)
	if err != nil {
		return nil, false, nil
	}
	value, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return value, true, nil
}

func (b *keyringBackend) writeFile(key string, value []byte) error {
	path, err := b.filePath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, value, 0o600)
}

func (b *keyringBackend) removeFile(key string) {
	if path, err := b.filePath(key); err == nil {
		_ = os.Remove(path)
	}
}

// memoryBackend keeps secrets for the life of the process.
type memoryBackend struct {
	values map[string][]byte
}

func (b memoryBackend) get(key string) ([]byte, bool, error) {
	value, ok := b.values[key]
	return value, ok, nil
}

func (b memoryBackend) set(key string, value []byte) error {
	b.values[key] = value
	return nil
}

func (b memoryBackend) delete(key string) error {
	delete(b.values, key)
	return nil
}
