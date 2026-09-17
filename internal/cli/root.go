// Package cli wires the commands. Conventions (plan §8.4): `--json` everywhere (stable shapes on
// stdout, diagnostics on stderr); exit codes 0 ok, 1 error, 2 usage/cancelled, 4 login required,
// 5 approval required (payload printed), 6 stale/conflict; help text names the non-interactive
// equivalent of every prompt because the primary reader is an LLM.
package cli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iugu/cli/internal/api"
	"github.com/iugu/cli/internal/auth"
	"github.com/iugu/cli/internal/config"
	"github.com/iugu/cli/internal/output"
	"github.com/iugu/cli/internal/store"
)

// Version, Commit and Date are set by the build (goreleaser -X).
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

func versionString() string {
	v := Version
	if Commit != "" {
		v += " (" + Commit
		if Date != "" {
			v += ", " + Date
		}
		v += ")"
	}
	return v
}

// Runtime is everything a command needs; built once per invocation.
type Runtime struct {
	Printer *output.Printer
	HTTP    *http.Client

	jsonFlag       bool
	jqFlag         string
	apiFlag        string
	profileFlag    string
	storeFlag      string
	yes            bool
	agentFlag      string
	idempotencyKey string
	quiet          bool

	configFile  *config.File
	profileName string
	profile     config.Profile
	store       store.Store

	prm      *auth.ProtectedResource
	metadata *auth.Metadata
	oauth    *auth.Client
	session  *auth.Session
}

// Execute runs the CLI and returns the process exit code.
func Execute(args []string) int {
	rt := &Runtime{HTTP: &http.Client{Timeout: 60 * time.Second}}
	root := rt.rootCommand()
	root.SetArgs(args)
	err := root.Execute()
	if err == nil {
		return output.ExitOK
	}
	return rt.exit(err)
}

func (rt *Runtime) exit(err error) int {
	p := rt.Printer
	if p == nil {
		p = output.New(rt.jsonFlag)
	}
	var ex *output.Exit
	if errors.As(err, &ex) {
		switch {
		case ex.Payload != nil && p.JSON:
			p.Result(ex.Payload, nil)
		case p.JSON: // agents read stdout: a bare message still needs a JSON envelope
			p.Result(map[string]any{"error": map[string]any{"code": exitCodeName(ex.Code), "message": ex.Message}}, nil)
		case ex.Message != "":
			p.Line("%s", ex.Message)
		}
		return ex.Code
	}
	if errors.Is(err, auth.ErrStoreNotWritable) || errors.Is(err, store.ErrNotWritable) {
		msg := "This environment can read the stored login but not update it (sandbox?). The access token expired and the refresh token was NOT rotated, so the login still works outside the sandbox."
		steps := []string{
			"run this command outside the sandbox (or let the harness run it unsandboxed) — the refreshed token is then valid for 30 minutes here too",
			"or give the agent its own login inside the workspace: IUGU_CONFIG_DIR=.iugu iugu login  (file store, 0600; .iugu/ is added to .gitignore), then prefix every iugu command with IUGU_CONFIG_DIR=.iugu",
			"Codex: `sandbox_workspace_write.network_access = true` is required for any iugu call; `writable_roots = [\"~/.config/iugu\"]` lets the file store work in place",
		}
		if p.JSON {
			p.Result(map[string]any{"error": map[string]any{"code": "store_not_writable", "message": msg, "detail": err.Error(), "next_steps": steps}}, nil)
		} else {
			p.Line("%s\n- %s", msg, strings.Join(steps, "\n- "))
		}
		return output.ExitFailure
	}
	if errors.Is(err, auth.ErrLoginRequired) || errors.Is(err, store.ErrNotFound) {
		msg := "Login required. Run `iugu login` (or `iugu login --non-interactive` from an agent and hand the URL to a human)."
		if p.JSON {
			p.Result(map[string]any{"error": map[string]any{"code": "login_required", "message": msg, "next_step": "iugu login"}}, nil)
		} else {
			p.Line("%s", msg)
		}
		return output.ExitAuthRequired
	}
	var apiErr *api.Error
	if errors.As(err, &apiErr) {
		if p.JSON {
			p.Result(map[string]any{"error": apiErr}, nil)
		} else {
			p.Line("API error: %s", apiErr.Error())
			if apiErr.Code == "workspace_not_consented" {
				p.Line("Hint: iugu login --workspace <id> re-consents and merges the workspace into your grant.")
			}
			if apiErr.Code == "insufficient_scope" {
				p.Line("Hint: re-consent with the missing scope, e.g. iugu login --scopes \"%s console:gia.write\"", DefaultScopes)
			}
		}
		switch apiErr.Code {
		case "invalid_token":
			return output.ExitAuthRequired
		case "stale_resource":
			return output.ExitConflict
		}
		return output.ExitFailure
	}
	hint := ""
	if msg := err.Error(); strings.Contains(msg, "operation not permitted") || strings.Contains(msg, "network is unreachable") || strings.Contains(msg, "no such host") {
		hint = "no network from here? sandboxed agents need outbound HTTPS (Codex: sandbox_workspace_write.network_access = true) or the command must run outside the sandbox"
	}
	if p.JSON {
		e := map[string]any{"code": "error", "message": err.Error()}
		if hint != "" {
			e["hint"] = hint
		}
		p.Result(map[string]any{"error": e}, nil)
	} else {
		p.Line("Error: %v", err)
		if hint != "" {
			p.Line("Hint: %s", hint)
		}
	}
	return output.ExitFailure
}

