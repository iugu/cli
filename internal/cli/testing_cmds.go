package cli

import (
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iugu/cli/internal/api"
	"github.com/iugu/cli/internal/output"
)

func (rt *Runtime) verifyCommand() *cobra.Command {
	var workspace string
	var principals, actions []string
	cmd := &cobra.Command{
		Use:   "verify --principal <user:…|temp:…|app:…> --action <tag:action>",
		Short: "Policy simulator: the answers /verify would give for these principals and actions",
		RunE: func(cmd *cobra.Command, args []string) error {
			ws, err := rt.workspaceArg(workspace)
			if err != nil {
				return err
			}
			if len(principals) == 0 || len(actions) == 0 {
				return &output.Exit{Code: output.ExitUsage, Message: "pass at least one --principal and one --action"}
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			r, err := client.Post(cmd.Context(), "/v1/workspaces/"+ws+"/authorization/test", map[string]any{"principals": principals, "actions": actions}, nil)
			if err != nil {
				return err
			}
			rt.Printer.Result(r.Body, func(w io.Writer) {
				rows := [][]string{}
				for action, allowed := range api.Map(r.Body, "actions") {
					rows = append(rows, []string{action, fmt.Sprint(allowed)})
				}
				output.Table(w, []string{"ACTION", "ALLOWED"}, rows)
			})
			return nil
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace id")
	cmd.Flags().StringSliceVar(&principals, "principal", nil, "principal (repeatable)")
	cmd.Flags().StringSliceVar(&actions, "action", nil, "action tag:name (repeatable)")
	return cmd
}

func (rt *Runtime) testPrincipalCommand() *cobra.Command {
	var workspace, name string
	var roles []string
	cmd := &cobra.Command{Use: "test-principal", Short: "Temporary principals (1 h) bounded by your own roles, for /verify tests"}
	create := &cobra.Command{Use: "create --name <name> --roles <role-id,…>", Short: "Create a temporary principal", RunE: func(cmd *cobra.Command, args []string) error {
		ws, err := rt.workspaceArg(workspace)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		if len(roles) == 0 {
			me, err := client.Get(cmd.Context(), "/v1/me", nil)
			if err != nil {
				return err
			}
			for _, w := range api.List(me.Body, "workspaces") {
				m, _ := w.(map[string]any)
				if api.Str(m, "id") == ws {
					for _, role := range api.List(m, "roles") {
						rm, _ := role.(map[string]any)
						roles = append(roles, api.Str(rm, "id"))
					}
				}
			}
			if len(roles) == 0 {
				return &output.Exit{Code: output.ExitUsage, Message: "pass --roles; you hold no roles in this workspace to copy"}
			}
			rt.Printer.Line("using your own roles: %s", strings.Join(roles, ","))
		}
		r, err := client.Post(cmd.Context(), "/v1/workspaces/"+ws+"/temp-principals", map[string]any{"name": name, "role_ids": roles}, nil)
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) {
			fmt.Fprintf(w, "%s (%s) — %d role(s), ttl %ss\n", api.Str(r.Body, "principal"), api.Str(r.Body, "name"), len(api.List(r.Body, "roles")), api.Str(r.Body, "ttl"))
		})
		return nil
	}}
	create.Flags().StringVar(&name, "name", "test principal", "name")
	create.Flags().StringSliceVar(&roles, "roles", nil, "role ids (default: your own roles in the workspace)")
	list := &cobra.Command{Use: "list", Short: "List temporary principals", RunE: func(cmd *cobra.Command, args []string) error {
		ws, err := rt.workspaceArg(workspace)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Get(cmd.Context(), "/v1/workspaces/"+ws+"/temp-principals", nil)
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) {
			rows := [][]string{}
			for _, it := range api.List(r.Body, "data") {
				m, _ := it.(map[string]any)
				rows = append(rows, []string{api.Str(m, "id"), api.Str(m, "principal"), api.Str(m, "name"), api.Str(m, "ttl")})
			}
			output.Table(w, []string{"ID", "PRINCIPAL", "NAME", "TTL"}, rows)
		})
		return nil
	}}
	del := &cobra.Command{Use: "delete <id | temp:id>", Short: "Delete a temporary principal (accepts the id or the temp:… principal string)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		id := strings.TrimPrefix(args[0], "temp:")
		if _, err := client.Delete(cmd.Context(), "/v1/temp-principals/"+id); err != nil {
			return err
		}
		rt.Printer.Result(map[string]any{"deleted": id}, func(w io.Writer) { fmt.Fprintln(w, "Deleted") })
		return nil
	}}
	cmd.PersistentFlags().StringVar(&workspace, "workspace", "", "workspace id")
	cmd.AddCommand(create, list, del)
	return cmd
}

