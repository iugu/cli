package cli

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iugu/cli/internal/agentsetup"
	"github.com/iugu/cli/internal/output"
)

//go:embed skill/SKILL.md
var skillMarkdown string

//go:embed skill/llms.txt
var llmsText string

//go:embed skill/AGENTS.snippet.md
var agentsSnippet string

func (rt *Runtime) agentCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "agent", Short: "Configure AI coding harnesses to use iugu"}
	var all, printOnly, force bool
	var mcpURL string
	selected := map[string]*bool{}
	setup := &cobra.Command{
		Use:   "setup [--claude] [--codex] [--opencode] [--cursor] [--vscode] [--all]",
		Short: "Write the remote-MCP entry and the iugu skill into each harness's config (no server is run)",
		Long: `Configures Claude Code, Codex, OpenCode, Cursor and VS Code to reach Iugu for AI, the remote MCP server
(https://mcp.console.iugu.com/mcp) and installs the iugu skill (SKILL.md) where the harness reads skills.
Nothing runs locally: the harness authenticates itself with OAuth on first use ("Connect"). Headless harnesses
(claude -p, codex exec) need a prior interactive connection or IUGU_TOKEN (deploy token) as a bearer header.
Files are written under $HOME; set HOME to a scratch directory to preview. --print shows the snippets only.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if mcpURL == "" {
				mcpURL = deriveMCPURL(rt.profile.API)
			}
			if printOnly {
				rt.Printer.Result(map[string]any{"mcp_url": mcpURL, "snippets": agentsetup.Snippets(mcpURL), "agents_md": agentsSnippet}, func(w io.Writer) {
					for name, snippet := range agentsetup.Snippets(mcpURL) {
						fmt.Fprintf(w, "# %s\n%s\n\n", name, snippet)
					}
					fmt.Fprintln(w, agentsSnippet)
				})
				return nil
			}
			var chosen []string
			for name, flag := range selected {
				if *flag {
					chosen = append(chosen, name)
				}
			}
			if all || len(chosen) == 0 {
				chosen = append(chosen, "all")
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			results, err := agentsetup.Setup(home, mcpURL, skillMarkdown, chosen, force || len(chosen) > 0 && !all && !contains(chosen, "all"))
			if err != nil {
				return err
			}
			recommendations := harnessRecommendations(results)
			rt.Printer.Result(map[string]any{"mcp_url": mcpURL, "results": results, "agents_md": agentsSnippet, "recommendations": recommendations}, func(w io.Writer) {
				rows := [][]string{}
				for _, r := range results {
					rows = append(rows, []string{r.Harness, r.Action, r.Config, r.Note})
				}
				output.Table(w, []string{"HARNESS", "ACTION", "CONFIG", "NOTE"}, rows)
				for _, rec := range recommendations {
					fmt.Fprintf(w, "\n%s — %s\n%s\n", rec.Harness, rec.Title, rec.Text)
				}
				fmt.Fprintf(w, "\nAdd to your project's AGENTS.md:\n%s\n", agentsSnippet)
			})
			return nil
		},
	}
	for _, h := range agentsetup.Harnesses {
		selected[h.Name] = setup.Flags().Bool(h.Name, false, "configure "+h.Name)
	}
	setup.Flags().BoolVar(&all, "all", false, "every harness detected on this machine")
	setup.Flags().BoolVar(&printOnly, "print", false, "print the snippets instead of writing files")
	setup.Flags().BoolVar(&force, "force", false, "write even when the harness is not detected")
	setup.Flags().StringVar(&mcpURL, "mcp-url", "", "remote MCP URL (default derived from the API host)")
	cmd.AddCommand(setup)
	return cmd
}

// Recommendation is advice `agent setup` prints but deliberately does not apply: it changes how the
// harness sandboxes every command, which is the user's call.
type Recommendation struct {
	Harness string `json:"harness"`
	Title   string `json:"title"`
	Text    string `json:"text"`
}

func harnessRecommendations(results []agentsetup.Result) []Recommendation {
	var recs []Recommendation
	for _, r := range results {
		if r.Action == "skipped" {
			continue
		}
		switch r.Harness {
		case "codex":
			recs = append(recs, Recommendation{Harness: "codex", Title: "sandbox settings (not written — review first)",
				Text: `Codex runs commands in a sandbox that, by default, has no network and can only write inside the
project. iugu needs outbound HTTPS, and refreshing its login needs a writable credential store. Either:
  • let the agent keep its own login inside the project (no config change):
      IUGU_CONFIG_DIR=.iugu iugu login      # file store, 0600, .iugu/ is git-ignored; one grant per project
  • or, in ~/.codex/config.toml, loosen the sandbox for every command Codex runs:
      [sandbox_workspace_write]
      network_access = true                 # required for any iugu call
      writable_roots = ["~/.config/iugu"]   # lets the default login refresh in place
In interactive Codex the agent can also ask you to run a failing command outside the sandbox.

Iugu for AI tools that change things (credentials, publishing, GIA, elevated app actions) are flagged
destructive, so Codex asks you before calling them — a y/n prompt in interactive Codex, nothing to set up.
Headless runs (codex exec) refuse them before the call reaches Iugu for AI ("approval policy is never"):
run "codex exec --approve-for-me", or add to [mcp_servers.iugu]:
      default_tools_approval_mode = "approve"  # Iugu for AI still asks you to confirm each elevated call on a Console page`})
		case "claude":
			recs = append(recs, Recommendation{Harness: "claude", Title: "permissions",
				Text: `Allow the CLI without prompts by adding "Bash(iugu:*)" to permissions.allow in the project's
.claude/settings.json (the workspace must be trusted once, interactively, before project settings apply).
Background waits (iugu changeset wait) die when a turn ends: the agent re-runs them when resumed.`})
		}
	}
	return recs
}

// deriveMCPURL maps https://api.console.<domain> to https://mcp.console.<domain>/mcp.
func deriveMCPURL(apiBase string) string {
	return strings.Replace(strings.TrimRight(apiBase, "/"), "://api.console.", "://mcp.console.", 1) + "/mcp"
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func (rt *Runtime) docsCommand() *cobra.Command {
	var llms bool
	cmd := &cobra.Command{
		Use:   "docs [topic]",
		Short: "Documentation for agents and humans (topics: golden-path, tiers, secrets, integration, actions, billing, sandboxes, exit-codes; --llms prints llms.txt)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if llms {
				fmt.Fprint(rt.Printer.Out, llmsText)
				return nil
			}
			topic := "all"
			if len(args) > 0 {
				topic = args[0]
			}
			text := skillMarkdown
			if topic != "all" {
				text = section(skillMarkdown, topic)
				if text == "" {
					return &output.Exit{Code: output.ExitUsage, Message: "unknown topic; try golden-path, tiers, secrets, integration, actions, billing, sandboxes, exit-codes"}
				}
			}
			fmt.Fprint(rt.Printer.Out, text)
			return nil
		},
	}
	cmd.Flags().BoolVar(&llms, "llms", false, "print llms.txt (machine-readable index)")
	return cmd
}

// section extracts a `## <topic>` block from the skill by its anchor id.
func section(md, topic string) string {
	lines := strings.Split(md, "\n")
	var out []string
	in := false
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			if in {
				break
			}
			if strings.Contains(strings.ToLower(line), "{#"+topic+"}") {
				in = true
			}
		}
		if in {
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
