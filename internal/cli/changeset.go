package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iugu-private/platform2-cli/internal/api"
	"github.com/iugu-private/platform2-cli/internal/config"
	"github.com/iugu-private/platform2-cli/internal/output"
)

func (rt *Runtime) changesetCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "changeset", Short: "Batches of changes that need one human approval", Aliases: []string{"change-set", "cs"}}
	var workspace, title, file string
	var ops []string
	var submit bool
	opts := approvalOptions{}
	create := &cobra.Command{
		Use:   "create --op <type>:<json params> [--op …] | -f ops.json",
		Short: "Prepare a change set (dry run: validation + diff); add --submit to send it for approval",
		Long: `Operations (server-validated; see \x60iugu docs tiers\x60):
  credentials.create {"app_id","name","workspace_ids"}   credentials.rotate/revoke {"app_id","credential_id"}
  oauth.update {"app_id","url","callbacks"}              listing.publish {"app_id","public","draft","billable"}
  certificates.add {"app_id","public_key_pem"}           certificates.revoke {"app_id","certificate_id"}
  ip_whitelist.set {"app_id","ips"}                      apps.discard {"app_id"}
  installations.create {"app_id"}                        installations.resync {"installation_id"}
  deploy_tokens.create {"app_id","name","scope","expires_in"}
Inside a project (iugu.toml) "app_id" defaults to the project's app and the workspace to its development workspace.
Example: iugu changeset create --op 'oauth.update:{"callbacks":["https://x/cb"]}' --op 'listing.publish:{"public":true,"draft":false}' --submit --wait`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, err := rt.workspaceArg(workspace)
			if err != nil {
				return err
			}
			operations, err := parseOps(ops, file)
			if err != nil {
				return err
			}
			if project, ok, _ := config.FindProject("."); ok && project.App.ID != "" {
				fillAppID(operations, project.App.ID) // inside a project, ops default to its app
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			body := map[string]any{"operations": operations}
			if title != "" {
				body["title"] = title
			}
			r, err := client.Post(cmd.Context(), "/v1/workspaces/"+ws+"/change-sets", body, rt.mutating())
			if err != nil {
				return err
			}
			if !submit {
				rt.Printer.Result(r.Body, func(w io.Writer) {
					fmt.Fprintf(w, "Draft %s:\n", api.Str(r.Body, "id"))
					for _, d := range api.List(r.Body, "diff") {
						m, _ := d.(map[string]any)
						fmt.Fprintf(w, "  - %s\n", api.Str(m, "summary"))
					}
					fmt.Fprintf(w, "Submit with: iugu changeset submit %s\n", api.Str(r.Body, "id"))
				})
				return nil
			}
			s, err := client.Post(cmd.Context(), "/v1/change-sets/"+api.Str(r.Body, "id")+"/submit", nil, nil)
			if err != nil {
				return err
			}
			return rt.handleResponse(cmd.Context(), client, s, opts, nil)
		},
	}
	create.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	create.Flags().StringVar(&title, "title", "", "title shown to the approver")
	create.Flags().StringArrayVar(&ops, "op", nil, "operation as type:json (repeatable)")
	create.Flags().StringVarP(&file, "file", "f", "", "JSON file with [{\"op\":…, \"params\":{…}}, …] (or - for stdin)")
	create.Flags().BoolVar(&submit, "submit", false, "submit right away")
	addApprovalFlags(create.Flags(), &opts)

	show := &cobra.Command{Use: "show <id>", Short: "Show a change set", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Get(cmd.Context(), "/v1/change-sets/"+args[0], nil)
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) {
			fmt.Fprintf(w, "%s  %s  %s\n", api.Str(r.Body, "id"), api.Str(r.Body, "status"), summarize(r.Body))
			if u := api.Str(r.Body, "approval", "url"); u != "" {
				fmt.Fprintf(w, "approve at %s\n", u)
			}
		})
		return nil
	}}
	submitCmd := &cobra.Command{Use: "submit <id>", Short: "Submit a draft for approval (202 + approval URL)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Post(cmd.Context(), "/v1/change-sets/"+args[0]+"/submit", nil, nil)
		if err != nil {
			return err
		}
		return rt.handleResponse(cmd.Context(), client, r, opts, nil)
	}}
	addApprovalFlags(submitCmd.Flags(), &opts)
	wait := &cobra.Command{Use: "wait <id>", Short: "Block until the human decides; deliver secrets with --write-env/--exec/--show-secret", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		opts.wait = true
		final, err := rt.waitChangeSet(cmd.Context(), client, args[0], opts.timeout)
		if err != nil {
			return err
		}
		return rt.deliverOutcome(cmd.Context(), client, final, opts)
	}}
	addApprovalFlags(wait.Flags(), &opts)
	secrets := &cobra.Command{Use: "secrets <id>", Short: "Retrieve the secrets of an applied change set — once, within one hour", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		if opts.writeEnv == "" && opts.execCmd == "" && !opts.showSecret {
			return &output.Exit{Code: output.ExitUsage, Message: "choose where the secret goes: --write-env <file>, --exec \"cmd {secret}\" or --show-secret"}
		}
		r, err := client.Get(cmd.Context(), "/v1/change-sets/"+args[0]+"/secrets", nil)
		if err != nil {
			return err
		}
		delivered, err := rt.deliverSecrets(api.Map(r.Body, "secrets"), opts)
		if err != nil {
			return err
		}
		rt.Printer.Result(map[string]any{"change_set_id": args[0], "secrets": delivered}, func(w io.Writer) { fmt.Fprintf(w, "Delivered: %v\n", delivered["keys"]) })
		return nil
	}}
	addApprovalFlags(secrets.Flags(), &opts)
	withdraw := &cobra.Command{Use: "withdraw <id>", Short: "Withdraw a draft or pending change set", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Delete(cmd.Context(), "/v1/change-sets/"+args[0])
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) { fmt.Fprintln(w, "Withdrawn") })
		return nil
	}}
	var listStatus string
	var full bool
	list := &cobra.Command{Use: "list", Short: "List change sets of the consented workspaces (compact rows; --status to filter, --full for whole objects)", RunE: func(cmd *cobra.Command, args []string) error {
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		q := url.Values{}
		if listStatus == "" {
			listStatus = "draft,pending_approval,approved,applied,rejected,expired,failed"
		}
		q.Set("status", listStatus)
		items, err := client.ListAll(cmd.Context(), "/v1/approvals", q, 500)
		if err != nil {
			return err
		}
		data := items
		if !full {
			data = make([]any, 0, len(items))
			for _, it := range items {
				data = append(data, compactChangeSet(it))
			}
		}
		rt.Printer.Result(map[string]any{"data": data}, func(w io.Writer) {
			rows := [][]string{}
			for _, it := range items {
				m, _ := it.(map[string]any)
				rows = append(rows, []string{api.Str(m, "id"), api.Str(m, "status"), api.Str(m, "workspace", "name"), short(summarize(m), 70), api.Str(m, "created_at")})
			}
			output.Table(w, []string{"ID", "STATUS", "WORKSPACE", "CHANGES", "CREATED"}, rows)
		})
		return nil
	}}
	list.Flags().StringVar(&listStatus, "status", "", "comma-separated statuses: draft, pending_approval, approved, applied, rejected, expired, failed")
	list.Flags().BoolVar(&full, "full", false, "print whole change set objects (diff, operations, result) instead of compact rows")
	cmd.AddCommand(create, list, show, submitCmd, wait, secrets, withdraw)

	return cmd
}

