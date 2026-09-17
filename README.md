# iugu — Platform 2 Console CLI

`iugu` is the command-line surface of the iugu Platform 2 Console for developers and the AI coding
agents that work with them: log in once, create and configure Console apps, get workspace-restricted
credentials without ever seeing them in a chat window, test authorization, publish. Every command has
`--json`, meaningful exit codes and a human-approval loop for sensitive operations (change sets).

The CLI talks to the **Lifecycle API** (`https://api.console.iugu.com/v1`, OpenAPI in Console's
`docs/public/api-v1.yaml`) with OAuth 2.1 tokens issued by Console's authorization server
(`https://identity.iugu.com`). It does not embed an MCP server: `iugu agent setup` points each harness at
the remote endpoint `https://mcp.console.iugu.com/mcp`.

## Install

```sh
brew install iugu-private/tap/iugu                                                        # macOS / Linux
curl -fsSL https://raw.githubusercontent.com/iugu-private/platform2-cli/main/install.sh | sh  # any unix
scoop bucket add iugu https://github.com/iugu-private/scoop-bucket && scoop install iugu  # Windows
npx @iugu/cli --help                                                                      # npm shim
```

Releases are built by goreleaser, signed with cosign (keyless) and ship an SBOM; `install.sh`, the
Homebrew cask and the npm shim verify the archive against the signed `checksums.txt`.

## Five minutes

```sh
iugu login                                   # browser: Console login + agent consent (scopes × workspaces)
iugu me                                      # who am I, which workspaces are consented
iugu app init --name "Acme Invoices" --workspace <dev-workspace-id>
#   → creates the app (private draft), installs it in the workspace, requests a workspace-restricted
#     credential (needs one human approval: exit 5 with approval.url), writes iugu.toml
iugu changeset wait <id> --write-env .env.local     # blocks until approved; secret → .env.local (0600)
iugu app permissions set --implemented invoice.create,invoice.read --grant-scopes informations
iugu test-principal create --name qa                # temp principal with roles ⊆ yours, 1 h
iugu verify --principal temp:… --action acme-invoices:invoice.create
iugu app env --format fly                           # non-secret environment for your deploy target
iugu changeset create --op 'oauth.update:{"url":"https://acme.example","callbacks":["https://acme.example/oauth/callback"]}' \
     --op 'listing.publish:{"public":true,"draft":false}' --submit --wait
```

Agents: run `iugu docs` (or `iugu docs --llms`) — the embedded skill explains the golden path, what
needs a human, how to wait, and how to handle secrets. `iugu agent setup --all` writes the remote MCP
entry and the skill for Claude Code, Codex, OpenCode, Cursor and VS Code.

## Authentication

| Situation | What happens |
|---|---|
| Human with a browser | `iugu login`: authorization code + PKCE (S256), redirect to a one-shot `http://127.0.0.1:<random>/callback`, `state` and `iss` validated. The browser runs the normal Console login (MFA, SSO) and the agent consent page (scopes × workspaces). |
| Headless / SSH | `iugu login --device`: RFC 8628 device flow; prints the URL and a code. |
| Agent (non-TTY, `CI`, `CLAUDECODE`, `CODEX_*`, `OPENCODE*`, `CURSOR_*`) | `iugu login` is non-blocking: prints `verification_uri_complete`, `user_code` and `next_step` (`iugu login --complete <handle> --wait`) and exits 0. Override with `--agent yes\|no\|auto`. |
| CI / unattended | `IUGU_TOKEN=<deploy token>` wins over any stored login and never touches disk. |
| Several accounts | `--profile`/`IUGU_PROFILE`; `iugu workspace use <id>`; `iugu login --workspace <id>` re-consents and merges a workspace into the grant. |

The refresh token is stored in the OS keychain (macOS Keychain, Secret Service, Windows Credential
Manager). When the keychain is unavailable — headless Linux, containers, a `$HOME` without a login
keychain — the CLI warns once and uses `~/.config/iugu/credentials.json` (0600). `--credentials-store
ephemeral` keeps it in memory only. `iugu auth status --json` always exits 0 and tells the truth in
`logged_in` (it checks the grant online; `--offline` to skip); `iugu logout` revokes the grant server-side.

