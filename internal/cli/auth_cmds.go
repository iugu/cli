package cli

import (
	"errors"
	"fmt"
	"io"
	"net/url"

	"github.com/spf13/cobra"

	"github.com/iugu/cli/internal/api"
	"github.com/iugu/cli/internal/auth"
	"github.com/iugu/cli/internal/output"
	"github.com/iugu/cli/internal/store"
)

func (rt *Runtime) logoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Revoke the grant at the authorization server and forget the local login",
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := rt.loadSession()
			if errors.Is(err, store.ErrNotFound) {
				rt.Printer.Result(map[string]any{"logged_in": false}, func(w io.Writer) { fmt.Fprintln(w, "Not logged in.") })
				return nil
			}
			if err != nil {
				return err
			}
			oauth, err := rt.oauthForSession(cmd.Context(), session)
			if err == nil && session.RefreshToken != "" {
				if err := oauth.Revoke(cmd.Context(), session.RefreshToken); err != nil {
					rt.Printer.Line("warning: revocation failed (%v); removing the local login anyway", err)
				}
			}
			if err := rt.store.Delete(rt.profileName); err != nil {
				return err
			}
			rt.Printer.Result(map[string]any{"logged_in": false, "revoked": true}, func(w io.Writer) { fmt.Fprintln(w, "Logged out; the grant was revoked.") })
			return nil
		},
	}
}

func (rt *Runtime) authCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "Inspect the stored login"}
	var offline bool
	status := &cobra.Command{
		Use:   "status",
		Short: "Show the login (always exits 0; `logged_in` tells the truth — the grant is checked online unless --offline)",
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := rt.loadSession()
			if err != nil {
				rt.Printer.Result(statusPayload(nil, rt.store.Name(), false), func(w io.Writer) { fmt.Fprintln(w, "Not logged in. Run `iugu login`.") })
				return nil
			}
			payload := statusPayload(session, rt.store.Name(), true)
			if !offline {
				// A grant can be revoked from the Connected agents page or by a logout elsewhere; the local
				// copy cannot know, so probe the API (a refresh happens here when the access token expired).
				payload["verified"] = false
				if client, err := rt.apiClient(cmd.Context()); err == nil {
					if _, err := client.Get(cmd.Context(), "/v1/me", nil); err == nil {
						payload["verified"] = true
					} else if api.IsCode(err, "invalid_token") || errors.Is(err, auth.ErrLoginRequired) {
						payload["logged_in"] = false
						payload["verified"] = true
						payload["reason"] = "the grant was revoked or expired; run `iugu login`"
					} else {
						payload["reason"] = "could not reach the API: " + err.Error()
					}
				} else if errors.Is(err, auth.ErrLoginRequired) {
					payload["logged_in"] = false
					payload["verified"] = true
					payload["reason"] = "the grant was revoked or expired; run `iugu login`"
				}
			}
			rt.Printer.Result(payload, func(w io.Writer) {
				if payload["logged_in"] == false {
					fmt.Fprintf(w, "Not logged in (%s).\n", payload["reason"])
					return
				}
				fmt.Fprintf(w, "Logged in as %s (%s)\nAPI: %s\nScopes: %v\nWorkspaces: %v\nGrant: %s\nStore: %s\n", session.Email, session.Sub, session.API, session.Scopes, session.Workspaces, session.GrantID, rt.store.Name())
				if payload["verified"] == false {
					fmt.Fprintf(w, "(not verified online: %v)\n", payload["reason"])
				}
			})
			return nil
		},
	}
	status.Flags().BoolVar(&offline, "offline", false, "do not verify the grant with the API")
	cmd.AddCommand(status, &cobra.Command{
		Use:   "token",
		Short: "Print a fresh access token (5 minutes) for scripts — prefer the CLI itself",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			token, err := client.Token(cmd.Context())
			if err != nil {
				return err
			}
			rt.Printer.Result(map[string]any{"access_token": token, "token_type": "Bearer"}, func(w io.Writer) { fmt.Fprintln(w, token) })
			return nil
		},
	})
	return cmd
}

func (rt *Runtime) meCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "me",
		Short: "Who the agent is acting as, through which client and grant, in which workspaces",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			r, err := client.Get(cmd.Context(), "/v1/me", nil)
			if err != nil {
				return err
			}
			rt.Printer.Result(r.Body, func(w io.Writer) {
				fmt.Fprintf(w, "%s <%s>  via %s\n", api.Str(r.Body, "principal", "name"), api.Str(r.Body, "principal", "email"), api.Str(r.Body, "client", "name"))
				rows := [][]string{}
				for _, ws := range api.List(r.Body, "workspaces") {
					m, _ := ws.(map[string]any)
					roles := []string{}
					for _, role := range api.List(m, "roles") {
						rm, _ := role.(map[string]any)
						roles = append(roles, api.Str(rm, "name"))
					}
					rows = append(rows, []string{api.Str(m, "id"), api.Str(m, "name"), fmt.Sprint(roles)})
				}
				output.Table(w, []string{"WORKSPACE", "NAME", "ROLES"}, rows)
			})
			return nil
		},
	}
}

func (rt *Runtime) workspaceCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "workspace", Short: "Consented workspaces and the default one for this profile"}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the workspaces this login may act in",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			r, err := client.Get(cmd.Context(), "/v1/workspaces", nil)
			if err != nil {
				return err
			}
			rt.Printer.Result(r.Body, func(w io.Writer) {
				rows := [][]string{}
				for _, ws := range api.List(r.Body, "data") {
					m, _ := ws.(map[string]any)
					mark := ""
					if api.Str(m, "id") == rt.profile.Workspace {
						mark = "*"
					}
					rows = append(rows, []string{mark, api.Str(m, "id"), api.Str(m, "name"), api.Str(m, "alias"), api.Str(m, "status")})
				}
				output.Table(w, []string{"", "ID", "NAME", "ALIAS", "STATUS"}, rows)
			})
			return nil
		},
	}, &cobra.Command{
		Use:   "use <workspace-id>",
		Short: "Make a consented workspace the default for this profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := rt.loadSession()
			if err != nil {
				return err
			}
			found := false
			for _, ws := range session.Workspaces {
				if ws == args[0] {
					found = true
				}
			}
			if !found {
				return &output.Exit{Code: output.ExitUsage, Message: fmt.Sprintf("workspace %s is not in this grant; run `iugu login --workspace %s` to consent to it", args[0], args[0])}
			}
			p := rt.configFile.Profiles[rt.profileName]
			p.API = rt.profile.API
			p.Workspace = args[0]
			rt.configFile.Profiles[rt.profileName] = p
			if err := rt.configFile.Save(); err != nil {
				return err
			}
			rt.Printer.Result(map[string]any{"profile": rt.profileName, "workspace": args[0]}, func(w io.Writer) { fmt.Fprintf(w, "Default workspace for profile %s: %s\n", rt.profileName, args[0]) })
			return nil
		},
	})
	return cmd
}

var _ = url.Values{}