func (rt *Runtime) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "iugu",
		Short:         "iugu Console for developers and their AI agents",
		Long:          "iugu — create, configure, test and publish Platform 2 apps from a terminal or an AI coding session.\nSensitive changes (credentials, certificates, permissions, publishing) become approvals a human completes in the browser.",
		Version:       versionString(),
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			rt.Printer = output.New(rt.jsonFlag || rt.jqFlag != "")
			rt.Printer.JQ = rt.jqFlag
			rt.Printer.Quiet = rt.quiet
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			rt.configFile = cfg
			rt.profileName = cfg.ProfileName(rt.profileFlag)
			rt.profile = cfg.Resolve(rt.profileName, rt.apiFlag)
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			auth.LockDir = dir
			kind := rt.storeFlag
			if kind == "" {
				kind = os.Getenv(config.EnvStore)
			}
			rt.store = store.Select(kind, dir, func(msg string) { rt.Printer.Line("warning: %s", msg) })
			return nil
		},
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&rt.jsonFlag, "json", false, "machine-readable JSON on stdout (agents: always use this)")
	pf.StringVar(&rt.jqFlag, "jq", "", "filter the JSON result with a jq expression (implies --json; strings print raw)")
	pf.StringVar(&rt.apiFlag, "api", "", "Lifecycle API base URL (default "+config.DefaultAPI+"; env IUGU_API)")
	pf.StringVar(&rt.profileFlag, "profile", "", "named login profile (env IUGU_PROFILE)")
	pf.StringVar(&rt.storeFlag, "credentials-store", "", "keychain | file | ephemeral (default: keychain, file fallback; env IUGU_CREDENTIALS_STORE)")
	pf.BoolVar(&rt.yes, "yes", false, "assume yes for confirmations")
	pf.StringVar(&rt.agentFlag, "agent", "auto", "agent mode: yes | no | auto (auto detects CI, Claude Code, Codex, OpenCode, Cursor or a non-TTY)")
	pf.StringVar(&rt.idempotencyKey, "idempotency-key", "", "Idempotency-Key for mutating requests")
	pf.BoolVarP(&rt.quiet, "quiet", "q", false, "suppress diagnostics on stderr")

	root.AddCommand(rt.loginCommand(), rt.logoutCommand(), rt.authCommand(), rt.meCommand(), rt.workspaceCommand(),
		rt.appCommand(), rt.changesetCommand(), rt.approvalsCommand(), rt.verifyCommand(), rt.testPrincipalCommand(),
		rt.giaCommand(), rt.catalogCommand(), rt.agentCommand(), rt.docsCommand())
	return root
}

