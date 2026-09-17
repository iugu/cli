package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/pflag"

	"github.com/iugu-private/platform2-cli/internal/api"
	"github.com/iugu-private/platform2-cli/internal/output"
)

// approvalOptions are the flags shared by every Tier 1 command.
type approvalOptions struct {
	wait       bool
	timeout    time.Duration
	writeEnv   string
	execCmd    string
	showSecret bool
}

// handleResponse deals with a Tier 0/Tier 1 response uniformly: 2xx prints the resource; 202 prints
// the approval (exit 5) or, with --wait, polls the change set until decided and delivers secrets.
func (rt *Runtime) handleResponse(ctx context.Context, client *api.Client, r *api.Response, opts approvalOptions, human func(io.Writer)) error {
	if r.Status != 202 {
		rt.Printer.Result(r.Body, human)
		return nil
	}
	cs := r.Body
	id := api.Str(cs, "id")
	approvalURL := api.Str(cs, "approval", "url")
	if !opts.wait {
		payload := approvalPayload(cs)
		return &output.Exit{Code: output.ExitApproval, Payload: payload, Message: fmt.Sprintf("Approval required: %s\nOpen %s (expires %s), then run: iugu changeset wait %s", summarize(cs), approvalURL, api.Str(cs, "approval", "expires_at"), id)}
	}
	rt.Printer.Line("Approval required — open %s\nWaiting for the human to decide (timeout %s)…", approvalURL, opts.timeout)
	final, err := rt.waitChangeSet(ctx, client, id, opts.timeout)
	if err != nil {
		return err
	}
	return rt.deliverOutcome(ctx, client, final, opts)
}

func approvalPayload(cs map[string]any) map[string]any {
	return map[string]any{
		"status":        "pending_approval",
		"change_set_id": api.Str(cs, "id"),
		"approval":      cs["approval"],
		"summary":       summarize(cs),
		"diff":          cs["diff"],
		"next_step":     fmt.Sprintf("iugu changeset wait %s [--write-env .env.local | --exec \"…{secret}…\" | --show-secret]", api.Str(cs, "id")),
		"instructions":  "Show approval.url to the human (they review the diff and approve with a verification code). Poll with next_step; never ask the human for the secret — it is delivered once to this CLI.",
	}
}

func summarize(cs map[string]any) string {
	parts := []string{}
	for _, d := range api.List(cs, "diff") {
		m, _ := d.(map[string]any)
		parts = append(parts, api.Str(m, "summary"))
	}
	if len(parts) == 0 {
		return api.Str(cs, "title")
	}
	return strings.Join(parts, "; ")
}

// waitChangeSet polls GET /v1/change-sets/{id} until it leaves pending_approval.
func (rt *Runtime) waitChangeSet(ctx context.Context, client *api.Client, id string, timeout time.Duration) (map[string]any, error) {
	if timeout == 0 {
		timeout = 24 * time.Hour
	}
	deadline := time.Now().Add(timeout)
	interval := 3 * time.Second
	for {
		r, err := client.Get(ctx, "/v1/change-sets/"+id, nil)
		if err != nil {
			return nil, err
		}
		status := api.Str(r.Body, "status")
		if rt.Printer.JSON {
			output.NDJSON(rt.Printer.Out, map[string]any{"event": "status", "change_set_id": id, "status": status, "at": time.Now().UTC()})
		}
		switch status {
		case "pending_approval", "approved":
			if time.Now().After(deadline) {
				return r.Body, &output.Exit{Code: output.ExitApproval, Payload: approvalPayload(r.Body), Message: "still pending after " + timeout.String()}
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(interval):
			}
			if interval < 15*time.Second {
				interval += time.Second
			}
		default:
			return r.Body, nil
		}
	}
}

