# Security

## Reporting a vulnerability

Please do not open a public issue for security problems. Report them to **security@iugu.com** (or through
[GitHub's private vulnerability reporting](https://github.com/iugu/cli/security/advisories/new) on this
repository). Include the CLI version (`iugu --version`), your OS, and the steps to reproduce. We acknowledge
reports within two business days and keep you informed until the fix ships.

## What the CLI stores, and where

- **Refresh tokens** — in the OS keychain (macOS Keychain, Secret Service, Windows Credential Manager) under the
  service `iugu-cli`, one entry per profile. When no keychain is available the CLI warns once and falls back to
  `~/.config/iugu/credentials.json` with mode `0600` (`--credentials-store file` forces it).
- **Access tokens** — in memory only, for the duration of one command.
- **Configuration** — `~/.config/iugu/config.json` (`0600`): profiles (API host, preferred workspace); never
  credentials. A project-local store is `IUGU_CONFIG_DIR=.iugu` (auto-gitignored).
- **Secrets delivered by change sets** — written only where you ask (`--write-env <file>` with `0600` and a
  `.gitignore` entry, or `--exec` in-process substitution); the API delivers each secret **once**.
- **`IUGU_TOKEN`** (deploy tokens for CI) is read from the environment and never written to disk.

A secret substituted with `--exec` is redacted from the command echoed in `--json` output. The CLI does not send
telemetry.

## Verifying a release

Every release is built by GitHub Actions from a tag of this repository, signed with
[cosign](https://github.com/sigstore/cosign) (keyless, GitHub OIDC), and ships an SBOM per archive.
`install.sh`, the Homebrew cask and the npm shim verify the downloaded archive against the signed
`checksums.txt`. To verify by hand:

```sh
cosign verify-blob \
  --certificate checksums.txt.pem --signature checksums.txt.sig \
  --certificate-identity-regexp 'github.com/iugu/cli' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum -c checksums.txt --ignore-missing
```

## Supported versions

Only the latest release receives fixes. The `--json` shapes follow the published API contract
(<https://developer.iugu.com/console/api-v1.yaml>); breaking changes are announced in the release notes.