// IsAgent applies the detection rule from the plan.
func (rt *Runtime) IsAgent() bool {
	switch strings.ToLower(rt.agentFlag) {
	case "yes", "true", "1":
		return true
	case "no", "false", "0":
		return false
	}
	for _, name := range []string{"CI", "CLAUDECODE", "CLAUDE_CODE", "CODEX_CI", "CODEX_SANDBOX", "OPENCODE", "CURSOR_TRACE_ID", "CURSOR_AGENT"} {
		if os.Getenv(name) != "" {
			return true
		}
	}
	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "CLAUDE_CODE_") || strings.HasPrefix(env, "CODEX_") || strings.HasPrefix(env, "OPENCODE") {
			return true
		}
	}
	return !isTerminal(os.Stdin) || !isTerminal(os.Stdout)
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// discover resolves PRM + AS metadata once.
func (rt *Runtime) discover(ctx context.Context) (*auth.Client, error) {
	if rt.oauth != nil {
		return rt.oauth, nil
	}
	prm, md, err := auth.Discover(ctx, rt.HTTP, rt.profile.API)
	if err != nil {
		return nil, err
	}
	rt.prm, rt.metadata = prm, md
	rt.oauth = &auth.Client{HTTP: rt.HTTP, Metadata: md, ClientID: rt.profile.ClientID}
	return rt.oauth, nil
}

// loadSession returns the stored login of the profile.
func (rt *Runtime) loadSession() (*auth.Session, error) {
	if rt.session != nil {
		return rt.session, nil
	}
	s, err := auth.Load(rt.store, rt.profileName)
	if err != nil {
		return nil, err
	}
	rt.session = s
	return s, nil
}

// apiClient builds the /v1 client; IUGU_TOKEN (deploy token) wins over the stored login.
func (rt *Runtime) apiClient(ctx context.Context) (*api.Client, error) {
	client := &api.Client{BaseURL: rt.profile.API, HTTP: rt.HTTP, UserAgent: "iugu-cli/" + Version}
	if token := os.Getenv(config.EnvToken); token != "" {
		client.Token = func(context.Context) (string, error) { return token, nil }
		return client, nil
	}
	session, err := rt.loadSession()
	if err != nil {
		return nil, err
	}
	client.Token = func(ctx context.Context) (string, error) {
		oauth, err := rt.oauthForSession(ctx, session)
		if err != nil {
			return "", err
		}
		return session.Fresh(ctx, oauth, rt.store, rt.profileName)
	}
	return client, nil
}

// oauthForSession builds the token client from the facts stored at login (no network discovery needed
// for a refresh unless the token endpoint is unknown).
func (rt *Runtime) oauthForSession(ctx context.Context, s *auth.Session) (*auth.Client, error) {
	if rt.oauth != nil {
		return rt.oauth, nil
	}
	if s.Issuer != "" {
		issuer := strings.TrimRight(s.Issuer, "/")
		rt.oauth = &auth.Client{HTTP: rt.HTTP, ClientID: s.ClientID, Metadata: &auth.Metadata{
			Issuer: s.Issuer, TokenEndpoint: issuer + "/token", RevocationEndpoint: issuer + "/revoke",
			AuthorizationEndpoint: issuer + "/authorize", DeviceAuthorizationEndpoint: issuer + "/device_authorization",
		}}
		return rt.oauth, nil
	}
	return rt.discover(ctx)
}

// workspaceArg resolves the workspace to act on: flag > iugu.toml development workspace > profile >
// the single consented workspace.
func (rt *Runtime) workspaceArg(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if project, ok, _ := config.FindProject("."); ok && project.Development.Workspace != "" {
		return project.Development.Workspace, nil
	}
	if rt.profile.Workspace != "" {
		return rt.profile.Workspace, nil
	}
	if s, err := rt.loadSession(); err == nil && len(s.Workspaces) == 1 {
		return s.Workspaces[0], nil
	}
	return "", &output.Exit{Code: output.ExitUsage, Message: "which workspace? pass --workspace <id>, run `iugu workspace use <id>`, or run inside a project with iugu.toml"}
}

// appArg resolves the app: positional/flag > iugu.toml.
func (rt *Runtime) appArg(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if project, ok, _ := config.FindProject("."); ok && project.App.ID != "" {
		return project.App.ID, nil
	}
	return "", &output.Exit{Code: output.ExitUsage, Message: "which app? pass --app <id> or run inside a project with iugu.toml (created by `iugu app init`)"}
}

func (rt *Runtime) mutating() *api.Options {
	return &api.Options{IdempotencyKey: rt.idempotencyKey}
}

func short(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func exitCodeName(code int) string {
	switch code {
	case output.ExitUsage:
		return "usage"
	case output.ExitAuthRequired:
		return "login_required"
	case output.ExitApproval:
		return "approval_required"
	case output.ExitConflict:
		return "conflict"
	default:
		return "error"
	}
}
