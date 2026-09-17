package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A provider app: the manifest lives at Console (fake), the endpoints live here.
type fakeProvider struct {
	srv       *httptest.Server
	lastAuth  string
	lastWS    string
	lastBody  string
	lastPath  string
	lastQuery string
	calls     atomic.Int32
	stepUp    bool // answer 401 insufficient_user_authentication on POST
}

func newFakeProvider(t *testing.T) *fakeProvider {
	p := &fakeProvider{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/things", func(w http.ResponseWriter, r *http.Request) {
		p.calls.Add(1)
		p.lastAuth, p.lastWS, p.lastPath, p.lastQuery = r.Header.Get("Authorization"), r.Header.Get("Workspace"), r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		if r.Method == "POST" {
			raw := make([]byte, 4096)
			n, _ := r.Body.Read(raw)
			p.lastBody = string(raw[:n])
			if p.stepUp {
				w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_user_authentication", acr_values="urn:iugu:grant_scopes:config", max_age="600"`)
				w.WriteHeader(401)
				json.NewEncoder(w).Encode(map[string]any{"error": "insufficient_user_authentication"})
				return
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t9"}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "t1"}}})
	})
	mux.HandleFunc("/api/things/", func(w http.ResponseWriter, r *http.Request) {
		p.calls.Add(1)
		p.lastAuth, p.lastWS, p.lastPath, p.lastQuery = r.Header.Get("Authorization"), r.Header.Get("Workspace"), r.URL.EscapedPath(), r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": strings.TrimPrefix(r.URL.Path, "/api/things/")}})
	})
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func (f *fakeConsole) serveActionsManifest(provider *fakeProvider, refreshHits *atomic.Int32) {
	base := provider.srv.URL + "/api"
	input := func(name, typ, source string, required bool) map[string]any {
		return map[string]any{"name": name, "type": typ, "required": required, "direction": "ParamInput", "source": source, "description": name}
	}
	f.srv.Config.Handler.(*http.ServeMux).HandleFunc("/v1/apps/app1/actions", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			w.WriteHeader(401)
			return
		}
		if r.URL.Query().Get("refresh") == "true" {
			refreshHits.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"app":      map[string]any{"id": "app1", "name": "Acme", "tag": "acme", "service_api_url": base, "actions_provider": true},
			"manifest": map[string]any{"url": base + "/actions", "spec": "iugu.actions/v1", "status": "ok"},
			"actions": []any{
				map[string]any{"name": "list_things", "tool_name": "acme__list_things", "title": "List things", "method": "GET", "url": base + "/things",
					"authorization": map[string]any{"action": "acme:thing.read", "acr": "informations"}, "parameters": []any{},
					"input_schema": map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}}},
				map[string]any{"name": "get_thing", "tool_name": "acme__get_thing", "title": "Get thing", "method": "GET", "url": base + "/things/:id",
					"authorization": map[string]any{"action": "acme:thing.read", "acr": "informations"},
					"parameters":    []any{input("id", "string", "path", true), input("verbose", "boolean", "query", false)},
					"input_schema":  map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}, "verbose": map[string]any{"type": "boolean"}}, "required": []any{"id"}}},
				map[string]any{"name": "create_thing", "tool_name": "acme__create_thing", "title": "Create thing", "method": "POST", "url": base + "/things",
					"authorization": map[string]any{"action": "acme:thing.create", "acr": "config"},
					"parameters":    []any{input("name", "string", "body", true), input("amount", "integer", "body", false)},
					"input_schema":  map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}, "amount": map[string]any{"type": "integer"}}, "required": []any{"name"}}},
			},
			"triggers": []any{},
			"problems": []any{map[string]any{"level": "error", "path": "actions[3].authorization.action", "message": "acme:nope is not one of the app's implemented actions"}},
		})
	})
	f.srv.Config.Handler.(*http.ServeMux).HandleFunc("/v1/apps/app1/tokens", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "app-token-xyz", "sub": "app:app1", "aud": []string{"Iugu.Platform.app1"}, "expires_in": 300})
	})
}

func loginFake(t *testing.T, f *fakeConsole, dir string) {
	t.Helper()
	var handoff map[string]any
	_, out, _ := run(t, dir, f.srv.URL, "login", "--json", "--agent", "yes")
	_ = json.Unmarshal([]byte(out), &handoff)
	f.devicePoll.Store(5)
	run(t, dir, f.srv.URL, "login", "--complete", handoff["handle"].(string), "--json")
}

