// Launches Playwright MCP on Camoufox with a fresh fingerprint per process.
// Usage: xvfb-run -a node wrap.mjs [playwright-mcp flags...]
// Camoufox runs headed (better fingerprint), so it needs an X display; xvfb-run -a provides one.
import { spawn } from "node:child_process";
import { createRequire } from "node:module";
import { rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { launchOptions } from "camoufox-js";

const require = createRequire(import.meta.url);
const mcpCli = join(dirname(require.resolve("@playwright/mcp/package.json")), "cli.js");

const o = await launchOptions({ headless: false, os: "windows", humanize: true });
// The fingerprint travels in CAMOU_CONFIG_* env vars; the browser inherits them via the MCP child.
for (const [k, v] of Object.entries(o.env)) if (k.startsWith("CAMOU_")) process.env[k] = v;

// One config per process: concurrent MCP instances must not overwrite each other's fingerprint.
const cfg = join(tmpdir(), `camoufox-mcp-${process.pid}.json`);
writeFileSync(cfg, JSON.stringify({
  browser: {
    browserName: "firefox",
    launchOptions: { executablePath: o.executablePath, args: o.args, firefoxUserPrefs: o.firefoxUserPrefs, headless: false },
  },
}));

const child = spawn(process.execPath, [mcpCli, "--config", cfg, ...process.argv.slice(2)], { stdio: "inherit" });
for (const sig of ["SIGINT", "SIGTERM", "SIGHUP"]) process.on(sig, () => child.kill(sig));
child.on("exit", (code, signal) => {
  rmSync(cfg, { force: true });
  process.exit(code ?? (signal ? 1 : 0));
});
