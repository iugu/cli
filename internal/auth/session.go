package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/iugu-private/platform2-cli/internal/store"
)

// Session is the stored login of a profile: the grant-bound refresh token plus the facts the CLI
// shows in `iugu auth status`. The access token is cached but never required to be present.
type Session struct {
	Issuer       string    `json:"issuer"`
	API          string    `json:"api"`
	Resource     string    `json:"resource"`
	ClientID     string    `json:"client_id"`
	Sub          string    `json:"sub"`
	Email        string    `json:"email,omitempty"`
	Name         string    `json:"name,omitempty"`
	GrantID      string    `json:"grant_id"`
	Scopes       []string  `json:"scopes"`
	Workspaces   []string  `json:"workspaces"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	AccessToken  string    `json:"access_token,omitempty"`
	ExpiresAt    time.Time `json:"expires_at"`
	LoggedInAt   time.Time `json:"logged_in_at"`
	Device       string    `json:"device,omitempty"`
}

// FromTokens builds a session from a fresh token set.
func FromTokens(ts *TokenSet, issuer, api, resource, clientID string) (*Session, error) {
	claims, err := Claims(ts.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("access token: %w", err)
	}
	s := &Session{Issuer: issuer, API: api, Resource: resource, ClientID: clientID, LoggedInAt: time.Now()}
	s.apply(ts, claims)
	if ts.IDToken != "" {
		if id, err := Claims(ts.IDToken); err == nil {
			s.Email, _ = id["email"].(string)
			s.Name, _ = id["name"].(string)
		}
	}
	return s, nil
}

func (s *Session) apply(ts *TokenSet, claims map[string]any) {
	s.AccessToken = ts.AccessToken
	s.ExpiresAt = ts.ExpiresAt
	if ts.RefreshToken != "" {
		s.RefreshToken = ts.RefreshToken
	}
	s.Sub, _ = claims["sub"].(string)
	s.GrantID, _ = claims["grant_id"].(string)
	if scope, ok := claims["scope"].(string); ok {
		s.Scopes = strings.Fields(scope)
	}
	s.Workspaces = nil
	if wsp, ok := claims["wsp"].([]any); ok {
		for _, w := range wsp {
			if str, ok := w.(string); ok {
				s.Workspaces = append(s.Workspaces, str)
			}
		}
	}
}

// Load reads the session of a profile.
func Load(st store.Store, profile string) (*Session, error) {
	data, err := st.Get(profile)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("stored login is corrupt: %w", err)
	}
	return &s, nil
}

// Save persists the session.
func (s *Session) Save(st store.Store, profile string) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return st.Set(profile, data)
}

// ErrLoginRequired means there is no usable login (exit code 4 in the CLI).
var ErrLoginRequired = errors.New("login required: run `iugu login`")

// Fresh returns a valid access token, refreshing (and re-saving) when it expires within 30 seconds.
func (s *Session) Fresh(ctx context.Context, c *Client, st store.Store, profile string) (string, error) {
	if s.AccessToken != "" && time.Until(s.ExpiresAt) > 30*time.Second {
		return s.AccessToken, nil
	}
	if s.RefreshToken == "" {
		return "", ErrLoginRequired
	}
	// Serialise refreshes across concurrent CLI processes: the refresh token rotates on use and reuse
	// revokes the whole grant, so a second process must adopt the first one's result instead of
	// presenting the same (now burnt) token.
	unlock := lockRefresh(profile)
	defer unlock()
	if st != nil {
		if other, err := Load(st, profile); err == nil && other.RefreshToken != s.RefreshToken && other.AccessToken != "" && time.Until(other.ExpiresAt) > 30*time.Second {
			*s = *other
			return s.AccessToken, nil
		}
	}
	ts, err := c.Refresh(ctx, s.RefreshToken, "")
	if err != nil {
		var oe *OAuthError
		if errors.As(err, &oe) && (oe.Code == "invalid_grant" || oe.Code == "invalid_client") {
			return "", fmt.Errorf("%w (the grant was revoked or expired: %v)", ErrLoginRequired, oe)
		}
		return "", err
	}
	claims, err := Claims(ts.AccessToken)
	if err != nil {
		return "", err
	}
	s.apply(ts, claims)
	if st != nil {
		if err := s.Save(st, profile); err != nil {
			return "", fmt.Errorf("saving refreshed login: %w", err)
		}
	}
	return s.AccessToken, nil
}

// LockDir holds the refresh lock files (the CLI sets it to its config directory).
var LockDir = os.TempDir()

// lockRefresh takes a best-effort, portable inter-process lock (O_EXCL file; stale after 30 s; gives
// up after 20 s so a wedged lock never blocks a login forever).
func lockRefresh(profile string) (unlock func()) {
	path := filepath.Join(LockDir, "refresh-"+sanitize(profile)+".lock")
	deadline := time.Now().Add(20 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = f.Close()
			return func() { _ = os.Remove(path) }
		}
		if info, statErr := os.Stat(path); statErr == nil && time.Since(info.ModTime()) > 30*time.Second {
			_ = os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return func() {}
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
