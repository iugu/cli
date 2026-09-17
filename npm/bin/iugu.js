#!/usr/bin/env node
// Runs the downloaded binary, forwarding argv, stdio and the exit code (0/1/2/4/5/6 are meaningful).
"use strict";
const path = require("node:path");
const fs = require("node:fs");
const { spawnSync } = require("node:child_process");

const bin = path.join(__dirname, process.platform === "win32" ? "iugu.exe" : "iugu");
if (!fs.existsSync(bin)) {
  console.error("@iugu/cli: binary missing; run `npm rebuild @iugu/cli` (postinstall downloads it) or install with Homebrew / install.sh");
  process.exit(1);
}
const r = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
if (r.error) {
  console.error(`@iugu/cli: ${r.error.message}`);
  process.exit(1);
}
process.exit(r.status === null ? 1 : r.status);
