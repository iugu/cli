package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/iugu/cli/internal/api"
	"github.com/iugu/cli/internal/output"
)

// iugu app billing … — the billing of the developer's app, through Console (Billing itself is never called):
//
//	plan            GET/POST /v1/apps/{id}/billing/plan — the plan with its versions; `create` makes it (draft v1)
//	version         GET  …/plan-versions/{v}            — one version with its prices in full
//	version create  POST …/plan-versions               — the next draft, cloning the default (or --clone-from)
//	prices set      PUT  …/plan-versions/{v}/prices     — replace the whole list (declarative)
//	prices add|update|remove                            — one price of a draft version
//	preview         POST …/plan-versions/{v}/preview    — what one month of usage would cost
//	publish         POST …/plan-versions/{v}/publish    — Tier 1: from then on the version prices subscriptions
//	default         PUT  …/plan/default-version         — Tier 1: which version new subscriptions get
//	discard         DELETE …/plan                       — Tier 1: every active subscription is cancelled
//	events          GET  …/reports/events/summary|failed
//
// Workspace reports live under `iugu billing …` (revenue, invoices, pending, subscriptions).
func (rt *Runtime) appBillingCommand() *cobra.Command {
	var appFlag string
	cmd := &cobra.Command{Use: "billing", Short: "Billing plan, versions and prices of the app (through Console)"}
	cmd.PersistentFlags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")

	plan := &cobra.Command{Use: "plan", Short: "The plan with its versions (create it with `plan create`)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Get(cmd.Context(), "/v1/apps/"+id+"/billing/plan", nil)
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) { printBillingPlan(w, api.Map(r.Body, "plan")) })
		return nil
	}}
	planCreate := &cobra.Command{Use: "create", Short: "Create the plan of the app with an empty draft version 1 (idempotent, Tier 0)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Post(cmd.Context(), "/v1/apps/"+id+"/billing/plan", map[string]any{}, rt.mutating())
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) { printBillingPlan(w, api.Map(r.Body, "plan")) })
		return nil
	}}
	plan.AddCommand(planCreate)

	version := &cobra.Command{Use: "version <version-id>", Short: "One version of the plan, with its prices", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Get(cmd.Context(), "/v1/apps/"+id+"/billing/plan-versions/"+args[0], nil)
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) { printBillingVersion(w, api.Map(r.Body, "plan_version")) })
		return nil
	}}
	var cloneFrom string
	versionCreate := &cobra.Command{Use: "create", Short: "The next draft version, cloning the default's prices (or --clone-from) — one draft at a time (Tier 0)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(appFlag)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		body := map[string]any{}
		if cloneFrom != "" {
			body["clone_from"] = cloneFrom
		}
		r, err := client.Post(cmd.Context(), "/v1/apps/"+id+"/billing/plan-versions", body, rt.mutating())
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) { printBillingVersion(w, api.Map(r.Body, "plan_version")) })
		return nil
	}}
	versionCreate.Flags().StringVar(&cloneFrom, "clone-from", "", "version id to clone the prices from (default: the default version)")
	version.AddCommand(versionCreate)

	cmd.AddCommand(plan, version, rt.appBillingPricesCommand(&appFlag), rt.appBillingPreviewCommand(&appFlag),
		rt.appBillingPublishCommand(&appFlag), rt.appBillingDefaultCommand(&appFlag), rt.appBillingDiscardCommand(&appFlag),
		rt.appBillingEventsCommand(&appFlag))
	return cmd
}

