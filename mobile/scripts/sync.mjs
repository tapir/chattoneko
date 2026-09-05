#!/usr/bin/env node
// sync: build ../web, copy its dist into ./www, then cap sync android.
// The web frontend is the single source of truth — www/ is generated.
import { execFileSync } from "node:child_process";
import { cpSync, rmSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const mobileDir = resolve(dirname(fileURLToPath(import.meta.url)), "..");
const webDir = resolve(mobileDir, "..", "web");
const run = (cmd, args, opts = {}) =>
  execFileSync(cmd, args, { stdio: "inherit", ...opts });

console.log("• building web frontend");
run("npm", ["run", "build"], { cwd: webDir });

console.log("• copying web/dist -> mobile/www");
rmSync(join(mobileDir, "www"), { recursive: true, force: true });
cpSync(join(webDir, "dist"), join(mobileDir, "www"), { recursive: true });

console.log("• cap sync android");
run("npx", ["cap", "sync", "android"], { cwd: mobileDir });

console.log("✓ mobile/www refreshed and synced");
