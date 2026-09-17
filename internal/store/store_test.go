package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestFileStoreRoundTripAndMode(t *testing.T) {
	dir := t.TempDir()
	f := &FileStore{Path: filepath.Join(dir, "sub", "credentials.json")}
	if _, err := f.Get("default"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	if err := f.Set("default", []byte(`{"refresh_token":"x"}`)); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("other", []byte(`{"refresh_token":"y"}`)); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(f.Path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file must be 0600, got %v", info.Mode().Perm())
	}
	v, err := f.Get("default")
	var got map[string]string
	if err != nil || json.Unmarshal(v, &got) != nil || got["refresh_token"] != "x" {
		t.Fatalf("get: %s %v", v, err)
	}
	if err := f.Delete("default"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get("default"); !errors.Is(err, ErrNotFound) {
		t.Fatal("delete did not remove the entry")
	}
	v, _ = f.Get("other")
	if json.Unmarshal(v, &got) != nil || got["refresh_token"] != "y" {
		t.Fatal("other profile lost")
	}
}

func TestMemoryStore(t *testing.T) {
	m := &Memory{}
	if _, err := m.Get("p"); !errors.Is(err, ErrNotFound) {
		t.Fatal("expected not found")
	}
	_ = m.Set("p", []byte("v"))
	if v, _ := m.Get("p"); string(v) != "v" {
		t.Fatal("bad value")
	}
	_ = m.Delete("p")
	if _, err := m.Get("p"); !errors.Is(err, ErrNotFound) {
		t.Fatal("not deleted")
	}
}

func TestSelectHonoursExplicitKinds(t *testing.T) {
	dir := t.TempDir()
	if Select("file", dir, nil).Name() != "file" || Select("ephemeral", dir, nil).Name() != "ephemeral" || Select("keychain", dir, nil).Name() != "keychain" {
		t.Fatal("explicit kinds must win")
	}
}

// Real keychain round trip (macOS/Linux/Windows); enabled with IUGU_TEST_KEYCHAIN=1 because it may prompt.
func TestKeyringRoundTrip(t *testing.T) {
	if os.Getenv("IUGU_TEST_KEYCHAIN") == "" {
		t.Skip("set IUGU_TEST_KEYCHAIN=1 to exercise the OS keychain")
	}
	k := Keyring{Service: "iugu-cli-test"}
	profile := "iugu-cli-test-profile"
	t.Cleanup(func() { _ = k.Delete(profile) })
	if err := k.Set(profile, []byte(`{"refresh_token":"secret"}`)); err != nil {
		t.Fatal(err)
	}
	v, err := k.Get(profile)
	if err != nil || string(v) != `{"refresh_token":"secret"}` {
		t.Fatalf("get: %s %v", v, err)
	}
	if err := k.Delete(profile); err != nil {
		t.Fatal(err)
	}
	if _, err := k.Get(profile); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found after delete, got %v", err)
	}
}
