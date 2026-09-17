// Package agentsetup writes the remote-MCP entry and the iugu skill into the configuration of each AI
// coding harness found on the machine (plan §8.7). It configures; it never runs a server. All paths
// are relative to $HOME, so tests (and cautious users) can point it at a temporary home.
package agentsetup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// Harness is one supported AI coding tool.
type Harness struct {
	Name     string
	Config   string // config file relative to home
	Detect   []string
	SkillDir string // where SKILL.md goes, relative to home ("" = none)
}

var Harnesses = []Harness{
	{Name: "claude", Config: ".claude.json", Detect: []string{".claude", ".claude.json"}, SkillDir: ".claude/skills/iugu"},
	{Name: "codex", Config: ".codex/config.toml", Detect: []string{".codex"}, SkillDir: ".codex/skills/iugu"},
	{Name: "opencode", Config: ".config/opencode/opencode.json", Detect: []string{".config/opencode"}, SkillDir: ".config/opencode/skills/iugu"},
	{Name: "cursor", Config: ".cursor/mcp.json", Detect: []string{".cursor"}, SkillDir: ""},
	{Name: "vscode", Config: "Library/Application Support/Code/User/mcp.json", Detect: []string{"Library/Application Support/Code/User"}, SkillDir: ""},
}

// Result reports what happened to one harness.
type Result struct {
	Harness  string `json:"harness"`
	Config   string `json:"config"`
	Skill    string `json:"skill,omitempty"`
	Action   string `json:"action"` // written | unchanged | skipped
	Detected bool   `json:"detected"`
	Note     string `json:"note,omitempty"`
}

// Setup writes the MCP entry (`iugu` → mcpURL) and the skill for the selected harnesses ("all" = detected ones).
func Setup(home, mcpURL, skill string, selected []string, force bool) ([]Result, error) {
	var results []Result
	for _, h := range Harnesses {
		if !wanted(h.Name, selected) {
			continue
		}
		detected := detect(home, h)
		if !detected && !force && contains(selected, "all") {
			results = append(results, Result{Harness: h.Name, Config: filepath.Join(home, h.Config), Action: "skipped", Detected: false, Note: "not installed here (use --" + h.Name + " to force)"})
			continue
		}
		path := filepath.Join(home, h.Config)
		changed, err := writeConfig(h.Name, path, mcpURL)
		if err != nil {
			return results, fmt.Errorf("%s: %w", h.Name, err)
		}
		res := Result{Harness: h.Name, Config: path, Detected: detected, Action: "unchanged"}
		if changed {
			res.Action = "written"
		}
		if h.SkillDir != "" && skill != "" {
			skillPath := filepath.Join(home, h.SkillDir, "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(skillPath), 0o755); err != nil {
				return results, err
			}
			if err := os.WriteFile(skillPath, []byte(skill), 0o644); err != nil {
				return results, err
			}
			res.Skill = skillPath
		}
		results = append(results, res)
	}
	return results, nil
}

func wanted(name string, selected []string) bool {
	return contains(selected, "all") || contains(selected, name)
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func detect(home string, h Harness) bool {
	for _, p := range h.Detect {
		if _, err := os.Stat(filepath.Join(home, p)); err == nil {
			return true
		}
	}
	return false
}

func writeConfig(harness, path, mcpURL string) (bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	switch harness {
	case "codex":
		return upsertCodex(path, mcpURL)
	default:
		return upsertJSON(harness, path, mcpURL)
	}
}

// upsertJSON merges the iugu entry into the JSON config, preserving everything else.
func upsertJSON(harness, path, mcpURL string) (bool, error) {
	root := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &root); err != nil {
			return false, fmt.Errorf("%s is not valid JSON; not touching it: %w", path, err)
		}
	}
	var key string
	var entry map[string]any
	switch harness {
	case "opencode":
		key = "mcp"
		entry = map[string]any{"type": "remote", "url": mcpURL, "enabled": true}
	case "vscode":
		key = "servers"
		entry = map[string]any{"type": "http", "url": mcpURL}
	default: // claude, cursor
		key = "mcpServers"
		entry = map[string]any{"type": "http", "url": mcpURL}
		if harness == "cursor" {
			entry = map[string]any{"url": mcpURL}
		}
	}
	section, _ := root[key].(map[string]any)
	if section == nil {
		section = map[string]any{}
	}
	if existing, ok := section["iugu"].(map[string]any); ok && equalJSON(existing, entry) {
		return false, nil
	}
	section["iugu"] = entry
	root[key] = section
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(path, append(out, '\n'), 0o600)
}

func upsertCodex(path, mcpURL string) (bool, error) {
	root := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := toml.Unmarshal(data, &root); err != nil {
			return false, fmt.Errorf("%s is not valid TOML; not touching it: %w", path, err)
		}
	}
	servers, _ := root["mcp_servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	entry := map[string]any{"url": mcpURL, "auth": "oauth"}
	if existing, ok := servers["iugu"].(map[string]any); ok && equalJSON(existing, entry) {
		return false, nil
	}
	servers["iugu"] = entry
	root["mcp_servers"] = servers
	out, err := toml.Marshal(root)
	if err != nil {
		return false, err
	}
	return true, os.WriteFile(path, out, 0o600)
}

func equalJSON(a, b map[string]any) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

// Snippets returns copy-paste instructions for harnesses (used by `--print` and the docs).
func Snippets(mcpURL string) map[string]string {
	return map[string]string{
		"claude":   fmt.Sprintf("claude mcp add --transport http iugu %s", mcpURL),
		"codex":    fmt.Sprintf("[mcp_servers.iugu]\nurl = %q\nauth = \"oauth\"", mcpURL),
		"opencode": fmt.Sprintf(`{"mcp": {"iugu": {"type": "remote", "url": %q, "enabled": true}}}`, mcpURL),
		"cursor":   fmt.Sprintf(`{"mcpServers": {"iugu": {"url": %q}}}`, mcpURL),
		"vscode":   fmt.Sprintf(`{"servers": {"iugu": {"type": "http", "url": %q}}}`, mcpURL),
		"gemini":   fmt.Sprintf(`{"mcpServers": {"iugu": {"httpUrl": %q}}}`, mcpURL),
	}
}
