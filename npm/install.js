#!/usr/bin/env node
// postinstall: download the iugu release matching this package's version for the current platform,
// verify its SHA-256 against checksums.txt (signed with cosign in the release), unpack into bin/.
// Env: IUGU_CLI_VERSION (override), IUGU_CLI_SKIP_DOWNLOAD=1 (e.g. when the binary is already on PATH),
// IUGU_CLI_REPO (default iugu-private/platform2-cli), IUGU_CLI_BASE_URL (mirror of the release assets).
"use strict";
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const https = require("node:https");
const http = require("node:http");
const crypto = require("node:crypto");
const zlib = require("node:zlib");
const { execFileSync } = require("node:child_process");

if (process.env.IUGU_CLI_SKIP_DOWNLOAD === "1") process.exit(0);

const pkg = require("./package.json");
const version = (process.env.IUGU_CLI_VERSION || pkg.version).replace(/^v/, "");
if (version === "0.0.0-development") {
  console.error("@iugu/cli: development checkout; set IUGU_CLI_VERSION or IUGU_CLI_SKIP_DOWNLOAD=1");
  process.exit(0);
}
const repo = process.env.IUGU_CLI_REPO || "iugu-private/platform2-cli";
const platform = { darwin: "darwin", linux: "linux", win32: "windows" }[process.platform];
const arch = { x64: "amd64", arm64: "arm64" }[process.arch];
if (!platform || !arch) {
  console.error(`@iugu/cli: unsupported platform ${process.platform}/${process.arch}`);
  process.exit(1);
}
const ext = platform === "windows" ? "zip" : "tar.gz";
const archive = `iugu_${version}_${platform}_${arch}.${ext}`;
const base = process.env.IUGU_CLI_BASE_URL || `https://github.com/${repo}/releases/download/v${version}/`;

function fetch(url, redirects = 5) {
  return new Promise((resolve, reject) => {
    const client = url.startsWith("http://") ? http : https;
    client.get(url, { headers: { "user-agent": `@iugu/cli npm shim ${pkg.version}` } }, (res) => {
      if ([301, 302, 303, 307, 308].includes(res.statusCode) && res.headers.location && redirects > 0) {
        res.resume();
        return resolve(fetch(new URL(res.headers.location, url).toString(), redirects - 1));
      }
      if (res.statusCode !== 200) {
        res.resume();
        return reject(new Error(`${url}: HTTP ${res.statusCode}`));
      }
      const chunks = [];
      res.on("data", (c) => chunks.push(c));
      res.on("end", () => resolve(Buffer.concat(chunks)));
      res.on("error", reject);
    }).on("error", reject);
  });
}

function untarSingle(buf, name) {
  // minimal tar reader: find the entry `name` and return its bytes
  let off = 0;
  while (off + 512 <= buf.length) {
    const header = buf.subarray(off, off + 512);
    if (header.every((b) => b === 0)) break;
    const entryName = header.subarray(0, 100).toString().replace(/\0.*$/, "");
    const size = parseInt(header.subarray(124, 136).toString().replace(/\0.*$/, "").trim(), 8);
    const start = off + 512;
    if (entryName === name || entryName.endsWith("/" + name)) return buf.subarray(start, start + size);
    off = start + Math.ceil(size / 512) * 512;
  }
  throw new Error(`${name} not found in archive`);
}

(async () => {
  const binDir = path.join(__dirname, "bin");
  fs.mkdirSync(binDir, { recursive: true });
  const target = path.join(binDir, platform === "windows" ? "iugu.exe" : "iugu");
  const [data, sums] = await Promise.all([fetch(base + archive), fetch(base + "checksums.txt")]);
  const line = sums.toString().split("\n").find((l) => l.trim().endsWith(" " + archive) || l.trim().endsWith("  " + archive));
  if (!line) throw new Error(`${archive} missing from checksums.txt`);
  const expected = line.trim().split(/\s+/)[0];
  const actual = crypto.createHash("sha256").update(data).digest("hex");
  if (expected !== actual) throw new Error(`checksum mismatch for ${archive}: expected ${expected}, got ${actual}`);

  let binary;
  if (ext === "tar.gz") {
    binary = untarSingle(zlib.gunzipSync(data), "iugu");
  } else {
    // zip: delegate to the platform's expander to avoid a dependency (Windows ships tar with zip support)
    const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "iugu-cli-"));
    const zipPath = path.join(tmp, archive);
    fs.writeFileSync(zipPath, data);
    execFileSync("tar", ["-xf", zipPath, "-C", tmp, "iugu.exe"], { stdio: "inherit" });
    binary = fs.readFileSync(path.join(tmp, "iugu.exe"));
    fs.rmSync(tmp, { recursive: true, force: true });
  }
  fs.writeFileSync(target, binary, { mode: 0o755 });
  console.log(`@iugu/cli: installed iugu ${version} (${platform}/${arch}, sha256 verified)`);
})().catch((err) => {
  console.error(`@iugu/cli: ${err.message}`);
  process.exit(1);
});
