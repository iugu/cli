package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/iugu-private/platform2-cli/internal/auth"
	"github.com/iugu-private/platform2-cli/internal/config"
	"github.com/iugu-private/platform2-cli/internal/output"
)

// DefaultScopes is what `iugu login` asks for unless --scopes narrows or widens it.
const DefaultScopes = "openid profile email offline_access console:read console:apps.write console:installs console:credentials console:testing console:approvals"

type pendingDevice struct {
	Device   auth.DeviceAuthorization `json:"device"`
	Issuer   string                   `json:"issuer"`
	API      string                   `json:"api"`
	Resource string                   `json:"resource"`
	ClientID string                   `json:"client_id"`
	Scope    string                   `json:"scope"`
	Created  time.Time                `json:"created"`
}

func (rt *Runtime) loginCommand() *cobra.Command {
	var device, noBrowser, nonInteractive, wait bool
	var scopes, workspace, complete string
	var port int
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to Console (browser, device code, or a non-blocking handoff for agents)",
		Long: `Log in to Console's authorization server and store a grant-bound login in the OS keychain.

  Human with a browser      iugu login                      opens the browser; loopback redirect (PKCE S256)
  Headless / SSH            iugu login --device             prints a code to type at the verification URL
  Agent (non-TTY, CI, Claude Code, Codex, OpenCode, Cursor — detected automatically)
                            iugu login                      prints JSON {verification_uri, user_code, next_step} and exits 0;
                                                            the human opens the URL; then: iugu login --complete <handle> [--wait]
  Widen a grant             iugu login --workspace <id>     re-consent merges the workspace into the existing grant
                            iugu login --scopes "console:read console:gia.write"

The consent page shows the scopes and the workspaces; the agent never gets more than you tick.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if complete != "" {
				return rt.completeDevice(ctx, complete, wait)
			}
			if scopes == "" {
				scopes = DefaultScopes
			}
			oauth, err := rt.discover(ctx)
			if err != nil {
				return err
			}
			resource := rt.prm.Resource
			if nonInteractive || (rt.IsAgent() && !device && !noBrowser) {
				return rt.deviceHandoff(ctx, oauth, scopes, resource)
			}
			if device {
				return rt.deviceInteractive(ctx, oauth, scopes, resource)
			}
			return rt.browserLogin(ctx, oauth, scopes, resource, workspace, noBrowser, port)
		},
	}
	cmd.Flags().BoolVar(&device, "device", false, "device flow: type a code at the verification URL (headless / SSH)")
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "print the authorization URL instead of opening the browser")
	cmd.Flags().BoolVar(&nonInteractive, "non-interactive", false, "start a device login, print JSON with the URL and code, exit; finish with --complete")
	cmd.Flags().StringVar(&complete, "complete", "", "finish a non-interactive login by handle (poll once; add --wait to block until decided)")
	cmd.Flags().BoolVar(&wait, "wait", false, "with --complete: block until the human decides (or the code expires)")
	cmd.Flags().StringVar(&scopes, "scopes", "", "space-separated scopes (default: all console:* scopes + openid profile email offline_access)")
	cmd.Flags().StringVar(&workspace, "workspace", "", "pre-select a workspace on the consent page (short id)")
	cmd.Flags().IntVar(&port, "port", 0, "fixed loopback port for the redirect (default: random)")
	return cmd
}

func (rt *Runtime) browserLogin(ctx context.Context, oauth *auth.Client, scopes, resource, workspace string, noBrowser bool, port int) error {
	pkce, err := auth.NewPKCE()
	if err != nil {
		return err
	}
	state, err := auth.RandomState()
	if err != nil {
		return err
	}
	lb, err := auth.ListenLoopback(port)
	if err != nil {
		rt.Printer.Line("cannot bind a loopback port (%v); falling back to the device flow", err)
		return rt.deviceInteractive(ctx, oauth, scopes, resource)
	}
	deviceID, deviceName := rt.deviceIdentity()
	url := oauth.AuthorizeURL(auth.AuthorizationRequest{Scope: scopes, Resource: resource, Workspace: workspace, DeviceID: deviceID, DeviceName: deviceName}, lb.RedirectURI(), state, pkce)
	if noBrowser || !openBrowser(url) {
		rt.Printer.Line("Open this URL in your browser to log in:\n\n  %s\n", url)
		if rt.Printer.JSON {
			output.NDJSON(rt.Printer.Out, map[string]any{"event": "authorization_url", "url": url, "redirect_uri": lb.RedirectURI()})
		}
	} else {
		rt.Printer.Line("Your browser was opened to log in. If it did not, open:\n\n  %s\n", url)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	code, err := lb.Wait(ctx, state, oauth.Metadata.Issuer)
	if err != nil {
		return fmt.Errorf("login not completed: %w", err)
	}
	ts, err := oauth.ExchangeCode(ctx, code, lb.RedirectURI(), pkce.Verifier)
	if err != nil {
		return err
	}
	return rt.finishLogin(ts, oauth.Metadata.Issuer, resource)
}

func (rt *Runtime) deviceInteractive(ctx context.Context, oauth *auth.Client, scopes, resource string) error {
	deviceID, deviceName := rt.deviceIdentity()
	da, err := oauth.StartDevice(ctx, scopes, resource, deviceID, deviceName)
	if err != nil {
		return err
	}
	rt.Printer.Line("Open %s and enter the code %s\n(or open %s)\nWaiting for approval…", da.VerificationURI, da.UserCode, da.VerificationURIComplete)
	_ = openBrowser(da.VerificationURIComplete)
	ts, err := oauth.PollDevice(ctx, da, nil)
	if err != nil {
		return fmt.Errorf("login not completed: %w", err)
	}
	return rt.finishLogin(ts, oauth.Metadata.Issuer, resource)
}

// deviceHandoff never blocks: it stores the pending device code and prints what the agent must show.
func (rt *Runtime) deviceHandoff(ctx context.Context, oauth *auth.Client, scopes, resource string) error {
	deviceID, deviceName := rt.deviceIdentity()
	da, err := oauth.StartDevice(ctx, scopes, resource, deviceID, deviceName)
	if err != nil {
		return err
	}
	handle, err := auth.RandomState()
	if err != nil {
		return err
	}
	handle = strings.ToLower(handle[:12])
	pending := pendingDevice{Device: *da, Issuer: oauth.Metadata.Issuer, API: rt.profile.API, Resource: resource, ClientID: oauth.ClientID, Scope: scopes, Created: time.Now()}
	data, _ := json.Marshal(pending)
	if err := rt.store.Set("pending:"+handle, data); err != nil {
		return fmt.Errorf("cannot store the pending login: %w", err)
	}
	payload := map[string]any{
		"status":                    "authorization_pending",
		"verification_uri":          da.VerificationURI,
		"verification_uri_complete": da.VerificationURIComplete,
		"user_code":                 da.UserCode,
		"expires_in":                da.ExpiresIn,
		"interval":                  da.Interval,
		"handle":                    handle,
		"next_step":                 fmt.Sprintf("iugu login --complete %s --wait", handle),
		"instructions":              "Show verification_uri_complete (or verification_uri + user_code) to the human. When they approve, run next_step.",
	}
	rt.Printer.Result(payload, func(w io.Writer) {
		fmt.Fprintf(w, "Ask the human to open %s\n(or %s and enter %s). Then run:\n\n  iugu login --complete %s --wait\n", da.VerificationURIComplete, da.VerificationURI, da.UserCode, handle)
	})
	return nil
}

func (rt *Runtime) completeDevice(ctx context.Context, handle string, wait bool) error {
	data, err := rt.store.Get("pending:" + handle)
	if err != nil {
		return &output.Exit{Code: output.ExitUsage, Message: "unknown or expired login handle; start again with `iugu login`"}
	}
	var pending pendingDevice
	if err := json.Unmarshal(data, &pending); err != nil {
		return err
	}
	issuer := strings.TrimRight(pending.Issuer, "/")
	oauth := &auth.Client{HTTP: rt.HTTP, ClientID: pending.ClientID, Metadata: &auth.Metadata{Issuer: pending.Issuer, TokenEndpoint: issuer + "/token", RevocationEndpoint: issuer + "/revoke"}}
	var ts *auth.TokenSet
	if wait {
		ts, err = oauth.PollDevice(ctx, &pending.Device, nil)
	} else {
		ts, err = oauth.PollDeviceOnce(ctx, pending.Device.DeviceCode)
	}
	if errors.Is(err, auth.ErrPending) {
		payload := map[string]any{"status": "authorization_pending", "handle": handle, "user_code": pending.Device.UserCode,
			"verification_uri_complete": pending.Device.VerificationURIComplete, "expires_at": pending.Device.ExpiresAt,
			"next_step": fmt.Sprintf("iugu login --complete %s --wait", handle)}
		rt.Printer.Result(payload, func(w io.Writer) {
			fmt.Fprintln(w, "Still waiting for the human to approve the code", pending.Device.UserCode)
		})
		return nil
	}
	if err != nil {
		_ = rt.store.Delete("pending:" + handle)
		return fmt.Errorf("login not completed: %w", err)
	}
	_ = rt.store.Delete("pending:" + handle)
	rt.profile.API = pending.API
	return rt.finishLogin(ts, pending.Issuer, pending.Resource)
}

func (rt *Runtime) finishLogin(ts *auth.TokenSet, issuer, resource string) error {
	session, err := auth.FromTokens(ts, issuer, rt.profile.API, resource, rt.profile.ClientID)
	if err != nil {
		return err
	}
	_, session.Device = rt.deviceIdentity()
	if err := session.Save(rt.store, rt.profileName); err != nil {
		return fmt.Errorf("storing the login: %w", err)
	}
	ignoreProjectLocalConfigDir()
	if _, ok := rt.configFile.Profiles[rt.profileName]; !ok || rt.configFile.Profiles[rt.profileName].API != rt.profile.API {
		p := rt.configFile.Profiles[rt.profileName]
		p.API = rt.profile.API
		rt.configFile.Profiles[rt.profileName] = p
		_ = rt.configFile.Save()
	}
	rt.session = session
	payload := statusPayload(session, rt.store.Name(), true)
	rt.Printer.Result(payload, func(w io.Writer) {
		fmt.Fprintf(w, "Logged in as %s (%s) — %d scope(s), %d workspace(s); stored in the %s.\n", session.Email, session.Sub, len(session.Scopes), len(session.Workspaces), rt.store.Name())
	})
	return nil
}

func statusPayload(s *auth.Session, storeName string, loggedIn bool) map[string]any {
	if s == nil {
		return map[string]any{"logged_in": false, "store": storeName}
	}
	return map[string]any{
		"logged_in": loggedIn, "sub": s.Sub, "email": s.Email, "name": s.Name, "client_id": s.ClientID, "issuer": s.Issuer, "api": s.API,
		"scopes": s.Scopes, "workspaces": s.Workspaces, "grant_id": s.GrantID, "store": storeName, "expires_at": s.ExpiresAt, "logged_in_at": s.LoggedInAt,
		"device": s.Device,
	}
}

func openBrowser(url string) bool {
	if os.Getenv("IUGU_NO_BROWSER") != "" {
		return false
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return false
		}
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start() == nil
}

// ignoreProjectLocalConfigDir adds IUGU_CONFIG_DIR to .gitignore when it lives under the working directory
// (sandboxed agents keep their login in the workspace: `IUGU_CONFIG_DIR=.iugu iugu login`).
func ignoreProjectLocalConfigDir() {
	dir := os.Getenv(config.EnvConfigDir)
	if dir == "" {
		return
	}
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return
	}
	ensureGitignore(cwd, filepath.ToSlash(rel)+"/")
}

// deviceIdentity is what the AS keys the grant by (one grant per stored login) and the label humans see.
// A missing or unwritable config dir degrades to "no device" (legacy shared grant) rather than failing the login.
func (rt *Runtime) deviceIdentity() (id, name string) {
	name = config.DeviceName(rt.profileName)
	if rt.configFile == nil {
		return "", name
	}
	id, err := rt.configFile.EnsureDeviceID()
	if err != nil {
		rt.Printer.Line("warning: could not save a device id in the config dir (%v); this login will share the client's default grant", err)
		return "", name
	}
	return id, name
}
