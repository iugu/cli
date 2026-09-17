# @iugu/cli

npm shim for the [iugu CLI](https://github.com/iugu-private/platform2-cli): `postinstall` downloads the
release binary for your platform from GitHub Releases, verifies its SHA-256 against the cosign-signed
`checksums.txt`, and `npx @iugu/cli …` (or the `iugu` bin) forwards to it.

```
npx @iugu/cli login
npx @iugu/cli app init --name "Acme Invoices" --workspace <id> --json
```

Prefer Homebrew (`brew install iugu-private/tap/iugu`) or `install.sh` for a system-wide binary.
