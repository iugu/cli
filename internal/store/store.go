// Package store keeps the CLI's login (rotating refresh token + session facts) out of agent-readable
// files: OS keychain first, a 0600 file as fallback, memory only when asked (`--credentials-store
// ephemeral`). One entry per profile.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

// ErrNotFound is returned when the profile has no stored login.
var ErrNotFound = errors.New("no stored login")

// Store is a tiny key/value store for secrets.
type Store interface {
	Get(profile string) ([]byte, error)
	Set(profile string, data []byte) error
	Delete(profile string) error
	Name() string
}

const service = "iugu-cli"

// Keyring uses the OS keychain (macOS Keychain, Secret Service, Windows Credential Manager).
type Keyring struct{ Service string }

func (k Keyring) svc() string {
	if k.Service != "" {
		return k.Service
	}
	return service
}

func (k Keyring) Get(profile string) ([]byte, error) {
	v, err := keyring.Get(k.svc(), profile)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return []byte(v), nil
}

func (k Keyring) Set(profile string, data []byte) error {
	return keyring.Set(k.svc(), profile, string(data))
}

func (k Keyring) Delete(profile string) error {
	err := keyring.Delete(k.svc(), profile)
	if errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}

func (Keyring) Name() string { return "keychain" }

// FileStore keeps a JSON object of profile → entry in a 0600 file.
type FileStore struct {
	Path string
	mu   sync.Mutex
}

func (f *FileStore) read() (map[string]json.RawMessage, error) {
	data, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	entries := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("%s is corrupt: %w", f.Path, err)
	}
	return entries, nil
}

func (f *FileStore) write(entries map[string]json.RawMessage) error {
	if err := os.MkdirAll(filepath.Dir(f.Path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(f.Path, data, 0o600)
}

func (f *FileStore) Get(profile string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := f.read()
	if err != nil {
		return nil, err
	}
	raw, ok := entries[profile]
	if !ok {
		return nil, ErrNotFound
	}
	return raw, nil
}

func (f *FileStore) Set(profile string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := f.read()
	if err != nil {
		return err
	}
	entries[profile] = json.RawMessage(data)
	return f.write(entries)
}

func (f *FileStore) Delete(profile string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := f.read()
	if err != nil {
		return err
	}
	delete(entries, profile)
	return f.write(entries)
}

func (*FileStore) Name() string { return "file" }

// Memory keeps entries for the life of the process only.
type Memory struct {
	mu sync.Mutex
	m  map[string][]byte
}

func (m *Memory) Get(profile string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.m[profile]
	if !ok {
		return nil, ErrNotFound
	}
	return v, nil
}

func (m *Memory) Set(profile string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.m == nil {
		m.m = map[string][]byte{}
	}
	m.m[profile] = data
	return nil
}

func (m *Memory) Delete(profile string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.m, profile)
	return nil
}

func (*Memory) Name() string { return "ephemeral" }

// Select picks the store: explicit kind (flag or IUGU_CREDENTIALS_STORE) or keychain with a file
// fallback when the keychain is unavailable (headless Linux, containers). `warn` receives the reason.
func Select(kind, configDir string, warn func(string)) Store {
	file := &FileStore{Path: filepath.Join(configDir, "credentials.json")}
	switch strings.ToLower(kind) {
	case "file":
		return file
	case "ephemeral", "memory":
		return &Memory{}
	case "keychain", "keyring":
		return Keyring{}
	}
	k := Keyring{}
	if _, err := k.Get("__probe__"); err != nil && !errors.Is(err, ErrNotFound) {
		if warn != nil {
			warn(fmt.Sprintf("keychain unavailable (%v); storing the login in %s (0600)", err, file.Path))
		}
		return file
	}
	return k
}