func (rt *Runtime) appBillingPricesCommand(appFlag *string) *cobra.Command {
	cmd := &cobra.Command{Use: "prices", Short: "Prices of a draft version: set the whole list, or add/update/remove one"}
	var input, inputFile string
	set := &cobra.Command{
		Use:   "set <version-id> --input '[…]' | --input-file prices.json",
		Short: "Replace every price of a draft version (declarative, Tier 0)",
		Long: `Each price: { "name", "event", "model": unit|package|bulk|tiered, "billing_unit"?, "config" } where config is
{ "amount" } (unit), { "amount", "size" } (package) or { "tiers": [{ "amount", "maximum_count" }] } (bulk, tiered; the last
tier has maximum_count null). Amounts are strings in BRL. Keep a price's "id" to update it in place.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(*appFlag)
			if err != nil {
				return err
			}
			prices, err := readPricesInput(input, inputFile)
			if err != nil {
				return &output.Exit{Code: output.ExitUsage, Message: err.Error()}
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			r, err := client.Put(cmd.Context(), "/v1/apps/"+id+"/billing/plan-versions/"+args[0]+"/prices", map[string]any{"prices": prices}, rt.mutating())
			if err != nil {
				return err
			}
			rt.Printer.Result(r.Body, func(w io.Writer) { printBillingVersion(w, api.Map(r.Body, "plan_version")) })
			return nil
		},
	}
	set.Flags().StringVar(&input, "input", "", `JSON: {"prices": [...]} or [...]`)
	set.Flags().StringVar(&inputFile, "input-file", "", "file with the JSON")

	var name, event, model, billingUnit, config string
	priceBody := func() (map[string]any, error) {
		body := map[string]any{}
		for key, value := range map[string]string{"name": name, "event": event, "model": model, "billing_unit": billingUnit} {
			if value != "" {
				body[key] = value
			}
		}
		if config != "" {
			parsed, err := parseJSONObject(config)
			if err != nil {
				return nil, &output.Exit{Code: output.ExitUsage, Message: "--config must be a JSON object: " + err.Error()}
			}
			body["config"] = parsed
		}
		return body, nil
	}
	addPriceFlags := func(c *cobra.Command) {
		c.Flags().StringVar(&name, "name", "", "price name (unique in the version)")
		c.Flags().StringVar(&event, "event", "", "event name your app reports")
		c.Flags().StringVar(&model, "model", "", "unit | package | bulk | tiered")
		c.Flags().StringVar(&billingUnit, "billing-unit", "", "optional unit label")
		c.Flags().StringVar(&config, "config", "", `JSON: {"amount":"0.50"} | {"amount":"9.90","size":100} | {"tiers":[{"amount":"0.5","maximum_count":1000},{"amount":"0.3","maximum_count":null}]}`)
	}
	add := &cobra.Command{Use: "add <version-id> --name … --event … --model … --config '{…}'", Short: "Add one price (Tier 0)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(*appFlag)
		if err != nil {
			return err
		}
		body, err := priceBody()
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Post(cmd.Context(), "/v1/apps/"+id+"/billing/plan-versions/"+args[0]+"/prices", body, rt.mutating())
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) { printBillingPrice(w, api.Map(r.Body, "price")) })
		return nil
	}}
	addPriceFlags(add)
	update := &cobra.Command{Use: "update <version-id> <price-id> [--name …] [--config '{…}']", Short: "Change one price; fields not given are kept (Tier 0)", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(*appFlag)
		if err != nil {
			return err
		}
		body, err := priceBody()
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Patch(cmd.Context(), "/v1/apps/"+id+"/billing/plan-versions/"+args[0]+"/prices/"+args[1], body, rt.mutating())
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) { printBillingPrice(w, api.Map(r.Body, "price")) })
		return nil
	}}
	addPriceFlags(update)
	remove := &cobra.Command{Use: "remove <version-id> <price-id>", Short: "Remove one price (Tier 0)", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(*appFlag)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		if _, err := client.Delete(cmd.Context(), "/v1/apps/"+id+"/billing/plan-versions/"+args[0]+"/prices/"+args[1]); err != nil {
			return err
		}
		rt.Printer.Result(map[string]any{"removed": args[1], "plan_version_id": args[0]}, func(w io.Writer) { fmt.Fprintf(w, "price %s removed from version %s\n", args[1], args[0]) })
		return nil
	}}
	cmd.AddCommand(set, add, update, remove)
	return cmd
}

func (rt *Runtime) appBillingPreviewCommand(appFlag *string) *cobra.Command {
	var kv []string
	var input string
	cmd := &cobra.Command{
		Use:   "preview <version-id> --quantity <event>=<n> …",
		Short: "Dry run: what one month of usage would cost with this version's prices (nothing is stored)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(*appFlag)
			if err != nil {
				return err
			}
			quantities := map[string]any{}
			if input != "" {
				parsed, err := parseJSONObject(input)
				if err != nil {
					return &output.Exit{Code: output.ExitUsage, Message: "--input must be a JSON object of event → quantity: " + err.Error()}
				}
				quantities = parsed
			}
			for _, pair := range kv {
				key, value, ok := strings.Cut(pair, "=")
				if !ok {
					return &output.Exit{Code: output.ExitUsage, Message: "--quantity expects event=number, got " + pair}
				}
				quantities[key] = value
			}
			if len(quantities) == 0 {
				return &output.Exit{Code: output.ExitUsage, Message: "give at least one --quantity event=number"}
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			r, err := client.Post(cmd.Context(), "/v1/apps/"+id+"/billing/plan-versions/"+args[0]+"/preview", map[string]any{"quantities": quantities}, nil)
			if err != nil {
				return err
			}
			rt.Printer.Result(r.Body, func(w io.Writer) {
				preview := api.Map(r.Body, "preview")
				rows := [][]string{}
				for _, l := range api.List(preview, "lines") {
					line, _ := l.(map[string]any)
					rows = append(rows, []string{api.Str(line, "price_name"), api.Str(line, "event"), api.Str(line, "model"), api.Str(line, "quantity"), "R$ " + api.Str(line, "amount")})
				}
				output.Table(w, []string{"PRICE", "EVENT", "MODEL", "QUANTITY", "AMOUNT"}, rows)
				fmt.Fprintf(w, "\nTotal: R$ %s\n", api.Str(preview, "total"))
			})
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&kv, "quantity", nil, "event=number (repeatable)")
	cmd.Flags().StringVar(&input, "input", "", `JSON object {"<event>": number}`)
	return cmd
}

func (rt *Runtime) appBillingPublishCommand(appFlag *string) *cobra.Command {
	var setDefault bool
	opts := approvalOptions{}
	cmd := &cobra.Command{
		Use:   "publish <version-id> [--default]",
		Short: "Publish a draft version — from then on it prices subscriptions (Tier 1: one approval)",
		Long: `The app must be billable (iugu app publish --billable) and the version must have prices. --default also makes it the
version every new subscription gets. Existing subscriptions keep their version.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(*appFlag)
			if err != nil {
				return err
			}
			return rt.tier1(cmd, "POST", "/v1/apps/"+id+"/billing/plan-versions/"+args[0]+"/publish", map[string]any{"set_as_default": setDefault}, opts, nil)
		},
	}
	cmd.Flags().BoolVar(&setDefault, "default", false, "also make it the default version")
	addApprovalFlags(cmd.Flags(), &opts)
	return cmd
}

