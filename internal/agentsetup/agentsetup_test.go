package agentsetup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupWritesDetectedHarnessesIdempotently(t *testing.T) {
	home := t.TempDir()
	// Existing configs must be merged, not replaced.
	_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".claude.json"), []byte(`{"theme":"dark","mcpServers":{"other":{"type":"http","url":"https://other/mcp"}}}`), 0o600)
	_ = os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	_ = os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("model = \"gpt-5\"\n\n[mcp_servers.other]\nurl = \"https://other/mcp\"\n"), 0o600)
	_ = os.MkdirAll(filepath.Join(home, ".config", "opencode"), 0o755)

	results, err := Setup(home, "https://mcp.console.iugu.test/mcp", "# skill", []string{"all"}, false)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]string{}
	for _, r := range results {
		actions[r.Harness] = r.Action
	}
	if actions["claude"] != "written" || actions["codex"] != "written" || actions["opencode"] != "written" || actions["cursor"] != "skipped" || actions["vscode"] != "skipped" {
		t.Fatalf("actions: %v", actions)
	}
	var claude map[string]any
	data, _ := os.ReadFile(filepath.Join(home, ".claude.json"))
	_ = json.Unmarshal(data, &claude)
	servers := claude["mcpServers"].(map[string]any)
	if claude["theme"] != "dark" || servers["other"] == nil || servers["iugu"].(map[string]any)["url"] != "https://mcp.console.iugu.test/mcp" {
		t.Fatalf("claude config not merged: %s", data)
	}
	codex, _ := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	if !strings.Contains(string(codex), `model = 'gpt-5'`) && !strings.Contains(string(codex), `model = "gpt-5"`) || !strings.Contains(string(codex), "[mcp_servers.iugu]") || !strings.Contains(string(codex), `auth = 'oauth'`) {
		t.Fatalf("codex config: %s", codex)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "iugu", "SKILL.md")); err != nil {
		t.Fatal("skill not installed for claude")
	}

	again, _ := Setup(home, "https://mcp.console.iugu.test/mcp", "# skill", []string{"all"}, false)
	for _, r := range again {
		if r.Detected && r.Action != "unchanged" {
			t.Fatalf("second run must be a no-op, got %s for %s", r.Action, r.Harness)
		}
	}
	forced, _ := Setup(home, "https://mcp.console.iugu.test/mcp", "", []string{"cursor"}, true)
	if forced[0].Action != "written" {
		t.Fatalf("forced cursor: %+v", forced[0])
	}
	if _, err := Setup(home, "x", "", []string{"claude"}, true); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{not json"), 0o600)
	if _, err := Setup(home, "https://mcp/mcp", "", []string{"claude"}, true); err == nil {
		t.Fatal("corrupt config must not be overwritten")
	}
}
