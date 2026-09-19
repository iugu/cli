---
name: iugu
description: Build, configure, test and publish iugu Platform 2 apps with the `iugu` CLI. Use when a task involves Console apps, OAuth credentials for iugu, /verify authorization, workspaces, or publishing to the iugu app store.
---

# iugu — working with Console from an AI coding session

Always call the CLI with `--json`. Read `error.code` and the exit code: `0` ok · `1` error · `2` usage · `4` login required (`iugu login`) · `5` approval required (payload has `approval.url` and `next_step`) · `6` stale/conflict (re-read, retry) · `7` **the human must act on this agent's authorization first** (payload has `url` and `next_step`): `elevation_required` — the developer verifies their identity (MFA) on `url`, then you re-run the same command (the verification lasts 15 minutes); `insufficient_scope` / `workspace_not_consented` — the developer widens the authorization on `url` (no new login), then you re-run. Never try to log in again to fix a 7. Run one `iugu` command per shell call (no `; echo $?` — your tool already reports the exit code; chained commands trip permission rules).

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

If the credential was requested through the **MCP server** (not this CLI), the secret still lands here: `iugu changeset secrets <change_set_id> --write-env .env.local` works for the same developer's logged-in CLI — the model never sees the value.

## Actions your app publishes for agents and Workflow {#actions}

An app can publish **actions** (and triggers) that Workflow and **Iugu for AI** (the remote MCP server) call on a person's behalf: serve `GET {service_api_url}/actions` with `spec: iugu.actions/v1` — per action `name`, `title`, `label`, `description`, `url` (same origin as the service URL), `method`, `parameters[]` (`direction` ParamInput/ParamOutput, `type`, `source` query|path|body) and `authorization: { action: "<app_tag>:<action>", acr: informations|config|transfers }` — then opt in with `iugu app entitlements set actions_provider`. Console accepts only actions whose `authorization.action` is one of the app's implemented actions.

