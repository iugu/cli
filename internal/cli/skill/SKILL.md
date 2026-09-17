---
name: iugu
description: Build, configure, test and publish iugu Platform 2 apps with the `iugu` CLI. Use when a task involves Console apps, OAuth credentials for iugu, /verify authorization, workspaces, or publishing to the iugu app store.
---

# iugu — working with Console from an AI coding session

Always call the CLI with `--json`. Read `error.code` and the exit code: `0` ok · `1` error · `2` usage · `4` login required (`iugu login`) · `5` approval required (payload has `approval.url` and `next_step`) · `6` stale/conflict (re-read, retry). Run one `iugu` command per shell call (no `; echo $?` — your tool already reports the exit code; chained commands trip permission rules).

## Golden path {#golden-path}

1. `iugu auth status --json` — if `logged_in` is false: `iugu login --json` prints `verification_uri_complete` + `user_code`; show them to the human, then run the printed `next_step` (`iugu login --complete <handle> --wait`).
2. `iugu me --json` — pick the development workspace (`workspaces[].id`); `iugu workspace use <id>`.
3. `iugu app init --name "<App>" --workspace <id> --json` — creates the app (private, draft), installs it in the dev workspace, requests a **workspace-restricted credential** (Tier 1) and writes `iugu.toml`. Exit 5 is normal: give `approval.url` to the human, then `iugu changeset wait <id> --write-env .env.local --json` (it also replaces `credential = 'pending:…'` in `iugu.toml`). Never ask the human for the secret; it lands in `.env.local` (0600). If your session ends before the human approves, run the same `wait` when they come back — secrets stay collectable for one hour after approval; after that, request a new credential.
4. `iugu app permissions set --implemented a,b --consumed tag:action --grant-scopes informations --json` (Tier 0). Find consumable actions with `iugu catalog actions --q <text> --json`.
5. Test authorization without real accounts: `iugu test-principal create --name qa --json` → `iugu verify --principal temp:… --action <tag>:<action> --json`. `iugu app token` mints a short-lived app token for `/verify`, `/userinfo`; use it through a sink so it never lands in the transcript: `iugu app token --exec "curl -sS -H 'Authorization: Bearer {token}' https://…/userinfo"` or `--write-env .env.local`.
6. Deploy the code anywhere (Fly, Netlify, Vercel…); `iugu app env --format fly` prints the non-secret environment; pipe secrets with `iugu changeset secrets <id> --exec "fly secrets set IUGU_CLIENT_SECRET={secret}"`.
7. Release: one change set → one approval: `iugu changeset create --op 'oauth.update:{…callbacks…}' --op 'listing.publish:{"public":true,"draft":false}' --op 'credentials.create:{"name":"prod"}' --submit --wait --write-env .env.production --json`.

Pending work: `iugu approvals list --json` (what still needs a human); `iugu changeset list --json` (compact history; `show <id>` for details).

Workspace access (GIA) — reads are Tier 0, every write is Tier 1 and needs the `console:gia.write` scope: `iugu gia roles|policies|members|invites --json`; `iugu gia policies create --name Invoices --actions <tag>:invoice.read,<tag>:invoice.create`; `iugu gia roles create --name Support --policies <policy-id>`; `iugu gia members set-roles <member-id|email> --roles <role-id>`; `iugu gia invites create --email dev@acme.test --roles <role-id>`. Batch several in one approval with `iugu changeset create --op gia.roles.upsert:{…} --op gia.invites.create:{…} --submit` (ids must exist before the batch).

## Tiers — what needs a human {#tiers}

Tier 0 (agent alone, within scope and the developer's permissions): read anything; create app; name/description/images/categories; permissions; entitlements; agreements; install/uninstall/resync **your own** apps; temp principals; policy simulator; own-app tokens; prepare change sets.
Tier 1 (human approval in the browser, with a verification code): credentials (create/rotate/revoke), OAuth URL/callbacks, certificates, IP whitelist, publish/billable, third-party installs, discarding an app, deploy tokens, GIA changes. The CLI returns exit 5 with `approval.url`; poll with `iugu changeset wait <id>`.
Tier 2 (iugu staff only): restricted entitlements, featured, blocks.

## Secrets {#secrets}

Secrets are delivered once, only to this CLI, within one hour of approval. Use `--write-env <file>` (0600, git-ignored), `--exec "<cmd> {secret}"` (substituted in-process) or, last resort, `--show-secret`. Never paste a secret into chat, a commit or a log. `iugu app credentials list` shows only prefixes.

## Integration facts {#integration}

`iugu app init --json` and `iugu app env --format json` print: `issuer`, `authorization_url`, `token_url`, `jwks_url`, `verify_url`, `userinfo_url`, `client_id` (= app id), `app_tag`, `workspace_id`, ACR values (`urn:iugu:grant_scopes:informations|config|transfers`). Users log in through the authorization code flow with PKCE; the app authenticates at `token_url` with client_id + client_secret; app-to-app calls use `client_credentials`; `/verify` takes `workspace_id`, `principals[]`, `actions[]` and answers per action. Actions your app implements are `<app_tag>:<action>`.

## Sandboxes and harness quirks {#sandboxes}

- **No network** (Codex `workspace-write` default, some CI): every `iugu` call fails with a connection error and a hint. Ask the human to enable outbound network for the sandbox (Codex: `sandbox_workspace_write.network_access = true`) or to run the command outside it.
- **Login cannot be updated** (`store_not_writable`: the keychain or `~/.config/iugu` is read-only from the sandbox): the CLI refuses to rotate the refresh token so the human's login stays valid. Keep your own login inside the project instead: `IUGU_CONFIG_DIR=.iugu iugu login --json` (device flow; `.iugu/` is git-ignored automatically), then prefix every `iugu` command with `IUGU_CONFIG_DIR=.iugu`. It is a separate credential holder with its own grant: logging it out never affects the human's terminal.
- **Scopes**: `insufficient_scope` means the grant lacks the scope of the operation (GIA writes need `console:gia.write`). Re-consent: `iugu login --scopes "<current scopes> console:gia.write"`.
- **Turn boundaries**: background `iugu changeset wait` processes die when your turn ends; when resumed, re-run the same `wait` (secrets stay collectable for one hour after approval).
- **One command per shell call**; no `; echo $?` chains — your tool reports the exit code and chained commands trip permission rules.
- **Claude Code**: `Bash(iugu:*)` in `permissions.allow` avoids prompts; project settings apply only after the workspace is trusted once.

## Exit codes {#exit-codes}

0 ok · 1 error · 2 usage or cancelled · 4 authentication required · 5 approval required (payload printed) · 6 stale or conflict.
