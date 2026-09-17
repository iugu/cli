package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// fakeConsole stands for the AS + Lifecycle API together (same host), enough for the command flows.
type fakeConsole struct {
	srv        *httptest.Server
	statusCode atomic.Int32 // change set status progression
	secretsHit atomic.Int32
	devicePoll atomic.Int32
}

func jwtWith(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func newFakeConsole(t *testing.T) *fakeConsole {
	f := &fakeConsole{}
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	access := func() string {
		return jwtWith(map[string]any{"sub": "user:abc", "scope": "openid offline_access console:read console:apps.write console:credentials console:installs console:testing", "grant_id": "g1", "wsp": []string{"ws1"}, "exp": time.Now().Add(5 * time.Minute).Unix()})
	}
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"resource": f.srv.URL, "authorization_servers": []string{f.srv.URL + "/"}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"issuer": f.srv.URL + "/", "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"device_authorization_endpoint": f.srv.URL + "/device_authorization", "revocation_endpoint": f.srv.URL + "/revoke", "code_challenge_methods_supported": []string{"S256"}})
	})
	mux.HandleFunc("/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"device_code": "dc1", "user_code": "WDJB-MJHT", "verification_uri": f.srv.URL + "/device", "verification_uri_complete": f.srv.URL + "/device?user_code=WDJB-MJHT", "expires_in": 600, "interval": 0})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("grant_type") == "urn:ietf:params:oauth:grant-type:device_code" {
			if f.devicePoll.Add(1) == 1 {
				writeJSON(w, 400, map[string]string{"error": "authorization_pending"})
				return
			}
		}
		writeJSON(w, 200, map[string]any{"access_token": access(), "id_token": jwtWith(map[string]any{"email": "dev@iugu.test", "name": "Dev"}), "refresh_token": "rt", "token_type": "Bearer", "expires_in": 300, "scope": "console:read"})
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{}")) })
	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token", resource_metadata="x"`)
			writeJSON(w, 401, map[string]any{"error": map[string]any{"code": "invalid_token", "message": "Missing or invalid bearer token"}})
			return false
		}
		return true
	}
	mux.HandleFunc("/v1/me", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		writeJSON(w, 200, map[string]any{"principal": map[string]any{"sub": "user:abc", "name": "Dev", "email": "dev@iugu.test"}, "client": map[string]any{"name": "iugu CLI"},
			"grant": map[string]any{"scopes": []string{"console:read"}}, "workspaces": []any{map[string]any{"id": "ws1", "name": "Dev Workspace", "roles": []any{map[string]any{"id": "r1", "name": "Administrator"}}}}})
	})
	mux.HandleFunc("/v1/workspaces/ws1/apps", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if r.Header.Get("Idempotency-Key") == "" && r.Method == "POST" {
			// fine either way; recorded for visibility
		}
		w.Header().Set("ETag", `"abc"`)
		writeJSON(w, 201, map[string]any{"id": "app1", "name": "Acme", "tag": "acme", "draft": true, "public": false})
	})
	mux.HandleFunc("/v1/workspaces/ws1/installations", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		writeJSON(w, 201, map[string]any{"id": "inst1", "app": map[string]any{"id": "app1"}})
	})
	mux.HandleFunc("/v1/apps/app1", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if r.Method == "PATCH" && r.Header.Get("If-Match") == `"stale"` {
			writeJSON(w, 409, map[string]any{"error": map[string]any{"code": "stale_resource", "message": "changed"}})
			return
		}
		w.Header().Set("ETag", `"abc"`)
		writeJSON(w, 200, map[string]any{"id": "app1", "name": "Acme", "tag": "acme"})
	})
	mux.HandleFunc("/v1/apps/app1/credentials", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		f.statusCode.Store(0)
		writeJSON(w, 202, map[string]any{"id": "cs1", "status": "pending_approval", "approval": map[string]any{"url": "https://console.test/workspace/approvals/cs1", "expires_at": "2030-01-01T00:00:00Z"},
			"diff": []any{map[string]any{"summary": `Create credential "dev" for app "Acme"`}}})
	})
	mux.HandleFunc("/v1/change-sets/cs1", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		n := f.statusCode.Add(1)
		status := "pending_approval"
		secrets := map[string]any{"available": false}
		if n >= 2 {
			status = "applied"
			secrets = map[string]any{"available": true, "expires_at": "2030-01-01T00:00:00Z"}
		}
		writeJSON(w, 200, map[string]any{"id": "cs1", "status": status, "result": []any{map[string]any{"credential_id": "cred1"}}, "secrets": secrets,
			"diff": []any{map[string]any{"summary": `Create credential "dev" for app "Acme"`}}})
	})
	mux.HandleFunc("/v1/change-sets/cs1/secrets", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		if f.secretsHit.Add(1) > 1 {
			writeJSON(w, 404, map[string]any{"error": map[string]any{"code": "not_found", "message": "gone"}})
			return
		}
		writeJSON(w, 200, map[string]any{"change_set_id": "cs1", "secrets": map[string]any{"0": map[string]any{"credential_id": "cred1", "client_id": "app1", "client_secret": "s3cr3t-value-with space"}}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// run executes the CLI in-process with an isolated config dir and file store, capturing stdout.
func run(t *testing.T, dir string, api string, args ...string) (int, string, string) {
	t.Helper()
	t.Setenv("IUGU_CONFIG_DIR", dir)
	t.Setenv("IUGU_CREDENTIALS_STORE", "file")
	t.Setenv("IUGU_API", api)
	t.Setenv("IUGU_TOKEN", "")
	t.Setenv("IUGU_NO_BROWSER", "1")
	t.Setenv("HOME", dir)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	rt := &Runtime{HTTP: http.DefaultClient}
	root := rt.rootCommand()
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	// PersistentPreRun creates the printer; redirect it after execution starts by wrapping.
	origPre := root.PersistentPreRunE
	root.PersistentPreRunE = func(cmd *cobra.Command, a []string) error {
		if err := origPre(cmd, a); err != nil {
			return err
		}
		rt.Printer.Out, rt.Printer.Err = stdout, stderr
		return nil
	}
	err := root.Execute()
	code := 0
	if err != nil {
		rt.Printer.Out, rt.Printer.Err = stdout, stderr
		code = rt.exit(err)
	}
	return code, stdout.String(), stderr.String()
}

func TestExitCodesAndHandoffFlow(t *testing.T) {
	f := newFakeConsole(t)
	dir := t.TempDir()

	// 4: nothing stored
	code, out, _ := run(t, dir, f.srv.URL, "me", "--json")
	if code != 4 || !strings.Contains(out, "login_required") {
		t.Fatalf("expected exit 4, got %d %s", code, out)
	}

	// agent handoff: exit 0 with the URL and a handle
	code, out, _ = run(t, dir, f.srv.URL, "login", "--json", "--agent", "yes")
	if code != 0 || !strings.Contains(out, "WDJB-MJHT") || !strings.Contains(out, "next_step") {
		t.Fatalf("handoff: %d %s", code, out)
	}
	var handoff map[string]any
	_ = json.Unmarshal([]byte(out), &handoff)
	handle := handoff["handle"].(string)

	// first completion: still pending (fake answers pending once)
	code, out, _ = run(t, dir, f.srv.URL, "login", "--complete", handle, "--json")
	if code != 0 || !strings.Contains(out, "authorization_pending") {
		t.Fatalf("pending: %d %s", code, out)
	}
	code, out, _ = run(t, dir, f.srv.URL, "login", "--complete", handle, "--json")
	if code != 0 || !strings.Contains(out, `"logged_in": true`) || !strings.Contains(out, "dev@iugu.test") {
		t.Fatalf("completion: %d %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "credentials.json")); err != nil {
		t.Fatal("file store not written")
	}

	// logged in: me works
	code, out, _ = run(t, dir, f.srv.URL, "me", "--json")
	if code != 0 || !strings.Contains(out, "Dev Workspace") {
		t.Fatalf("me: %d %s", code, out)
	}

	// 6: stale ETag
	code, out, _ = run(t, dir, f.srv.URL, "app", "update", "--app", "app1", "--name", "x", "--if-match", `"stale"`, "--json")
	if code != 6 || !strings.Contains(out, "stale_resource") {
		t.Fatalf("expected exit 6, got %d %s", code, out)
	}

	// 5: Tier 1 without --wait
	code, out, _ = run(t, dir, f.srv.URL, "app", "credentials", "create", "--app", "app1", "--name", "dev", "--json")
	if code != 5 || !strings.Contains(out, "approvals/cs1") || !strings.Contains(out, "changeset wait cs1") {
		t.Fatalf("expected exit 5 with approval url, got %d %s", code, out)
	}

	// wait + write-env: secrets land in a 0600 file, never in stdout
	envPath := filepath.Join(dir, ".env.local")
	code, out, _ = run(t, dir, f.srv.URL, "changeset", "wait", "cs1", "--write-env", envPath, "--json")
	if code != 0 || !strings.Contains(out, `"status": "applied"`) {
		t.Fatalf("wait: %d %s", code, out)
	}
	if strings.Contains(out, "s3cr3t") {
		t.Fatal("secret leaked to stdout")
	}
	data, err := os.ReadFile(envPath)
	if err != nil || !strings.Contains(string(data), `IUGU_CLIENT_SECRET="s3cr3t-value-with space"`) || !strings.Contains(string(data), "IUGU_CLIENT_ID=app1") {
		t.Fatalf("env file: %s %v", data, err)
	}
	info, _ := os.Stat(envPath)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("env file must be 0600, got %v", info.Mode().Perm())
	}

	// logout revokes and forgets
	code, out, _ = run(t, dir, f.srv.URL, "logout", "--json")
	if code != 0 || !strings.Contains(out, `"revoked": true`) {
		t.Fatalf("logout: %d %s", code, out)
	}
	code, _, _ = run(t, dir, f.srv.URL, "me", "--json")
	if code != 4 {
		t.Fatalf("after logout expected 4, got %d", code)
	}
}

func TestAppInitWritesProjectAndExits5(t *testing.T) {
	f := newFakeConsole(t)
	dir := t.TempDir()
	project := t.TempDir()
	run(t, dir, f.srv.URL, "login", "--json", "--agent", "yes")
	var handoff map[string]any
	_, out, _ := run(t, dir, f.srv.URL, "login", "--json", "--agent", "yes")
	_ = json.Unmarshal([]byte(out), &handoff)
	f.devicePoll.Store(5)
	run(t, dir, f.srv.URL, "login", "--complete", handoff["handle"].(string), "--json")

	code, out, _ := run(t, dir, f.srv.URL, "app", "init", "--name", "Acme", "--workspace", "ws1", "--dir", project, "--json")
	if code != 5 {
		t.Fatalf("expected exit 5, got %d %s", code, out)
	}
	if !strings.Contains(out, `"project"`) || !strings.Contains(out, "integration") || !strings.Contains(out, "changeset wait cs1 --write-env .env.local") {
		t.Fatalf("payload: %s", out)
	}
	toml, err := os.ReadFile(filepath.Join(project, "iugu.toml"))
	if err != nil || !strings.Contains(string(toml), `id = 'app1'`) || !strings.Contains(string(toml), `workspace = 'ws1'`) {
		t.Fatalf("iugu.toml: %s %v", toml, err)
	}
	gitignore, _ := os.ReadFile(filepath.Join(project, ".gitignore"))
	if !strings.Contains(string(gitignore), ".env.local") {
		t.Fatal(".env.local must be git-ignored")
	}
}

func TestExecSubstitutesSecretsInProcess(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "out.txt")
	env := map[string]string{"IUGU_CLIENT_SECRET": "top-secret", "IUGU_CLIENT_ID": "app1"}
	if err := execWithSecrets(`sh -c "printf %s {IUGU_CLIENT_ID}:{secret} > `+marker+`"`, env); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(marker)
	if string(data) != "app1:top-secret" {
		t.Fatalf("substitution failed: %q", data)
	}
	if got := splitCommand(`fly secrets set "A B={secret}" 'c d'`); len(got) != 5 || got[3] != "A B={secret}" || got[4] != "c d" {
		t.Fatalf("split: %q", got)
	}
	if redactCommand("fly secrets set S={secret}") != "fly secrets set S={secret:redacted}" {
		t.Fatal("redaction")
	}
	got := envFromSecrets(map[string]any{"0": map[string]any{"client_id": "a", "client_secret": "s", "credential_id": "c"}, "1": map[string]any{"token": "t", "deploy_token_id": "d"}})
	if got["IUGU_CLIENT_SECRET"] != "s" || got["IUGU_TOKEN_2"] != "t" {
		t.Fatalf("env mapping: %v", got)
	}
}

func TestFillAppID(t *testing.T) {
	ops := []map[string]any{
		{"op": "oauth.update", "params": map[string]any{"url": "https://x"}},
		{"op": "credentials.create", "params": map[string]any{"app_id": "other"}},
		{"op": "installations.resync", "params": map[string]any{"installation_id": "i"}},
		{"op": "listing.publish"},
	}
	fillAppID(ops, "app1")
	if ops[0]["params"].(map[string]any)["app_id"] != "app1" || ops[1]["params"].(map[string]any)["app_id"] != "other" {
		t.Fatalf("app_id defaulting: %v", ops)
	}
	if _, has := ops[2]["params"].(map[string]any)["app_id"]; has {
		t.Fatal("resync takes no app_id")
	}
	if ops[3]["params"].(map[string]any)["app_id"] != "app1" {
		t.Fatal("nil params must be created")
	}
}
