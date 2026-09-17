package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TokenSet is what /token returns.
type TokenSet struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	IDToken      string    `json:"id_token,omitempty"`
	TokenType    string    `json:"token_type"`
	ExpiresIn    int       `json:"expires_in"`
	Scope        string    `json:"scope"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// OAuthError is an RFC 6749 error response.
type OAuthError struct {
	Status      int    `json:"-"`
	Code        string `json:"error"`
	Description string `json:"error_description"`
}

func (e *OAuthError) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("%s: %s", e.Code, e.Description)
	}
	return e.Code
}

// Client talks to the token, device and revocation endpoints as a public client.
type Client struct {
	HTTP     HTTPClient
	Metadata *Metadata
	ClientID string
}

func (c *Client) post(ctx context.Context, endpoint string, form url.Values) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return body, resp.StatusCode, err
}

func (c *Client) tokenRequest(ctx context.Context, form url.Values) (*TokenSet, error) {
	form.Set("client_id", c.ClientID)
	body, status, err := c.post(ctx, c.Metadata.TokenEndpoint, form)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		oe := &OAuthError{Status: status}
		if json.Unmarshal(body, oe) != nil || oe.Code == "" {
			oe.Code = fmt.Sprintf("http_%d", status)
			oe.Description = strings.TrimSpace(string(body))
		}
		return nil, oe
	}
	var ts TokenSet
	if err := json.Unmarshal(body, &ts); err != nil {
		return nil, fmt.Errorf("token response is not JSON: %w", err)
	}
	if ts.AccessToken == "" {
		return nil, errors.New("token response has no access_token")
	}
	ts.ExpiresAt = time.Now().Add(time.Duration(ts.ExpiresIn) * time.Second)
	return &ts, nil
}

// ExchangeCode redeems an authorization code with the PKCE verifier.
func (c *Client) ExchangeCode(ctx context.Context, code, redirectURI, verifier string) (*TokenSet, error) {
	return c.tokenRequest(ctx, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {verifier},
	})
}

// Refresh rotates the refresh token; optional `scope` narrows the result.
func (c *Client) Refresh(ctx context.Context, refreshToken, scope string) (*TokenSet, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}}
	if scope != "" {
		form.Set("scope", scope)
	}
	return c.tokenRequest(ctx, form)
}

// Revoke revokes a refresh (or access) token — for agent clients the whole grant (RFC 7009).
func (c *Client) Revoke(ctx context.Context, token string) error {
	if c.Metadata.RevocationEndpoint == "" {
		return errors.New("authorization server has no revocation endpoint")
	}
	_, status, err := c.post(ctx, c.Metadata.RevocationEndpoint, url.Values{"token": {token}, "client_id": {c.ClientID}})
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("revocation failed with HTTP %d", status)
	}
	return nil
}

// Claims decodes a JWT payload without verifying it — the CLI only reads facts about its own token
// (sub, scope, wsp, grant_id, exp); the server is the one that verifies.
func Claims(jwt string) (map[string]any, error) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return nil, errors.New("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, err
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, err
	}
	return claims, nil
}