// deliverOutcome prints the final change set and hands secrets over exactly once, to where the user asked.
func (rt *Runtime) deliverOutcome(ctx context.Context, client *api.Client, cs map[string]any, opts approvalOptions) error {
	status := api.Str(cs, "status")
	id := api.Str(cs, "id")
	switch status {
	case "applied":
	case "rejected":
		return &output.Exit{Code: output.ExitFailure, Payload: map[string]any{"status": status, "change_set_id": id, "reason": api.Str(cs, "rejected_reason")}, Message: fmt.Sprintf("Rejected: %s", api.Str(cs, "rejected_reason"))}
	case "failed":
		return &output.Exit{Code: output.ExitConflict, Payload: map[string]any{"status": status, "change_set_id": id, "error": cs["error"]}, Message: fmt.Sprintf("Apply failed: %s — fix and re-submit", api.Str(cs, "error", "message"))}
	case "expired":
		return &output.Exit{Code: output.ExitFailure, Payload: map[string]any{"status": status, "change_set_id": id}, Message: "The approval expired (24 h); submit again"}
	default:
		rt.Printer.Result(cs, nil)
		return nil
	}
	result := map[string]any{"status": status, "change_set_id": id, "result": cs["result"]}
	available := api.Str(cs, "secrets", "available") == "true"
	if available && (opts.writeEnv != "" || opts.execCmd != "" || opts.showSecret) {
		r, err := client.Get(ctx, "/v1/change-sets/"+id+"/secrets", nil)
		if err != nil {
			return fmt.Errorf("retrieving secrets: %w", err)
		}
		secrets := api.Map(r.Body, "secrets")
		delivered, err := rt.deliverSecrets(secrets, opts)
		if err != nil {
			return err
		}
		result["secrets"] = delivered
	} else if available {
		result["secrets"] = map[string]any{"available": true, "expires_at": api.Str(cs, "secrets", "expires_at"),
			"next_step": fmt.Sprintf("iugu changeset secrets %s --write-env .env.local   (once, within 10 minutes)", id)}
	}
	rt.Printer.Result(result, func(w io.Writer) {
		fmt.Fprintf(w, "Applied: %s\n", summarize(cs))
		if s, ok := result["secrets"].(map[string]any); ok {
			if next, ok := s["next_step"].(string); ok {
				fmt.Fprintf(w, "Secrets are ready for this CLI — %s\n", next)
			} else if written, ok := s["written"].(string); ok {
				fmt.Fprintf(w, "Secrets written to %s (0600)\n", written)
			}
		}
	})
	return nil
}

// deliverSecrets flattens the per-operation secrets into env vars and delivers them by the chosen route.
// Returned payload never contains the secret values unless --show-secret.
func (rt *Runtime) deliverSecrets(secrets map[string]any, opts approvalOptions) (map[string]any, error) {
	env := envFromSecrets(secrets)
	out := map[string]any{"keys": sortedKeys(env)}
	if opts.writeEnv != "" {
		if err := writeEnvFile(opts.writeEnv, env); err != nil {
			return nil, err
		}
		out["written"] = opts.writeEnv
	}
	if opts.execCmd != "" {
		if err := execWithSecrets(opts.execCmd, env); err != nil {
			return nil, err
		}
		out["executed"] = redactCommand(opts.execCmd)
	}
	if opts.showSecret {
		out["values"] = env
	}
	return out, nil
}

// envFromSecrets maps the vault payload to environment variable names.
func envFromSecrets(secrets map[string]any) map[string]string {
	env := map[string]string{}
	idx := 0
	for _, key := range sortedKeys(anyKeys(secrets)) {
		entry, _ := secrets[key].(map[string]any)
		suffix := ""
		if idx > 0 {
			suffix = fmt.Sprintf("_%d", idx+1)
		}
		if v := api.Str(entry, "client_secret"); v != "" {
			env["IUGU_CLIENT_ID"+suffix] = api.Str(entry, "client_id")
			env["IUGU_CLIENT_SECRET"+suffix] = v
			env["IUGU_CREDENTIAL_ID"+suffix] = api.Str(entry, "credential_id")
		}
		if v := api.Str(entry, "token"); v != "" {
			env["IUGU_TOKEN"+suffix] = v
			env["IUGU_DEPLOY_TOKEN_ID"+suffix] = api.Str(entry, "deploy_token_id")
		}
		idx++
	}
	return env
}

