# AGENTS.md — working in the `iugu` CLI repository

This file is for AI coding agents and humans alike. The product is a single Go binary, `iugu`, the
local surface of the Platform 2 Console for developers and their agents (plan §8, Console repo
`docs/tmp/plan/05-cli.md`). It talks only to the Lifecycle API and the OAuth endpoints described in
Console's `docs/public/api-v1.yaml`; the contract there wins over anything in this repository.

## Layout

| Path | What |
|---|---|
| `cmd/iugu/main.go` | entry point: `os.Exit(cli.Execute(args))` |
| `internal/cli/` | cobra commands. `root.go` (runtime, flags, exit-code mapping), `login.go`, `auth_cmds.go`, `app.go`, `app_security.go` (Tier 1 app settings), `changeset.go` (+ approvals), `pending.go` (202 handling, `wait`, secret delivery: `--write-env`, `--exec`, `--show-secret`), `testing_cmds.go` (verify, test principals, gia, catalog), `agent.go` (`agent setup`, `docs`) |
| `internal/cli/skill/` | embedded `SKILL.md`, `llms.txt`, `AGENTS.snippet.md` — what `iugu docs` prints and `agent setup` installs |
| `internal/auth/` | discovery (RFC 9728 → RFC 8414), PKCE, loopback redirect, device flow, token refresh/revoke, `Session` |
| `internal/store/` | credential stores: keychain (with timeout, namespaced per config dir), 0600 file, memory, `Fallback` (keychain → file, never a second copy of an existing login) |
| `internal/api/` | HTTP client for `/v1` (bearer, `Idempotency-Key`, `If-Match`, error decoding, cursor pagination) |
| `internal/config/` | `~/.config/iugu/config.json` profiles, `iugu.toml` project file, env names |
| `internal/output/` | `--json`/`--jq` printing, tables, NDJSON, exit codes |
| `internal/agentsetup/` | writes remote-MCP entries and skills for Claude Code, Codex, OpenCode, Cursor, VS Code |
| `npm/`, `install.sh`, `.goreleaser.yaml`, `scripts/` | distribution: npm shim, `curl \| sh`, goreleaser (Homebrew cask, Scoop, cosign, SBOM) |

## Build and test

```sh
export PATH=/opt/homebrew/bin:$PATH   # macOS: Homebrew Go (1.27) must win over a stale /usr/local/go
make build && ./bin/iugu --help
make test                             # unit tests with fake AS + API servers; ~15 s (device-flow timing)
IUGU_TEST_KEYCHAIN=1 go test ./internal/store/   # also touches the real keychain (service iugu-cli-test)
make lint                             # gofmt, go vet, staticcheck
make snapshot                         # goreleaser --snapshot (no publish/sign); needs goreleaser on PATH
```

End to end against a Console worktree (the Console repo has the Playwright script
`e2e/cli-e2e.mjs` used for the Phase 2 review): `IUGU_API=https://api.console.<slug>.iugu.test
IUGU_CLIENT_ID=<cli client short id> ./bin/iugu login --no-browser --json`. Go trusts the local Caddy
CA through the system keychain; Node needs `NODE_EXTRA_CA_CERTS`.

## Rules

- **Never print a secret** outside the three explicit sinks (`--write-env`, `--exec`, `--show-secret`),
  never log tokens, never write `IUGU_TOKEN` to disk. Redact `{secret}` in echoed commands.
- **Stable JSON.** Every command's `--json` shape is part of the contract with agents; add fields, do
  not rename or remove. Empty lists are `[]`, never `null`. URLs are not HTML-escaped.
- **Exit codes** are semantic: 0 ok · 1 error · 2 usage/cancelled · 4 login required · 5 approval
  required (payload printed) · 6 stale/conflict. Map new failure modes onto these.
- Help text is read by LLMs: spell out the non-interactive equivalent of every prompt and the JSON shape.
- Agent detection (`--agent auto`) must stay conservative: non-TTY or a known harness variable.
  Interactive prompts are only allowed when `!rt.IsAgent()`.
- Keychain calls go through `store.Keyring` (bounded by `KeyringTimeout`); tests never touch the real
  `iugu-cli` service — use `iugu-cli-test`.
- **Never rotate a refresh token you cannot persist**: `Session.Fresh` writes the session back before calling
  the AS; keep that order. Each config dir is a credential holder (`device_id`); do not share a store between holders.
- Dependencies: keep the tree small (cobra, go-keyring, gojq, toml). No telemetry.
- Commit messages in Portuguese, imperative, one topic per commit. Pushing is disabled in this checkout.
