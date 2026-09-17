package cli

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iugu-private/platform2-cli/internal/api"
	"github.com/iugu-private/platform2-cli/internal/output"
)

// tier1 posts/patches and routes the 202 through the approval machinery.
func (rt *Runtime) tier1(cmd *cobra.Command, method, path string, body any, opts approvalOptions, human func(io.Writer)) error {
	client, err := rt.apiClient(cmd.Context())
	if err != nil {
		return err
	}
	var r *api.Response
	switch method {
	case "POST":
		r, err = client.Post(cmd.Context(), path, body, rt.mutating())
	case "PATCH":
		r, err = client.Patch(cmd.Context(), path, body, rt.mutating())
	case "PUT":
		r, err = client.Put(cmd.Context(), path, body, rt.mutating())
	}
	if err != nil {
		return err
	}
	return rt.handleResponse(cmd.Context(), client, r, opts, human)
}

func (rt *Runtime) listPrint(cmd *cobra.Command, path string, header []string, row func(map[string]any) []string) error {
	client, err := rt.apiClient(cmd.Context())
	if err != nil {
		return err
	}
	items, err := client.ListAll(cmd.Context(), path, nil, 1000)
	if err != nil {
		return err
	}
	rt.Printer.Result(map[string]any{"data": items}, func(w io.Writer) {
		rows := [][]string{}
		for _, it := range items {
			m, _ := it.(map[string]any)
			rows = append(rows, row(m))
		}
		output.Table(w, header, rows)
	})
	return nil
}

func (rt *Runtime) appPermissionsCommand() *cobra.Command {
	var appFlag string
	cmd := &cobra.Command{Use: "permissions", Short: "Implemented/consumed actions, grant scopes, allowed workspaces (Tier 0)"}
	get := &cobra.Command{
		Use: "get", Short: "Show permissions",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(appFlag)
			if err != nil {
				return err
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			r, err := client.Get(cmd.Context(), "/v1/apps/"+id+"/permissions", nil)
			if err != nil {
				return err
			}
			rt.Printer.Result(r.Body, nil)
			return nil
		},
	}
	var implemented, consumed, grantScopes, allowed []string
	var ifMatch string
	set := &cobra.Command{
		Use: "set", Short: "Set permissions (changing consumed actions makes installing workspaces re-consent)",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(appFlag)
			if err != nil {
				return err
			}
			body := map[string]any{}
			if cmd.Flags().Changed("implemented") {
				body["implemented_actions"] = implemented
			}
			if cmd.Flags().Changed("consumed") {
				body["consumed_actions"] = consumed
			}
			if cmd.Flags().Changed("grant-scopes") {
				body["grant_scopes"] = grantScopes
			}
			if cmd.Flags().Changed("allowed-workspaces") {
				body["allowed_workspace_ids"] = allowed
			}
			if len(body) == 0 {
				return &output.Exit{Code: output.ExitUsage, Message: "pass --implemented, --consumed, --grant-scopes and/or --allowed-workspaces"}
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			r, err := client.Patch(cmd.Context(), "/v1/apps/"+id+"/permissions", body, &api.Options{IfMatch: ifMatch, IdempotencyKey: rt.idempotencyKey})
			if err != nil {
				return err
			}
			rt.Printer.Result(r.Body, func(w io.Writer) {
				fmt.Fprintf(w, "Permissions updated; %d installation(s) need re-consent\n", len(api.List(r.Body, "reconsent_workspaces")))
			})
			return nil
		},
	}
	set.Flags().StringSliceVar(&implemented, "implemented", nil, "actions the app implements (without the tag prefix)")
	set.Flags().StringSliceVar(&consumed, "consumed", nil, "actions the app consumes, `tag:action` (see `iugu catalog actions`)")
	set.Flags().StringSliceVar(&grantScopes, "grant-scopes", nil, "informations, config, transfers")
	set.Flags().StringSliceVar(&allowed, "allowed-workspaces", nil, "workspaces allowed to install a private app")
	set.Flags().StringVar(&ifMatch, "if-match", "", "ETag")
	cmd.PersistentFlags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")
	cmd.AddCommand(get, set)
	return cmd
}

