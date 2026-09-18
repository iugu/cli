package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/iugu/cli/internal/api"
	"github.com/iugu/cli/internal/output"
	"github.com/spf13/cobra"
)

// iugu app actions — the actions manifest an app publishes at GET {service_api_url}/actions (spec iugu.actions/v1)
// as Console reads it, and a way to exercise one action against the provider while building it.
//
//	list   GET /v1/apps/{id}/actions[?refresh=true] — accepted actions (with the schemas Console derived), triggers,
//	       and every problem that excluded something.
//	call   mints the app's OWN token (client_credentials, aud = the app) and performs the action's HTTP operation
//	       exactly as a consumer would — path/query/body per the manifest, `Workspace` header — printing the answer.
//	       It is a test aid for the provider's author: the caller is the app itself, so the provider's /verify sees
//	       sub = app:<id> and `acr` rules for humans are not exercised. Humans reach the same action through
//	       Iugu for AI (tool <tag>__<name>), where the person's identity and step-ups apply.
func (rt *Runtime) appActionsCommand() *cobra.Command {
	var appFlag string
	cmd := &cobra.Command{Use: "actions", Short: "The app's actions manifest (iugu.actions/v1) as Console reads it, and a way to test one action"}
	cmd.PersistentFlags().StringVar(&appFlag, "app", "", "app id (default: iugu.toml)")

	var refresh bool
	list := &cobra.Command{
		Use:   "list [app-id]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Accepted actions, triggers and the problems Console found in GET {service_api_url}/actions",
		Long: `Console fetches the manifest at {service_api_url}/actions (or the service URL itself when it already ends in /actions),
validates it and keeps the result: only actions whose authorization.action is one of the app's implemented actions and whose
url is on the manifest's origin are accepted; everything else is listed under problems with its path and reason. The
document is cached per its Cache-Control (default 5 minutes); --refresh fetches it again now (own apps only).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := rt.appArg(appFromArgs(args, appFlag))
			if err != nil {
				return err
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			body, err := rt.fetchActionsManifest(cmd.Context(), client, id, refresh)
			if err != nil {
				return err
			}
			rt.Printer.Result(body, func(w io.Writer) { printActionsManifest(w, body) })
			return nil
		},
	}
	list.Flags().BoolVar(&refresh, "refresh", false, "fetch the manifest again now (publisher only)")

	var input, inputFile, workspace, token string
	var kv []string
	call := &cobra.Command{
		Use:   "call <name> [app-id]",
		Args:  cobra.RangeArgs(1, 2),
		Short: "Perform one action against the provider with the app's own token (test aid; humans go through Iugu for AI)",
		Long: `Finds <name> (or the tool name <tag>__<name>) in the manifest Console accepted, mints a short-lived token of the app
itself (POST /v1/apps/{id}/tokens: client_credentials, audience = the app) and performs the action's HTTP operation as a
consumer would: path parameters into :name placeholders, query parameters on the URL, body parameters as a JSON object,
plus the Workspace header (short id). Inputs come from --input '{json}', --input-file <path> or repeated --arg name=value.

The caller is the app itself, so the provider's /verify sees sub = app:<id> and the action's acr requirement for
humans is not exercised (app tokens carry no acr): a 401 insufficient_user_authentication answer means the provider
enforces it — through Iugu for AI the person confirms that call. --token <jwt> uses another bearer token instead (for
example a user token obtained by your app's login) to exercise those rules.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			id, err := rt.appArg(appFromArgs(args[1:], appFlag))
			if err != nil {
				return err
			}
			ws, err := rt.workspaceArg(workspace)
			if err != nil {
				return err
			}
			arguments, err := collectArguments(input, inputFile, kv)
			if err != nil {
				return &output.Exit{Code: output.ExitUsage, Message: err.Error()}
			}
			client, err := rt.apiClient(cmd.Context())
			if err != nil {
				return err
			}
			manifest, err := rt.fetchActionsManifest(cmd.Context(), client, id, false)
			if err != nil {
				return err
			}
			action, ok := findAction(manifest, name)
			if !ok {
				names := actionNames(manifest)
				problems := len(api.List(manifest, "problems"))
				return &output.Exit{Code: output.ExitUsage, Message: fmt.Sprintf("no accepted action %q in the manifest (accepted: %s; %d problem(s) — see `iugu app actions list`)",
					name, strings.Join(names, ", "), problems), Payload: map[string]any{"error": map[string]any{"code": "unknown_action", "accepted": names}}}
			}
			if problems := checkInputs(action, arguments); len(problems) > 0 {
				return &output.Exit{Code: output.ExitUsage, Message: "input does not match the action: " + strings.Join(problems, "; "),
					Payload: map[string]any{"error": map[string]any{"code": "invalid_input", "problems": problems, "input_schema": action["input_schema"]}}}
			}
			bearer := token
			if bearer == "" {
				r, err := client.Post(cmd.Context(), "/v1/apps/"+id+"/tokens", map[string]any{}, nil)
				if err != nil {
					return err
				}
				bearer = api.Str(r.Body, "access_token")
			}
			req, err := buildActionRequest(action, arguments)
			if err != nil {
				return &output.Exit{Code: output.ExitUsage, Message: err.Error()}
			}
			result, err := rt.performAction(cmd.Context(), req, bearer, ws)
			if err != nil {
				return err
			}
			payload := map[string]any{
				"tool_name":     api.Str(action, "tool_name"),
				"action":        api.Str(action, "authorization", "action"),
				"acr":           api.Str(action, "authorization", "acr"),
				"request":       map[string]any{"method": req.method, "url": req.url, "body": req.body, "workspace": ws, "as": callerLabel(token)},
				"response":      map[string]any{"status": result.status, "body": result.body, "www_authenticate": result.wwwAuthenticate},
				"ok":            result.status >= 200 && result.status < 300,
				"step_up_human": strings.Contains(result.wwwAuthenticate, "insufficient_user_authentication"),
			}
			rt.Printer.Result(payload, func(w io.Writer) { printActionResult(w, payload) })
			if result.status >= 200 && result.status < 300 {
				return nil
			}
			return &output.Exit{Code: output.ExitFailure, Message: fmt.Sprintf("%s answered HTTP %d", req.url, result.status)}
		},
	}
	call.Flags().StringVar(&input, "input", "", "arguments as a JSON object")
	call.Flags().StringVar(&inputFile, "input-file", "", "arguments as a JSON object read from a file (- for stdin)")
	call.Flags().StringArrayVar(&kv, "arg", nil, "one argument as name=value (repeatable; JSON values are parsed, others are strings)")
	call.Flags().StringVar(&workspace, "workspace", "", "workspace short id sent in the Workspace header (default: iugu.toml / profile)")
	call.Flags().StringVar(&token, "token", "", "use this bearer token instead of minting the app's own token (e.g. a user token to exercise acr rules)")

	cmd.AddCommand(list, call)
	return cmd
}

