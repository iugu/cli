package cli

import (
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iugu-private/platform2-cli/internal/api"
	"github.com/iugu-private/platform2-cli/internal/config"
	"github.com/iugu-private/platform2-cli/internal/output"
)

func (rt *Runtime) appCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "app", Short: "Create, configure, test and publish apps"}
	cmd.AddCommand(rt.appInitCommand(), rt.appListCommand(), rt.appGetCommand(), rt.appUpdateCommand(), rt.appPublishCommand())
	cmd.AddCommand(rt.appInstallCommands()...)
	cmd.AddCommand(rt.appTokenCommand(), rt.appEnvCommand(), rt.appDiscardCommand())
	cmd.AddCommand(rt.appPermissionsCommand(), rt.appOauthCommand(), rt.appCredentialsCommand(), rt.appCertificatesCommand(),
		rt.appIPWhitelistCommand(), rt.appEntitlementsCommand(), rt.appAgreementsCommand(), rt.appImagesCommand(), rt.appDeployTokensCommand())
	return cmd
}

// app init: create (private, draft) → install in the dev workspace → request a workspace-restricted
// credential (Tier 1) → write iugu.toml (+ .env.local once the secret is delivered). No code templates in v1.
func (rt *Runtime) appInitCommand() *cobra.Command {
	var name, workspace, publisher, description, dir string
	var noCredential bool
	opts := approvalOptions{}
	cmd := &cobra.Command{
		Use:   "init --name <name> [--workspace <dev workspace id>]",
		Short: "Create a private draft app, install it in your dev workspace and request its first credential",
		Long: `Creates the app (private, draft) in the publisher workspace, installs it into the development workspace
(own app: immediate), requests a credential restricted to that workspace (Tier 1: a human approves it in the
browser) and writes iugu.toml. Run with --wait --write-env .env.local to block until the approval and get the
secret written locally (0600); without --wait it exits 5 and prints the approval URL and the next step.

It does not scaffold code. It prints (--json) the integration facts an agent needs: issuer, authorize/token/
JWKS/verify/userinfo endpoints, client_id, redirect-URI rules, ACR values, the app tag for implemented actions.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if name == "" {
				return &output.Exit{Code: output.ExitUsage, Message: "--name is required"}
			}
			ws, err := rt.workspaceArg(workspace)
			if err != nil {
				return err
			}
			if publisher == "" {
				publisher = ws
			}
			client, err := rt.apiClient(ctx)
			if err != nil {
				return err
			}
			body := map[string]any{"name": name}
			if description != "" {
				body["description"] = description
			}
			r, err := client.Post(ctx, "/v1/workspaces/"+publisher+"/apps", body, rt.mutating())
			if err != nil {
				return err
			}
			app := r.Body
			appID := api.Str(app, "id")
			rt.Printer.Line("Created app %s (%s) in workspace %s", api.Str(app, "name"), appID, publisher)

			inst, err := client.Post(ctx, "/v1/workspaces/"+ws+"/installations", map[string]any{"app_id": appID}, nil)
			if err != nil && !api.IsCode(err, "validation_failed") {
				return err
			}
			if inst != nil && inst.Status == 201 {
				rt.Printer.Line("Installed in workspace %s", ws)
			}

			project := &config.Project{}
			project.App.ID, project.App.Name, project.App.Tag, project.App.PublisherWorkspace = appID, api.Str(app, "name"), api.Str(app, "tag"), publisher
			project.Development.Workspace = ws
			target := filepath.Join(dir, config.ProjectFile)
			if err := project.Save(target); err != nil {
				return err
			}
			ensureGitignore(dir, ".env.local")
			rt.Printer.Line("Wrote %s", target)

			facts := rt.integrationFacts(ctx, app, ws)
			if noCredential {
				rt.Printer.Result(map[string]any{"app": app, "workspace": ws, "project": target, "integration": facts}, func(w io.Writer) { fmt.Fprintf(w, "App %s ready (no credential requested).\n", appID) })
				return nil
			}

			cred, err := client.Post(ctx, "/v1/apps/"+appID+"/credentials", map[string]any{"name": "dev", "workspace_ids": []string{ws}}, rt.mutating())
			if err != nil {
				return err
			}
			if cred.Status == 202 {
				project.Development.Credential = "pending:" + api.Str(cred.Body, "id")
				_ = project.Save(target)
			}
			if opts.writeEnv == "" && opts.wait {
				opts.writeEnv = filepath.Join(dir, ".env.local")
			}
			opts.projectDir = dir
			human := func(w io.Writer) { fmt.Fprintf(w, "App %s ready.\n", appID) }
			err = rt.handleResponse(ctx, client, cred, opts, human)
			if ex, ok := err.(*output.Exit); ok && ex.Code == output.ExitApproval {
				payload, _ := ex.Payload.(map[string]any)
				payload["app"] = app
				payload["workspace"] = ws
				payload["project"] = target
				payload["integration"] = facts
				payload["next_step"] = fmt.Sprintf("iugu changeset wait %s --write-env .env.local", api.Str(cred.Body, "id"))
				ex.Payload = payload
			}
			return err
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "app name (required)")
	cmd.Flags().StringVar(&workspace, "workspace", "", "development workspace (default: profile / single consented workspace)")
	cmd.Flags().StringVar(&publisher, "publisher", "", "publisher workspace (default: the development workspace)")
	cmd.Flags().StringVar(&description, "description", "", "short description")
	cmd.Flags().StringVar(&dir, "dir", ".", "where to write iugu.toml and .env.local")
	cmd.Flags().BoolVar(&noCredential, "no-credential", false, "skip the credential request")
	addApprovalFlags(cmd.Flags(), &opts)
	return cmd
}

func ensureGitignore(dir, entry string) {
	path := filepath.Join(dir, ".gitignore")
	data, _ := os.ReadFile(path)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == entry {
			return
		}
	}
	content := string(data)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	_ = os.WriteFile(path, []byte(content+entry+"\n"), 0o644)
}

// integrationFacts is what an agent needs to wire any stack to Console without templates.
func (rt *Runtime) integrationFacts(ctx interface{ Done() <-chan struct{} }, app map[string]any, workspace string) map[string]any {
	issuer := ""
	if rt.metadata != nil {
		issuer = strings.TrimRight(rt.metadata.Issuer, "/")
	} else if s, err := rt.loadSession(); err == nil {
		issuer = strings.TrimRight(s.Issuer, "/")
	}
	return map[string]any{
		"issuer":              issuer + "/",
		"authorization_url":   issuer + "/authorize",
		"token_url":           issuer + "/token",
		"jwks_url":            issuer + "/.well-known/jwks.json",
		"verify_url":          issuer + "/verify",
		"userinfo_url":        issuer + "/userinfo",
		"discovery_url":       issuer + "/.well-known/openid-configuration",
		"client_id":           api.Str(app, "id"),
		"app_tag":             api.Str(app, "tag"),
		"workspace_id":        workspace,
		"acr_values":          map[string]string{"informations": "urn:iugu:grant_scopes:informations", "config": "urn:iugu:grant_scopes:config", "transfers": "urn:iugu:grant_scopes:transfers"},
		"redirect_uri_rules":  "register exact https callback URLs with `iugu app oauth set --callbacks …` (Tier 1); the app authenticates with client_id + client_secret at token_url; user tokens carry sub, acr, sid; app tokens use client_credentials",
		"workspace_header":    "send the workspace short id as the `Workspace` header / workspace_id param when calling Console APIs and /verify",
		"implemented_actions": fmt.Sprintf("declare actions your app implements as `%s:<action>` via `iugu app permissions set --implemented …`", api.Str(app, "tag")),
	}
}

func (rt *Runtime) appListCommand() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Apps published by a workspace (publisher view)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, err := rt.workspaceArg(workspace)
			if err != nil {
				return err
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			items, err := client.ListAll(cmd.Context(), "/v1/workspaces/"+ws+"/apps", nil, 500)
			if err != nil {
				return err
			}
			rt.Printer.Result(map[string]any{"data": items}, func(w io.Writer) {
				rows := [][]string{}
				for _, it := range items {
					m, _ := it.(map[string]any)
					rows = append(rows, []string{api.Str(m, "id"), api.Str(m, "name"), api.Str(m, "tag"), api.Str(m, "public"), api.Str(m, "draft"), api.Str(m, "installations")})
				}
				output.Table(w, []string{"ID", "NAME", "TAG", "PUBLIC", "DRAFT", "INSTALLS"}, rows)
			})
			return nil
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	return cmd
}

func (rt *Runtime) appGetCommand() *cobra.Command {
	var appFlag string
	cmd := &cobra.Command{
		Use:   "get [app-id]",
		Short: "Show an app (full view for your own apps)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				appFlag = args[0]
			}
			id, err := rt.appArg(appFlag)
			if err != nil {
				return err
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			r, err := client.Get(cmd.Context(), "/v1/apps/"+id, nil)
			if err != nil {
				return err
			}
			r.Body["etag"] = r.ETag
			rt.Printer.Result(r.Body, func(w io.Writer) {
				fmt.Fprintf(w, "%s  %s  tag=%s public=%s draft=%s\nurl=%s\n", api.Str(r.Body, "id"), api.Str(r.Body, "name"), api.Str(r.Body, "tag"), api.Str(r.Body, "public"), api.Str(r.Body, "draft"), api.Str(r.Body, "url"))
			})
			return nil
		},
	}
	cmd.Flags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")
	return cmd
}

func (rt *Runtime) appUpdateCommand() *cobra.Command {
	var appFlag, name, description, promo, serviceURL, ifMatch string
	var categories []string
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update name, description, promotional text, categories or service API URL (Tier 0)",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(appFlag)
			if err != nil {
				return err
			}
			body := map[string]any{}
			if name != "" {
				body["name"] = name
			}
			if description != "" {
				body["description"] = description
			}
			if promo != "" {
				body["promotional_text"] = promo
			}
			if serviceURL != "" {
				body["service_api_url"] = serviceURL
			}
			if len(categories) > 0 {
				body["categories"] = categories
			}
			if len(body) == 0 {
				return &output.Exit{Code: output.ExitUsage, Message: "nothing to update; pass --name, --description, --promotional-text, --categories or --service-api-url"}
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			r, err := client.Patch(cmd.Context(), "/v1/apps/"+id, body, &api.Options{IfMatch: ifMatch, IdempotencyKey: rt.idempotencyKey})
			if err != nil {
				return err
			}
			rt.Printer.Result(r.Body, func(w io.Writer) { fmt.Fprintf(w, "Updated %s\n", api.Str(r.Body, "name")) })
			return nil
		},
	}
	cmd.Flags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")
	cmd.Flags().StringVar(&name, "name", "", "new name")
	cmd.Flags().StringVar(&description, "description", "", "description")
	cmd.Flags().StringVar(&promo, "promotional-text", "", "promotional text")
	cmd.Flags().StringVar(&serviceURL, "service-api-url", "", "service API URL")
	cmd.Flags().StringSliceVar(&categories, "categories", nil, "store categories (draft is changed by `iugu app publish`)")
	cmd.Flags().StringVar(&ifMatch, "if-match", "", "ETag from `iugu app get` (409 if the app changed meanwhile)")
	return cmd
}

func (rt *Runtime) appPublishCommand() *cobra.Command {
	var appFlag string
	var public, draft, billable string
	opts := approvalOptions{}
	cmd := &cobra.Command{
		Use:   "publish",
		Short: "Publishing decision: leave draft, make public, mark billable (Tier 1 → approval)",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(appFlag)
			if err != nil {
				return err
			}
			body := map[string]any{}
			if public != "" {
				body["public"] = public == "true"
			}
			if draft != "" {
				body["draft"] = draft == "true"
			}
			if billable != "" {
				body["billable"] = billable == "true"
			}
			if len(body) == 0 {
				body["public"], body["draft"] = true, false
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			r, err := client.Patch(cmd.Context(), "/v1/apps/"+id+"/listing", body, rt.mutating())
			if err != nil {
				return err
			}
			return rt.handleResponse(cmd.Context(), client, r, opts, nil)
		},
	}
	cmd.Flags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")
	cmd.Flags().StringVar(&public, "public", "", "true|false")
	cmd.Flags().StringVar(&draft, "draft", "", "true|false (default action: public=true, draft=false)")
	cmd.Flags().StringVar(&billable, "billable", "", "true|false")
	addApprovalFlags(cmd.Flags(), &opts)
	return cmd
}

func (rt *Runtime) appInstallCommands() []*cobra.Command {
	var workspace, appFlag string
	opts := approvalOptions{}
	install := &cobra.Command{
		Use:   "install",
		Short: "Install an app in a workspace (own apps: immediate; third-party: approval)",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(appFlag)
			if err != nil {
				return err
			}
			ws, err := rt.workspaceArg(workspace)
			if err != nil {
				return err
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			r, err := client.Post(cmd.Context(), "/v1/workspaces/"+ws+"/installations", map[string]any{"app_id": id}, rt.mutating())
			if err != nil {
				return err
			}
			return rt.handleResponse(cmd.Context(), client, r, opts, func(w io.Writer) { fmt.Fprintf(w, "Installed (%s)\n", api.Str(r.Body, "id")) })
		},
	}
	install.Flags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")
	install.Flags().StringVar(&workspace, "workspace", "", "target workspace")
	addApprovalFlags(install.Flags(), &opts)

	var installationID string
	uninstall := &cobra.Command{
		Use:   "uninstall --installation <id>",
		Short: "Uninstall an app from a workspace",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.installationID(cmd, installationID, appFlag, workspace)
			if err != nil {
				return err
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			if _, err := client.Delete(cmd.Context(), "/v1/installations/"+id); err != nil {
				return err
			}
			rt.Printer.Result(map[string]any{"uninstalled": id}, func(w io.Writer) { fmt.Fprintln(w, "Uninstalled") })
			return nil
		},
	}
	uninstall.Flags().StringVar(&installationID, "installation", "", "installation id (or --app + --workspace)")
	uninstall.Flags().StringVar(&appFlag, "app", "", "app id")
	uninstall.Flags().StringVar(&workspace, "workspace", "", "workspace id")

	resync := &cobra.Command{
		Use:   "resync --installation <id>",
		Short: "Accept the app's changed permissions in a workspace (own: immediate; third-party: approval)",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.installationID(cmd, installationID, appFlag, workspace)
			if err != nil {
				return err
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			r, err := client.Post(cmd.Context(), "/v1/installations/"+id+"/resync", nil, nil)
			if err != nil {
				return err
			}
			return rt.handleResponse(cmd.Context(), client, r, opts, func(w io.Writer) { fmt.Fprintln(w, "Resynced") })
		},
	}
	resync.Flags().StringVar(&installationID, "installation", "", "installation id (or --app + --workspace)")
	resync.Flags().StringVar(&appFlag, "app", "", "app id")
	resync.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	addApprovalFlags(resync.Flags(), &opts)
	return []*cobra.Command{install, uninstall, resync}
}

// installationID finds the installation of an app in a workspace when only those are known.
func (rt *Runtime) installationID(cmd *cobra.Command, explicit, appFlag, workspace string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	appID, err := rt.appArg(appFlag)
	if err != nil {
		return "", err
	}
	ws, err := rt.workspaceArg(workspace)
	if err != nil {
		return "", err
	}
	client, err := rt.apiClient(cmd.Context())
	if err != nil {
		return "", err
	}
	items, err := client.ListAll(cmd.Context(), "/v1/workspaces/"+ws+"/installations", nil, 1000)
	if err != nil {
		return "", err
	}
	for _, it := range items {
		m, _ := it.(map[string]any)
		if api.Str(m, "app", "id") == appID {
			return api.Str(m, "id"), nil
		}
	}
	return "", &output.Exit{Code: output.ExitUsage, Message: fmt.Sprintf("app %s is not installed in workspace %s", appID, ws)}
}

func (rt *Runtime) appTokenCommand() *cobra.Command {
	var appFlag, audience, scope, execCmd, writeEnv string
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Mint a short-lived Console app token for one of your own apps (test /verify, /userinfo, other apps)",
		Long: `Prints the token unless a sink is given: --exec "curl -H 'Authorization: Bearer {token}' …" runs a command with the
token substituted in-process (also as $IUGU_ACCESS_TOKEN); --write-env FILE stores IUGU_ACCESS_TOKEN (0600). Agents should
prefer a sink so the token never lands in a transcript.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(appFlag)
			if err != nil {
				return err
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			body := map[string]any{}
			if audience != "" {
				body["audience"] = audience
			}
			if scope != "" {
				body["scope"] = scope
			}
			r, err := client.Post(cmd.Context(), "/v1/apps/"+id+"/tokens", body, nil)
			if err != nil {
				return err
			}
			if execCmd == "" && writeEnv == "" {
				rt.Printer.Result(r.Body, func(w io.Writer) { fmt.Fprintln(w, api.Str(r.Body, "access_token")) })
				return nil
			}
			token := api.Str(r.Body, "access_token")
			env := map[string]string{"IUGU_ACCESS_TOKEN": token}
			out := map[string]any{"sub": r.Body["sub"], "aud": r.Body["aud"], "expires_in": r.Body["expires_in"], "token_type": r.Body["token_type"]}
			if writeEnv != "" {
				if err := writeEnvFile(writeEnv, env); err != nil {
					return err
				}
				out["written"] = writeEnv
			}
			if execCmd != "" {
				if err := execWithSecrets(execCmd, env); err != nil {
					return err
				}
				out["executed"] = redactCommand(execCmd)
			}
			rt.Printer.Result(out, func(w io.Writer) {
				if written, ok := out["written"]; ok {
					fmt.Fprintf(w, "Token written to %s (0600)\n", written)
				}
				if executed, ok := out["executed"]; ok {
					fmt.Fprintf(w, "Ran: %s\n", executed)
				}
			})
			return nil
		},
	}
	cmd.Flags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")
	cmd.Flags().StringVar(&audience, "audience", "", "e.g. Iugu.Platform.<app id> of the app to call")
	cmd.Flags().StringVar(&scope, "scope", "", "scope")
	cmd.Flags().StringVar(&execCmd, "exec", "", "run a command with {token} substituted in-process instead of printing the token")
	cmd.Flags().StringVar(&writeEnv, "write-env", "", "write IUGU_ACCESS_TOKEN into this dotenv file (0600) instead of printing the token")
	return cmd
}