func (rt *Runtime) appOauthCommand() *cobra.Command {
	var appFlag, appURL string
	var callbacks []string
	opts := approvalOptions{}
	cmd := &cobra.Command{Use: "oauth", Short: "OAuth settings of the app"}
	set := &cobra.Command{
		Use: "set", Short: "Set the app URL and redirect callbacks (Tier 1 → approval)",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(appFlag)
			if err != nil {
				return err
			}
			body := map[string]any{}
			if appURL != "" {
				body["url"] = appURL
			}
			if cmd.Flags().Changed("callbacks") {
				body["callbacks"] = callbacks
			}
			if len(body) == 0 {
				return &output.Exit{Code: output.ExitUsage, Message: "pass --url and/or --callbacks"}
			}
			return rt.tier1(cmd, "PATCH", "/v1/apps/"+id+"/oauth", body, opts, nil)
		},
	}
	set.Flags().StringVar(&appURL, "url", "", "app URL")
	set.Flags().StringSliceVar(&callbacks, "callbacks", nil, "exact https redirect URIs")
	addApprovalFlags(set.Flags(), &opts)
	cmd.PersistentFlags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")
	cmd.AddCommand(set)
	return cmd
}

func (rt *Runtime) appCredentialsCommand() *cobra.Command {
	var appFlag, name string
	var workspaces []string
	opts := approvalOptions{}
	cmd := &cobra.Command{Use: "credentials", Short: "OAuth client secrets of the app"}
	list := &cobra.Command{
		Use: "list", Short: "List credentials (prefix and status; never secrets)",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(appFlag)
			if err != nil {
				return err
			}
			return rt.listPrint(cmd, "/v1/apps/"+id+"/credentials", []string{"ID", "NAME", "PREFIX", "STATUS", "WORKSPACES"}, func(m map[string]any) []string {
				return []string{api.Str(m, "id"), api.Str(m, "name"), api.Str(m, "secret_prefix"), api.Str(m, "status"), fmt.Sprint(api.List(m, "workspace_ids"))}
			})
		},
	}
	create := &cobra.Command{
		Use: "create --name <name> [--workspaces <ids>]", Short: "Request a credential (Tier 1 → approval; the secret is delivered once to this CLI)",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(appFlag)
			if err != nil {
				return err
			}
			body := map[string]any{"name": name}
			if len(workspaces) > 0 {
				body["workspace_ids"] = workspaces
			}
			return rt.tier1(cmd, "POST", "/v1/apps/"+id+"/credentials", body, opts, nil)
		},
	}
	create.Flags().StringVar(&name, "name", "", "credential name")
	create.Flags().StringSliceVar(&workspaces, "workspaces", nil, "restrict the credential's tokens to these workspaces (recommended for development)")
	addApprovalFlags(create.Flags(), &opts)
	var credentialID string
	rotate := &cobra.Command{
		Use: "rotate --id <credential-id>", Short: "Rotate a credential (new secret, old one revoked; Tier 1)",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(appFlag)
			if err != nil {
				return err
			}
			return rt.tier1(cmd, "POST", "/v1/apps/"+id+"/credentials/"+credentialID+"/rotate", nil, opts, nil)
		},
	}
	rotate.Flags().StringVar(&credentialID, "id", "", "credential id")
	addApprovalFlags(rotate.Flags(), &opts)
	revoke := &cobra.Command{
		Use: "revoke --id <credential-id>", Short: "Revoke a credential (Tier 1)",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(appFlag)
			if err != nil {
				return err
			}
			return rt.tier1(cmd, "POST", "/v1/apps/"+id+"/credentials/"+credentialID+"/revoke", nil, opts, nil)
		},
	}
	revoke.Flags().StringVar(&credentialID, "id", "", "credential id")
	addApprovalFlags(revoke.Flags(), &opts)
	cmd.PersistentFlags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")
	cmd.AddCommand(list, create, rotate, revoke)
	return cmd
}