func (rt *Runtime) giaCommand() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{Use: "gia", Short: "Roles, policies, members and invites of a workspace (reads are Tier 0; every write is Tier 1 → approval)",
		Long: `Reads: iugu gia roles | policies | members | invites
Writes (need the console:gia.write scope — re-consent with: iugu login --scopes "` + DefaultScopes + ` console:gia.write"):
  iugu gia roles create --name Support --policies <policy-id,…>        iugu gia roles update <id> [--name …] [--policies …]   iugu gia roles delete <id>
  iugu gia policies create --name Invoices --actions acme:invoice.*     iugu gia policies update <id> [--name …] [--actions …] iugu gia policies delete <id>
  iugu gia members set-roles <member-id|email> --roles <role-id,…>     iugu gia members remove <member-id|email>
  iugu gia invites create --email dev@acme.test --roles <role-id,…>    iugu gia invites resend <invite-id>
Each write answers exit 5 with approval.url (or blocks with --wait); the approver sees the diff, and the change is
applied with the same guards as the UI (managed policies untouched, the workspace keeps an administrator).`}
	giaResources := map[string]*cobra.Command{}
	for _, resource := range []string{"roles", "policies", "members", "invites"} {
		res := resource
		list := &cobra.Command{Use: res, Short: "List " + res + " (subcommands write)", RunE: func(cmd *cobra.Command, args []string) error {
			ws, err := rt.workspaceArg(workspace)
			if err != nil {
				return err
			}
			return rt.listPrint(cmd, "/v1/workspaces/"+ws+"/gia/"+res, []string{"ID", "NAME/EMAIL", "DETAILS"}, func(m map[string]any) []string {
				name := api.Str(m, "name")
				if name == "" {
					name = api.Str(m, "email")
				}
				if name == "" {
					name = api.Str(m, "user", "email")
				}
				details := ""
				switch res {
				case "policies":
					details = strings.Join(toStrings(api.List(m, "actions")), " ")
				case "roles", "members":
					names := []string{}
					for _, p := range api.List(m, "policies") {
						pm, _ := p.(map[string]any)
						names = append(names, api.Str(pm, "name"))
					}
					for _, p := range api.List(m, "roles") {
						pm, _ := p.(map[string]any)
						names = append(names, api.Str(pm, "name"))
					}
					details = strings.Join(names, ", ")
				case "invites":
					details = "pending=" + api.Str(m, "pending")
				}
				return []string{api.Str(m, "id"), name, short(details, 80)}
			})
		}}
		giaResources[res] = list
		cmd.AddCommand(list)
	}
	rt.addGiaWrites(giaResources, &workspace)
	cmd.PersistentFlags().StringVar(&workspace, "workspace", "", "workspace id")
	return cmd
}

