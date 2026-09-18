package cli

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The billing commands talk only to Console's Lifecycle API (Billing is never called by the CLI): plan and
// versions read, prices set/added, preview, publish as a change set (exit 5 with the approval), workspace reports.
func (f *fakeConsole) serveBilling(t *testing.T) *billingCalls {
	calls := &billingCalls{}
	mux := f.srv.Config.Handler.(*http.ServeMux)
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	plan := map[string]any{"id": "plan1", "app_id": "app1", "name": "Acme", "currency": "BRL", "status": "draft", "default_version_id": "v1",
		"versions": []any{map[string]any{"id": "v1", "version": 1, "status": "draft", "default": true, "prices_count": 1}}}
	version := map[string]any{"id": "v1", "plan_id": "plan1", "version": 1, "status": "draft", "default": true, "prices_count": 1,
		"prices": []any{map[string]any{"id": "p1", "name": "Por fatura", "event": "invoice.created", "model": "unit", "config": map[string]any{"amount": "0.5000"}}}}
	mux.HandleFunc("/v1/apps/app1/billing/plan", func(w http.ResponseWriter, r *http.Request) {
		calls.record(r)
		writeJSON(w, 200, map[string]any{"plan": plan})
	})
	mux.HandleFunc("/v1/apps/app1/billing/plan-versions/v1", func(w http.ResponseWriter, r *http.Request) {
		calls.record(r)
		writeJSON(w, 200, map[string]any{"plan_version": version})
	})
	mux.HandleFunc("/v1/apps/app1/billing/plan-versions/v1/prices", func(w http.ResponseWriter, r *http.Request) {
		calls.record(r)
		if r.Method == "PUT" {
			writeJSON(w, 200, map[string]any{"plan_version": version})
			return
		}
		writeJSON(w, 201, map[string]any{"price": map[string]any{"id": "p2", "name": "Lote", "event": "invoice.bulk", "model": "package", "config": map[string]any{"amount": "9.90", "size": 100}}})
	})
	mux.HandleFunc("/v1/apps/app1/billing/plan-versions/v1/preview", func(w http.ResponseWriter, r *http.Request) {
		calls.record(r)
		writeJSON(w, 200, map[string]any{"preview": map[string]any{"total": "5.0000",
			"lines": []any{map[string]any{"price_name": "Por fatura", "event": "invoice.created", "model": "unit", "quantity": "10", "amount": "5.0000"}}}})
	})
	mux.HandleFunc("/v1/apps/app1/billing/plan-versions/v1/publish", func(w http.ResponseWriter, r *http.Request) {
		calls.record(r)
		writeJSON(w, 202, map[string]any{"id": "cs9", "status": "pending_approval", "approval": map[string]any{"url": "https://console.test/workspace/approvals/cs9", "expires_at": "2030-01-01T00:00:00Z"},
			"diff": []any{map[string]any{"summary": `Publish billing plan version v1 of app "Acme"`}}})
	})
	mux.HandleFunc("/v1/workspaces/ws1/billing/reports/revenue", func(w http.ResponseWriter, r *http.Request) {
		calls.record(r)
		writeJSON(w, 200, map[string]any{"periods": []any{map[string]any{"competency": "2026-09", "total_amount": "100.0000", "paid_amount": "60.0000", "pending_amount": "40.0000",
			"apps": []any{map[string]any{"app_id": "app1", "plan_name": "Acme", "total_amount": "100.0000", "paid_amount": "60.0000", "pending_amount": "40.0000"}}}}})
	})
	mux.HandleFunc("/v1/workspaces/ws1/billing/reports/invoices", func(w http.ResponseWriter, r *http.Request) {
		calls.record(r)
		writeJSON(w, 200, map[string]any{"invoices": []any{map[string]any{"id": "i1", "competency": "2026-09", "plan_name": "Acme", "status": "issued", "total_amount": "100.0000", "amount_due": "40.0000"}}})
	})
	return calls
}

type billingCalls struct {
	last     string
	lastBody string
	query    string
}

func (c *billingCalls) record(r *http.Request) {
	c.last = r.Method + " " + r.URL.Path
	c.query = r.URL.RawQuery
	raw, _ := io.ReadAll(r.Body)
	c.lastBody = string(raw)
}