func (rt *Runtime) appCertificatesCommand() *cobra.Command {
	var appFlag, keyFile, certID string
	opts := approvalOptions{}
	cmd := &cobra.Command{Use: "certificates", Short: "Client certificates of the app"}
	list := &cobra.Command{Use: "list", Short: "List certificates", RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		return rt.listPrint(cmd, "/v1/apps/"+id+"/certificates", []string{"ID", "THUMBPRINT", "NOT AFTER", "REVOKED"}, func(m map[string]any) []string {
			return []string{api.Str(m, "id"), short(api.Str(m, "thumbprint"), 20), api.Str(m, "not_after"), api.Str(m, "revoked")}
		})
	}}
	add := &cobra.Command{Use: "add --public-key <file.pem>", Short: "Register an RSA public key (Tier 1)", RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		pem, err := os.ReadFile(keyFile)
		if err != nil {
			return err
		}
		return rt.tier1(cmd, "POST", "/v1/apps/"+id+"/certificates", map[string]any{"public_key_pem": string(pem)}, opts, nil)
	}}
	add.Flags().StringVar(&keyFile, "public-key", "", "PEM file with the RSA public key")
	addApprovalFlags(add.Flags(), &opts)
	revoke := &cobra.Command{Use: "revoke --id <certificate-id>", Short: "Revoke a certificate (Tier 1)", RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		return rt.tier1(cmd, "POST", "/v1/apps/"+id+"/certificates/"+certID+"/revoke", nil, opts, nil)
	}}
	revoke.Flags().StringVar(&certID, "id", "", "certificate id")
	addApprovalFlags(revoke.Flags(), &opts)
	cmd.PersistentFlags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")
	cmd.AddCommand(list, add, revoke)
	return cmd
}

func (rt *Runtime) appIPWhitelistCommand() *cobra.Command {
	var appFlag string
	var ips []string
	opts := approvalOptions{}
	cmd := &cobra.Command{Use: "ip-whitelist", Short: "IP whitelist of the app"}
	set := &cobra.Command{Use: "set --ips <cidr,...>", Short: "Replace the whitelist (≤ 5 CIDR; empty disables) — Tier 1", RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		return rt.tier1(cmd, "PUT", "/v1/apps/"+id+"/ip-whitelist", map[string]any{"ips": ips}, opts, nil)
	}}
	set.Flags().StringSliceVar(&ips, "ips", []string{}, "CIDR entries")
	addApprovalFlags(set.Flags(), &opts)
	test := &cobra.Command{Use: "test <ip>", Short: "Would this IP pass the current whitelist?", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Post(cmd.Context(), "/v1/apps/"+id+"/ip-whitelist/test", map[string]any{"ip": args[0]}, nil)
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) { fmt.Fprintf(w, "%s allowed=%s\n", args[0], api.Str(r.Body, "allowed")) })
		return nil
	}}
	cmd.PersistentFlags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")
	cmd.AddCommand(set, test)
	return cmd
}

func (rt *Runtime) appEntitlementsCommand() *cobra.Command {
	var appFlag string
	var entitlements []string
	cmd := &cobra.Command{Use: "entitlements", Short: "Non-restricted entitlements of the app"}
	set := &cobra.Command{Use: "set <entitlement,...>", Short: "Set entitlements (Tier 0)", RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		if len(args) > 0 {
			entitlements = append(entitlements, strings.Split(strings.Join(args, ","), ",")...)
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Patch(cmd.Context(), "/v1/apps/"+id+"/entitlements", map[string]any{"entitlements": entitlements}, rt.mutating())
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, nil)
		return nil
	}}
	cmd.PersistentFlags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")
	cmd.AddCommand(set)
	return cmd
}

func (rt *Runtime) appAgreementsCommand() *cobra.Command {
	var appFlag, file string
	cmd := &cobra.Command{Use: "agreements", Short: "Terms the installing workspaces must accept"}
	list := &cobra.Command{Use: "list", Short: "List agreements and versions", RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		return rt.listPrint(cmd, "/v1/apps/"+id+"/agreements", []string{"ID", "NAME", "VERSIONS"}, func(m map[string]any) []string {
			return []string{api.Str(m, "id"), api.Str(m, "name"), fmt.Sprint(len(api.List(m, "versions")))}
		})
	}}
	publish := &cobra.Command{Use: "publish -f <terms.md>", Short: "Publish a new agreement version (Tier 0)", RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Post(cmd.Context(), "/v1/apps/"+id+"/agreements/versions", map[string]any{"content": string(content)}, rt.mutating())
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) { fmt.Fprintf(w, "Published version %s\n", api.Str(r.Body, "version", "version")) })
		return nil
	}}
	publish.Flags().StringVarP(&file, "file", "f", "", "file with the agreement text")
	cmd.PersistentFlags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")
	cmd.AddCommand(list, publish)
	return cmd
}

