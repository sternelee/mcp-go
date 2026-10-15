package gateway

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/egoist/mygo"
)

// AccessTokenTTL is how long a token the gateway issues stays good for.
const AccessTokenTTL = 12 * time.Hour

// RefreshTokenTTL is how long a client may renew its access token for.
const RefreshTokenTTL = 30 * 24 * time.Hour

// tokenRecord describes one issued token.
type tokenRecord struct {
	ClientID  string    `json:"client_id"`
	Scope     string    `json:"scope,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

// clientRecord is a client that registered itself with the gateway.
type clientRecord struct {
	RedirectURIs []string  `json:"redirect_uris"`
	RegisteredAt time.Time `json:"registered_at"`
}

// tokenFile is how the issued tokens are written down.
type tokenFile struct {
	// Access and Refresh are keyed by the SHA-256 of the token, so the file by
	// itself does not grant access to the gateway.
	Access  map[string]tokenRecord `json:"access"`
	Refresh map[string]tokenRecord `json:"refresh"`
	// Clients are keyed by the client identifier handed out at registration.
	Clients map[string]clientRecord `json:"clients,omitempty"`
}

// issuedTokens keeps the tokens the gateway has handed to callers, across
// restarts so a client does not have to sign in again every launch. It also
// keeps the clients that registered, and the codes awaiting exchange.
type issuedTokens struct {
	mu      sync.Mutex
	path    string
	access  map[string]tokenRecord
	refresh map[string]tokenRecord
	clients map[string]clientRecord
	codes   map[string]authCode
}

// newIssuedTokens opens the token store in the application's data directory.
func newIssuedTokens() *issuedTokens {
	store := &issuedTokens{
		access:  map[string]tokenRecord{},
		refresh: map[string]tokenRecord{},
		clients: map[string]clientRecord{},
		codes:   map[string]authCode{},
	}
	if dir, err := mygo.App.Path(mygo.PathUserData); err == nil {
		store.path = filepath.Join(dir, "gateway-tokens.json")
		store.load()
	}
	return store
}

// load reads the stored tokens, ignoring a file that cannot be read: losing
// them only means the clients sign in again.
func (s *issuedTokens) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var file tokenFile
	if err := json.Unmarshal(data, &file); err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if file.Access != nil {
		s.access = file.Access
	}
	if file.Refresh != nil {
		s.refresh = file.Refresh
	}
	if file.Clients != nil {
		s.clients = file.Clients
	}
	s.pruneLocked()
}

// save writes the tokens, dropping any that have expired. The caller holds the
// lock.
func (s *issuedTokens) saveLocked() {
	if s.path == "" {
		return
	}
	file := tokenFile{Access: s.access, Refresh: s.refresh, Clients: s.clients}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), "tokens-*.json")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		return
	}
	_ = os.Rename(tmp.Name(), s.path)
}

// pruneLocked forgets the tokens that have expired. The caller holds the lock.
func (s *issuedTokens) pruneLocked() {
	now := time.Now()
	for hash, record := range s.access {
		if now.After(record.ExpiresAt) {
			delete(s.access, hash)
		}
	}
	for hash, record := range s.refresh {
		if now.After(record.ExpiresAt) {
			delete(s.refresh, hash)
		}
	}
}

// issue mints an access token and a refresh token for a client.
func (s *issuedTokens) issue(clientID, scope string) (access, refresh string, expiresAt time.Time, err error) {
	access, err = randomToken()
	if err != nil {
		return "", "", time.Time{}, err
	}
	refresh, err = randomToken()
	if err != nil {
		return "", "", time.Time{}, err
	}
	now := time.Now()
	expiresAt = now.Add(AccessTokenTTL)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.access[hashToken(access)] = tokenRecord{ClientID: clientID, Scope: scope, ExpiresAt: expiresAt}
	s.refresh[hashToken(refresh)] = tokenRecord{ClientID: clientID, Scope: scope, ExpiresAt: now.Add(RefreshTokenTTL)}
	s.saveLocked()
	return access, refresh, expiresAt, nil
}

// renew mints a fresh access token for a refresh token that is still good.
func (s *issuedTokens) renew(refresh, clientID string) (access string, expiresAt time.Time, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()

	record, ok := s.refresh[hashToken(refresh)]
	if !ok || record.ClientID != clientID {
		return "", time.Time{}, errors.New("invalid_grant: the refresh token is not valid for this client")
	}
	if access, err = randomToken(); err != nil {
		return "", time.Time{}, err
	}
	expiresAt = time.Now().Add(AccessTokenTTL)
	s.access[hashToken(access)] = tokenRecord{ClientID: clientID, Scope: record.Scope, ExpiresAt: expiresAt}
	s.saveLocked()
	return access, expiresAt, nil
}

// accepts reports whether a presented token is one the gateway issued and is
// still good for.
func (s *issuedTokens) accepts(token string) bool {
	if token == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.access[hashToken(token)]
	if !ok {
		return false
	}
	if time.Now().After(record.ExpiresAt) {
		delete(s.access, hashToken(token))
		s.saveLocked()
		return false
	}
	return true
}

// count reports how many callers currently hold a token.
func (s *issuedTokens) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	return len(s.access)
}

// revokeAll forgets every issued token, so callers have to sign in again.
func (s *issuedTokens) revokeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.access = map[string]tokenRecord{}
	s.refresh = map[string]tokenRecord{}
	s.saveLocked()
}

// hashToken is what the token store is keyed by.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// registerClient records a client and hands it an identifier. Clients are
// public, so the identifier is all they get.
func (s *issuedTokens) registerClient(redirectURIs []string) (string, error) {
	clientID, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[clientID] = clientRecord{RedirectURIs: redirectURIs, RegisteredAt: time.Now()}
	s.saveLocked()
	return clientID, nil
}

// clientRedirects returns the redirect URIs a client registered.
func (s *issuedTokens) clientRedirects(clientID string) ([]string, bool) {
	if clientID == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.clients[clientID]
	if !ok {
		return nil, false
	}
	return record.RedirectURIs, true
}

// newCode issues an authorization code, bound to the client, the redirect URI
// and the PKCE challenge it was asked for.
func (s *issuedTokens) newCode(clientID, redirectURI, challenge, scope string) (string, error) {
	code, err := randomToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneCodesLocked()
	s.codes[code] = authCode{
		clientID:    clientID,
		redirectURI: redirectURI,
		challenge:   challenge,
		scope:       scope,
		expiresAt:   time.Now().Add(authCodeTTL),
	}
	return code, nil
}

// takeCode returns an authorization code and forgets it, so it can be
// exchanged exactly once.
func (s *issuedTokens) takeCode(code string) (authCode, bool) {
	if code == "" {
		return authCode{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneCodesLocked()
	record, ok := s.codes[code]
	if !ok {
		return authCode{}, false
	}
	delete(s.codes, code)
	return record, true
}

// pruneCodesLocked forgets the authorization codes that have expired. The
// caller holds the lock.
func (s *issuedTokens) pruneCodesLocked() {
	now := time.Now()
	for code, record := range s.codes {
		if now.After(record.expiresAt) {
			delete(s.codes, code)
		}
	}
}

// randomToken returns a token with 256 bits behind it.
func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("make a token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
