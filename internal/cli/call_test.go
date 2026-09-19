package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// iugu call <app> <action>: the bridge over HTTP — Console calls the provider as the person; elevated actions are
// gated with a confirmation page (agents: exit 7 + url; humans: wait and retry).
func TestCallActionThroughTheBridge(t *testing.T) {
	f := newFakeConsole(t)
	provider := newFakeProvider(t)
	var refreshHits atomic.Int32
	f.serveActionsManifest(provider, &refreshHits)
	dir := t.TempDir()
	loginFake(t, f, dir)

	var calls atomic.Int32
	confirmed := atomic.Bool{}
	f.srv.Config.Handler.(*http.ServeMux).HandleFunc("/v1/apps/app1/actions/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/call") {
			w.WriteHeader(404)
			return
		}
		calls.Add(1)
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/apps/app1/actions/"), "/call")
		w.Header().Set("Content-Type", "application/json")
		switch name {
		case "list_things":
			if body["workspace_id"] != "ws1" {
				w.WriteHeader(422)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "bad_test", "message": "workspace missing"}})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"id": "t1"}}})
		case "create_thing":
			if !confirmed.Load() {
				w.WriteHeader(403)
				json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "step_up_required", "message": "confirm this exact call",
					"details": map[string]any{"step_up_url": "https://console.example.com/workspace/iugu-for-ai/calls/c1", "acr": "config"}}})
				return
			}
			args, _ := body["arguments"].(map[string]any)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"id": "t9", "name": args["name"]}})
		default:
			w.WriteHeader(404)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "not_found", "message": "no such action"}})
		}
	})
	// the catalog resolves a tag to the app id
	f.srv.Config.Handler.(*http.ServeMux).HandleFunc("/v1/catalog/actions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"app": map[string]any{"id": "app1", "tag": "acme", "slug": "acme"}, "actions": []any{"acme:thing.read"}}}})
	})

	// a read, by tag, as JSON: the provider's answer as it came
	code, out, _ := run(t, dir, f.srv.URL, "call", "acme", "list_things", "--workspace", "ws1", "--json")
	if code != 0 || !strings.Contains(out, `"id": "t1"`) {
		t.Fatalf("call list_things: code %d out %s", code, out)
	}

	// inputs are checked against the manifest before anything is sent
	code, out, _ = run(t, dir, f.srv.URL, "call", "app1", "create_thing", "--workspace", "ws1", "--arg", "colour=red", "--json")
	if code != 2 || !strings.Contains(out, "missing required input") {
		t.Fatalf("schema check: code %d out %s", code, out)
	}

	// elevated, as an agent: exit 7 with the confirmation URL and a next step; nothing retried
	before := calls.Load()
	code, out, _ = run(t, dir, f.srv.URL, "call", "app1", "create_thing", "--workspace", "ws1", "--arg", "name=Widget", "--json", "--agent", "yes")
	if code != 7 || !strings.Contains(out, "iugu-for-ai/calls/c1") || !strings.Contains(out, "re-run this exact command") {
		t.Fatalf("agent gate: code %d out %s", code, out)
	}
	if calls.Load() != before+1 {
		t.Fatalf("an agent must not retry: %d calls", calls.Load()-before)
	}

	// elevated, as a human: the CLI waits for the confirmation and retries the same call
	oldPoll := gatePollInterval
	gatePollInterval = 10 * time.Millisecond
	defer func() { gatePollInterval = oldPoll }()
	go func() { time.Sleep(50 * time.Millisecond); confirmed.Store(true) }()
	code, out, _ = run(t, dir, f.srv.URL, "call", "app1", "create_thing", "--workspace", "ws1", "--arg", "name=Widget", "--json", "--agent", "no")
	if code != 0 || !strings.Contains(out, `"id": "t9"`) {
		t.Fatalf("human gate then success: code %d out %s", code, out)
	}
}
