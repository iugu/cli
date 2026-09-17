package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProfilesAndOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(EnvConfigDir, dir)
	t.Setenv(EnvAPI, "")
	t.Setenv(EnvProfile, "")
	t.Setenv(EnvClientID, "")
	f, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	p := f.Resolve(f.ProfileName(""), "")
	if p.API != DefaultAPI || p.ClientID != DefaultClientID {
		t.Fatalf("defaults: %+v", p)
	}
	f.Profiles["dev"] = Profile{API: "https://api.console.dev.iugu.test", Workspace: "ws1"}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(filepath.Join(dir, "config.json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatal("config must be 0600")
	}
	f2, _ := Load()
	t.Setenv(EnvProfile, "dev")
	name := f2.ProfileName("")
	p = f2.Resolve(name, "")
	if name != "dev" || p.API != "https://api.console.dev.iugu.test" || p.Workspace != "ws1" {
		t.Fatalf("profile: %s %+v", name, p)
	}
	p = f2.Resolve(name, "https://api.console.flag.iugu.test/")
	if p.API != "https://api.console.flag.iugu.test" {
		t.Fatal("flag must win and trailing slash be trimmed")
	}
	t.Setenv(EnvAPI, "https://api.console.env.iugu.test")
	t.Setenv(EnvClientID, "customclient")
	p = f2.Resolve(name, "")
	if p.API != "https://api.console.env.iugu.test" || p.ClientID != "customclient" {
		t.Fatalf("env overrides: %+v", p)
	}
}

func TestProjectFileWalksUp(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	_ = os.MkdirAll(nested, 0o755)
	if _, ok, err := FindProject(nested); ok || err != nil {
		t.Fatalf("expected no project: %v %v", ok, err)
	}
	p := &Project{}
	p.App.ID, p.App.Name, p.App.PublisherWorkspace = "app1", "Acme", "ws1"
	p.Development.Workspace = "ws1"
	if err := p.Save(filepath.Join(root, ProjectFile)); err != nil {
		t.Fatal(err)
	}
	found, ok, err := FindProject(nested)
	if err != nil || !ok || found.App.ID != "app1" || found.Development.Workspace != "ws1" || found.Path != filepath.Join(root, ProjectFile) {
		t.Fatalf("find: %v %v %+v", ok, err, found)
	}
	data, _ := os.ReadFile(found.Path)
	if !contains(string(data), "no secrets") || !contains(string(data), "[app]") {
		t.Fatalf("toml: %s", data)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