func (rt *Runtime) fetchActionsManifest(ctx context.Context, client *api.Client, id string, refresh bool) (map[string]any, error) {
	query := url.Values{}
	if refresh {
		query.Set("refresh", "true")
	}
	r, err := client.Get(ctx, "/v1/apps/"+id+"/actions", query)
	if err != nil {
		return nil, err
	}
	return r.Body, nil
}

func actionNames(manifest map[string]any) []string {
	var names []string
	for _, a := range api.List(manifest, "actions") {
		if m, ok := a.(map[string]any); ok {
			names = append(names, api.Str(m, "name"))
		}
	}
	return names
}

// findAction accepts the action name or its tool name (<tag>__<name>).
func findAction(manifest map[string]any, name string) (map[string]any, bool) {
	for _, a := range api.List(manifest, "actions") {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		if api.Str(m, "name") == name || api.Str(m, "tool_name") == name {
			return m, true
		}
	}
	return nil, false
}

// collectArguments merges --input, --input-file and --arg (later sources win).
func collectArguments(input, inputFile string, kv []string) (map[string]any, error) {
	arguments := map[string]any{}
	decode := func(raw []byte, from string) error {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 {
			return nil
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return fmt.Errorf("%s must be a JSON object: %v", from, err)
		}
		for k, v := range m {
			arguments[k] = v
		}
		return nil
	}
	if input != "" {
		if err := decode([]byte(input), "--input"); err != nil {
			return nil, err
		}
	}
	if inputFile != "" {
		var raw []byte
		var err error
		if inputFile == "-" {
			raw, err = io.ReadAll(os.Stdin)
		} else {
			raw, err = os.ReadFile(inputFile)
		}
		if err != nil {
			return nil, fmt.Errorf("--input-file: %v", err)
		}
		if err := decode(raw, "--input-file"); err != nil {
			return nil, err
		}
	}
	for _, pair := range kv {
		name, value, ok := strings.Cut(pair, "=")
		if !ok || name == "" {
			return nil, fmt.Errorf("--arg expects name=value, got %q", pair)
		}
		var parsed any
		if err := json.Unmarshal([]byte(value), &parsed); err == nil {
			arguments[name] = parsed // numbers, booleans, objects, arrays, quoted strings
		} else {
			arguments[name] = value
		}
	}
	return arguments, nil
}

