// Package config resolves where the CLI talks to and as whom: the global config file with profiles
// (~/.config/iugu/config.json, or $IUGU_CONFIG_DIR), environment overrides, and the per-project
// iugu.toml that pins the app and the development workspace.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// DefaultClientID is the first-party `iugu` CLI client seeded by Console (`OauthClient.ensure_first_party!`);
// the short id of the fixed UUID in config/iugu.yml. Override with IUGU_CLIENT_ID for other environments.
const DefaultClientID = "5KRabYLSLO1ohVrMQSsAUF"

// DefaultAPI is the production Lifecycle API; local worktrees use https://api.console.<slug>.iugu.test.
const DefaultAPI = "https://api.console.iugu.com"

const (
	EnvConfigDir   = "IUGU_CONFIG_DIR"
	EnvProfile     = "IUGU_PROFILE"
	EnvAPI         = "IUGU_API"
	EnvClientID    = "IUGU_CLIENT_ID"
	EnvToken       = "IUGU_TOKEN" // static bearer (deploy token) — wins over stored logins, never written to disk
	EnvStore       = "IUGU_CREDENTIALS_STORE"
	EnvLogSanitize = "IUGU_LOG_SANITIZE"
)

// Profile is one login target (API host + preferred workspace); credentials live in the store, not here.
type Profile struct {
	API       string `json:"api"`
	ClientID  string `json:"client_id,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}

// File is the global config file.
type File struct {
	DefaultProfile string             `json:"default_profile"`
	Profiles       map[string]Profile `json:"profiles"`
}

// Dir returns the config directory, creating it with 0700 when missing.
func Dir() (string, error) {
	dir := os.Getenv(EnvConfigDir)
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(home, ".config", "iugu")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

func path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the global config; a missing file yields an empty one.
func Load() (*File, error) {
	p, err := path()
	if err != nil {
		return nil, err
	}
	f := &File{DefaultProfile: "default", Profiles: map[string]Profile{}}
	data, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, f); err != nil {
		return nil, fmt.Errorf("config %s is not valid JSON: %w", p, err)
	}
	if f.Profiles == nil {
		f.Profiles = map[string]Profile{}
	}
	if f.DefaultProfile == "" {
		f.DefaultProfile = "default"
	}
	return f, nil
}

// Save writes the global config with 0600.
func (f *File) Save() error {
	p, err := path()
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0o600)
}

// ProfileName resolves the active profile: flag > IUGU_PROFILE > default_profile.
func (f *File) ProfileName(flag string) string {
	if flag != "" {
		return flag
	}
	if env := os.Getenv(EnvProfile); env != "" {
		return env
	}
	return f.DefaultProfile
}

// Resolve returns the effective profile after environment overrides (IUGU_API, IUGU_CLIENT_ID).
func (f *File) Resolve(name, apiFlag string) Profile {
	p := f.Profiles[name]
	if apiFlag != "" {
		p.API = apiFlag
	} else if env := os.Getenv(EnvAPI); env != "" {
		p.API = env
	}
	if p.API == "" {
		p.API = DefaultAPI
	}
	p.API = strings.TrimRight(p.API, "/")
	if env := os.Getenv(EnvClientID); env != "" {
		p.ClientID = env
	}
	if p.ClientID == "" {
		p.ClientID = DefaultClientID
	}
	return p
}

// Project is the committed, non-secret project context (iugu.toml).
type Project struct {
	App struct {
		ID                 string `toml:"id"`
		Name               string `toml:"name"`
		Tag                string `toml:"tag,omitempty"`
		PublisherWorkspace string `toml:"publisher_workspace"`
	} `toml:"app"`
	Development struct {
		Workspace  string `toml:"workspace"`
		Credential string `toml:"credential,omitempty"`
	} `toml:"development"`
	Production struct {
		Workspace  string `toml:"workspace,omitempty"`
		Credential string `toml:"credential,omitempty"`
	} `toml:"production,omitempty"`
	Path string `toml:"-"`
}

const ProjectFile = "iugu.toml"

// FindProject walks up from dir looking for iugu.toml; ok=false when there is none.
func FindProject(dir string) (*Project, bool, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, false, err
	}
	for {
		p := filepath.Join(dir, ProjectFile)
		if data, err := os.ReadFile(p); err == nil {
			var project Project
			if err := toml.Unmarshal(data, &project); err != nil {
				return nil, false, fmt.Errorf("%s: %w", p, err)
			}
			project.Path = p
			return &project, true, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, false, nil
		}
		dir = parent
	}
}

// Save writes iugu.toml (0644: it is meant to be committed; it holds no secrets).
func (p *Project) Save(path string) error {
	data, err := toml.Marshal(p)
	if err != nil {
		return err
	}
	header := "# iugu project context — committed, no secrets (secrets live in .env.local, written by `iugu app init` / `iugu changeset wait --write-env`).\n"
	if err := os.WriteFile(path, append([]byte(header), data...), 0o644); err != nil {
		return err
	}
	p.Path = path
	return nil
}