// addGiaWrites attaches the Tier 1 subcommands to `iugu gia roles|policies|members|invites`.
func (rt *Runtime) addGiaWrites(res map[string]*cobra.Command, workspace *string) {
	gia := func(cmd *cobra.Command, method, suffix string, body map[string]any, opts approvalOptions) error {
		ws, err := rt.workspaceArg(*workspace)
		if err != nil {
			return err
		}
		return rt.tier1(cmd, method, "/v1/workspaces/"+ws+"/gia/"+suffix, body, opts, nil)
	}
	ids := func(list []string) []string {
		out := []string{}
		for _, v := range list {
			out = append(out, strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })...)
		}
		return out
	}

	// roles
	{
		var name string
		var policies []string
		opts := approvalOptions{}
		create := &cobra.Command{Use: "create --name <name> --policies <policy-id,…>", Short: "Create a role (Tier 1)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" || len(policies) == 0 {
				return &output.Exit{Code: output.ExitUsage, Message: "pass --name and --policies (ids from `iugu gia policies`)"}
			}
			return gia(cmd, "POST", "roles", map[string]any{"name": name, "policy_ids": ids(policies)}, opts)
		}}
		create.Flags().StringVar(&name, "name", "", "role name")
		create.Flags().StringSliceVar(&policies, "policies", nil, "policy ids (editable policies only)")
		addApprovalFlags(create.Flags(), &opts)

		var uName string
		var uPolicies []string
		uOpts := approvalOptions{}
		update := &cobra.Command{Use: "update <role-id> [--name <name>] [--policies <policy-id,…>]", Short: "Rename a role and/or replace its policies (Tier 1)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{}
			if uName != "" {
				body["name"] = uName
			}
			if cmd.Flags().Changed("policies") {
				body["policy_ids"] = ids(uPolicies)
			}
			if len(body) == 0 {
				return &output.Exit{Code: output.ExitUsage, Message: "pass --name and/or --policies"}
			}
			return gia(cmd, "PATCH", "roles/"+args[0], body, uOpts)
		}}
		update.Flags().StringVar(&uName, "name", "", "new name")
		update.Flags().StringSliceVar(&uPolicies, "policies", nil, "policy ids replacing the current ones (managed policies stay)")
		addApprovalFlags(update.Flags(), &uOpts)

		dOpts := approvalOptions{}
		del := &cobra.Command{Use: "delete <role-id>", Short: "Delete a role (Tier 1; refused if the workspace would lose its administrator)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			return gia(cmd, "DELETE", "roles/"+args[0], nil, dOpts)
		}}
		addApprovalFlags(del.Flags(), &dOpts)
		res["roles"].AddCommand(create, update, del)
	}

	// policies
	{
		var name string
		var actions []string
		opts := approvalOptions{}
		create := &cobra.Command{Use: "create --name <name> --actions <prefix:action,…>", Short: "Create a policy (Tier 1)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			if name == "" || len(actions) == 0 {
				return &output.Exit{Code: output.ExitUsage, Message: "pass --name and --actions (e.g. acme:invoice.read,acme:invoice.*)"}
			}
			return gia(cmd, "POST", "policies", map[string]any{"name": name, "actions": ids(actions)}, opts)
		}}
		create.Flags().StringVar(&name, "name", "", "policy name")
		create.Flags().StringSliceVar(&actions, "actions", nil, "actions granted (prefix:action, wildcards allowed)")
		addApprovalFlags(create.Flags(), &opts)

		var uName string
		var uActions []string
		uOpts := approvalOptions{}
		update := &cobra.Command{Use: "update <policy-id> [--name <name>] [--actions <prefix:action,…>]", Short: "Rename a policy and/or replace its actions (Tier 1)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{}
			if uName != "" {
				body["name"] = uName
			}
			if cmd.Flags().Changed("actions") {
				body["actions"] = ids(uActions)
			}
			if len(body) == 0 {
				return &output.Exit{Code: output.ExitUsage, Message: "pass --name and/or --actions"}
			}
			return gia(cmd, "PATCH", "policies/"+args[0], body, uOpts)
		}}
		update.Flags().StringVar(&uName, "name", "", "new name")
		update.Flags().StringSliceVar(&uActions, "actions", nil, "actions replacing the current ones")
		addApprovalFlags(update.Flags(), &uOpts)

		dOpts := approvalOptions{}
		del := &cobra.Command{Use: "delete <policy-id>", Short: "Delete a policy (Tier 1; managed policies are refused)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			return gia(cmd, "DELETE", "policies/"+args[0], nil, dOpts)
		}}
		addApprovalFlags(del.Flags(), &dOpts)
		res["policies"].AddCommand(create, update, del)
	}

	// members
	{
		var roles []string
		opts := approvalOptions{}
		setRoles := &cobra.Command{Use: "set-roles <member-id|email> --roles <role-id,…>", Short: "Replace a member's roles (Tier 1)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			if len(roles) == 0 {
				return &output.Exit{Code: output.ExitUsage, Message: "pass --roles (ids from `iugu gia roles`)"}
			}
			member, err := rt.giaMemberID(cmd, *workspace, args[0])
			if err != nil {
				return err
			}
			return gia(cmd, "PATCH", "members/"+member, map[string]any{"role_ids": ids(roles)}, opts)
		}}
		setRoles.Flags().StringSliceVar(&roles, "roles", nil, "role ids replacing the current ones")
		addApprovalFlags(setRoles.Flags(), &opts)

		rOpts := approvalOptions{}
		remove := &cobra.Command{Use: "remove <member-id|email>", Short: "Remove a member (Tier 1; the approver cannot remove themselves)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			member, err := rt.giaMemberID(cmd, *workspace, args[0])
			if err != nil {
				return err
			}
			return gia(cmd, "DELETE", "members/"+member, nil, rOpts)
		}}
		addApprovalFlags(remove.Flags(), &rOpts)
		res["members"].AddCommand(setRoles, remove)
	}

	// invites
	{
		var email string
		var roles []string
		opts := approvalOptions{}
		create := &cobra.Command{Use: "create --email <email> --roles <role-id,…>", Short: "Invite someone with roles (Tier 1; the e-mail goes out on approval)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			if email == "" || len(roles) == 0 {
				return &output.Exit{Code: output.ExitUsage, Message: "pass --email and --roles"}
			}
			return gia(cmd, "POST", "invites", map[string]any{"email": email, "role_ids": ids(roles)}, opts)
		}}
		create.Flags().StringVar(&email, "email", "", "e-mail to invite")
		create.Flags().StringSliceVar(&roles, "roles", nil, "role ids the invitee gets on acceptance")
		addApprovalFlags(create.Flags(), &opts)

		rOpts := approvalOptions{}
		resend := &cobra.Command{Use: "resend <invite-id>", Short: "Renew and resend a pending invite (Tier 1)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			return gia(cmd, "POST", "invites/"+args[0]+"/resend", map[string]any{}, rOpts)
		}}
		addApprovalFlags(resend.Flags(), &rOpts)
		res["invites"].AddCommand(create, resend)
	}
}