func anyKeys(m map[string]any) map[string]string {
	out := map[string]string{}
	for k := range m {
		out[k] = ""
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// writeEnvFile upserts KEY=VALUE lines into a dotenv file with mode 0600.
func writeEnvFile(path string, env map[string]string) error {
	existing := map[string]string{}
	order := []string{}
	if f, err := os.Open(path); err == nil {
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(strings.TrimSpace(line), "#") || !strings.Contains(line, "=") {
				continue
			}
			k, v, _ := strings.Cut(line, "=")
			existing[strings.TrimSpace(k)] = v
			order = append(order, strings.TrimSpace(k))
		}
		f.Close()
	}
	for _, k := range sortedKeys(env) {
		if _, ok := existing[k]; !ok {
			order = append(order, k)
		}
		existing[k] = quoteEnv(env[k])
	}
	var b strings.Builder
	b.WriteString("# managed by iugu CLI — secrets, do not commit\n")
	for _, k := range order {
		fmt.Fprintf(&b, "%s=%s\n", k, existing[k])
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil && filepath.Dir(path) != "." {
		return err
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

func quoteEnv(v string) string {
	if strings.ContainsAny(v, " \t\"'$#") {
		return `"` + strings.ReplaceAll(v, `"`, `\"`) + `"`
	}
	return v
}

// execWithSecrets substitutes {KEY} and {secret} (= IUGU_CLIENT_SECRET or IUGU_TOKEN) placeholders in the
// command's arguments in-process and runs it without a shell, so the secret never reaches argv logs
// through this process's own output.
func execWithSecrets(command string, env map[string]string) error {
	parts := splitCommand(command)
	if len(parts) == 0 {
		return errors.New("--exec needs a command")
	}
	secret := env["IUGU_CLIENT_SECRET"]
	if secret == "" {
		secret = env["IUGU_TOKEN"]
	}
	for i, p := range parts {
		p = strings.ReplaceAll(p, "{secret}", secret)
		for k, v := range env {
			p = strings.ReplaceAll(p, "{"+k+"}", v)
		}
		parts[i] = p
	}
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = os.Stderr, os.Stderr, os.Stdin
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd.Run()
}

// splitCommand splits a command line into argv honouring single and double quotes (no shell expansion).
func splitCommand(command string) []string {
	var args []string
	var cur strings.Builder
	quote := rune(0)
	has := false
	for _, r := range command {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			has = true
		case r == ' ' || r == '\t' || r == '\n':
			if has || cur.Len() > 0 {
				args = append(args, cur.String())
				cur.Reset()
				has = false
			}
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	if has || cur.Len() > 0 {
		args = append(args, cur.String())
	}
	return args
}

func redactCommand(command string) string {
	return strings.ReplaceAll(command, "{secret}", "{secret:redacted}")
}

func addApprovalFlags(f *pflag.FlagSet, opts *approvalOptions) {
	f.BoolVar(&opts.wait, "wait", false, "block until the human approves or rejects (otherwise exit 5 with the approval URL)")
	f.DurationVar(&opts.timeout, "timeout", 30*time.Minute, "with --wait: how long to wait")
	f.StringVar(&opts.writeEnv, "write-env", "", "with --wait: write delivered secrets into this dotenv file (0600)")
	f.StringVar(&opts.execCmd, "exec", "", "with --wait: run a command with {secret}/{IUGU_*} placeholders substituted in-process (e.g. \"fly secrets set IUGU_CLIENT_SECRET={secret}\")")
	f.BoolVar(&opts.showSecret, "show-secret", false, "with --wait: print secret values (avoid in agent transcripts)")
}

var _ = url.Values{}
