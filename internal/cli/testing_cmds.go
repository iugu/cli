package cli

import (
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iugu-private/platform2-cli/internal/api"
	"github.com/iugu-private/platform2-cli/internal/output"
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
	cmd := &cobra.Command{Use: "gia", Short: "Roles, policies, members and invites of a workspace (reads; writes arrive as change-set operations)"}
	for _, resource := range []string{"roles", "policies", "members", "invites"} {
		res := resource
		cmd.AddCommand(&cobra.Command{Use: res, Short: "List " + res, RunE: func(cmd *cobra.Command, args []string) error {
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
		}})
	}
	cmd.PersistentFlags().StringVar(&workspace, "workspace", "", "workspace id")
	return cmd
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
