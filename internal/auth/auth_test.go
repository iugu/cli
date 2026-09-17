package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iugu-private/platform2-cli/internal/store"
)

// fakeAS is a minimal Console authorization server: PRM chain, token endpoint with code / refresh /
// device grants, device authorization and revocation.
type fakeAS struct {
	srv          *httptest.Server
	api          *httptest.Server
	polls        atomic.Int32
	approveAfter int32
	refreshes    atomic.Int32
	revoked      []string
	lastVerifier string
}

func newFakeAS(t *testing.T) *fakeAS {
	f := &fakeAS{approveAfter: 2}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL + "/", "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"device_authorization_endpoint": f.srv.URL + "/device_authorization", "revocation_endpoint": f.srv.URL + "/revoke",
			"code_challenge_methods_supported": []string{"S256"}, "authorization_response_iss_parameter_supported": true,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			f.lastVerifier = r.Form.Get("code_verifier")
			if r.Form.Get("code") != "good-code" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "bad code"})
				return
			}
			json.NewEncoder(w).Encode(tokens("rt-1", "openid offline_access console:read"))
		case "refresh_token":
			f.refreshes.Add(1)
			if r.Form.Get("refresh_token") != "rt-1" && r.Form.Get("refresh_token") != "rt-2" {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
				return
			}
			json.NewEncoder(w).Encode(tokens("rt-2", "openid offline_access console:read"))
		case "urn:ietf:params:oauth:grant-type:device_code":
			n := f.polls.Add(1)
			if n == 1 {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "slow_down"})
				return
			}
			if n <= f.approveAfter {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
				return
			}
			json.NewEncoder(w).Encode(tokens("rt-dev", "console:read offline_access"))
		default:
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "unsupported_grant_type"})
		}
	})
	mux.HandleFunc("/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"device_code": "dc", "user_code": "ABCD-EFGH", "verification_uri": f.srv.URL + "/device",
			"verification_uri_complete": f.srv.URL + "/device?user_code=ABCD-EFGH", "expires_in": 600, "interval": 0})
	})
	mux.HandleFunc("/revoke", func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		f.revoked = append(f.revoked, r.Form.Get("token"))
		w.Write([]byte("{}"))
	})
	f.srv = httptest.NewServer(mux)
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"resource": f.api.URL, "authorization_servers": []string{f.srv.URL + "/"}})
	})
	f.api = httptest.NewServer(apiMux)
	t.Cleanup(func() { f.srv.Close(); f.api.Close() })
	return f
}

func tokens(refresh, scope string) map[string]any {
	return map[string]any{"access_token": fakeJWT(map[string]any{"sub": "user:abc", "scope": scope, "grant_id": "g1", "wsp": []string{"ws1"}, "exp": time.Now().Add(5 * time.Minute).Unix()}),
		"id_token": fakeJWT(map[string]any{"email": "dev@iugu.test", "name": "Dev"}), "refresh_token": refresh, "token_type": "Bearer", "expires_in": 300, "scope": scope}
}