func (rt *Runtime) appImagesCommand() *cobra.Command {
	var appFlag string
	cmd := &cobra.Command{Use: "images", Short: "Store images of the app"}
	list := &cobra.Command{Use: "list", Short: "List images", RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Get(cmd.Context(), "/v1/apps/"+id+"/images", nil)
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, nil)
		return nil
	}}
	upload := &cobra.Command{Use: "upload <file...>", Short: "Upload images", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		results := []any{}
		for _, path := range args {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			part, _ := mw.CreateFormFile("image", filepath.Base(path))
			_, _ = part.Write(data)
			_ = mw.Close()
			r, err := client.Post(cmd.Context(), "/v1/apps/"+id+"/images", nil, &api.Options{RawBody: &buf, ContentType: mw.FormDataContentType()})
			if err != nil {
				return err
			}
			results = append(results, r.Body)
		}
		rt.Printer.Result(map[string]any{"data": results}, func(w io.Writer) { fmt.Fprintf(w, "Uploaded %d image(s)\n", len(results)) })
		return nil
	}}
	highlight := &cobra.Command{Use: "highlight <image-id>", Short: "Highlight an image", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Post(cmd.Context(), "/v1/apps/"+id+"/images/"+args[0]+"/highlight", nil, nil)
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, nil)
		return nil
	}}
	remove := &cobra.Command{Use: "remove <image-id>", Short: "Remove an image", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		if _, err := client.Delete(cmd.Context(), "/v1/apps/"+id+"/images/"+args[0]); err != nil {
			return err
		}
		rt.Printer.Result(map[string]any{"removed": args[0]}, func(w io.Writer) { fmt.Fprintln(w, "Removed") })
		return nil
	}}
	cmd.PersistentFlags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")
	cmd.AddCommand(list, upload, highlight, remove)
	return cmd
}

func (rt *Runtime) appDeployTokensCommand() *cobra.Command {
	var appFlag, name, expires, tokenID string
	var scope []string
	opts := approvalOptions{}
	cmd := &cobra.Command{Use: "deploy-tokens", Short: "CI tokens attenuated to one app (oauth, ip_whitelist, rotate_self)"}
	list := &cobra.Command{Use: "list", Short: "List deploy tokens", RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Get(cmd.Context(), "/v1/apps/"+id+"/deploy-tokens", nil)
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) {
			rows := [][]string{}
			for _, it := range api.List(r.Body, "data") {
				m, _ := it.(map[string]any)
				rows = append(rows, []string{api.Str(m, "id"), api.Str(m, "name"), fmt.Sprint(api.List(m, "scope")), api.Str(m, "live"), api.Str(m, "expires_at")})
			}
			output.Table(w, []string{"ID", "NAME", "SCOPE", "LIVE", "EXPIRES"}, rows)
		})
		return nil
	}}
	create := &cobra.Command{Use: "create --scope oauth,rotate_self [--expires 24h]", Short: "Request a deploy token (Tier 1; delivered once, use it as IUGU_TOKEN in CI)", RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		body := map[string]any{"name": name, "scope": scope}
		if expires != "" {
			d, err := parseDuration(expires)
			if err != nil {
				return err
			}
			body["expires_in"] = int(d.Seconds())
		}
		return rt.tier1(cmd, "POST", "/v1/apps/"+id+"/deploy-tokens", body, opts, nil)
	}}
	create.Flags().StringVar(&name, "name", "CI", "token name")
	create.Flags().StringSliceVar(&scope, "scope", []string{"oauth", "ip_whitelist", "rotate_self"}, "oauth | ip_whitelist | rotate_self")
	create.Flags().StringVar(&expires, "expires", "", "lifetime, e.g. 24h, 7d (max 30d)")
	addApprovalFlags(create.Flags(), &opts)
	revoke := &cobra.Command{Use: "revoke --id <token-id>", Short: "Revoke a deploy token (Tier 0)", RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Post(cmd.Context(), "/v1/apps/"+id+"/deploy-tokens/"+tokenID+"/revoke", nil, nil)
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) { fmt.Fprintln(w, "Revoked") })
		return nil
	}}
	revoke.Flags().StringVar(&tokenID, "id", "", "deploy token id")
	cmd.PersistentFlags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")
	cmd.AddCommand(list, create, revoke)
	return cmd
}