- **Display hints** (optional, per input parameter): `label` (≤ 40 chars), `format` (`money_cents`, `money`, `date`, `datetime`, `percent`, `count`, `text`) and, for a `json` array of flat objects, `items: { "<column>": { "label", "format" } }` in the order the columns should appear. They decide how Console shows the arguments to the person who **confirms** an elevated call (and in the workspace audit): labelled fields, arrays as tables, money formatted and **totalled by Console**. Declare them for anything a person approves in bulk — without them the person sees raw keys and cents.
- `iugu app actions list --json` (`--refresh` after changing the document): what Console accepted (`actions[].tool_name` = `<tag>__<name>` with the tag's hyphens as underscores — the name every MCP client shows the model; the schemas it derived) and every `problems[]` entry (path + reason) that excluded something. Fix the manifest until `problems` is empty.
- `iugu app actions call <name> --input '{…}' --json` (or `--arg k=v`): performs the action against your endpoint **as the app itself** (client_credentials token, `Workspace` header), the way a consumer would — path/query/body per the manifest. Exit 0 on 2xx; the payload has `request` and `response`. A `401 insufficient_user_authentication` with `step_up_human: true` means your endpoint enforces the `acr` for humans — correct: app tokens carry no `acr`; through Iugu for AI the person confirms that exact call on a Console page and the call is made with their identity. `--token <jwt>` calls with another token (e.g. a user token from your own login) to exercise those rules.

Your endpoint must verify the bearer token (JWKS, `iss`, `aud = Iugu.Platform.<client_id>`, `typ at+JWT`) and call `/verify` with `token.sub` for `authorization.action` — the principal is a person (`user:…`) or an app acting as itself (`app:…`); the app that carried the call (`token.client_id`: Iugu for AI, a Workflow) is attribution to record, never something to authorize. A consumer can never do what its principal cannot. Bridge tokens have no session — never rely on `/userinfo` for them.

## Billing of your app {#billing}

Billing is an **actions provider**: its plan configuration and reports are actions of the app `billing`, called **as you** through
Console's bridge — `iugu call billing <action> …` here, tools `billing__<action>` in Iugu for AI. Console verifies your own permission for
the action in the workspace (`billing:plans.read|edit|publish|discard`, `billing:reports.read`, `billing:invoices.show`) and calls Billing
with your identity; Billing answers as it is. A **plan** has **versions**; a version has **prices** (`unit` — amount per event; `package` —
amount per `size` events; `bulk` / `tiered` — `tiers: [{ amount, maximum_count }]`, the last tier with `maximum_count: null`); amounts are
strings in BRL; each price names the **event** your app reports to Billing. The **default** version prices every new subscription; existing
subscriptions keep theirs. Statuses: `draft` (editable) → `published` (frozen; create a new version to change prices).

- Discover: `iugu app actions list --app billing --json` (every action with its inputs) — or `iugu catalog actions --q billing`.
- Read (no confirmation): `iugu call billing get_plan --arg app_id=<your app>` · `get_plan_version --arg plan_version_id=<v>` ·
  `preview_pricing --arg plan_version_id=<v> --input '{"quantities":{"<event>":<n>}}'` (what a month of usage would cost — run it before
  publishing) · `get_events_summary --arg app_id=<your app> [--arg from=YYYY-MM-DD]` (usage as Billing counts it; an event name no price
  uses shows here and is never priced) · `get_revenue` · `list_invoices [--arg competency=YYYY-MM]` · `get_invoice --arg invoice_id=<id>` ·
  `get_pending_amount` · `list_subscriptions`. The workspace is `--workspace` (default: the project's / profile's): the app's **publisher**
  for plan operations and revenue, the **customer** workspace for invoices and pending amount.
- Configure (no confirmation; drafts have no effect until published): `create_plan --arg app_id=<your app>` (first time; empty draft v1) ·
  `create_plan_version --arg plan_id=<p> [--arg clone_from=<v>]` (one draft at a time: 409 `draft_exists` with `details.draft_version_id`) ·
  `set_prices --arg plan_version_id=<v> --input '{"prices":[{"name","event","model","config"}]}'` (declarative, the whole list) ·
  `add_price|update_price|remove_price`.
- Change what customers pay — **the person confirms each call** (exit 7 with `url`; show it, they see the prices in Billing's words and
  confirm with a verification code; then re-run the same command): `publish_version --arg plan_version_id=<v> [--arg set_as_default=true]`
  (the app must be billable: `iugu app publish --billable`; 422 `not_billable`) · `set_default_version --arg plan_id=<p> --arg plan_version_id=<v>` ·
  `discard_plan --arg plan_id=<p>` (cancels every active subscription — say so to the person).
- Errors (from Billing, passed through): `version_published` (409) — create a new version instead; `draft_exists` (409); `already_published`,
  `no_prices`, `invalid_prices`, `not_billable`, `version_not_eligible` (422); `validation_failed` (422, `details` per field); `provider_unavailable`
  (502) — Billing did not answer.
- Metering, in the app's own code (the only Billing call an app makes): `iugu app permissions set --consumed billing:event.create,billing:event.show` (discover with `iugu catalog actions --q billing`), then `POST {billing}/api/events` as the app itself (client_credentials with `audience=Iugu.Platform.33qqIXOLGKohFVkWBN1Apf`, header `Workspace-Id` = the workspace where the usage happened) with `{"events":[{"name","idempotency_key","timestamp","custom_unit_quantity"?,"tpv"?,"test"?}]}`. `name` must equal a price's `event`; `test: true` events are stored but never invoiced (sandbox). `idempotency_key` (≤ 60 chars) is unique per app and workspace — make it deterministic from the billed entity (`<tag>:<entity>:<suffix>`; the tag prefix keeps keys readable in reports) and never random per attempt: a key Billing already has answers 201 with the key absent from `created`, which is how a safe retry looks. Reference implementation: the sample app's `lib/billing.js`.

## Sandboxes and harness quirks {#sandboxes}

- **No network** (Codex `workspace-write` default, some CI): every `iugu` call fails with a connection error and a hint. Ask the human to enable outbound network for the sandbox (Codex: `sandbox_workspace_write.network_access = true`) or to run the command outside it.
- **Login cannot be updated** (`store_not_writable`: the keychain or `~/.config/iugu` is read-only from the sandbox): the CLI refuses to rotate the refresh token so the human's login stays valid. Keep your own login inside the project instead: `IUGU_CONFIG_DIR=.iugu iugu login --json` (device flow; `.iugu/` is git-ignored automatically), then prefix every `iugu` command with `IUGU_CONFIG_DIR=.iugu`. It is a separate credential holder with its own grant: logging it out never affects the human's terminal.
- **Scopes**: `insufficient_scope` means the grant lacks the scope of the operation (GIA writes need `console:gia.write`). Re-consent: `iugu login --scopes "<current scopes> console:gia.write"`.
- **Turn boundaries**: background `iugu changeset wait` processes die when your turn ends; when resumed, re-run the same `wait` (secrets stay collectable for one hour after approval).
- **One command per shell call**; no `; echo $?` chains — your tool reports the exit code and chained commands trip permission rules.
- **Claude Code**: `Bash(iugu:*)` in `permissions.allow` avoids prompts; project settings apply only after the workspace is trusted once.
- **Iugu for AI through Codex**: tools flagged destructive (every `config`/`transfers` action, Tier 1 requests) make Codex ask the person before the call — interactive Codex shows a y/n prompt, no configuration needed. Headless `codex exec` runs with `approval_policy = never` and refuses them ("MCP tool call requires approval, but approval policy is never") **before the server is reached**; use `codex exec --approve-for-me`, or per server `[mcp_servers.iugu] default_tools_approval_mode = "approve"` — Iugu for AI still makes the person confirm the exact call on a Console page (`step_up_required` with the URL). Clients prefix tool names (`mcp__iugu__<tool_name>` in Codex and Claude Code): look the tool up by its suffix rather than assuming it is missing. Never set `required = true` on the server entry — Codex would refuse to start while Iugu for AI is unreachable.

## Exit codes {#exit-codes}

0 ok · 1 error · 2 usage or cancelled · 4 authentication required · 5 approval required (payload printed) · 6 stale or conflict · 7 human must act on the grant first (verify identity or widen the authorization; payload has `url`, then re-run the same command).

## Who approves what {#approval-policy}

Every change set carries `approval_policy`: `four_eyes` (the requester cannot approve their own change — e.g. a member changing their own roles; someone else with the permissions must), `approver_actions` (what the approver must hold), `requester_elevation` (step-up levels the requester proved before requesting; all GIA writes require `config`). Tell the human who can approve when `four_eyes` is true.