func (rt *Runtime) appEnvCommand() *cobra.Command {
	var appFlag, format, workspace string
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Print the non-secret integration environment (dotenv | fly | netlify | vercel | json)",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(appFlag)
			if err != nil {
				return err
			}
			ws, _ := rt.workspaceArg(workspace)
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			r, err := client.Get(cmd.Context(), "/v1/apps/"+id, nil)
			if err != nil {
				return err
			}
			facts := rt.integrationFacts(cmd.Context(), r.Body, ws)
			env := map[string]string{
				"IUGU_ISSUER": facts["issuer"].(string), "IUGU_AUTHORIZE_URL": facts["authorization_url"].(string), "IUGU_TOKEN_URL": facts["token_url"].(string),
				"IUGU_JWKS_URL": facts["jwks_url"].(string), "IUGU_VERIFY_URL": facts["verify_url"].(string), "IUGU_USERINFO_URL": facts["userinfo_url"].(string),
				"IUGU_CLIENT_ID": id, "IUGU_APP_TAG": api.Str(r.Body, "tag"), "IUGU_WORKSPACE_ID": ws,
			}
			if rt.Printer.JSON || format == "json" {
				rt.Printer.Result(map[string]any{"env": env, "integration": facts}, nil)
				return nil
			}
			for _, k := range sortedKeys(env) {
				switch format {
				case "fly":
					fmt.Fprintf(rt.Printer.Out, "fly secrets set %s=%s\n", k, env[k])
				case "netlify":
					fmt.Fprintf(rt.Printer.Out, "netlify env:set %s %s\n", k, env[k])
				case "vercel":
					fmt.Fprintf(rt.Printer.Out, "printf %%s %s | vercel env add %s production\n", env[k], k)
				default:
					fmt.Fprintf(rt.Printer.Out, "%s=%s\n", k, env[k])
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id for IUGU_WORKSPACE_ID")
	cmd.Flags().StringVar(&format, "format", "dotenv", "dotenv | fly | netlify | vercel | json")
	return cmd
}

func (rt *Runtime) appDiscardCommand() *cobra.Command {
	var appFlag string
	opts := approvalOptions{}
	cmd := &cobra.Command{
		Use:   "discard",
		Short: "Discard (soft-delete) an app — approval required; refused for billable apps",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(appFlag)
			if err != nil {
				return err
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			r, err := client.Delete(cmd.Context(), "/v1/apps/"+id)
			if err != nil {
				return err
			}
			return rt.handleResponse(cmd.Context(), client, r, opts, nil)
		},
	}
	cmd.Flags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")
	addApprovalFlags(cmd.Flags(), &opts)
	return cmd
}

var _ = url.Values{}