// parseOps reads --op type:json entries or a JSON file.
func parseOps(ops []string, file string) ([]map[string]any, error) {
	var list []map[string]any
	if file != "" {
		var data []byte
		var err error
		if file == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(file)
		}
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, fmt.Errorf("operations file: %w", err)
		}
	}
	for _, op := range ops {
		typ, params, found := strings.Cut(op, ":")
		entry := map[string]any{"op": typ, "params": map[string]any{}}
		if found && strings.TrimSpace(params) != "" {
			var p map[string]any
			if err := json.Unmarshal([]byte(params), &p); err != nil {
				return nil, fmt.Errorf("--op %s: params must be JSON: %w", typ, err)
			}
			entry["params"] = p
		}
		list = append(list, entry)
	}
	if len(list) == 0 {
		return nil, &output.Exit{Code: output.ExitUsage, Message: "no operations: pass --op type:json or -f ops.json"}
	}
	return list, nil
}

// compactChangeSet keeps what an agent needs to pick a change set; `iugu changeset show <id>` has the rest.
func compactChangeSet(it any) map[string]any {
	m, _ := it.(map[string]any)
	ops := []string{}
	for _, d := range api.List(m, "diff") {
		dm, _ := d.(map[string]any)
		ops = append(ops, api.Str(dm, "op"))
	}
	row := map[string]any{
		"id": api.Str(m, "id"), "status": api.Str(m, "status"), "title": api.Str(m, "title"), "summary": summarize(m), "operations": ops,
		"workspace": m["workspace"], "app": m["app"], "created_at": api.Str(m, "created_at"), "expires_at": api.Str(m, "expires_at"),
	}
	if u := api.Str(m, "approval", "url"); u != "" {
		row["approval_url"] = u
	}
	return row
}

