package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// DeviceAuthorization is the RFC 8628 §3.2 response.
type DeviceAuthorization struct {
	DeviceCode              string    `json:"device_code"`
	UserCode                string    `json:"user_code"`
	VerificationURI         string    `json:"verification_uri"`
	VerificationURIComplete string    `json:"verification_uri_complete"`
	ExpiresIn               int       `json:"expires_in"`
	Interval                int       `json:"interval"`
	ExpiresAt               time.Time `json:"expires_at"`
}

// StartDevice requests a device code.
func (c *Client) StartDevice(ctx context.Context, scope, resource string) (*DeviceAuthorization, error) {
	if c.Metadata.DeviceAuthorizationEndpoint == "" {
		return nil, errors.New("authorization server has no device authorization endpoint")
	}
	body, status, err := c.post(ctx, c.Metadata.DeviceAuthorizationEndpoint, url.Values{"client_id": {c.ClientID}, "scope": {scope}, "resource": {resource}})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		oe := &OAuthError{Status: status}
		if json.Unmarshal(body, oe) != nil || oe.Code == "" {
			oe.Code = fmt.Sprintf("http_%d", status)
		}
		return nil, oe
	}
	var da DeviceAuthorization
	if err := json.Unmarshal(body, &da); err != nil {
		return nil, err
	}
	if da.Interval <= 0 {
		da.Interval = 5
	}
	da.ExpiresAt = time.Now().Add(time.Duration(da.ExpiresIn) * time.Second)
	return &da, nil
}

// ErrPending is returned by PollDeviceOnce while the user has not decided yet.
var ErrPending = errors.New("authorization_pending")

// PollDeviceOnce makes one token request for the device code. It returns ErrPending, or an
// *OAuthError with Code slow_down / access_denied / expired_token, or the tokens.
func (c *Client) PollDeviceOnce(ctx context.Context, deviceCode string) (*TokenSet, error) {
	ts, err := c.tokenRequest(ctx, url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {deviceCode}})
	var oe *OAuthError
	if errors.As(err, &oe) && oe.Code == "authorization_pending" {
		return nil, ErrPending
	}
	return ts, err
}

// PollDevice polls until the user decides, honouring interval and slow_down.
func (c *Client) PollDevice(ctx context.Context, da *DeviceAuthorization, onTick func(remaining time.Duration)) (*TokenSet, error) {
	interval := time.Duration(da.Interval) * time.Second
	for {
		if time.Now().After(da.ExpiresAt) {
			return nil, &OAuthError{Code: "expired_token", Description: "the device code expired before the login was completed"}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}
		ts, err := c.PollDeviceOnce(ctx, da.DeviceCode)
		if err == nil {
			return ts, nil
		}
		if errors.Is(err, ErrPending) {
			if onTick != nil {
				onTick(time.Until(da.ExpiresAt))
			}
			continue
		}
		var oe *OAuthError
		if errors.As(err, &oe) && oe.Code == "slow_down" {
			interval += 5 * time.Second
			continue
		}
		return nil, err
	}
}