func (rt *Runtime) appBillingDefaultCommand(appFlag *string) *cobra.Command {
	opts := approvalOptions{}
	cmd := &cobra.Command{Use: "default <version-id>", Short: "Make a published version the one new subscriptions get (Tier 1)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(*appFlag)
		if err != nil {
			return err
		}
		return rt.tier1(cmd, "PUT", "/v1/apps/"+id+"/billing/plan/default-version", map[string]any{"plan_version_id": args[0]}, opts, nil)
	}}
	addApprovalFlags(cmd.Flags(), &opts)
	return cmd
}

func (rt *Runtime) appBillingDiscardCommand(appFlag *string) *cobra.Command {
	opts := approvalOptions{}
	cmd := &cobra.Command{Use: "discard", Short: "Discard the plan — every active subscription is cancelled (Tier 1)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(*appFlag)
		if err != nil {
			return err
		}
		return rt.tier1(cmd, "DELETE", "/v1/apps/"+id+"/billing/plan", nil, opts, nil)
	}}
	addApprovalFlags(cmd.Flags(), &opts)
	return cmd
}

func (rt *Runtime) appBillingEventsCommand(appFlag *string) *cobra.Command {
	var from, to string
	cmd := &cobra.Command{Use: "events", Short: "Usage events of the app as Billing sees them"}
	summary := &cobra.Command{Use: "summary [--from YYYY-MM-DD] [--to YYYY-MM-DD]", Short: "Quantity per day and event, across the workspaces that use the app", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(*appFlag)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Get(cmd.Context(), "/v1/apps/"+id+"/billing/reports/events/summary", dateQuery(from, to))
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) {
			rows := [][]string{}
			for _, s := range api.List(r.Body, "summaries") {
				m, _ := s.(map[string]any)
				rows = append(rows, []string{api.Str(m, "date"), api.Str(m, "event"), api.Str(m, "quantity"), api.Str(m, "workspaces")})
			}
			fmt.Fprintf(w, "%s → %s\n", api.Str(r.Body, "from"), api.Str(r.Body, "to"))
			output.Table(w, []string{"DATE", "EVENT", "QUANTITY", "WORKSPACES"}, rows)
		})
		return nil
	}}
	failed := &cobra.Command{Use: "failed [--from …] [--to …]", Short: "Events Billing refused, grouped by reason", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		id, err := rt.appArg(*appFlag)
		if err != nil {
			return err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return err
		}
		r, err := client.Get(cmd.Context(), "/v1/apps/"+id+"/billing/reports/events/failed", dateQuery(from, to))
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) {
			rows := [][]string{}
			for _, f := range api.List(r.Body, "failures") {
				m, _ := f.(map[string]any)
				rows = append(rows, []string{api.Str(m, "message"), api.Str(m, "count"), api.Str(m, "workspaces")})
			}
			output.Table(w, []string{"REASON", "COUNT", "WORKSPACES"}, rows)
		})
		return nil
	}}
	for _, c := range []*cobra.Command{summary, failed} {
		c.Flags().StringVar(&from, "from", "", "YYYY-MM-DD (default 30 days before --to)")
		c.Flags().StringVar(&to, "to", "", "YYYY-MM-DD (default today)")
	}
	cmd.AddCommand(summary, failed)
	return cmd
}

