package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AuthorizationRequest builds the /authorize URL for the code flow.
type AuthorizationRequest struct {
	Scope     string
	Resource  string
	Workspace string // optional: pre-selects a workspace on the consent page (`iugu login --workspace`)
}

// Loopback is a one-shot HTTP listener on 127.0.0.1 for the redirect (RFC 8252 §7.3).
type Loopback struct {
	listener net.Listener
	server   *http.Server
	result   chan callback
}

type callback struct {
	code, state, iss, errCode, errDesc string
}

// ListenLoopback binds 127.0.0.1 on a random (or given) port.
func ListenLoopback(port int) (*Loopback, error) {
	ln, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	lb := &Loopback{listener: ln, result: make(chan callback, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		cb := callback{code: q.Get("code"), state: q.Get("state"), iss: q.Get("iss"), errCode: q.Get("error"), errDesc: q.Get("error_description")}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if cb.errCode != "" {
			fmt.Fprintf(w, page("Login not completed", "The authorization server answered: %s. You can close this tab and return to the terminal."), cb.errCode)
		} else {
			fmt.Fprint(w, page("iugu CLI: login complete", "You can close this tab and return to the terminal."))
		}
		select {
		case lb.result <- cb:
		default:
		}
	})
	lb.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go lb.server.Serve(ln) //nolint:errcheck
	return lb, nil
}

// RedirectURI is http://127.0.0.1:<port>/callback.
func (lb *Loopback) RedirectURI() string {
	return fmt.Sprintf("http://127.0.0.1:%d/callback", lb.listener.Addr().(*net.TCPAddr).Port)
}

// Wait blocks for the single callback and validates state and iss (RFC 9207).
func (lb *Loopback) Wait(ctx context.Context, state, expectedIss string) (string, error) {
	defer lb.Close()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case cb := <-lb.result:
		if cb.errCode != "" {
			return "", &OAuthError{Code: cb.errCode, Description: cb.errDesc}
		}
		if cb.state != state {
			return "", errors.New("authorization response state does not match (possible CSRF); aborting")
		}
		if expectedIss != "" && cb.iss != "" && strings.TrimRight(cb.iss, "/") != strings.TrimRight(expectedIss, "/") {
			return "", fmt.Errorf("authorization response came from %q, expected %q (RFC 9207 mix-up); aborting", cb.iss, expectedIss)
		}
		if cb.code == "" {
			return "", errors.New("authorization response has no code")
		}
		return cb.code, nil
	}
}

func (lb *Loopback) Close() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = lb.server.Shutdown(ctx)
}

// AuthorizeURL renders the authorization request.
func (c *Client) AuthorizeURL(req AuthorizationRequest, redirectURI, state string, pkce PKCE) string {
	q := url.Values{
		"client_id":             {c.ClientID},
		"response_type":         {"code"},
		"redirect_uri":          {redirectURI},
		"state":                 {state},
		"scope":                 {req.Scope},
		"resource":              {req.Resource},
		"code_challenge":        {pkce.Challenge},
		"code_challenge_method": {"S256"},
	}
	if req.Workspace != "" {
		q.Set("workspace", req.Workspace)
	}
	return c.Metadata.AuthorizationEndpoint + "?" + q.Encode()
}

func page(title, body string) string {
	return "<!doctype html><meta charset=utf-8><title>" + title + "</title><body style=\"font-family:system-ui;margin:3rem auto;max-width:32rem;color:#1f2328\"><h1 style=\"font-size:1.4rem\">" + title + "</h1><p>" + body + "</p></body>"
}
