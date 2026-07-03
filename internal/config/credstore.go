package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/zalando/go-keyring"
	"sigs.k8s.io/yaml"
)

// ErrNoToken indicates no credential is stored for the server.
var ErrNoToken = errors.New("no stored token")

const keyringService = "rancher-cli"

// CredStore persists per-server Rancher tokens.
type CredStore interface {
	GetToken(server string) (string, error)
	SetToken(server, token string) error
	DeleteToken(server string) error
}

// fileStore keeps tokens in a 0600 YAML map file.
type fileStore struct {
	path string
	mu   sync.Mutex
}

func NewFileStore(dir string) CredStore {
	return &fileStore{path: filepath.Join(dir, "credentials.yaml")}
}

func (f *fileStore) read() (map[string]string, error) {
	m := map[string]string{}
	b, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (f *fileStore) write(m map[string]string) error {
	b, err := yaml.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWriteFile(f.path, b, 0600)
}

func (f *fileStore) GetToken(server string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.read()
	if err != nil {
		return "", err
	}
	tok, ok := m[server]
	if !ok {
		return "", ErrNoToken
	}
	return tok, nil
}

func (f *fileStore) SetToken(server, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.read()
	if err != nil {
		return err
	}
	m[server] = token
	return f.write(m)
}

func (f *fileStore) DeleteToken(server string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.read()
	if err != nil {
		return err
	}
	delete(m, server)
	return f.write(m)
}

// keyringStore uses the OS keychain via go-keyring.
type keyringStore struct{}

func NewKeyringStore() CredStore { return &keyringStore{} }

func (keyringStore) GetToken(server string) (string, error) {
	tok, err := keyring.Get(keyringService, server)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrNoToken
	}
	return tok, err
}

func (keyringStore) SetToken(server, token string) error {
	return keyring.Set(keyringService, server, token)
}

func (keyringStore) DeleteToken(server string) error {
	err := keyring.Delete(keyringService, server)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

// OpenStore selects a store by policy (keychain|file|auto), honoring
// RANCHER_CREDENTIAL_STORE, falling back file-ward when the keyring is unusable.
func OpenStore(policy, dir string) CredStore {
	if env := os.Getenv("RANCHER_CREDENTIAL_STORE"); env != "" {
		policy = env
	}
	switch policy {
	case "file":
		return NewFileStore(dir)
	case "keychain":
		return NewKeyringStore()
	default: // "" or "auto"
		if keyringUsable() {
			return NewKeyringStore()
		}
		return NewFileStore(dir)
	}
}

// keyringUsable probes the keyring with a harmless set/delete of a marker key.
func keyringUsable() bool {
	const probe = "__rancher_cli_probe__"
	if err := keyring.Set(keyringService, probe, "1"); err != nil {
		return false
	}
	_ = keyring.Delete(keyringService, probe)
	return true
}