func fakeJWT(claims map[string]any) string {
	payload, _ := json.Marshal(claims)
	return "eyJhbGciOiJub25lIn0." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestDiscoveryFollowsPRM(t *testing.T) {
	f := newFakeAS(t)
	prm, md, err := Discover(context.Background(), http.DefaultClient, f.api.URL)
	if err != nil {
		t.Fatal(err)
	}
	if prm.Resource != f.api.URL || md.TokenEndpoint != f.srv.URL+"/token" || !md.IssParameterSupported {
		t.Fatalf("unexpected discovery: %+v %+v", prm, md)
	}
}

func TestPKCEAndAuthorizeURL(t *testing.T) {
	f := newFakeAS(t)
	_, md, _ := Discover(context.Background(), http.DefaultClient, f.api.URL)
	c := &Client{HTTP: http.DefaultClient, Metadata: md, ClientID: "cli"}
	pkce, _ := NewPKCE()
	if len(pkce.Verifier) < 43 || pkce.Challenge == "" {
		t.Fatal("bad pkce")
	}
	u, _ := url.Parse(c.AuthorizeURL(AuthorizationRequest{Scope: "console:read", Resource: "https://api", Workspace: "ws1"}, "http://127.0.0.1:1/callback", "st", pkce))
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("resource") != "https://api" || q.Get("workspace") != "ws1" || q.Get("client_id") != "cli" {
		t.Fatalf("bad authorize url: %s", u)
	}
}

func TestLoopbackValidatesStateAndIss(t *testing.T) {
	f := newFakeAS(t)
	_, md, _ := Discover(context.Background(), http.DefaultClient, f.api.URL)
	lb, err := ListenLoopback(0)
	if err != nil {
		t.Fatal(err)
	}
	go http.Get(lb.RedirectURI() + "?code=good-code&state=wrong&iss=" + url.QueryEscape(md.Issuer))
	if _, err := lb.Wait(context.Background(), "right", md.Issuer); err == nil || !strings.Contains(err.Error(), "state") {
		t.Fatalf("expected state error, got %v", err)
	}
	lb, _ = ListenLoopback(0)
	go http.Get(lb.RedirectURI() + "?code=good-code&state=right&iss=https%3A%2F%2Fevil.test%2F")
	if _, err := lb.Wait(context.Background(), "right", md.Issuer); err == nil || !strings.Contains(err.Error(), "RFC 9207") {
		t.Fatalf("expected iss error, got %v", err)
	}
	lb, _ = ListenLoopback(0)
	go http.Get(lb.RedirectURI() + "?code=good-code&state=right&iss=" + url.QueryEscape(md.Issuer))
	code, err := lb.Wait(context.Background(), "right", md.Issuer)
	if err != nil || code != "good-code" {
		t.Fatalf("expected code, got %q %v", code, err)
	}
	lb, _ = ListenLoopback(0)
	go http.Get(lb.RedirectURI() + "?error=access_denied&state=right")
	var oe *OAuthError
	if _, err := lb.Wait(context.Background(), "right", md.Issuer); !errors.As(err, &oe) || oe.Code != "access_denied" {
		t.Fatalf("expected access_denied, got %v", err)
	}
}

func TestExchangeRefreshRevokeAndSession(t *testing.T) {
	f := newFakeAS(t)
	prm, md, _ := Discover(context.Background(), http.DefaultClient, f.api.URL)
	c := &Client{HTTP: http.DefaultClient, Metadata: md, ClientID: "cli"}
	if _, err := c.ExchangeCode(context.Background(), "bad", "http://127.0.0.1:1/callback", "v"); err == nil {
		t.Fatal("expected invalid_grant")
	}
	ts, err := c.ExchangeCode(context.Background(), "good-code", "http://127.0.0.1:1/callback", "verifier-123")
	if err != nil || f.lastVerifier != "verifier-123" {
		t.Fatalf("exchange: %v (verifier %q)", err, f.lastVerifier)
	}
	s, err := FromTokens(ts, md.Issuer, f.api.URL, prm.Resource, "cli")
	if err != nil {
		t.Fatal(err)
	}
	if s.Sub != "user:abc" || s.Email != "dev@iugu.test" || s.GrantID != "g1" || len(s.Workspaces) != 1 || s.RefreshToken != "rt-1" {
		t.Fatalf("bad session: %+v", s)
	}
	st := &store.Memory{}
	if err := s.Save(st, "default"); err != nil {
		t.Fatal(err)
	}
	loaded, _ := Load(st, "default")
	tok, err := loaded.Fresh(context.Background(), c, st, "default")
	if err != nil || tok != s.AccessToken || f.refreshes.Load() != 0 {
		t.Fatalf("fresh token should be reused without refresh: %v", err)
	}
	loaded.ExpiresAt = time.Now().Add(10 * time.Second)
	tok2, err := loaded.Fresh(context.Background(), c, st, "default")
	if err != nil || tok2 == "" || f.refreshes.Load() != 1 || loaded.RefreshToken != "rt-2" {
		t.Fatalf("expected a refresh with rotation: %v refreshes=%d rt=%s", err, f.refreshes.Load(), loaded.RefreshToken)
	}
	reloaded, _ := Load(st, "default")
	if reloaded.RefreshToken != "rt-2" {
		t.Fatal("rotated refresh token was not saved")
	}
	loaded.RefreshToken, loaded.AccessToken = "dead", ""
	_ = loaded.Save(st, "default") // the store agrees the token is dead (otherwise Fresh adopts the stored, fresher login)
	if _, err := loaded.Fresh(context.Background(), c, st, "default"); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("expected login required, got %v", err)
	}
	if err := c.Revoke(context.Background(), "rt-2"); err != nil || f.revoked[0] != "rt-2" {
		t.Fatalf("revoke: %v", err)
	}
}

func TestDevicePollingHonoursSlowDownAndPending(t *testing.T) {
	f := newFakeAS(t)
	_, md, _ := Discover(context.Background(), http.DefaultClient, f.api.URL)
	c := &Client{HTTP: http.DefaultClient, Metadata: md, ClientID: "cli"}
	da, err := c.StartDevice(context.Background(), "console:read", "https://api")
	if err != nil || da.UserCode != "ABCD-EFGH" || da.Interval != 5 {
		t.Fatalf("start device: %v %+v", err, da)
	}
	da.Interval = 0 // keep the test fast; the +5s slow_down bump is what we check
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	start := time.Now()
	ts, err := c.PollDevice(ctx, da, nil)
	if err != nil || ts.RefreshToken != "rt-dev" {
		t.Fatalf("poll: %v", err)
	}
	if time.Since(start) < 5*time.Second {
		t.Fatalf("slow_down must add 5s to the interval; took %s", time.Since(start))
	}
	if f.polls.Load() != f.approveAfter+1 {
		t.Fatalf("polls=%d", f.polls.Load())
	}
	if _, err := c.PollDeviceOnce(context.Background(), "dc"); err != nil {
		t.Fatalf("once after approval: %v", err)
	}
}

func TestFreshSerialisesConcurrentRefreshes(t *testing.T) {
	LockDir = t.TempDir()
	var refreshes int32
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/token" {
			http.NotFound(w, r)
			return
		}
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "rt-1" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		atomic.AddInt32(&refreshes, 1)
		time.Sleep(150 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rt-2","expires_in":1800,"token_type":"Bearer"}`, fakeJWT(map[string]any{"sub": "user:1", "exp": time.Now().Add(time.Hour).Unix()}))
	}))
	defer as.Close()
	client := &Client{ClientID: "cli", Metadata: &Metadata{Issuer: as.URL, TokenEndpoint: as.URL + "/token"}, HTTP: as.Client()}
	st := &store.Memory{}
	base := &Session{RefreshToken: "rt-1", ExpiresAt: time.Now().Add(-time.Minute)}
	if err := base.Save(st, "p"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, _ := Load(st, "p")
			_, err := s.Fresh(context.Background(), client, st, "p")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent refresh failed: %v", err)
		}
	}
	if n := atomic.LoadInt32(&refreshes); n != 1 {
		t.Fatalf("expected exactly one refresh, got %d", n)
	}
}