**One grant per credential holder.** Every config dir (`~/.config/iugu`, a `--profile`, or a project-local
`IUGU_CONFIG_DIR=.iugu`) is a holder with its own stable `device_id`: Console keeps a separate grant for
each, lists them by label ("iugu CLI on mbp (.iugu in acme)") on the Connected agents page, and logging one
out never affects another. Sandboxed agents (Codex `workspace-write`) should keep their own login in the
project: `IUGU_CONFIG_DIR=.iugu iugu login` (auto-gitignored). The CLI never rotates a refresh token it
cannot persist (`store_not_writable`), so a read-only sandbox cannot burn the human's login.

## Conventions

- `--json` on every command: stable JSON on stdout, diagnostics on stderr, nothing else. `--jq <expr>`
  filters the result (strings print raw). Long waits stream NDJSON events (`{"event":…}`).
- Exit codes: `0` ok · `1` error · `2` usage/cancelled · `4` login required · `5` approval required (the
  payload carries `approval.url`, `change_set_id`, `next_step`) · `6` stale/conflict (re-read, retry).
- Tier 0 operations execute immediately; Tier 1 (credentials, OAuth settings, certificates, IP whitelist,
  publish, third-party installs, discard, deploy tokens, GIA roles/policies/members/invites) become a **change set** that a human
  approves in Console. Batch them: `iugu changeset create --op … --op … --submit --wait`.
- Secrets are delivered once, to the requesting CLI, within one hour of approval, through
  `--write-env <file>` (0600, `.gitignore`d), `--exec "<cmd> {secret}"` (in-process substitution, no
  shell, redacted in output) or `--show-secret`. `iugu app credentials list` shows prefixes only.
- Project context lives in `iugu.toml` (app id, name, tag, publisher workspace, development
  workspace/credential); `--app <id>` overrides it.
- `--idempotency-key` for mutating requests; `--yes` for confirmations; `--quiet` silences stderr.

## Command tree

```
login [--device|--non-interactive|--complete <h> --wait] [--scopes …] [--workspace <id>] [--no-browser]
logout · auth status|token · me · workspace list|use <id>
app init --name … --workspace <id> [--wait] · app list|get|update|discard|publish [--billable]
app permissions get|set · app oauth set · app credentials list|create|rotate|revoke
app certificates list|add|revoke · app ip-whitelist set|test <ip> · app entitlements set
app agreements list|publish · app images list|upload|highlight|remove · app deploy-tokens list|create|revoke
app install|uninstall|resync --workspace <id> · app token [--credential <id>] [--scope …] · app env --format dotenv|fly|netlify|vercel|json
changeset create --op <type>:<json>… [--submit] [--wait] · changeset list|show|submit|wait|secrets|withdraw <id>
approvals list|open|wait <id> · verify --principal … --action … · test-principal create|list|delete
gia roles|policies|members|invites (reads) · gia roles|policies create|update|delete · gia members set-roles|remove <id|email> · gia invites create|resend   (Tier 1, scope console:gia.write)
catalog actions [--q text]
agent setup [--claude|--codex|--opencode|--cursor|--vscode|--all] · docs [topic] [--llms] · completion <shell>
```

## Development

```sh
make build      # bin/iugu
make test       # go test ./... (IUGU_TEST_KEYCHAIN=1 also exercises the OS keychain)
make lint       # go vet + staticcheck
make snapshot   # goreleaser snapshot (no publish, no signing)
```

Point the CLI at a Console worktree with `IUGU_API=https://api.console.<slug>.iugu.test` and
`IUGU_CLIENT_ID=<cli client short id>` (printed by Console's `bin/rails oauth_clients:ensure_first_party`).
The AS is discovered from the API (`/.well-known/oauth-protected-resource` → `/.well-known/oauth-authorization-server`).

## License

MIT.
