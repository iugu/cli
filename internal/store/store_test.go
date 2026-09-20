package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
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
	if info, _ := os.Stat(f.Path); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("credentials file must be 0600, got %v", info.Mode().Perm()) // no mode bits on Windows: profile ACLs
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

type failing struct{ err error }

func (f failing) Get(string) ([]byte, error) { return nil, f.err }
func (f failing) Set(string, []byte) error   { return f.err }
func (f failing) Delete(string) error        { return f.err }
func (failing) Name() string                 { return "failing" }

func TestFallbackDegradesToFileOnce(t *testing.T) {
	var warnings []string
	fb := &Fallback{Keychain: failing{errors.New("boom")}, File: &FileStore{Path: filepath.Join(t.TempDir(), "c.json")}, Warn: func(m string) { warnings = append(warnings, m) }}
	if fb.Name() != "keychain" {
		t.Fatal("starts as keychain")
	}
	if err := fb.Set("default", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	v, err := fb.Get("default")
	if err != nil || compact(v) != `{"a":1}` || fb.Name() != "file" || len(warnings) != 1 {
		t.Fatalf("fallback: %v %s %s %v", err, v, fb.Name(), warnings)
	}
	if err := fb.Delete("default"); err != nil {
		t.Fatal(err)
	}
	if _, err := fb.Get("default"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestFallbackReadsFileWhenKeychainEmpty(t *testing.T) {
	file := &FileStore{Path: filepath.Join(t.TempDir(), "c.json")}
	_ = file.Set("default", []byte(`{"from":"file"}`))
	fb := &Fallback{Keychain: &Memory{}, File: file}
	v, err := fb.Get("default")
	if err != nil || compact(v) != `{"from":"file"}` || fb.Name() != "file" {
		t.Fatalf("got %s %v %s", v, err, fb.Name())
	}
	if err := fb.Set("default", []byte(`{"v":2}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := fb.Keychain.Get("default"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a login living in the file must stay in the file")
	}
	if v, _ := file.Get("default"); compact(v) != `{"v":2}` {
		t.Fatalf("file not updated: %s", v)
	}
}

func TestKeyringTimeout(t *testing.T) {
	old := KeyringTimeout
	KeyringTimeout = time.Nanosecond
	defer func() { KeyringTimeout = old }()
	if err := withTimeout(func() error { time.Sleep(20 * time.Millisecond); return nil }); !errors.Is(err, ErrKeyringTimeout) {
		t.Fatalf("expected timeout, got %v", err)
	}
}

func compact(v []byte) string {
	var buf bytes.Buffer
	_ = json.Compact(&buf, v)
	return buf.String()
}

type readableNotWritable struct{ inner Store }

func (r readableNotWritable) Get(p string) ([]byte, error) { return r.inner.Get(p) }
func (r readableNotWritable) Set(string, []byte) error     { return errors.New("exit status 161") }
func (r readableNotWritable) Delete(string) error          { return errors.New("exit status 161") }
func (readableNotWritable) Name() string                   { return "ro-keychain" }

func TestFallbackDoesNotSplitBrainWhenKeychainIsReadOnly(t *testing.T) {
	kc := &Memory{}
	_ = kc.Set("default", []byte(`{"rt":"old"}`))
	file := &FileStore{Path: filepath.Join(t.TempDir(), "c.json")}
	fb := &Fallback{Keychain: readableNotWritable{kc}, File: file}
	if _, err := fb.Get("default"); err != nil {
		t.Fatal(err)
	}
	err := fb.Set("default", []byte(`{"rt":"new"}`))
	if !errors.Is(err, ErrNotWritable) {
		t.Fatalf("expected ErrNotWritable, got %v", err)
	}
	if _, err := file.Get("default"); !errors.Is(err, ErrNotFound) {
		t.Fatal("the file must not receive a second copy of the login")
	}
	if fb.Name() != "keychain" {
		t.Fatalf("must not degrade to file: %s", fb.Name())
	}
	// a keychain that is readable but EMPTY for the profile (no login keychain in $HOME, CI) is a safe
	// place to fall over from: a fresh login goes to the file with a warning
	var warned string
	fresh := &Fallback{Keychain: readableNotWritable{&Memory{}}, File: &FileStore{Path: filepath.Join(t.TempDir(), "c.json")}, Warn: func(m string) { warned = m }}
	if err := fresh.Set("default", []byte(`{"rt":"first"}`)); err != nil {
		t.Fatalf("fresh login should fall back to the file: %v", err)
	}
	if fresh.Name() != "file" || warned == "" {
		t.Fatalf("expected file store with a warning, got %s %q", fresh.Name(), warned)
	}
}

func TestNamespaceIsEmptyForTheDefaultDirAndStableOtherwise(t *testing.T) {
	home, _ := os.UserHomeDir()
	if Namespace(filepath.Join(home, ".config", "iugu")) != "" {
		t.Fatal("default dir must not be namespaced (existing keychain entries keep working)")
	}
	a, b := Namespace("/tmp/project/.iugu"), Namespace("/tmp/other/.iugu")
	if a == "" || len(a) != 8 || a == b || a != Namespace("/tmp/project/.iugu") {
		t.Fatalf("namespace: %q %q", a, b)
	}
	if (Keyring{Namespace: a}).account("default") != "default@"+a || (Keyring{}).account("default") != "default" {
		t.Fatal("account naming")
	}
}

func TestFileStoreReportsNotWritable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory modes do not restrict file creation on Windows")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Skip("cannot make dir read-only")
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()
	f := &FileStore{Path: filepath.Join(dir, "c.json")}
	if err := f.Set("default", []byte(`{}`)); !errors.Is(err, ErrNotWritable) {
		t.Fatalf("expected ErrNotWritable, got %v", err)
	}
}