// iugu billing … — the workspace's side: what its apps earned and what it owes for the apps it uses.
func (rt *Runtime) billingCommand() *cobra.Command {
	var workspace string
	cmd := &cobra.Command{Use: "billing", Short: "Billing reports of the workspace: revenue of your apps, invoices and subscriptions of your own"}
	cmd.PersistentFlags().StringVar(&workspace, "workspace", "", "workspace id (default: iugu.toml, profile or the only consented one)")

	get := func(cmd *cobra.Command, path string, query url.Values) (*api.Response, string, error) {
		ws, err := rt.workspaceArg(workspace)
		if err != nil {
			return nil, "", err
		}
		client, err := rt.apiClient(cmd.Context())
		if err != nil {
			return nil, "", err
		}
		r, err := client.Get(cmd.Context(), "/v1/workspaces/"+ws+"/billing/reports/"+path, query)
		return r, ws, err
	}

	revenue := &cobra.Command{Use: "revenue", Short: "What the workspace's apps billed, received and still have outstanding, per competency and app", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		r, _, err := get(cmd, "revenue", nil)
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) {
			rows := [][]string{}
			for _, p := range api.List(r.Body, "periods") {
				period, _ := p.(map[string]any)
				rows = append(rows, []string{api.Str(period, "competency"), "", "R$ " + api.Str(period, "total_amount"), "R$ " + api.Str(period, "paid_amount"), "R$ " + api.Str(period, "pending_amount")})
				for _, a := range api.List(period, "apps") {
					app, _ := a.(map[string]any)
					rows = append(rows, []string{"", api.Str(app, "plan_name") + " (" + api.Str(app, "app_id") + ")", "R$ " + api.Str(app, "total_amount"), "R$ " + api.Str(app, "paid_amount"), "R$ " + api.Str(app, "pending_amount")})
				}
			}
			output.Table(w, []string{"COMPETENCY", "APP / PLAN", "BILLED", "RECEIVED", "OUTSTANDING"}, rows)
		})
		return nil
	}}
	var status, competency string
	invoices := &cobra.Command{Use: "invoices [invoice-id]", Short: "Invoices the workspace owes for the apps it uses (one invoice: its lines and payments)", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 {
			r, _, err := get(cmd, "invoices/"+args[0], nil)
			if err != nil {
				return err
			}
			rt.Printer.Result(r.Body, func(w io.Writer) { printBillingInvoice(w, api.Map(r.Body, "invoice")) })
			return nil
		}
		query := url.Values{}
		if status != "" {
			query.Set("status", status)
		}
		if competency != "" {
			query.Set("competency", competency)
		}
		r, _, err := get(cmd, "invoices", query)
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) {
			rows := [][]string{}
			for _, i := range api.List(r.Body, "invoices") {
				inv, _ := i.(map[string]any)
				rows = append(rows, []string{api.Str(inv, "id"), api.Str(inv, "competency"), api.Str(inv, "plan_name"), api.Str(inv, "status"), "R$ " + api.Str(inv, "total_amount"), "R$ " + api.Str(inv, "amount_due")})
			}
			output.Table(w, []string{"ID", "COMPETENCY", "PLAN", "STATUS", "TOTAL", "DUE"}, rows)
		})
		return nil
	}}
	invoices.Flags().StringVar(&status, "status", "", "draft,issued,issued_and_paid,canceled,in_protest")
	invoices.Flags().StringVar(&competency, "competency", "", "YYYY-MM")
	var appID string
	pending := &cobra.Command{Use: "pending", Short: "What the workspace still owes (issued and draft invoices)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		query := url.Values{}
		if appID != "" {
			query.Set("app_id", appID)
		}
		r, _, err := get(cmd, "pending-amount", query)
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) {
			fmt.Fprintf(w, "R$ %s pending on %s invoice(s)\n", api.Str(r.Body, "pending_amount"), api.Str(r.Body, "invoices_count"))
		})
		return nil
	}}
	pending.Flags().StringVar(&appID, "app", "", "only this app")
	subscriptions := &cobra.Command{Use: "subscriptions", Short: "The apps the workspace subscribes to and the plan version each one is on", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		r, _, err := get(cmd, "subscriptions", nil)
		if err != nil {
			return err
		}
		rt.Printer.Result(r.Body, func(w io.Writer) {
			rows := [][]string{}
			for _, s := range api.List(r.Body, "subscriptions") {
				sub, _ := s.(map[string]any)
				active := "ended " + api.Str(sub, "ended_at")
				if b, ok := sub["active"].(bool); ok && b {
					active = "active"
				}
				rows = append(rows, []string{api.Str(sub, "app_id"), "v" + api.Str(sub, "plan_version"), active, api.Str(sub, "created_at")})
			}
			output.Table(w, []string{"APP", "VERSION", "STATUS", "SINCE"}, rows)
		})
		return nil
	}}
	cmd.AddCommand(revenue, invoices, pending, subscriptions)
	return cmd
}