// fillAppID sets params.app_id on operations that take one and did not specify it.
func fillAppID(operations []map[string]any, appID string) {
	for _, op := range operations {
		if op["op"] == "installations.resync" {
			continue
		}
		params, _ := op["params"].(map[string]any)
		if params == nil {
			params = map[string]any{}
			op["params"] = params
		}
		if _, has := params["app_id"]; !has {
			params["app_id"] = appID
		}
	}
}

func (rt *Runtime) approvalsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "approvals", Short: "Change sets awaiting a human decision"}
	var status string
	list := &cobra.Command{Use: "list", Short: "List pending approvals in the consented workspaces", RunE: func(cmd *cobra.Command, args []string) error {
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		q := url.Values{}
		if status != "" {
			q.Set("status", status)
		}
		items, err := client.ListAll(cmd.Context(), "/v1/approvals", q, 500)
		if err != nil {
			return err
		}
		rt.Printer.Result(map[string]any{"data": items}, func(w io.Writer) {
			rows := [][]string{}
			for _, it := range items {
				m, _ := it.(map[string]any)
				rows = append(rows, []string{api.Str(m, "id"), api.Str(m, "status"), api.Str(m, "workspace", "name"), short(summarize(m), 70), api.Str(m, "approval", "url")})
			}
			output.Table(w, []string{"ID", "STATUS", "WORKSPACE", "CHANGES", "APPROVE AT"}, rows)
		})
		return nil
	}}
	list.Flags().StringVar(&status, "status", "", "comma-separated statuses (default: pending)")
	open := &cobra.Command{Use: "open <id>", Short: "Open the approval page in the browser (or print its URL)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Get(cmd.Context(), "/v1/change-sets/"+args[0], nil)
		if err != nil {
			return err
		}
		u := api.Str(r.Body, "approval", "url")
		if u == "" {
			return &output.Exit{Code: output.ExitFailure, Message: "this change set is not pending approval (status " + api.Str(r.Body, "status") + ")"}
		}
		if !rt.IsAgent() {
			_ = openBrowser(u)
		}
		rt.Printer.Result(map[string]any{"change_set_id": args[0], "url": u}, func(w io.Writer) { fmt.Fprintln(w, u) })
		return nil
	}}
	opts := approvalOptions{}
	wait := &cobra.Command{Use: "wait <id>", Short: "Same as `iugu changeset wait`", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		final, err := rt.waitChangeSet(cmd.Context(), client, args[0], opts.timeout)
		if err != nil {
			return err
		}
		return rt.deliverOutcome(cmd.Context(), client, final, opts)
	}}
	addApprovalFlags(wait.Flags(), &opts)
	cmd.AddCommand(list, open, wait)
	return cmd
}
