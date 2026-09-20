package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// fakeConsole stands for the AS + Lifecycle API together (same host), enough for the command flows.
type fakeConsole struct {
	srv              *httptest.Server
	statusCode       atomic.Int32 // change set status progression
	secretsHit       atomic.Int32
	devicePoll       atomic.Int32
	revokedGrant     bool                      // when true every API call answers 401 invalid_token
	lastDevice       struct{ id, name string } // device identity seen on /device_authorization
	lastGia          string                    // last GIA write seen: "METHOD /path"
	lastGiaBody      map[string]any
	requireElevation bool // GIA writes answer elevation_required until the fake human "verifies" (second /v1/me poll)
	elevated         atomic.Int32
	mePolls          atomic.Int32
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
		_ = r.ParseForm()
		f.lastDevice.id, f.lastDevice.name = r.Form.Get("device_id"), r.Form.Get("device_name")
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
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || f.revokedGrant {
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
		grant := map[string]any{"scopes": []string{"console:read"}, "elevations": []any{}}
		if f.mePolls.Add(1) >= 2 && f.requireElevation { // the human verified after the first poll
			f.elevated.Store(1)
			grant["elevations"] = []any{map[string]any{"acr": "config"}}
		}
		writeJSON(w, 200, map[string]any{"principal": map[string]any{"sub": "user:abc", "name": "Dev", "email": "dev@iugu.test"}, "client": map[string]any{"name": "iugu CLI"},
			"grant": grant, "workspaces": []any{map[string]any{"id": "ws1", "name": "Dev Workspace", "roles": []any{map[string]any{"id": "r1", "name": "Administrator"}}}}})
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
	mux.HandleFunc("/v1/workspaces/ws1/gia/members", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		writeJSON(w, 200, map[string]any{"data": []any{map[string]any{"id": "m1", "user": map[string]any{"email": "dev@iugu.test"}, "roles": []any{}}}})
	})
	mux.HandleFunc("/v1/workspaces/ws1/gia/", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.lastGia = r.Method + " " + r.URL.Path
		f.lastGiaBody = body
		if f.requireElevation && f.elevated.Load() == 0 {
			w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_user_authentication", acr_values="urn:iugu:grant_scopes:config"`)
			writeJSON(w, 403, map[string]any{"error": map[string]any{"code": "elevation_required", "message": "verify first",
				"details": map[string]any{"acr": "config", "ttl": 900, "elevate_url": f.srv.URL + "/elevate?acr=config", "next_step": "open elevate_url"}}})
			return
		}
		writeJSON(w, 202, map[string]any{"id": "cs9", "status": "pending_approval", "approval": map[string]any{"url": f.srv.URL + "/approvals/cs9"},
			"operations": []any{map[string]any{"op": "gia.x"}}, "diff": []any{map[string]any{"summary": "GIA change"}}})
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
		writeJSON(w, 200, map[string]any{"id": "cs1", "status": status, "app": map[string]any{"id": "app1"}, "result": []any{map[string]any{"op": "credentials.create", "credential_id": "cred1"}}, "secrets": secrets,
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

	if !strings.HasPrefix(f.lastDevice.id, "cli_") || len(f.lastDevice.id) != 36 || !strings.HasPrefix(f.lastDevice.name, "iugu CLI on ") {
		t.Fatalf("device identity not sent on /device_authorization: %+v", f.lastDevice)
	}
	firstDevice := f.lastDevice.id
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
	// the device id is generated once per config dir and stays stable across logins; auth status shows the label
	code, out, _ = run(t, dir, f.srv.URL, "login", "--json", "--agent", "yes")
	if code != 0 || f.lastDevice.id != firstDevice {
		t.Fatalf("device id must be stable per config dir: %q vs %q (%d %s)", f.lastDevice.id, firstDevice, code, out)
	}
	code, out, _ = run(t, dir, f.srv.URL, "auth", "status", "--offline", "--json")
	if code != 0 || !strings.Contains(out, `"device": "iugu CLI on `) {
		t.Fatalf("auth status should show the device label: %d %s", code, out)
	}
	// auth status verifies the grant online; a revoked grant flips logged_in even though the store has a login
	code, out, _ = run(t, dir, f.srv.URL, "auth", "status", "--json")
	if code != 0 || !strings.Contains(out, `"logged_in": true`) || !strings.Contains(out, `"verified": true`) {
		t.Fatalf("auth status online: %d %s", code, out)
	}
	f.revokedGrant = true
	code, out, _ = run(t, dir, f.srv.URL, "auth", "status", "--json")
	if code != 0 || !strings.Contains(out, `"logged_in": false`) || !strings.Contains(out, "revoked") {
		t.Fatalf("auth status after revocation: %d %s", code, out)
	}
	code, out, _ = run(t, dir, f.srv.URL, "auth", "status", "--offline", "--json")
	if code != 0 || !strings.Contains(out, `"logged_in": true`) {
		t.Fatalf("auth status offline: %d %s", code, out)
	}
	f.revokedGrant = false

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
	if err := os.WriteFile(filepath.Join(dir, "iugu.toml"), []byte("[app]\nid = 'app1'\n[development]\nworkspace = 'ws1'\ncredential = 'pending:cs0-expired'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	code, out, _ = run(t, dir, f.srv.URL, "changeset", "wait", "cs1", "--write-env", envPath, "--json")
	if code != 0 || !strings.Contains(out, `"status": "applied"`) {
		t.Fatalf("wait: %d %s", code, out)
	}
	if toml, _ := os.ReadFile(filepath.Join(dir, "iugu.toml")); !strings.Contains(string(toml), "credential = 'cred1'") || !strings.Contains(out, `"credential": "cred1"`) {
		t.Fatalf("iugu.toml should point at the created credential: %s\n%s", toml, out)
	}
	if strings.Contains(out, "s3cr3t") {
		t.Fatal("secret leaked to stdout")
	}
	data, err := os.ReadFile(envPath)
	if err != nil || !strings.Contains(string(data), `IUGU_CLIENT_SECRET="s3cr3t-value-with space"`) || !strings.Contains(string(data), "IUGU_CLIENT_ID=app1") {
		t.Fatalf("env file: %s %v", data, err)
	}
	assertPrivateFile(t, envPath)

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

// TestHelperProcess is the child of TestExecSubstitutesSecretsInProcess: it writes its last argument into the
// file named by the one before (no shell involved, so it runs the same on every OS).
func TestHelperProcess(t *testing.T) {
	if os.Getenv("IUGU_TEST_HELPER_PROCESS") != "1" {
		t.Skip("helper process")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if len(args) != 2 {
		os.Exit(3)
	}
	content := args[1]
	if strings.HasPrefix(content, "env:") { // "env:A,B" → the child's own environment, as a script would read it
		var values []string
		for _, name := range strings.Split(strings.TrimPrefix(content, "env:"), ",") {
			values = append(values, os.Getenv(name))
		}
		content = strings.Join(values, ":")
	}
	if err := os.WriteFile(args[0], []byte(content), 0o600); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}

func TestExecSubstitutesSecretsInProcess(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "out.txt")
	env := map[string]string{"IUGU_CLIENT_SECRET": "top-secret", "IUGU_CLIENT_ID": "app1", "IUGU_TEST_HELPER_PROCESS": "1"}
	// the test binary itself is the command; placeholders are substituted into argv, not through a shell
	helper := func(marker, content string) string {
		return `"` + os.Args[0] + `" -test.run=^TestHelperProcess$ -- "` + marker + `" ` + content
	}
	if err := execWithSecrets(helper(marker, "{IUGU_CLIENT_ID}:{secret}"), env); err != nil {
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
	if redactCommand("curl -H 'Authorization: Bearer {token}'") != "curl -H 'Authorization: Bearer {token:redacted}'" {
		t.Fatal("token redaction")
	}
	// the child also gets the secrets in its environment: a script can read "$IUGU_CLIENT_SECRET" and feed a tool's stdin
	envMarker := filepath.Join(dir, "env.txt")
	if err := execWithSecrets(helper(envMarker, "env:IUGU_CLIENT_SECRET,IUGU_CLIENT_ID"), env); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(envMarker); string(data) != "top-secret:app1" {
		t.Fatalf("environment export failed: %q", data)
	}
	tokenMarker := filepath.Join(dir, "token.txt")
	tokenEnv := map[string]string{"IUGU_ACCESS_TOKEN": "at-123", "IUGU_TEST_HELPER_PROCESS": "1"}
	if err := execWithSecrets(helper(tokenMarker, "{token}"), tokenEnv); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(tokenMarker); string(data) != "at-123" {
		t.Fatalf("{token} substitution failed: %q", data)
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

func TestGiaWritesAreTier1AndResolveMembersByEmail(t *testing.T) {
	f := newFakeConsole(t)
	dir := t.TempDir()
	// log in through the handoff (fake answers immediately on the second poll)
	code, out, _ := run(t, dir, f.srv.URL, "login", "--json", "--agent", "yes")
	if code != 0 {
		t.Fatalf("login: %d %s", code, out)
	}
	var hand map[string]any
	_ = json.Unmarshal([]byte(out), &hand)
	run(t, dir, f.srv.URL, "login", "--complete", hand["handle"].(string), "--json")
	code, out, _ = run(t, dir, f.srv.URL, "login", "--complete", hand["handle"].(string), "--json")
	if code != 0 || !strings.Contains(out, `"logged_in": true`) {
		t.Fatalf("completion: %d %s", code, out)
	}

	code, out, _ = run(t, dir, f.srv.URL, "gia", "roles", "create", "--workspace", "ws1", "--name", "Support", "--policies", "p1,p2", "--json")
	if code != 5 || !strings.Contains(out, "approvals/cs9") || f.lastGia != "POST /v1/workspaces/ws1/gia/roles" {
		t.Fatalf("roles create: %d %s (%s)", code, out, f.lastGia)
	}
	if ids, _ := f.lastGiaBody["policy_ids"].([]any); len(ids) != 2 || ids[1] != "p2" {
		t.Fatalf("policy ids: %v", f.lastGiaBody)
	}
	code, _, _ = run(t, dir, f.srv.URL, "gia", "policies", "update", "pol1", "--workspace", "ws1", "--actions", "acme:invoice.*", "--json")
	if code != 5 || f.lastGia != "PATCH /v1/workspaces/ws1/gia/policies/pol1" || f.lastGiaBody["name"] != nil {
		t.Fatalf("policies update: %d %s %v", code, f.lastGia, f.lastGiaBody)
	}
	code, _, _ = run(t, dir, f.srv.URL, "gia", "members", "set-roles", "dev@iugu.test", "--workspace", "ws1", "--roles", "r1", "--json")
	if code != 5 || f.lastGia != "PATCH /v1/workspaces/ws1/gia/members/m1" {
		t.Fatalf("members set-roles by e-mail: %d %s", code, f.lastGia)
	}
	code, out, _ = run(t, dir, f.srv.URL, "gia", "members", "remove", "nobody@iugu.test", "--workspace", "ws1", "--json")
	if code != 1 || !strings.Contains(out, "no member with e-mail") {
		t.Fatalf("unknown e-mail: %d %s", code, out)
	}
	code, _, _ = run(t, dir, f.srv.URL, "gia", "invites", "resend", "inv1", "--workspace", "ws1", "--json")
	if code != 5 || f.lastGia != "POST /v1/workspaces/ws1/gia/invites/inv1/resend" {
		t.Fatalf("invites resend: %d %s", code, f.lastGia)
	}
	code, _, _ = run(t, dir, f.srv.URL, "gia", "roles", "delete", "r9", "--workspace", "ws1", "--json")
	if code != 5 || f.lastGia != "DELETE /v1/workspaces/ws1/gia/roles/r9" {
		t.Fatalf("roles delete: %d %s", code, f.lastGia)
	}
	code, out, _ = run(t, dir, f.srv.URL, "gia", "roles", "create", "--workspace", "ws1", "--name", "x", "--json")
	if code != 2 || !strings.Contains(out, "--policies") {
		t.Fatalf("usage error expected: %d %s", code, out)
	}
}

func TestElevationGateAgentExit7AndHumanWaitRetry(t *testing.T) {
	f := newFakeConsole(t)
	f.requireElevation = true
	dir := t.TempDir()
	code, out, _ := run(t, dir, f.srv.URL, "login", "--json", "--agent", "yes")
	if code != 0 {
		t.Fatalf("login: %d %s", code, out)
	}
	var hand map[string]any
	_ = json.Unmarshal([]byte(out), &hand)
	run(t, dir, f.srv.URL, "login", "--complete", hand["handle"].(string), "--json")
	run(t, dir, f.srv.URL, "login", "--complete", hand["handle"].(string), "--json")

	// agent mode: exit 7, URL + next_step, no waiting
	code, out, _ = run(t, dir, f.srv.URL, "gia", "roles", "create", "--workspace", "ws1", "--name", "S", "--policies", "p1", "--json", "--agent", "yes")
	if code != 7 || !strings.Contains(out, "/elevate?acr=config") || !strings.Contains(out, "re-run this exact command") || !strings.Contains(out, `"elevation_required"`) {
		t.Fatalf("agent gate: %d %s", code, out)
	}
	if f.elevated.Load() != 0 {
		t.Fatal("agent mode must not poll for the human")
	}

	// human mode: the CLI waits for the elevation (fake: verified on the second /v1/me poll) and retries → 202 → exit 5
	oldPoll := gatePollInterval
	gatePollInterval = 10 * time.Millisecond
	defer func() { gatePollInterval = oldPoll }()
	code, out, _ = run(t, dir, f.srv.URL, "gia", "roles", "create", "--workspace", "ws1", "--name", "S", "--policies", "p1", "--json", "--agent", "no")
	if code != 5 || !strings.Contains(out, "approvals/cs9") {
		t.Fatalf("human gate should wait, retry and reach the approval: %d %s", code, out)
	}
}

// `iugu app env` prints the public integration values the way each deploy target takes them, and never the client
// secret — every format says where the secret comes from instead of omitting it silently.
func TestAppEnvFormatsExplainTheSecret(t *testing.T) {
	f := newFakeConsole(t)
	dir := t.TempDir()
	loginFake(t, f, dir)

	code, out, _ := run(t, dir, f.srv.URL, "app", "env", "app1", "--workspace", "ws1")
	if code != 0 {
		t.Fatalf("dotenv exit %d: %s", code, out)
	}
	for _, want := range []string{"IUGU_CLIENT_ID=app1\n", "IUGU_APP_TAG=acme\n", "IUGU_WORKSPACE_ID=ws1\n", "IUGU_TOKEN_URL=" + f.srv.URL + "/token\n",
		"# IUGU_CLIENT_SECRET is never printed here", "iugu changeset wait <change-set-id> --write-env .env.local"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dotenv output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "IUGU_CLIENT_SECRET=") {
		t.Fatalf("a secret value line must never appear:\n%s", out)
	}

	_, out, _ = run(t, dir, f.srv.URL, "app", "env", "app1", "--workspace", "ws1", "--format", "fly")
	if n := strings.Count(out, "fly secrets set --stage "); n != 1 {
		t.Fatalf("fly: the nine values go in one staged call (one release), got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "IUGU_CLIENT_ID=app1 IUGU_ISSUER=") || !strings.Contains(out, "--exec 'fly secrets set IUGU_CLIENT_SECRET={secret}'") {
		t.Fatalf("fly output:\n%s", out)
	}

	_, out, _ = run(t, dir, f.srv.URL, "app", "env", "app1", "--workspace", "ws1", "--format", "railway")
	if n := strings.Count(out, "railway variable set --skip-deploys "); n != 1 {
		t.Fatalf("railway: one call, no deploy per variable, got %d:\n%s", n, out)
	}
	if !strings.Contains(out, "--exec 'railway variable set IUGU_CLIENT_SECRET={secret}'") {
		t.Fatalf("railway secret hint:\n%s", out)
	}

	_, out, _ = run(t, dir, f.srv.URL, "app", "env", "app1", "--workspace", "ws1", "--format", "netlify")
	if n := strings.Count(out, "\nnetlify env:set IUGU_") + 1; n != 9 || !strings.Contains(out, "netlify env:set IUGU_CLIENT_SECRET {secret} --secret") {
		t.Fatalf("netlify: nine sets + a --secret hint, got %d:\n%s", n, out)
	}

	_, out, _ = run(t, dir, f.srv.URL, "app", "env", "app1", "--workspace", "ws1", "--format", "vercel")
	if n := strings.Count(out, "| vercel env add IUGU_"); n != 9 || !strings.Contains(out, "vercel env add IUGU_CLIENT_SECRET production --value {secret} --yes") {
		// the nine value lines pipe through printf; the hint's --value form does not, so the count is exact
		t.Fatalf("vercel: nine adds + a --value hint, got %d:\n%s", n, out)
	}

	code, out, _ = run(t, dir, f.srv.URL, "app", "env", "app1", "--workspace", "ws1", "--format", "json")
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil || code != 0 {
		t.Fatalf("json exit %d err %v:\n%s", code, err, out)
	}
	env := doc["env"].(map[string]any)
	if env["IUGU_CLIENT_ID"] != "app1" || len(env) != 9 {
		t.Fatalf("json env: %v", env)
	}
	delivery := doc["secret_delivery"].(map[string]any)
	if delivery["variable"] != "IUGU_CLIENT_SECRET" {
		t.Fatalf("secret_delivery: %v", delivery)
	}
	if by := delivery["exec_by_target"].(map[string]any); by["railway"] != "railway variable set IUGU_CLIENT_SECRET={secret}" || by["fly"] == nil {
		t.Fatalf("exec_by_target: %v", by)
	}
	if cmds := delivery["commands"].(map[string]any); !strings.Contains(cmds["rotate"].(string), "iugu app credentials rotate") {
		t.Fatalf("commands: %v", cmds)
	}

	code, out, _ = run(t, dir, f.srv.URL, "app", "env", "app1", "--format", "heroku")
	if code != 2 {
		t.Fatalf("unknown format is a usage error (2), got %d: %s", code, out)
	}
}

// assertPrivateFile checks the 0600 mode the CLI writes secrets with. Windows has no POSIX mode bits (Go
// reports 0666 for any writable file); there the files rely on the ACLs of the user's profile directory.
func assertPrivateFile(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("%s must be 0600, got %v", filepath.Base(path), info.Mode().Perm())
	}
}