func dateQuery(from, to string) url.Values {
	query := url.Values{}
	if from != "" {
		query.Set("from", from)
	}
	if to != "" {
		query.Set("to", to)
	}
	return query
}

func printBillingPlan(w io.Writer, plan map[string]any) {
	fmt.Fprintf(w, "%s — plan %s · %s · %s\n", api.Str(plan, "name"), api.Str(plan, "id"), api.Str(plan, "status"), api.Str(plan, "currency"))
	rows := [][]string{}
	for _, v := range api.List(plan, "versions") {
		version, _ := v.(map[string]any)
		def := ""
		if b, ok := version["default"].(bool); ok && b {
			def = "default"
		}
		rows = append(rows, []string{"v" + api.Str(version, "version"), api.Str(version, "id"), api.Str(version, "status"), api.Str(version, "prices_count"), def})
	}
	output.Table(w, []string{"VERSION", "ID", "STATUS", "PRICES", ""}, rows)
}

func printBillingVersion(w io.Writer, version map[string]any) {
	def := ""
	if b, ok := version["default"].(bool); ok && b {
		def = " · default"
	}
	fmt.Fprintf(w, "v%s %s · %s%s\n", api.Str(version, "version"), api.Str(version, "id"), api.Str(version, "status"), def)
	rows := [][]string{}
	for _, p := range api.List(version, "prices") {
		price, _ := p.(map[string]any)
		rows = append(rows, []string{api.Str(price, "id"), api.Str(price, "name"), api.Str(price, "event"), api.Str(price, "model"), describePriceConfig(price)})
	}
	output.Table(w, []string{"ID", "NAME", "EVENT", "MODEL", "CONFIG"}, rows)
}

