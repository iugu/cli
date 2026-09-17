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
	"time"

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

// KeyringTimeout bounds every keychain call: on macOS `security` can block forever waiting for a GUI
// prompt (or when $HOME has no login keychain); we would rather fail and fall back to the file store.
var KeyringTimeout = 10 * time.Second

// ErrKeyringTimeout is returned when the OS keychain does not answer within KeyringTimeout.
var ErrKeyringTimeout = errors.New("keychain did not answer (locked or waiting for a prompt?)")

// ErrNotWritable wraps a failed write to a store that can be read: typically a sandbox (Codex
// workspace-write, containers with a mounted $HOME) that lets the CLI see a login but not update it.
// Callers must not rotate a refresh token when they hit it.
var ErrNotWritable = errors.New("credential store is not writable here")

func withTimeout(fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(KeyringTimeout):
		return ErrKeyringTimeout
	}
}

// Keyring uses the OS keychain (macOS Keychain, Secret Service, Windows Credential Manager).
type Keyring struct{ Service string }

func (k Keyring) svc() string {
	if k.Service != "" {
		return k.Service
	}
	return service
}

func (k Keyring) Get(profile string) ([]byte, error) {
	var v string
	err := withTimeout(func() (err error) {
		v, err = keyring.Get(k.svc(), profile)
		return err
	})
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return []byte(v), nil
}

func (k Keyring) Set(profile string, data []byte) error {
	return withTimeout(func() error { return keyring.Set(k.svc(), profile, string(data)) })
}

func (k Keyring) Delete(profile string) error {
	err := withTimeout(func() error { return keyring.Delete(k.svc(), profile) })
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
	if err := f.write(entries); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%w: %v", ErrNotWritable, err)
		}
		return err
	}
	return nil
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

// Fallback tries the keychain first and degrades to the file store when the keychain fails: headless
// Linux, containers, a macOS $HOME without a login keychain, a prompt nobody answers. Reads consult
// both places so a login saved during a degraded run is still found once the keychain is back.
type Fallback struct {
	Keychain Store
	File     *FileStore
	Warn     func(string)
	mu       sync.Mutex
	degraded bool
	fromFile bool // the last read was served by the file (login saved during a degraded run)
}

func (f *Fallback) isDegraded() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.degraded
}

func (f *Fallback) degrade(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.degraded && f.Warn != nil {
		f.Warn(fmt.Sprintf("keychain unavailable (%v); storing the login in %s (0600)", err, f.File.Path))
	}
	f.degraded = true
}

func (f *Fallback) Get(profile string) ([]byte, error) {
	if !f.isDegraded() {
		v, err := f.Keychain.Get(profile)
		if err == nil {
			return v, nil
		}
		if !errors.Is(err, ErrNotFound) {
			f.degrade(err)
		}
	}
	v, err := f.File.Get(profile)
	if err == nil {
		f.mu.Lock()
		f.fromFile = true
		f.mu.Unlock()
	}
	return v, err
}

func (f *Fallback) Set(profile string, data []byte) error {
	f.mu.Lock()
	stayInFile := f.fromFile // a login that lives in the file stays there (no 10 s keychain stall on every refresh)
	f.mu.Unlock()
	if !stayInFile && !f.isDegraded() {
		err := f.Keychain.Set(profile, data)
		if err == nil {
			_ = f.File.Delete(profile) // never keep two copies
			return nil
		}
		// A keychain that answers reads but refuses writes (sandbox) must not make us start a second copy
		// of the login in the file: the two copies would rotate the same refresh token and burn the grant.
		if _, getErr := f.Keychain.Get(profile); getErr == nil || errors.Is(getErr, ErrNotFound) {
			return fmt.Errorf("%w: keychain refused the write (%v)", ErrNotWritable, err)
		}
		f.degrade(err)
	}
	if err := f.File.Set(profile, data); err != nil {
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%w: %v", ErrNotWritable, err)
		}
		return err
	}
	return nil
}

func (f *Fallback) Delete(profile string) error {
	if !f.isDegraded() {
		if err := f.Keychain.Delete(profile); err != nil {
			f.degrade(err)
		}
	}
	return f.File.Delete(profile)
}

func (f *Fallback) Name() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.degraded || f.fromFile {
		return "file"
	}
	return "keychain"
}

// Select picks the store: explicit kind (flag or IUGU_CREDENTIALS_STORE) or keychain with a file
// fallback when the keychain is unavailable. `warn` receives the reason of a fallback.
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
	return &Fallback{Keychain: Keyring{}, File: file, Warn: warn}
}