func TestAppBillingCommands(t *testing.T) {
	f := newFakeConsole(t)
	calls := f.serveBilling(t)
	dir := t.TempDir()
	loginFake(t, f, dir)

	code, out, _ := run(t, dir, f.srv.URL, "app", "billing", "plan", "--app", "app1")
	if code != 0 || calls.last != "GET /v1/apps/app1/billing/plan" || !strings.Contains(out, "Acme — plan plan1 · draft · BRL") || !strings.Contains(out, "default") {
		t.Fatalf("plan: %d %q %s", code, calls.last, out)
	}
	code, out, _ = run(t, dir, f.srv.URL, "app", "billing", "plan", "create", "--app", "app1", "--json")
	if code != 0 || calls.last != "POST /v1/apps/app1/billing/plan" || !strings.Contains(out, `"id": "plan1"`) {
		t.Fatalf("plan create: %d %q %s", code, calls.last, out)
	}
	code, out, _ = run(t, dir, f.srv.URL, "app", "billing", "version", "v1", "--app", "app1")
	if code != 0 || !strings.Contains(out, "R$ 0.5000 per unit") || !strings.Contains(out, "invoice.created") {
		t.Fatalf("version: %d %s", code, out)
	}

	code, out, _ = run(t, dir, f.srv.URL, "app", "billing", "prices", "set", "v1", "--app", "app1",
		"--input", `[{"name":"Por fatura","event":"invoice.created","model":"unit","config":{"amount":"0.50"}}]`)
	if code != 0 || calls.last != "PUT /v1/apps/app1/billing/plan-versions/v1/prices" || !strings.Contains(calls.lastBody, `"prices":[{`) {
		t.Fatalf("prices set: %d %q body=%s %s", code, calls.last, calls.lastBody, out)
	}
	code, _, err := run(t, dir, f.srv.URL, "app", "billing", "prices", "set", "v1", "--app", "app1", "--input", `{"nope": 1}`)
	if code != 2 || !strings.Contains(err, "JSON array of prices") {
		t.Fatalf("prices set bad input: %d %s", code, err)
	}
	code, out, _ = run(t, dir, f.srv.URL, "app", "billing", "prices", "add", "v1", "--app", "app1",
		"--name", "Lote", "--event", "invoice.bulk", "--model", "package", "--config", `{"amount":"9.90","size":100}`)
	if code != 0 || calls.last != "POST /v1/apps/app1/billing/plan-versions/v1/prices" || !strings.Contains(calls.lastBody, `"size":100`) || !strings.Contains(out, "R$ 9.90 per 100") {
		t.Fatalf("prices add: %d %q body=%s %s", code, calls.last, calls.lastBody, out)
	}

	code, out, _ = run(t, dir, f.srv.URL, "app", "billing", "preview", "v1", "--app", "app1", "--quantity", "invoice.created=10")
	if code != 0 || !strings.Contains(calls.lastBody, `"invoice.created":"10"`) || !strings.Contains(out, "Total: R$ 5.0000") {
		t.Fatalf("preview: %d body=%s %s", code, calls.lastBody, out)
	}

	// publish is Tier 1: exit 5 with the approval URL, JSON payload has next_step
	code, out, err = run(t, dir, f.srv.URL, "app", "billing", "publish", "v1", "--default", "--app", "app1", "--json")
	if code != 5 || calls.last != "POST /v1/apps/app1/billing/plan-versions/v1/publish" || !strings.Contains(calls.lastBody, `"set_as_default":true`) ||
		!strings.Contains(out, "https://console.test/workspace/approvals/cs9") {
		t.Fatalf("publish: %d %q body=%s out=%s err=%s", code, calls.last, calls.lastBody, out, err)
	}

	// workspace reports
	code, out, _ = run(t, dir, f.srv.URL, "billing", "revenue", "--workspace", "ws1")
	if code != 0 || calls.last != "GET /v1/workspaces/ws1/billing/reports/revenue" || !strings.Contains(out, "2026-09") || !strings.Contains(out, "R$ 40.0000") {
		t.Fatalf("revenue: %d %q %s", code, calls.last, out)
	}
	code, out, _ = run(t, dir, f.srv.URL, "billing", "invoices", "--workspace", "ws1", "--status", "issued", "--competency", "2026-09")
	if code != 0 || calls.query != "competency=2026-09&status=issued" || !strings.Contains(out, "i1") {
		t.Fatalf("invoices: %d query=%q %s", code, calls.query, out)
	}
}