// checkInputs: required inputs present, no unknown names (the schema Console derived from `parameters`).
func checkInputs(action map[string]any, arguments map[string]any) []string {
	var problems []string
	schema := api.Map(action, "input_schema")
	properties := api.Map(schema, "properties")
	for _, req := range api.List(schema, "required") {
		name, _ := req.(string)
		if _, ok := arguments[name]; name != "" && !ok {
			problems = append(problems, fmt.Sprintf("missing required input %q", name))
		}
	}
	if properties != nil {
		for name := range arguments {
			if _, ok := properties[name]; !ok {
				problems = append(problems, fmt.Sprintf("unknown input %q (inputs: %s)", name, strings.Join(sortedKeysAny(properties), ", ")))
			}
		}
	}
	sort.Strings(problems)
	return problems
}

func sortedKeysAny(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type actionRequest struct {
	method string
	url    string
	body   map[string]any // nil for GET/DELETE or when no body input
}

// buildActionRequest places inputs where the manifest says: path → ":name" placeholders, query → query string,
// body → a JSON object (GET and DELETE carry no body).
func buildActionRequest(action map[string]any, arguments map[string]any) (*actionRequest, error) {
	method := strings.ToUpper(api.Str(action, "method"))
	target := api.Str(action, "url")
	query := url.Values{}
	body := map[string]any{}
	for _, p := range api.List(action, "parameters") {
		param, ok := p.(map[string]any)
		if !ok || api.Str(param, "direction") != "ParamInput" {
			continue
		}
		name := api.Str(param, "name")
		value, present := arguments[name]
		if !present {
			continue
		}
		switch api.Str(param, "source") {
		case "path":
			target = strings.Replace(target, ":"+name, url.PathEscape(stringify(value)), 1)
		case "query":
			query.Set(name, stringify(value))
		default:
			body[name] = value
		}
	}
	if strings.Contains(target, "/:") {
		return nil, fmt.Errorf("missing path parameter in %s", target)
	}
	if len(query) > 0 {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + query.Encode()
	}
	req := &actionRequest{method: method, url: target}
	if method != "GET" && method != "DELETE" && len(body) > 0 {
		req.body = body
	}
	return req, nil
}

func stringify(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strings.TrimSuffix(strings.TrimSuffix(fmt.Sprintf("%f", x), "000000"), ".")
	case bool:
		return fmt.Sprintf("%t", x)
	default:
		raw, _ := json.Marshal(v)
		return string(raw)
	}
}

type actionResult struct {
	status          int
	body            any
	wwwAuthenticate string
}

func (rt *Runtime) performAction(ctx context.Context, req *actionRequest, bearer, workspace string) (*actionResult, error) {
	var payload io.Reader
	if req.body != nil {
		raw, _ := json.Marshal(req.body)
		payload = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, req.method, req.url, payload)
	if err != nil {
		return nil, &output.Exit{Code: output.ExitUsage, Message: fmt.Sprintf("invalid action URL: %v", err)}
	}
	httpReq.Header.Set("Authorization", "Bearer "+bearer)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "iugu-cli/"+Version+" (app actions call)")
	if workspace != "" {
		httpReq.Header.Set("Workspace", workspace)
	}
	if req.body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	resp, err := rt.HTTP.Do(httpReq)
	if err != nil {
		return nil, &output.Exit{Code: output.ExitFailure, Message: fmt.Sprintf("the provider did not answer: %v", err),
			Payload: map[string]any{"error": map[string]any{"code": "provider_unavailable", "message": err.Error(), "url": req.url}}}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	result := &actionResult{status: resp.StatusCode, wwwAuthenticate: resp.Header.Get("WWW-Authenticate")}
	var decoded any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			decoded = map[string]any{"raw": string(raw)}
		}
	}
	result.body = decoded
	return result, nil
}