func printBillingPrice(w io.Writer, price map[string]any) {
	fmt.Fprintf(w, "%s  %s · %s · %s · %s\n", api.Str(price, "id"), api.Str(price, "name"), api.Str(price, "event"), api.Str(price, "model"), describePriceConfig(price))
}

func describePriceConfig(price map[string]any) string {
	config := api.Map(price, "config")
	switch api.Str(price, "model") {
	case "unit":
		return "R$ " + api.Str(config, "amount") + " per unit"
	case "package":
		return "R$ " + api.Str(config, "amount") + " per " + api.Str(config, "size")
	default:
		parts := []string{}
		for _, t := range api.List(config, "tiers") {
			tier, _ := t.(map[string]any)
			max := api.Str(tier, "maximum_count")
			if max == "" {
				max = "∞"
			}
			parts = append(parts, "≤"+max+": R$ "+api.Str(tier, "amount"))
		}
		return strings.Join(parts, ", ")
	}
}

func printBillingInvoice(w io.Writer, invoice map[string]any) {
	fmt.Fprintf(w, "%s · %s · %s (%s v%s) · total R$ %s · paid R$ %s · due R$ %s\n", api.Str(invoice, "id"), api.Str(invoice, "competency"), api.Str(invoice, "status"),
		api.Str(invoice, "plan_name"), api.Str(invoice, "plan_version"), api.Str(invoice, "total_amount"), api.Str(invoice, "paid_amount"), api.Str(invoice, "amount_due"))
	if lines := api.List(invoice, "lines"); len(lines) > 0 {
		rows := [][]string{}
		for _, l := range lines {
			line, _ := l.(map[string]any)
			rows = append(rows, []string{api.Str(line, "price_name"), api.Str(line, "event"), api.Str(line, "quantity"), "R$ " + api.Str(line, "amount")})
		}
		output.Table(w, []string{"PRICE", "EVENT", "QUANTITY", "AMOUNT"}, rows)
	}
	if payments := api.List(invoice, "payments"); len(payments) > 0 {
		rows := [][]string{}
		for _, p := range payments {
			payment, _ := p.(map[string]any)
			rows = append(rows, []string{api.Str(payment, "status"), "R$ " + api.Str(payment, "total_amount"), api.Str(payment, "due_at"), api.Str(payment, "url")})
		}
		output.Table(w, []string{"PAYMENT", "AMOUNT", "DUE", "URL"}, rows)
	}
}

// parseJSONObject decodes a JSON object given inline.
func parseJSONObject(raw string) (map[string]any, error) {
	var parsed map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

// readPricesInput accepts {"prices": [...]} or a bare JSON array, inline or from a file ("-" = stdin).
func readPricesInput(input, inputFile string) ([]any, error) {
	raw := []byte(input)
	if inputFile != "" {
		var err error
		if inputFile == "-" {
			raw, err = io.ReadAll(os.Stdin)
		} else {
			raw, err = os.ReadFile(inputFile)
		}
		if err != nil {
			return nil, fmt.Errorf("--input-file: %v", err)
		}
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("give the prices with --input '[…]' or --input-file <path>")
	}
	var list []any
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var wrapped struct {
		Prices []any `json:"prices"`
	}
	if err := json.Unmarshal(raw, &wrapped); err != nil || wrapped.Prices == nil {
		return nil, errors.New(`the input must be a JSON array of prices or {"prices": [...]}`)
	}
	return wrapped.Prices, nil
}