// giaMemberID accepts a member id or an e-mail (resolved through `gia members`).
func (rt *Runtime) giaMemberID(cmd *cobra.Command, workspace, ref string) (string, error) {
	if !strings.Contains(ref, "@") {
		return ref, nil
	}
	ws, err := rt.workspaceArg(workspace)
	if err != nil {
		return "", err
	}
	client, err := rt.apiClient(cmd.Context())
	if err != nil {
		return "", err
	}
	members, err := client.ListAll(cmd.Context(), "/v1/workspaces/"+ws+"/gia/members", nil, 1000)
	if err != nil {
		return "", err
	}
	for _, it := range members {
		m, _ := it.(map[string]any)
		if strings.EqualFold(api.Str(m, "user", "email"), ref) {
			return api.Str(m, "id"), nil
		}
	}
	return "", &output.Exit{Code: output.ExitFailure, Message: "no member with e-mail " + ref + " in workspace " + ws + " (see `iugu gia members`)"}
}

func (rt *Runtime) catalogCommand() *cobra.Command {
	var q string
	cmd := &cobra.Command{Use: "catalog", Short: "What apps may consume"}
	actions := &cobra.Command{Use: "actions [--q <text>]", Short: "Implemented actions of installable apps (tag:action)", RunE: func(cmd *cobra.Command, args []string) error {
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		query := url.Values{}
		if q != "" {
			query.Set("q", q)
		}
		items, err := client.ListAll(cmd.Context(), "/v1/catalog/actions", query, 500)
		if err != nil {
			return err
		}
		rt.Printer.Result(map[string]any{"data": items}, func(w io.Writer) {
			rows := [][]string{}
			for _, it := range items {
				m, _ := it.(map[string]any)
				rows = append(rows, []string{api.Str(m, "app", "tag"), api.Str(m, "app", "name"), short(strings.Join(toStrings(api.List(m, "actions")), " "), 90)})
			}
			output.Table(w, []string{"TAG", "APP", "ACTIONS"}, rows)
		})
		return nil
	}}
	actions.Flags().StringVar(&q, "q", "", "search text (action or tag)")
	cmd.AddCommand(actions)
	return cmd
}

func toStrings(list []any) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		out = append(out, fmt.Sprint(v))
	}
	return out
}