func TestAppActionsListAndCall(t *testing.T) {
	f := newFakeConsole(t)
	provider := newFakeProvider(t)
	var refreshHits atomic.Int32
	f.serveActionsManifest(provider, &refreshHits)
	dir := t.TempDir()
	loginFake(t, f, dir)

	// list: human output names accepted actions, requirements and problems
	code, out, _ := run(t, dir, f.srv.URL, "app", "actions", "list", "app1")
	if code != 0 || !strings.Contains(out, "acme__create_thing") || !strings.Contains(out, "requires acme:thing.create · acr config") ||
		!strings.Contains(out, "in: name:string*@body amount:integer@body") || !strings.Contains(out, "1 problem(s)") || !strings.Contains(out, "acme:nope") {
		t.Fatalf("list: %d %s", code, out)
	}
	code, out, _ = run(t, dir, f.srv.URL, "app", "actions", "list", "app1", "--refresh", "--json")
	if code != 0 || refreshHits.Load() != 1 || !strings.Contains(out, `"tool_name": "acme__list_things"`) {
		t.Fatalf("list --refresh --json: %d hits=%d %s", code, refreshHits.Load(), out)
	}

	// call GET with no inputs: the app's own token, Workspace header, 2xx → exit 0
	code, out, _ = run(t, dir, f.srv.URL, "app", "actions", "call", "list_things", "app1", "--workspace", "ws1", "--json")
	if code != 0 || provider.lastAuth != "Bearer app-token-xyz" || provider.lastWS != "ws1" || !strings.Contains(out, `"ok": true`) || !strings.Contains(out, `"id": "t1"`) {
		t.Fatalf("call list_things: %d auth=%q ws=%q %s", code, provider.lastAuth, provider.lastWS, out)
	}

	// path + query inputs, by tool name, values parsed from --arg
	code, out, _ = run(t, dir, f.srv.URL, "app", "actions", "call", "acme__get_thing", "app1", "--workspace", "ws1", "--arg", "id=abc d", "--arg", "verbose=true", "--json")
	if code != 0 || provider.lastPath != "/api/things/abc%20d" || provider.lastQuery != "verbose=true" {
		t.Fatalf("call get_thing: %d path=%q query=%q %s", code, provider.lastPath, provider.lastQuery, out)
	}

	// body inputs from --input; numbers stay numbers
	code, out, _ = run(t, dir, f.srv.URL, "app", "actions", "call", "create_thing", "app1", "--workspace", "ws1", "--input", `{"name":"Widget","amount":42}`, "--json")
	if code != 0 || !strings.Contains(provider.lastBody, `"amount":42`) || !strings.Contains(provider.lastBody, `"name":"Widget"`) || !strings.Contains(out, `"status": 201`) {
		t.Fatalf("call create_thing: %d body=%s %s", code, provider.lastBody, out)
	}

	// input validation happens before any call: missing required, unknown name
	before := provider.calls.Load()
	code, out, _ = run(t, dir, f.srv.URL, "app", "actions", "call", "create_thing", "app1", "--workspace", "ws1", "--arg", "amount=1", "--arg", "colour=red", "--json")
	if code != 2 || provider.calls.Load() != before || !strings.Contains(out, `missing required input \"name\"`) || !strings.Contains(out, `unknown input \"colour\"`) {
		t.Fatalf("invalid input: %d %s", code, out)
	}
	code, out, _ = run(t, dir, f.srv.URL, "app", "actions", "call", "nope", "app1", "--workspace", "ws1", "--json")
	if code != 2 || !strings.Contains(out, "unknown_action") || !strings.Contains(out, "list_things") {
		t.Fatalf("unknown action: %d %s", code, out)
	}

	// the provider enforces a human level: exit 1 with the explanation, and --token lets a user token be used instead
	provider.stepUp = true
	code, out, _ = run(t, dir, f.srv.URL, "app", "actions", "call", "create_thing", "app1", "--workspace", "ws1", "--input", `{"name":"Widget"}`)
	if code != 1 || !strings.Contains(out, "requires a person authenticated at level config") || !strings.Contains(out, "acme__create_thing asks that person") {
		t.Fatalf("step-up answer: %d %s", code, out)
	}
	code, _, _ = run(t, dir, f.srv.URL, "app", "actions", "call", "create_thing", "app1", "--workspace", "ws1", "--input", `{"name":"Widget"}`, "--token", "user-token-123", "--json")
	if provider.lastAuth != "Bearer user-token-123" || code != 1 {
		t.Fatalf("--token: auth=%q code=%d", provider.lastAuth, code)
	}
}
