package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/iugu/cli/internal/api"
	"github.com/iugu/cli/internal/output"
	"github.com/spf13/cobra"
)

// iugu call <app> <action> — calls an action of a provider app AS YOU, through Console's bridge
// (POST /v1/apps/{id}/actions/{name}/call): the same call Iugu for AI makes for the tool `<tag>__<name>`.
// Console checks the workspace, the installation and your own permission for the action (GIA), then calls the
// provider with a token that carries your identity; the provider's answer is printed as it came. Elevated
// actions (config/transfers) are gated: the person confirms the exact call on a Console page, then the same
// call succeeds once — a human at the terminal gets the page opened and the CLI retries; an agent gets exit 7
// with the URL and a next step. Billing's plan configuration and reports are reached this way (`iugu call billing …`).
func (rt *Runtime) callCommand() *cobra.Command {
	var workspace, input, inputFile string
	var kv []string
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "call <app id|tag|slug> <action> [--arg k=v …] [--input '{…}' | --input-file f] [--workspace <id>]",
		Short: "Call an action of a provider app as you (Billing: plans, prices, publish, reports)",
		Long: "Calls one action of an app that publishes an actions manifest, as the current principal — the same call Iugu for AI makes " +
			"for the tool `<tag>__<name>`. Console verifies your permission for the action in the workspace and calls the provider with " +
			"your identity; the answer is printed as the provider gave it. Elevated actions answer with a confirmation page: a human at the " +
			"terminal has it opened and the CLI retries once confirmed; an agent gets the URL (exit 7) and re-runs the same command after " +
			"the person confirmed.\n\nDiscover actions with `iugu app actions list --app <id>`; e.g. `iugu call billing get_plan --arg app_id=<your app>`.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			provider, err := rt.resolveProvider(cmd.Context(), client, args[0])
			if err != nil {
				return err
			}
			manifest, err := rt.fetchActionsManifest(cmd.Context(), client, provider, false)
			if err != nil {
				return err
			}
			action, ok := findAction(manifest, args[1])
			if !ok {
				return &output.Exit{Code: output.ExitUsage, Message: fmt.Sprintf("%s has no action %q (actions: %s)", api.Str(manifest, "app", "tag"), args[1], strings.Join(actionNames(manifest), ", "))}
			}
			arguments, err := collectArguments(input, inputFile, kv)
			if err != nil {
				return &output.Exit{Code: output.ExitUsage, Message: err.Error()}
			}
			if problems := checkInputs(action, arguments); len(problems) > 0 {
				return &output.Exit{Code: output.ExitUsage, Message: "inputs do not match the action's schema:\n  " + strings.Join(problems, "\n  ")}
			}
			ws := workspace
			if ws == "" {
				ws, _ = rt.workspaceArg("") // the profile's workspace, if any; Console needs it only when several apply
			}
			body := map[string]any{"arguments": arguments}
			if ws != "" {
				body["workspace_id"] = ws
			}
			path := "/v1/apps/" + provider + "/actions/" + api.Str(action, "name") + "/call"
			deadline := time.Now().Add(wait)
			gated := false
			for {
				r, err := client.Post(cmd.Context(), path, body, nil)
				var apiErr *api.Error
				if err == nil {
					rt.Printer.Result(r.Body, func(w io.Writer) {
						// the provider's answer as it came — pretty JSON is the honest human form for an arbitrary document
						pretty, _ := json.MarshalIndent(r.Body, "", "  ")
						fmt.Fprintln(w, string(pretty))
					})
					return nil
				}
				if !errors.As(err, &apiErr) || apiErr.Code != "step_up_required" {
					return err
				}
				stepURL := api.Str(apiErr.Details, "step_up_url")
				if rt.IsAgent() || stepURL == "" {
					return &output.Exit{Code: output.ExitHumanGate, Message: fmt.Sprintf("%s\nOpen %s — the person confirms this exact call there. Then run this command again with the same arguments.", apiErr.Message, stepURL),
						Payload: map[string]any{"error": apiErr, "url": stepURL, "next_step": "Show url to the person: they review this exact call and confirm with a verification code. Then re-run this exact command (same arguments) — the confirmation is spent on one call."}}
				}
				if !gated {
					gated = true
					rt.Printer.Line("This action needs your confirmation (%s level). Review and confirm it here:\n\n  %s\n\nWaiting…", api.Str(apiErr.Details, "acr"), stepURL)
					_ = openBrowser(stepURL)
				}
				if time.Now().After(deadline) {
					return &output.Exit{Code: output.ExitHumanGate, Message: "not confirmed in time; run the command again after confirming " + stepURL}
				}
				select {
				case <-cmd.Context().Done():
					return cmd.Context().Err()
				case <-time.After(gatePollInterval):
				}
			}
		},
	}
	cmd.Flags().StringVar(&workspace, "workspace", "", "workspace the call is about (default: the profile's; required when several apply)")
	cmd.Flags().StringVar(&input, "input", "", "inputs as a JSON object")
	cmd.Flags().StringVar(&inputFile, "input-file", "", "inputs as a JSON object read from a file ('-' = stdin)")
	cmd.Flags().StringArrayVar(&kv, "arg", nil, "one input, k=v (JSON values are parsed: --arg set_as_default=true, --arg 'prices=[…]')")
	cmd.Flags().DurationVar(&wait, "wait", 10*time.Minute, "how long to wait for the person's confirmation (humans only)")
	return cmd
}

// resolveProvider accepts an app id, or a tag/slug looked up in the action catalog (every app that implements actions):
// a catalog entry whose tag or slug equals the reference wins; otherwise the reference is taken as the id.
func (rt *Runtime) resolveProvider(ctx context.Context, client *api.Client, ref string) (string, error) {
	query := url.Values{}
	query.Set("q", ref)
	items, err := client.ListAll(ctx, "/v1/catalog/actions", query, 200)
	if err == nil {
		for _, it := range items {
			m, _ := it.(map[string]any)
			app := api.Map(m, "app")
			if api.Str(app, "tag") == ref || api.Str(app, "slug") == ref {
				return api.Str(app, "id"), nil
			}
		}
	}
	return ref, nil
}
