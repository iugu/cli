// Package auth implements the client side of Console's authorization server for agent clients:
// discovery (RFC 9728 → RFC 8414), authorization code + PKCE with a loopback redirect (RFC 8252),
// the device flow (RFC 8628), grant-bound refresh with rotation, and revocation (RFC 7009).
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Metadata is the subset of RFC 8414 / OIDC discovery the CLI uses.
type Metadata struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	DeviceAuthorizationEndpoint   string   `json:"device_authorization_endpoint"`
	RevocationEndpoint            string   `json:"revocation_endpoint"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
	IssParameterSupported         bool     `json:"authorization_response_iss_parameter_supported"`
	ScopesSupported               []string `json:"scopes_supported"`
}

// ProtectedResource is RFC 9728 metadata of the Lifecycle API host.
type ProtectedResource struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

// HTTPClient is what the package needs from an http.Client.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// Discover follows the RFC 9728 chain from the API base URL to the authorization server metadata.
func Discover(ctx context.Context, hc HTTPClient, apiBase string) (*ProtectedResource, *Metadata, error) {
	apiBase = strings.TrimRight(apiBase, "/")
	var prm ProtectedResource
	if err := getJSON(ctx, hc, apiBase+"/.well-known/oauth-protected-resource", &prm); err != nil {
		return nil, nil, fmt.Errorf("protected resource metadata at %s: %w", apiBase, err)
	}
	if len(prm.AuthorizationServers) == 0 {
		return nil, nil, fmt.Errorf("protected resource metadata at %s names no authorization server", apiBase)
	}
	if prm.Resource == "" {
		prm.Resource = apiBase
	}
	issuer := strings.TrimRight(prm.AuthorizationServers[0], "/")
	var md Metadata
	if err := getJSON(ctx, hc, issuer+"/.well-known/oauth-authorization-server", &md); err != nil {
		return nil, nil, fmt.Errorf("authorization server metadata for %s: %w", issuer, err)
	}
	if md.Issuer == "" || md.AuthorizationEndpoint == "" || md.TokenEndpoint == "" {
		return nil, nil, fmt.Errorf("authorization server metadata for %s is incomplete", issuer)
	}
	if !contains(md.CodeChallengeMethodsSupported, "S256") {
		return nil, nil, fmt.Errorf("authorization server %s does not support PKCE S256", md.Issuer)
	}
	return &prm, &md, nil
}

func getJSON(ctx context.Context, hc HTTPClient, url string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func contains(list []string, item string) bool {
	for _, v := range list {
		if v == item {
			return true
		}
	}
	return false
}