func callerLabel(token string) string {
	if token != "" {
		return "the given token"
	}
	return "the app itself (client_credentials)"
}

func printActionsManifest(w io.Writer, body map[string]any) {
	app := api.Map(body, "app")
	manifest := api.Map(body, "manifest")
	fmt.Fprintf(w, "%s (%s) · actions_provider: %s\n", api.Str(app, "name"), api.Str(app, "tag"), api.Str(app, "actions_provider"))
	fmt.Fprintf(w, "manifest %s · %s · spec %s", api.Str(manifest, "url"), api.Str(manifest, "status"), orDash(api.Str(manifest, "spec")))
	if fetched := api.Str(manifest, "fetched_at"); fetched != "" {
		fmt.Fprintf(w, " · fetched %s", fetched)
	}
	fmt.Fprintln(w)
	actions := api.List(body, "actions")
	if len(actions) == 0 {
		fmt.Fprintln(w, "\nno accepted actions")
	} else {
		fmt.Fprintf(w, "\n%d action(s):\n", len(actions))
	}
	for _, a := range actions {
		m, _ := a.(map[string]any)
		auth := api.Map(m, "authorization")
		fmt.Fprintf(w, "  %-40s %s %s\n", api.Str(m, "tool_name"), api.Str(m, "method"), api.Str(m, "url"))
		fmt.Fprintf(w, "  %-40s %s · requires %s · acr %s\n", "", api.Str(m, "title"), api.Str(auth, "action"), api.Str(auth, "acr"))
		var ins, outs []string
		for _, p := range api.List(m, "parameters") {
			param, _ := p.(map[string]any)
			label := api.Str(param, "name") + ":" + api.Str(param, "type")
			if api.Str(param, "direction") == "ParamInput" {
				if api.Str(param, "required") == "true" {
					label += "*"
				}
				ins = append(ins, label+"@"+api.Str(param, "source"))
			} else {
				outs = append(outs, label)
			}
		}
		fmt.Fprintf(w, "  %-40s in: %s · out: %s\n", "", orDash(strings.Join(ins, " ")), orDash(strings.Join(outs, " ")))
	}
	if triggers := api.List(body, "triggers"); len(triggers) > 0 {
		names := make([]string, 0, len(triggers))
		for _, t := range triggers {
			m, _ := t.(map[string]any)
			names = append(names, api.Str(m, "name"))
		}
		fmt.Fprintf(w, "\n%d trigger(s): %s\n", len(triggers), strings.Join(names, ", "))
	}
	problems := api.List(body, "problems")
	if len(problems) > 0 {
		fmt.Fprintf(w, "\n%d problem(s):\n", len(problems))
		for _, p := range problems {
			m, _ := p.(map[string]any)
			fmt.Fprintf(w, "  %-7s %s — %s\n", api.Str(m, "level"), api.Str(m, "path"), api.Str(m, "message"))
		}
	}
}

func printActionResult(w io.Writer, payload map[string]any) {
	req := api.Map(payload, "request")
	resp := api.Map(payload, "response")
	fmt.Fprintf(w, "%s → %s %s · as %s · workspace %s\n", api.Str(payload, "tool_name"), api.Str(req, "method"), api.Str(req, "url"), api.Str(req, "as"), orDash(api.Str(req, "workspace")))
	fmt.Fprintf(w, "HTTP %v\n", resp["status"])
	if body := resp["body"]; body != nil {
		pretty, _ := json.MarshalIndent(body, "", "  ")
		fmt.Fprintln(w, string(pretty))
	}
	if payload["step_up_human"] == true {
		why := "App tokens carry no acr: this is expected here."
		if api.Str(req, "as") == "the given token" {
			why = "The token you passed does not carry that level."
		}
		fmt.Fprintf(w, "\nThe provider requires a person authenticated at level %s for this action (%s). %s\n"+
			"Through Iugu for AI the tool %s asks that person to confirm the exact call, then performs it with their identity.\n",
			api.Str(payload, "acr"), api.Str(resp, "www_authenticate"), why, api.Str(payload, "tool_name"))
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
