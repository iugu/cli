## iugu (Platform 2 Console)
- Use the `iugu` CLI with `--json`; exit 4 → `iugu login`; exit 5 → show `approval.url` to a human, then `iugu changeset wait <id>`.
- Never ask for or paste secrets: `--write-env .env.local` / `--exec "… {secret}"` deliver them once.
- Project context lives in `iugu.toml` (committed); secrets in `.env.local` (git-ignored).
- `iugu docs` explains tiers (what needs a human) and the integration facts (issuer, endpoints, ACR values).
