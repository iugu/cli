// Package api is the HTTP client of the Lifecycle API (contract: https://developer.iugu.com/console/api-v1.yaml).
// Responses are kept as generic JSON (the CLI mostly prints them) with typed accessors for the facts
// the commands act on (status, approval URL, ids, secrets).
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TokenSource yields a fresh bearer token.
type TokenSource func(ctx context.Context) (string, error)

// Client calls /v1.
type Client struct {
	BaseURL string
	HTTP    interface {
		Do(*http.Request) (*http.Response, error)
	}
	Token     TokenSource
	UserAgent string
	// OnGate is consulted when the API answers a "human first" 403 (elevation_required, insufficient_scope,
	// workspace_not_consented). It may involve the human (open the URL, wait) and return true to have the
	// request retried once with the same token: the server changed the grant, not the token.
	OnGate func(ctx context.Context, err *Error) bool
}

// Gate codes the API resolves on the grant side (see OnGate).
var GateCodes = map[string]bool{"elevation_required": true, "insufficient_scope": true, "workspace_not_consented": true}

// Error is an API error response ({ "error": { code, message, details } }).
type Error struct {
	Status          int            `json:"status"`
	Code            string         `json:"code"`
	Message         string         `json:"message"`
	Details         map[string]any `json:"details,omitempty"`
	WWWAuthenticate string         `json:"www_authenticate,omitempty"`
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s (HTTP %d): %s", e.Code, e.Status, e.Message)
	}
	return fmt.Sprintf("%s (HTTP %d)", e.Code, e.Status)
}

// Response is a decoded JSON response with the headers the CLI cares about.
type Response struct {
	Status int
	Body   map[string]any
	Raw    []byte
	ETag   string
}

// Options tunes one request.
type Options struct {
	IdempotencyKey string
	IfMatch        string
	Query          url.Values
	ContentType    string
	RawBody        io.Reader
}

// Do performs a JSON request. Bodies that are not JSON objects are returned in Raw only.
func (c *Client) Do(ctx context.Context, method, path string, body any, opts *Options) (*Response, error) {
	r, err := c.once(ctx, method, path, body, opts)
	var apiErr *Error
	if err != nil && c.OnGate != nil && errors.As(err, &apiErr) && GateCodes[apiErr.Code] && opts.rawBodyUnused() && c.OnGate(ctx, apiErr) {
		return c.once(ctx, method, path, body, opts)
	}
	return r, err
}

func (o *Options) rawBodyUnused() bool { return o == nil || o.RawBody == nil }

func (c *Client) once(ctx context.Context, method, path string, body any, opts *Options) (*Response, error) {
	if opts == nil {
		opts = &Options{}
	}
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(opts.Query) > 0 {
		u += "?" + opts.Query.Encode()
	}
	var reader io.Reader
	contentType := "application/json"
	switch {
	case opts.RawBody != nil:
		reader = opts.RawBody
		contentType = opts.ContentType
	case body != nil:
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(data)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return nil, err
	}
	if reader != nil {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if opts.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", opts.IdempotencyKey)
	}
	if opts.IfMatch != "" {
		req.Header.Set("If-Match", opts.IfMatch)
	}
	if c.Token != nil {
		token, err := c.Token(ctx)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	out := &Response{Status: resp.StatusCode, Raw: raw, ETag: resp.Header.Get("ETag")}
	if len(raw) > 0 && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		var parsed map[string]any
		if json.Unmarshal(raw, &parsed) == nil {
			out.Body = parsed
		}
	}
	if resp.StatusCode >= 400 {
		return out, c.decodeError(out, resp)
	}
	return out, nil
}

func (c *Client) decodeError(r *Response, resp *http.Response) error {
	e := &Error{Status: r.Status, WWWAuthenticate: resp.Header.Get("WWW-Authenticate")}
	if inner, ok := r.Body["error"].(map[string]any); ok {
		e.Code, _ = inner["code"].(string)
		e.Message, _ = inner["message"].(string)
		e.Details, _ = inner["details"].(map[string]any)
	}
	if e.Code == "" {
		switch r.Status {
		case http.StatusUnauthorized:
			e.Code = "invalid_token"
		case http.StatusTooManyRequests:
			e.Code = "rate_limited"
		default:
			e.Code = fmt.Sprintf("http_%d", r.Status)
		}
		e.Message = strings.TrimSpace(string(r.Raw))
		if len(e.Message) > 300 {
			e.Message = e.Message[:300] + "…"
		}
	}
	return e
}

// Get/Post/Patch/Put/Delete are thin wrappers.
func (c *Client) Get(ctx context.Context, path string, query url.Values) (*Response, error) {
	return c.Do(ctx, http.MethodGet, path, nil, &Options{Query: query})
}
func (c *Client) Post(ctx context.Context, path string, body any, opts *Options) (*Response, error) {
	return c.Do(ctx, http.MethodPost, path, body, opts)
}
func (c *Client) Patch(ctx context.Context, path string, body any, opts *Options) (*Response, error) {
	return c.Do(ctx, http.MethodPatch, path, body, opts)
}
func (c *Client) Put(ctx context.Context, path string, body any, opts *Options) (*Response, error) {
	return c.Do(ctx, http.MethodPut, path, body, opts)
}
func (c *Client) Delete(ctx context.Context, path string) (*Response, error) {
	return c.Do(ctx, http.MethodDelete, path, nil, nil)
}

// ListAll follows `next_cursor` and returns every item of a paginated collection (bounded by max).
func (c *Client) ListAll(ctx context.Context, path string, query url.Values, max int) ([]any, error) {
	if query == nil {
		query = url.Values{}
	}
	items := []any{} // never nil: `--json` consumers expect an array
	for {
		r, err := c.Get(ctx, path, query)
		if err != nil {
			return items, err
		}
		data, _ := r.Body["data"].([]any)
		items = append(items, data...)
		next, _ := r.Body["next_cursor"].(string)
		if next == "" || (max > 0 && len(items) >= max) {
			return items, nil
		}
		query.Set("cursor", next)
	}
}

// IsCode reports whether err is an API error with the given code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

// Str/Map/List are tolerant accessors for generic JSON.
func Str(m map[string]any, keys ...string) string {
	cur := any(m)
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	switch v := cur.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%v", v)
	case bool:
		return fmt.Sprintf("%t", v)
	}
	return ""
}

func Map(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func List(m map[string]any, key string) []any {
	v, _ := m[key].([]any)
	return v
}
